// Package github implements forge.Forge for github.com using the GitHub REST API.
// This file provides GitHub App authentication via RS256 JWT and installation access tokens.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/forge"
)

// AppClient authenticates as a GitHub App using RS256 JWTs and caches
// installation access tokens.
type AppClient struct {
	AppID      int64
	privateKey *rsa.PrivateKey
	mu         sync.Mutex
	tokenCache map[int64]cachedToken // keyed by installation ID
}

type cachedToken struct {
	token     string
	expiresAt time.Time
}

// NewAppClient parses a PEM-encoded RSA private key and returns an AppClient.
// Both PKCS8 and PKCS1 key formats are supported.
func NewAppClient(appID int64, privateKeyPEM []byte) (*AppClient, error) {
	block, _ := pem.Decode(privateKeyPEM)
	if block == nil {
		return nil, errors.New("github app: failed to decode PEM block")
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("github app: parse PKCS1 key: %w", err)
		}
		key = k
	default:
		// Try PKCS8.
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("github app: parse PKCS8 key: %w", err)
		}
		rsaKey, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("github app: key is not RSA")
		}
		key = rsaKey
	}
	return &AppClient{
		AppID:      appID,
		privateKey: key,
		tokenCache: make(map[int64]cachedToken),
	}, nil
}

// generateJWT creates a signed RS256 JWT for GitHub App authentication.
func (a *AppClient) generateJWT() (string, error) {
	now := time.Now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, err := json.Marshal(struct {
		IAT int64  `json:"iat"`
		EXP int64  `json:"exp"`
		ISS string `json:"iss"`
	}{
		IAT: now.Add(-60 * time.Second).Unix(),
		EXP: now.Add(9 * time.Minute).Unix(),
		ISS: strconv.FormatInt(a.AppID, 10),
	})
	if err != nil {
		return "", err
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := header + "." + encodedPayload
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("github app: sign JWT: %w", err)
	}
	encodedSig := base64.RawURLEncoding.EncodeToString(sig)
	return signingInput + "." + encodedSig, nil
}

// InstallationToken returns a cached or freshly-obtained installation access token.
func (a *AppClient) InstallationToken(ctx context.Context, installationID int64) (string, error) {
	a.mu.Lock()
	if cached, ok := a.tokenCache[installationID]; ok {
		if time.Until(cached.expiresAt) > 5*time.Minute {
			token := cached.token
			a.mu.Unlock()
			return token, nil
		}
	}
	a.mu.Unlock()

	jwt, err := a.generateJWT()
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("github app token: status %d: %s", resp.StatusCode, data)
	}
	var tokenResp struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(data, &tokenResp); err != nil {
		return "", fmt.Errorf("github app token: parse response: %w", err)
	}

	a.mu.Lock()
	a.tokenCache[installationID] = cachedToken{token: tokenResp.Token, expiresAt: tokenResp.ExpiresAt}
	a.mu.Unlock()

	return tokenResp.Token, nil
}

// DeleteInstallation removes the app installation, effectively uninstalling it.
// Used to reject installs from non-allowlisted owners.
func (a *AppClient) DeleteInstallation(ctx context.Context, installationID int64) error {
	jwt, err := a.generateJWT()
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://api.github.com/app/installations/%d", installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("github app delete installation: status %d: %s", resp.StatusCode, data)
	}
	return nil
}

// RepoInstallation returns the installation ID for the app on the given repository.
// This is used to obtain an installation token when no installation ID is cached.
func (a *AppClient) RepoInstallation(ctx context.Context, owner, repo string) (int64, error) {
	jwt, err := a.generateJWT()
	if err != nil {
		return 0, err
	}
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/installation", owner, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("github app repo installation: status %d: %s", resp.StatusCode, data)
	}
	var installResp struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(data, &installResp); err != nil {
		return 0, fmt.Errorf("github app repo installation: parse response: %w", err)
	}
	return installResp.ID, nil
}

// ForgeClient returns a forge.Forge authenticated with an installation access token.
func (a *AppClient) ForgeClient(ctx context.Context, installationID int64) (forge.Forge, error) {
	token, err := a.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	return &Client{Token: token}, nil
}

// PostComment posts a comment on the given issue or pull request using an installation token.
func (a *AppClient) PostComment(ctx context.Context, installationID int64, owner, repo string, issueNumber int, body string) error {
	token, err := a.InstallationToken(ctx, installationID)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(struct {
		Body string `json:"body"`
	}{Body: body})
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d/comments", owner, repo, issueNumber)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		respData, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("github app post comment: status %d: %s", resp.StatusCode, respData)
	}
	return nil
}

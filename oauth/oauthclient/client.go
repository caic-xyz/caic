// Authorization-code client helpers and forge provider configurations.

package oauthclient

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/caic-xyz/caic/oauth"
)

// exchangeTimeout bounds a single token-exchange request so a hung provider
// cannot pin a goroutine indefinitely. It is applied via the request context,
// so a shorter deadline already on the caller's context still wins.
const exchangeTimeout = 30 * time.Second

// PKCEChallenge holds an S256 PKCE verifier and its derived code challenge (RFC 7636).
type PKCEChallenge struct {
	Verifier  string // the secret code_verifier, kept by the caller for the token exchange
	Challenge string // the S256 code_challenge sent on the authorization request
}

// NewPKCEChallenge generates a random S256 PKCE verifier and challenge.
//
// PKCE is recommended for all OAuth flows (GitHub supports it since 2025-07,
// GitLab has supported it for years). Callers that opt in pass the Challenge
// to AuthorizationURL and the Verifier to ExchangeCode.
func NewPKCEChallenge() (PKCEChallenge, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return PKCEChallenge{}, fmt.Errorf("generate pkce verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw[:])
	sum := sha256.Sum256([]byte(verifier))
	return PKCEChallenge{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
	}, nil
}

// AuthorizationURL returns the provider authorization URL with the state param.
//
// When codeChallenge is non-empty, S256 PKCE parameters are added; when empty,
// the emitted URL is byte-for-byte identical to a flow without PKCE.
func AuthorizationURL(authEndpoint, clientID, redirectURI string, scopes []string, state, codeChallenge string) string {
	v := url.Values{}
	v.Set("client_id", clientID)
	v.Set("redirect_uri", redirectURI)
	v.Set("scope", strings.Join(scopes, " "))
	v.Set("state", state)
	v.Set("response_type", oauth.ResponseTypeCode)
	if codeChallenge != "" {
		v.Set("code_challenge", codeChallenge)
		v.Set("code_challenge_method", "S256")
	}
	return authEndpoint + "?" + v.Encode()
}

// ExchangeCode exchanges an authorization code for tokens.
//
// When codeVerifier is non-empty it is sent as the PKCE code_verifier;
// when empty the request body is identical to a flow without PKCE.
func ExchangeCode(ctx context.Context, c oauth.ClientConfig, code, codeVerifier string) (*oauth.Token, error) {
	body := url.Values{}
	body.Set("grant_type", oauth.GrantAuthorizationCode)
	body.Set("code", code)
	body.Set("redirect_uri", c.RedirectURI)
	body.Set("client_id", c.ClientID)
	body.Set("client_secret", c.ClientSecret)
	if codeVerifier != "" {
		body.Set("code_verifier", codeVerifier)
	}
	return doTokenExchange(ctx, c, body)
}

// RefreshAccessToken exchanges a refresh token for a new access token.
//
// It sends grant_type=refresh_token per RFC 6749 §6 and returns the new
// token. The returned token may include a new refresh token (rotation) or
// omit it (meaning the stored refresh token remains valid).
func RefreshAccessToken(ctx context.Context, c oauth.ClientConfig, refreshToken string) (*oauth.Token, error) {
	body := url.Values{}
	body.Set("grant_type", oauth.GrantRefreshToken)
	body.Set("refresh_token", refreshToken)
	body.Set("client_id", c.ClientID)
	body.Set("client_secret", c.ClientSecret)
	return doTokenExchange(ctx, c, body)
}

// doTokenExchange sends a token request and parses the response into a Token.
func doTokenExchange(ctx context.Context, c oauth.ClientConfig, body url.Values) (*oauth.Token, error) {
	ctx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, strings.NewReader(body.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Keep the raw provider body out of the returned (potentially
		// user-visible / info-logged) error; send it to a debug log only.
		slog.DebugContext(ctx, "oauth token exchange non-200", "status", resp.StatusCode, "body", string(data))
		return nil, fmt.Errorf("token exchange failed with status %d", resp.StatusCode)
	}

	var tr codeTokenResponse
	if err := json.Unmarshal(data, &tr); err != nil {
		return nil, fmt.Errorf("parse token response: %w", err)
	}
	if tr.Error != "" {
		return nil, fmt.Errorf("oauth error: %s", tr.Error)
	}
	if tr.AccessToken == "" {
		return nil, errors.New("no access_token in response")
	}
	if tr.TokenType == "" {
		tr.TokenType = "Bearer"
	}

	var exp time.Time
	if tr.ExpiresIn > 0 {
		exp = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	return &oauth.Token{
		AccessToken:  tr.AccessToken,
		TokenType:    tr.TokenType,
		RefreshToken: tr.RefreshToken,
		Expiry:       exp,
	}, nil
}

type codeTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	Error        string `json:"error,omitempty"`
}

// Authorization-code client helpers and provider configurations.

// Package oauthclient provides an OAuth 2.0 authorization-code client
// for third-party providers. It is dependency-free.
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
	"strconv"
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

// RetrieveError is returned when the token endpoint responds with a
// non-2xx status or includes an RFC 6749 error parameter.
type RetrieveError struct {
	StatusCode       int    // HTTP status code from the token endpoint
	ErrorCode        string // RFC 6749 "error"
	ErrorDescription string // RFC 6749 "error_description"
	ErrorURI         string // RFC 6749 "error_uri"
	Body             []byte // raw response body for programmatic inspection
}

// Error formats the error for logging. It includes StatusCode, ErrorCode,
// and ErrorDescription but not the raw body.
func (e *RetrieveError) Error() string {
	msg := fmt.Sprintf("oauth2: token endpoint error (status %d)", e.StatusCode)
	if e.ErrorCode != "" {
		msg += ": " + e.ErrorCode
	}
	if e.ErrorDescription != "" {
		msg += ": " + e.ErrorDescription
	}
	return msg
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

	// Parse JSON response first (the common case).
	var tr codeTokenResponse
	jsonErr := json.Unmarshal(data, &tr)

	if resp.StatusCode != http.StatusOK || (jsonErr == nil && tr.Error != "") {
		errResp := buildRetrieveError(resp.StatusCode, data, jsonErr == nil, &tr)
		slog.DebugContext(ctx, "oauth token exchange error", "status", resp.StatusCode, "body", string(data))
		return nil, errResp
	}

	// JSON unmarshal succeeded and no error field: use parsed response.
	if jsonErr == nil {
		if tr.AccessToken == "" {
			return nil, errors.New("no access_token in response")
		}
		return tokenFromCodeResponse(&tr), nil
	}

	// JSON unmarshal failed: fall back to form-urlencoded parsing.
	slog.DebugContext(ctx, "oauth token json parse failed, trying form-encoded fallback", "err", jsonErr)
	return parseFormEncodedToken(data)
}

// buildRetrieveError constructs a RetrieveError from a token endpoint response.
func buildRetrieveError(statusCode int, data []byte, jsonOK bool, tr *codeTokenResponse) *RetrieveError {
	errResp := &RetrieveError{
		StatusCode: statusCode,
		Body:       data,
	}
	if jsonOK && tr.Error != "" {
		errResp.ErrorCode = tr.Error
		errResp.ErrorDescription = tr.ErrorDescription
		errResp.ErrorURI = tr.ErrorURI
	}
	return errResp
}

// parseFormEncodedToken attempts to parse a form-encoded token response
// (application/x-www-form-urlencoded). Returns an error if the parse fails.
func parseFormEncodedToken(data []byte) (*oauth.Token, error) {
	vals, err := url.ParseQuery(string(data))
	if err != nil {
		return nil, fmt.Errorf("parse token response (form): %w", err)
	}
	if errorCode := vals.Get("error"); errorCode != "" {
		return nil, &RetrieveError{
			StatusCode:       http.StatusOK,
			ErrorCode:        errorCode,
			ErrorDescription: vals.Get("error_description"),
			ErrorURI:         vals.Get("error_uri"),
			Body:             data,
		}
	}
	accessToken := vals.Get("access_token")
	if accessToken == "" {
		return nil, errors.New("no access_token in form-encoded response")
	}
	tokenType := vals.Get("token_type")
	if tokenType == "" {
		tokenType = "Bearer"
	}
	var exp time.Time
	if expiresIn := vals.Get("expires_in"); expiresIn != "" {
		if sec, convErr := strconv.Atoi(expiresIn); convErr == nil && sec > 0 {
			exp = time.Now().Add(time.Duration(sec) * time.Second)
		}
	}
	return &oauth.Token{
		AccessToken:  accessToken,
		TokenType:    tokenType,
		RefreshToken: vals.Get("refresh_token"),
		Expiry:       exp,
	}, nil
}

// tokenFromCodeResponse extracts a Token from the parsed JSON response.
func tokenFromCodeResponse(tr *codeTokenResponse) *oauth.Token {
	tokenType := tr.TokenType
	if tokenType == "" {
		tokenType = "Bearer"
	}
	var exp time.Time
	if tr.ExpiresIn > 0 {
		exp = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	return &oauth.Token{
		AccessToken:  tr.AccessToken,
		TokenType:    tokenType,
		RefreshToken: tr.RefreshToken,
		Expiry:       exp,
	}
}

type codeTokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token,omitempty"`
	ExpiresIn        int    `json:"expires_in,omitempty"`
	TokenType        string `json:"token_type,omitempty"`
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
	ErrorURI         string `json:"error_uri,omitempty"`
}

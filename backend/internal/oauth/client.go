// OAuth authorization-code client helpers.

package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ClientConfig holds OAuth 2.0 endpoint configuration for one client.
type ClientConfig struct {
	ClientID     string
	ClientSecret string
	AuthEndpoint string
	TokenURL     string
	RedirectURI  string
	Scopes       []string
}

// AuthorizationURL returns the provider authorization URL with the state param.
func AuthorizationURL(c *ClientConfig, state string) string {
	v := url.Values{}
	v.Set("client_id", c.ClientID)
	v.Set("redirect_uri", c.RedirectURI)
	v.Set("scope", strings.Join(c.Scopes, " "))
	v.Set("state", state)
	v.Set("response_type", ResponseTypeCode)
	return c.AuthEndpoint + "?" + v.Encode()
}

// ExchangeCode exchanges an authorization code for tokens.
//
// It returns an access token, an optional refresh token, and an optional expiry.
func ExchangeCode(ctx context.Context, c *ClientConfig, code string) (access, refresh string, expiry time.Time, err error) {
	body := url.Values{}
	body.Set("grant_type", GrantAuthorizationCode)
	body.Set("code", code)
	body.Set("redirect_uri", c.RedirectURI)
	body.Set("client_id", c.ClientID)
	body.Set("client_secret", c.ClientSecret)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, strings.NewReader(body.Encode()))
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("token exchange: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", time.Time{}, fmt.Errorf("token exchange status %d: %s", resp.StatusCode, data)
	}

	var tr codeTokenResponse
	if err := json.Unmarshal(data, &tr); err != nil {
		return "", "", time.Time{}, fmt.Errorf("parse token response: %w", err)
	}
	if tr.Error != "" {
		return "", "", time.Time{}, fmt.Errorf("oauth error: %s", tr.Error)
	}
	if tr.AccessToken == "" {
		return "", "", time.Time{}, errors.New("no access_token in response")
	}

	var exp time.Time
	if tr.ExpiresIn > 0 {
		exp = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	return tr.AccessToken, tr.RefreshToken, exp, nil
}

type codeTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	Error        string `json:"error,omitempty"`
}

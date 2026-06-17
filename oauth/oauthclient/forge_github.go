// GitHub OAuth client provider configuration.

package oauthclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/caic-xyz/caic/oauth"
)

// GitHubConfig is the OAuth client configuration for github.com.
type GitHubConfig struct {
	ClientID     string
	ClientSecret string
	redirectURI  string // pre-built redirect URI, computed by the caller
	AuthEndpoint string
	TokenURL     string
	UserInfoURL  string
	Scopes       []string
}

// NewGitHubConfig returns a GitHubConfig with defaults.
// Scopes: ["repo", "read:user"].
//
//nolint:gosec // ClientSecret is a function parameter, not a hardcoded credential
func NewGitHubConfig(clientID, clientSecret, redirectURI string) GitHubConfig {
	return GitHubConfig{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		redirectURI:  redirectURI,
		AuthEndpoint: "https://github.com/login/oauth/authorize",
		TokenURL:     "https://github.com/login/oauth/access_token",
		UserInfoURL:  "https://api.github.com/user",
		Scopes:       []string{"repo", "read:user"},
	}
}

// RedirectURI returns the pre-configured redirect URI.
func (c GitHubConfig) RedirectURI() string { //nolint:gocritic // value receiver is intentional
	return c.redirectURI
}

// AuthURL returns the GitHub authorization URL with the state param.
func (c GitHubConfig) AuthURL(state string) string { //nolint:gocritic // value receiver is intentional
	return AuthorizationURL(c.AuthEndpoint, c.ClientID, c.redirectURI, c.Scopes, state, "")
}

// OAuthClientConfig returns the generic OAuth client settings for GitHub.
func (c GitHubConfig) OAuthClientConfig() oauth.ClientConfig { //nolint:gocritic // value receiver is intentional
	return oauth.ClientConfig{
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		TokenURL:     c.TokenURL,
		RedirectURI:  c.redirectURI,
	}
}

// githubUserResponse is the JSON response from the GitHub /user endpoint.
type githubUserResponse struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
}

// FetchGitHubUser fetches the authenticated user from api.github.com/user.
//
//nolint:dupl,gocritic // provider-specific; value param is intentional for config
func FetchGitHubUser(ctx context.Context, cfg GitHubConfig, accessToken string) (providerID, username, avatarURL string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.UserInfoURL, http.NoBody)
	if err != nil {
		return "", "", "", fmt.Errorf("build userinfo request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("userinfo fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", "", fmt.Errorf("read userinfo response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("userinfo status %d: %s", resp.StatusCode, data)
	}

	var gh githubUserResponse
	if err := json.Unmarshal(data, &gh); err != nil {
		return "", "", "", fmt.Errorf("parse userinfo: %w", err)
	}
	if gh.ID == 0 || gh.Login == "" {
		return "", "", "", fmt.Errorf("empty userinfo from %s", cfg.UserInfoURL)
	}
	return strconv.FormatInt(gh.ID, 10), gh.Login, gh.AvatarURL, nil
}

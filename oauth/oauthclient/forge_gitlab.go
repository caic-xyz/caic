// GitLab OAuth client provider configuration.

package oauthclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/caic-xyz/caic/oauth"
)

// GitLabConfig is the OAuth client configuration for a GitLab instance.
type GitLabConfig struct {
	ClientID     string
	ClientSecret string
	redirectURI  string // pre-built redirect URI, computed by the caller
	AuthEndpoint string
	TokenURL     string
	UserInfoURL  string
	Scopes       []string
}

// NewGitLabConfig returns a GitLabConfig with defaults.
// Scopes: ["api", "read_user"]. gitlabURL defaults to "https://gitlab.com".
func NewGitLabConfig(clientID, clientSecret, gitlabURL, redirectURI string) GitLabConfig {
	if gitlabURL == "" {
		gitlabURL = "https://gitlab.com"
	}
	gitlabURL = strings.TrimRight(gitlabURL, "/")
	return GitLabConfig{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		redirectURI:  redirectURI,
		AuthEndpoint: gitlabURL + "/oauth/authorize",
		TokenURL:     gitlabURL + "/oauth/token",
		UserInfoURL:  gitlabURL + "/api/v4/user",
		Scopes:       []string{"api", "read_user"},
	}
}

// RedirectURI returns the pre-configured redirect URI.
func (c GitLabConfig) RedirectURI() string { //nolint:gocritic // value receiver is intentional
	return c.redirectURI
}

// AuthURL returns the GitLab authorization URL with the state param.
func (c GitLabConfig) AuthURL(state string) string { //nolint:gocritic // value receiver is intentional
	return AuthorizationURL(c.AuthEndpoint, c.ClientID, c.redirectURI, c.Scopes, state, "")
}

// OAuthClientConfig returns the generic OAuth client settings for GitLab.
func (c GitLabConfig) OAuthClientConfig() oauth.ClientConfig { //nolint:gocritic // value receiver is intentional
	return oauth.ClientConfig{
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		TokenURL:     c.TokenURL,
		RedirectURI:  c.redirectURI,
	}
}

// gitlabUserResponse is the JSON response from the GitLab /api/v4/user endpoint.
type gitlabUserResponse struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	AvatarURL string `json:"avatar_url"`
}

// FetchGitLabUser fetches the authenticated user from the GitLab user endpoint.
//
//nolint:dupl,gocritic // provider-specific; value param is intentional for config
func FetchGitLabUser(ctx context.Context, cfg GitLabConfig, accessToken string) (providerID, username, avatarURL string, err error) {
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

	var gl gitlabUserResponse
	if err := json.Unmarshal(data, &gl); err != nil {
		return "", "", "", fmt.Errorf("parse userinfo: %w", err)
	}
	if gl.ID == 0 || gl.Username == "" {
		return "", "", "", fmt.Errorf("empty userinfo from %s", cfg.UserInfoURL)
	}
	return strconv.FormatInt(gl.ID, 10), gl.Username, gl.AvatarURL, nil
}

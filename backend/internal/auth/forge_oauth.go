// Forge OAuth client login flow for GitHub/GitLab authorization-code exchange.

package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/oauth"
)

// ProviderConfig holds the OAuth 2.0 endpoint configuration for one provider.
type ProviderConfig struct {
	ClientID     string
	ClientSecret string
	AuthEndpoint string // e.g. "https://github.com/login/oauth/authorize"
	TokenURL     string // e.g. "https://github.com/login/oauth/access_token"
	UserInfoURL  string // e.g. "https://api.github.com/user"
	Scopes       []string
	Provider     string // "github" or "gitlab"
	Label        string // human-readable provider name, e.g. "GitHub"
	Host         *HostState
}

// RedirectURI returns the OAuth redirect URI built from the external URL and
// provider name. Returns "" if the external URL is not yet known (auto mode
// before the first FQDN request).
func (c *ProviderConfig) RedirectURI() string {
	extURL := c.Host.ExternalURL()
	if extURL == "" {
		return ""
	}
	return extURL + "/api/caic/v1/auth/" + c.Provider + "/callback"
}

// GitHubConfig returns a ProviderConfig for github.com.
// Scopes: ["repo", "read:user"].
func GitHubConfig(clientID, secret string, host *HostState) ProviderConfig {
	return ProviderConfig{ //nolint:gosec // G101: ClientSecret is a function parameter, not a hardcoded credential
		ClientID:     clientID,
		ClientSecret: secret,
		AuthEndpoint: "https://github.com/login/oauth/authorize",
		TokenURL:     "https://github.com/login/oauth/access_token",
		UserInfoURL:  "https://api.github.com/user",
		Scopes:       []string{"repo", "read:user"},
		Provider:     "github",
		Label:        "GitHub",
		Host:         host,
	}
}

// GitLabConfig returns a ProviderConfig for a GitLab instance.
// Scopes: ["api", "read_user"].
// gitlabURL defaults to "https://gitlab.com".
func GitLabConfig(clientID, secret, gitlabURL string, host *HostState) ProviderConfig {
	if gitlabURL == "" {
		gitlabURL = "https://gitlab.com"
	}
	gitlabURL = strings.TrimRight(gitlabURL, "/")
	return ProviderConfig{
		ClientID:     clientID,
		ClientSecret: secret,
		AuthEndpoint: gitlabURL + "/oauth/authorize",
		TokenURL:     gitlabURL + "/oauth/token",
		UserInfoURL:  gitlabURL + "/api/v4/user",
		Scopes:       []string{"api", "read_user"},
		Provider:     "gitlab",
		Label:        "GitLab",
		Host:         host,
	}
}

// AuthURL returns the provider's authorization URL with the state param.
func (c *ProviderConfig) AuthURL(state string) string {
	return oauth.AuthorizationURL(c.AuthEndpoint, c.ClientID, c.RedirectURI(), c.Scopes, state)
}

// OAuthClientConfig returns the generic OAuth client settings for this provider.
func (c *ProviderConfig) OAuthClientConfig() oauth.ClientConfig {
	return oauth.ClientConfig{
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		TokenURL:     c.TokenURL,
		RedirectURI:  c.RedirectURI(),
	}
}

// githubUserResponse is the JSON response from the GitHub /user endpoint.
type githubUserResponse struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
}

// gitlabUserResponse is the JSON response from the GitLab /api/v4/user endpoint.
type gitlabUserResponse struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	AvatarURL string `json:"avatar_url"`
}

// FetchUserInfo fetches the user's identity from the provider.
// Returns providerID (string form of numeric ID), username, avatarURL.
func FetchUserInfo(ctx context.Context, cfg *ProviderConfig, accessToken string) (providerID, username, avatarURL string, err error) {
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

	// Try GitHub format first (has "login" field).
	var gh githubUserResponse
	if err := json.Unmarshal(data, &gh); err == nil && gh.ID != 0 && gh.Login != "" {
		return strconv.FormatInt(gh.ID, 10), gh.Login, gh.AvatarURL, nil
	}
	// Fall back to GitLab format (has "username" field).
	var gl gitlabUserResponse
	if err := json.Unmarshal(data, &gl); err == nil && gl.ID != 0 && gl.Username != "" {
		return strconv.FormatInt(gl.ID, 10), gl.Username, gl.AvatarURL, nil
	}
	return "", "", "", fmt.Errorf("unrecognized userinfo response from %s", cfg.UserInfoURL)
}

// MaskedToken is a credential string that logs as "xxx...1234" (last 4 chars
// visible, remainder replaced with "x"). Implements [slog.LogValuer].
type MaskedToken string

// LogValue implements [slog.LogValuer].
func (m MaskedToken) LogValue() slog.Value {
	s := string(m)
	if s == "" {
		return slog.StringValue("")
	}
	if len(s) <= 4 {
		return slog.StringValue(s)
	}
	return slog.StringValue(strings.Repeat("x", len(s)-4) + s[len(s)-4:])
}

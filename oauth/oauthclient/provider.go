// Package oauthclient is an OAuth 2.0 authorization-code client library with (GitHub/GitLab) provider configurations.
package oauthclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/caic-xyz/caic/oauth"
)

// RedirectURIFunc returns the OAuth redirect URI from a request, resolved lazily
// per call. Mirrors the oauthserver.BaseURLFunc late-binding pattern.
type RedirectURIFunc func(r *http.Request) string

// Provider is the common interface for forge OAuth client configs.
type Provider interface {
	RedirectURI(r *http.Request) string
	AuthURL(r *http.Request, state string) string
	OAuthClientConfig(r *http.Request) oauth.ClientConfig
}

// ForgeConfig is the OAuth client configuration for a forge provider.
type ForgeConfig struct {
	ClientID        string
	ClientSecret    string
	RedirectURIFunc RedirectURIFunc
	AuthEndpoint    string
	TokenURL        string
	UserInfoURL     string
	Scopes          []string
	ParseUser       func(data []byte) (providerID, username, avatarURL string, err error)
}

// NewGitHubConfig returns a ForgeConfig for github.com
// with scopes ["repo", "read:user"].
//
//nolint:gosec // ClientSecret is a function parameter, not a hardcoded credential
func NewGitHubConfig(clientID, clientSecret string, redirectURI RedirectURIFunc) *ForgeConfig {
	return &ForgeConfig{
		ClientID:        clientID,
		ClientSecret:    clientSecret,
		RedirectURIFunc: redirectURI,
		AuthEndpoint:    "https://github.com/login/oauth/authorize",
		TokenURL:        "https://github.com/login/oauth/access_token",
		UserInfoURL:     "https://api.github.com/user",
		Scopes:          []string{"repo", "read:user"},
		ParseUser:       ParseGitHubUser,
	}
}

// NewGitLabConfig returns a ForgeConfig for a GitLab instance
// with scopes ["api", "read_user"]. gitlabURL defaults to "https://gitlab.com".
func NewGitLabConfig(clientID, clientSecret, gitlabURL string, redirectURI RedirectURIFunc) *ForgeConfig {
	if gitlabURL == "" {
		gitlabURL = "https://gitlab.com"
	}
	gitlabURL = strings.TrimRight(gitlabURL, "/")
	return &ForgeConfig{
		ClientID:        clientID,
		ClientSecret:    clientSecret,
		RedirectURIFunc: redirectURI,
		AuthEndpoint:    gitlabURL + "/oauth/authorize",
		TokenURL:        gitlabURL + "/oauth/token",
		UserInfoURL:     gitlabURL + "/api/v4/user",
		Scopes:          []string{"api", "read_user"},
		ParseUser:       ParseGitLabUser,
	}
}

// RedirectURI returns the redirect URI for a request.
func (c *ForgeConfig) RedirectURI(r *http.Request) string { //nolint:gocritic
	return c.RedirectURIFunc(r)
}

// AuthURL returns the provider's authorization URL with the state param.
func (c *ForgeConfig) AuthURL(r *http.Request, state string) string { //nolint:gocritic
	return AuthorizationURL(c.AuthEndpoint, c.ClientID, c.RedirectURI(r), c.Scopes, state, "")
}

// OAuthClientConfig returns the generic OAuth client settings.
func (c *ForgeConfig) OAuthClientConfig(r *http.Request) oauth.ClientConfig { //nolint:gocritic
	return oauth.ClientConfig{
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		TokenURL:     c.TokenURL,
		RedirectURI:  c.RedirectURI(r),
	}
}

// FetchUser fetches the authenticated user from the forge's userinfo endpoint.
func (c *ForgeConfig) FetchUser(ctx context.Context, accessToken string) (providerID, username, avatarURL string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.UserInfoURL, http.NoBody)
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
	return c.ParseUser(data)
}

// githubUser is the user info responses from GitHub.
type githubUser struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
}

// ParseGitHubUser parses the GitHub user info response.
func ParseGitHubUser(data []byte) (providerID, username, avatarURL string, err error) {
	var u githubUser
	if err := json.Unmarshal(data, &u); err != nil {
		return "", "", "", fmt.Errorf("parse userinfo: %w", err)
	}
	if u.ID == 0 || u.Login == "" {
		return "", "", "", errors.New("empty userinfo from GitHub")
	}
	return strconv.FormatInt(u.ID, 10), u.Login, u.AvatarURL, nil
}

// gitlabUser is the user info responses from GitLab.
type gitlabUser struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	AvatarURL string `json:"avatar_url"`
}

// ParseGitLabUser parses the GitLab user info response.
func ParseGitLabUser(data []byte) (providerID, username, avatarURL string, err error) {
	var u gitlabUser
	if err := json.Unmarshal(data, &u); err != nil {
		return "", "", "", fmt.Errorf("parse userinfo: %w", err)
	}
	if u.ID == 0 || u.Username == "" {
		return "", "", "", errors.New("empty userinfo from GitLab")
	}
	return strconv.FormatInt(u.ID, 10), u.Username, u.AvatarURL, nil
}

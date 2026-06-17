// Thin forge OAuth adapters mapping caic host state to oauth/client providers.

package auth

import (
	"errors"
	"log/slog"
	"strings"

	"github.com/caic-xyz/caic/oauth/oauthclient"
)

// NewGitHubProvider returns a GitHub OAuth client config with the redirect URI
// built from the host state's external URL.
func NewGitHubProvider(clientID, secret string, hostState *HostState) (*oauthclient.GitHubConfig, error) {
	uri := hostState.ExternalURL()
	if uri == "" {
		return nil, errors.New("external URL not available for GitHub OAuth redirect URI")
	}
	cfg := oauthclient.NewGitHubConfig(clientID, secret, uri+"/api/caic/v1/auth/github/callback")
	return &cfg, nil
}

// NewGitLabProvider returns a GitLab OAuth client config with the redirect URI
// built from the host state's external URL.
func NewGitLabProvider(clientID, secret, gitlabURL string, hostState *HostState) (*oauthclient.GitLabConfig, error) {
	uri := hostState.ExternalURL()
	if uri == "" {
		return nil, errors.New("external URL not available for GitLab OAuth redirect URI")
	}
	cfg := oauthclient.NewGitLabConfig(clientID, secret, gitlabURL, uri+"/api/caic/v1/auth/gitlab/callback")
	return &cfg, nil
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

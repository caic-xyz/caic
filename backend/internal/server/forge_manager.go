// Manages forge clients, authentication tokens, and rate-limit throttles.

package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/maruel/roundtrippers"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/bot"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/github"
	"github.com/caic-xyz/caic/backend/internal/forge/gitlab"
)

// ForgeManager resolves forge clients for repos, manages per-user rate-limit
// throttles, and caches GitHub App installation IDs.
type ForgeManager struct {
	githubToken string
	gitlabToken string
	githubApp   GitHubAppClient // nil when GitHub App not configured

	mu                   sync.Mutex
	githubOAuthThrottles map[string]http.RoundTripper // keyed by user ID
	githubPATThrottle    http.RoundTripper
	githubAppThrottle    http.RoundTripper
	gitlabOAuthThrottles map[string]http.RoundTripper // keyed by user ID
	gitlabPATThrottle    http.RoundTripper
	githubInstallations  map[string]int64 // owner (lowercase) → installation ID; -1 = not installed
}

// NewForgeManager creates a forge manager for configured forge tokens.
func NewForgeManager(githubToken, gitlabToken string) *ForgeManager {
	return newForgeManager(githubToken, gitlabToken, nil)
}

func newForgeManager(githubToken, gitlabToken string, githubApp GitHubAppClient) *ForgeManager {
	return &ForgeManager{
		githubToken:          githubToken,
		gitlabToken:          gitlabToken,
		githubApp:            githubApp,
		githubOAuthThrottles: make(map[string]http.RoundTripper),
		githubPATThrottle:    newThrottle(),
		githubAppThrottle:    newThrottle(),
		gitlabOAuthThrottles: make(map[string]http.RoundTripper),
		gitlabPATThrottle:    newThrottle(),
		githubInstallations:  make(map[string]int64),
	}
}

// SetGitHubApp sets the GitHub App client used for installation-scoped access.
func (m *ForgeManager) SetGitHubApp(c GitHubAppClient) {
	m.githubApp = c
}

// GitHubAppThrottle returns the shared throttle for GitHub App API requests.
func (m *ForgeManager) GitHubAppThrottle() http.RoundTripper {
	return m.githubAppThrottle
}

// forgeForInfo returns the appropriate forge.Forge for the repo's remote, using
// the configured tokens. Falls back to a GitHub App installation token when no
// user OAuth token or PAT is available. Returns nil if no token is available.
func (m *ForgeManager) forgeForInfo(ctx context.Context, info *RepoInfo) forge.Forge {
	if f := m.forgeFor(ctx, info.ForgeKind); f != nil {
		return f
	}
	if info.ForgeKind == forge.KindGitHub && m.githubApp != nil {
		installID := m.installationID(info.ForgeOwner)
		if installID == 0 {
			id, err := m.githubApp.RepoInstallation(ctx, info.ForgeOwner, info.ForgeRepo)
			if err != nil {
				// Cache -1 to avoid repeating the lookup on every call.
				m.storeInstallationID(info.ForgeOwner, -1)
				return nil
			}
			m.storeInstallationID(info.ForgeOwner, id)
			installID = id
		}
		if installID < 0 {
			return nil // app not installed for this owner
		}
		client, err := m.githubApp.ForgeClient(ctx, installID)
		if err != nil {
			slog.Warn("forgeForInfo: app forge client", "err", err)
			return nil
		}
		return client
	}
	return nil
}

// forgeFor returns a Forge client for the given kind.
// In OAuth mode the authenticated user's access token is used.
// In PAT mode (no OAuth) the global token is used.
// Config.Validate ensures these two modes are never mixed.
// Returns nil if no token is available.
func (m *ForgeManager) forgeFor(ctx context.Context, kind forge.Kind) forge.Forge {
	if u, ok := auth.UserFromContext(ctx); ok && u.Provider == kind && u.AccessToken != "" {
		switch kind {
		case forge.KindGitHub:
			return github.NewClient(u.AccessToken, m.githubOAuthThrottle(u.ID))
		case forge.KindGitLab:
			return gitlab.NewClient(u.AccessToken, m.gitlabOAuthThrottle(u.ID))
		}
	}
	switch kind {
	case forge.KindGitHub:
		if m.githubToken != "" {
			return github.NewClient(m.githubToken, m.githubPATThrottle)
		}
	case forge.KindGitLab:
		if m.gitlabToken != "" {
			return gitlab.NewClient(m.gitlabToken, m.gitlabPATThrottle)
		}
	}
	return nil
}

// storeInstallationID caches the GitHub App installation ID for the given owner.
// id == -1 means the app is not installed for that owner.
func (m *ForgeManager) storeInstallationID(owner string, id int64) {
	if id == 0 {
		return
	}
	m.mu.Lock()
	m.githubInstallations[strings.ToLower(owner)] = id
	m.mu.Unlock()
}

// installationID returns the cached installation ID for the given owner, or 0 if unknown.
// Returns -1 if the app is known to not be installed for that owner.
func (m *ForgeManager) installationID(owner string) int64 {
	m.mu.Lock()
	id := m.githubInstallations[strings.ToLower(owner)]
	m.mu.Unlock()
	return id
}

// githubOAuthThrottle returns the per-user throttle for GitHub OAuth.
// Each OAuth user has a separate GitHub rate-limit bucket; throttles must not be shared.
func (m *ForgeManager) githubOAuthThrottle(userID string) http.RoundTripper {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.githubOAuthThrottles[userID]; ok {
		return t
	}
	t := newThrottle()
	m.githubOAuthThrottles[userID] = t
	return t
}

// gitlabOAuthThrottle returns the per-user throttle for GitLab OAuth.
func (m *ForgeManager) gitlabOAuthThrottle(userID string) http.RoundTripper {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.gitlabOAuthThrottles[userID]; ok {
		return t
	}
	t := newThrottle()
	m.gitlabOAuthThrottles[userID] = t
	return t
}

// commenterFor returns a bot.Commenter for posting comments via the GitHub App
// (when installationID is non-zero) or the configured PAT, or nil if neither
// is available.
func (m *ForgeManager) commenterFor(installationID int64) bot.Commenter {
	if m.githubApp != nil && installationID != 0 {
		return &appInstallCommenter{app: m.githubApp, installationID: installationID}
	}
	if m.githubToken != "" {
		return github.NewClient(m.githubToken, m.githubPATThrottle)
	}
	return nil
}

// newThrottle returns a Throttle transport at 1 QPS backed by http.DefaultTransport.
func newThrottle() http.RoundTripper {
	return &roundtrippers.Throttle{QPS: 1, Transport: http.DefaultTransport}
}

// githubClient returns a GitHub API client authenticated with the configured PAT.
// Returns nil when no GitHub token is configured.
func (m *ForgeManager) githubClient() *github.Client {
	if m.githubToken == "" {
		return nil
	}
	return github.NewClient(m.githubToken, m.githubPATThrottle)
}

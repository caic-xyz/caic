// Package forgemgr resolves forge clients for repos, manages per-user
// rate-limit throttles, and caches GitHub App installation IDs.
package forgemgr

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/maruel/roundtrippers"

	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/github"
	"github.com/caic-xyz/caic/backend/internal/forge/gitlab"
	"github.com/caic-xyz/caic/backend/internal/repo"
)

// GitHubAppClient is the GitHub App surface the manager depends on. Abstracted
// so tests can substitute a stub.
type GitHubAppClient interface {
	ForgeClient(ctx context.Context, installationID int64) (forge.Forge, error)
	DeleteInstallation(ctx context.Context, installationID int64) error
	RepoInstallation(ctx context.Context, owner, repo string) (int64, error)
	PostComment(ctx context.Context, installationID int64, owner, repo string, issueNumber int, body string) error
}

// OAuthToken identifies a user-scoped OAuth token for forge API requests.
type OAuthToken struct {
	AccessToken string
	UserID      string
}

// OAuthTokenSource resolves a user-scoped OAuth token for a forge kind.
// It returns false when the request has no matching token.
type OAuthTokenSource interface {
	TokenFor(ctx context.Context, kind forge.Kind) (OAuthToken, bool)
}

type noOAuthTokenSource struct{}

func (noOAuthTokenSource) TokenFor(context.Context, forge.Kind) (OAuthToken, bool) {
	return OAuthToken{}, false
}

// NoOAuthTokenSource returns a source for deployments that use only PATs or a
// GitHub App.
func NoOAuthTokenSource() OAuthTokenSource { return noOAuthTokenSource{} }

// Manager resolves forge clients for repos, manages per-user rate-limit
// throttles, and caches GitHub App installation IDs.
type Manager struct {
	// Immutable.
	log         *slog.Logger
	githubToken string
	gitlabToken string
	oauthTokens OAuthTokenSource

	// Set during startup before the manager serves requests.
	githubApp GitHubAppClient // nil when GitHub App not configured

	// Guarded by mu.
	mu                   sync.Mutex
	githubOAuthThrottles map[string]http.RoundTripper // keyed by user ID
	githubPATThrottle    http.RoundTripper
	githubAppThrottle    http.RoundTripper
	gitlabOAuthThrottles map[string]http.RoundTripper // keyed by user ID
	gitlabPATThrottle    http.RoundTripper
	githubInstallations  map[string]int64 // owner (lowercase) → installation ID; -1 = not installed
}

// New creates a forge manager for configured forge tokens, an optional GitHub
// App client, and a user-scoped OAuth token source.
func New(githubToken, gitlabToken string, githubApp GitHubAppClient, oauthTokens OAuthTokenSource) *Manager {
	if oauthTokens == nil {
		panic("OAuth token source is required")
	}
	return &Manager{
		log:                  slog.Default().With(slog.String("cmp", "forgemgr")),
		githubToken:          githubToken,
		gitlabToken:          gitlabToken,
		githubApp:            githubApp,
		oauthTokens:          oauthTokens,
		githubOAuthThrottles: make(map[string]http.RoundTripper),
		githubPATThrottle:    newThrottle(),
		githubAppThrottle:    newThrottle(),
		gitlabOAuthThrottles: make(map[string]http.RoundTripper),
		gitlabPATThrottle:    newThrottle(),
		githubInstallations:  make(map[string]int64),
	}
}

// SetGitHubApp sets the GitHub App client used for installation-scoped access.
func (m *Manager) SetGitHubApp(c GitHubAppClient) {
	m.githubApp = c
}

// GitHubApp returns the configured GitHub App client, or nil.
func (m *Manager) GitHubApp() GitHubAppClient {
	return m.githubApp
}

// GitHubToken returns the configured GitHub PAT, or "".
func (m *Manager) GitHubToken() string {
	return m.githubToken
}

// GitHubAppThrottle returns the shared throttle for GitHub App API requests.
func (m *Manager) GitHubAppThrottle() http.RoundTripper {
	return m.githubAppThrottle
}

// ForgeForInfo returns the appropriate forge.Forge for the repo's remote, using
// the configured tokens. Falls back to a GitHub App installation token when no
// user OAuth token or PAT is available. Returns nil if no token is available.
func (m *Manager) ForgeForInfo(ctx context.Context, info *repo.Repository) forge.Forge {
	if f := m.ForgeFor(ctx, info.ForgeKind); f != nil {
		return f
	}
	if info.ForgeKind == forge.KindGitHub && m.githubApp != nil {
		installID := m.InstallationID(info.ForgeOwner)
		if installID == 0 {
			id, err := m.githubApp.RepoInstallation(ctx, info.ForgeOwner, info.ForgeRepo)
			if err != nil {
				// Cache -1 to avoid repeating the lookup on every call.
				m.StoreInstallationID(info.ForgeOwner, -1)
				return nil
			}
			m.StoreInstallationID(info.ForgeOwner, id)
			installID = id
		}
		if installID < 0 {
			return nil // app not installed for this owner
		}
		client, err := m.githubApp.ForgeClient(ctx, installID)
		if err != nil {
			m.log.WarnContext(ctx, "create GitHub App forge client", "err", err)
			return nil
		}
		return client
	}
	return nil
}

// ForgeFor returns a Forge client for the given kind.
// A user-scoped OAuth token returned by the configured source takes priority
// over the global PAT.
// Config.Validate ensures these two modes are never mixed.
// Returns nil if no token is available.
func (m *Manager) ForgeFor(ctx context.Context, kind forge.Kind) forge.Forge {
	if token, ok := m.oauthTokens.TokenFor(ctx, kind); ok && token.AccessToken != "" {
		switch kind {
		case forge.KindGitHub:
			return github.NewClient(token.AccessToken, m.githubOAuthThrottle(token.UserID))
		case forge.KindGitLab:
			return gitlab.NewClient(token.AccessToken, m.gitlabOAuthThrottle(token.UserID))
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

// StoreInstallationID caches the GitHub App installation ID for the given owner.
// id == -1 means the app is not installed for that owner.
func (m *Manager) StoreInstallationID(owner string, id int64) {
	if id == 0 {
		return
	}
	m.mu.Lock()
	m.githubInstallations[strings.ToLower(owner)] = id
	m.mu.Unlock()
}

// InstallationID returns the cached installation ID for the given owner, or 0 if
// unknown. Returns -1 if the app is known to not be installed for that owner.
func (m *Manager) InstallationID(owner string) int64 {
	m.mu.Lock()
	id := m.githubInstallations[strings.ToLower(owner)]
	m.mu.Unlock()
	return id
}

// CommenterFor returns a forge.Commenter for posting comments via the GitHub App
// (when installationID is non-zero) or the configured PAT, or nil if neither
// is available.
func (m *Manager) CommenterFor(installationID int64) forge.Commenter {
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

// GitHubClient returns a GitHub API client authenticated with the configured PAT.
// Returns nil when no GitHub token is configured.
func (m *Manager) GitHubClient() *github.Client {
	if m.githubToken == "" {
		return nil
	}
	return github.NewClient(m.githubToken, m.githubPATThrottle)
}

// githubOAuthThrottle returns the per-user throttle for GitHub OAuth.
// Each OAuth user has a separate GitHub rate-limit bucket; throttles must not be shared.
func (m *Manager) githubOAuthThrottle(userID string) http.RoundTripper {
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
func (m *Manager) gitlabOAuthThrottle(userID string) http.RoundTripper {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.gitlabOAuthThrottles[userID]; ok {
		return t
	}
	t := newThrottle()
	m.gitlabOAuthThrottles[userID] = t
	return t
}

// appInstallCommenter adapts GitHubAppClient.PostComment to forge.Commenter by
// binding a fixed installation ID.
type appInstallCommenter struct {
	app            GitHubAppClient
	installationID int64
}

func (c *appInstallCommenter) PostComment(ctx context.Context, owner, repoName string, issueNumber int, body string) error {
	return c.app.PostComment(ctx, c.installationID, owner, repoName, issueNumber, body)
}

// HTTP handlers for server configuration, preferences, repos, harnesses, caches, and voice token.

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/caic-xyz/md"
	"github.com/caic-xyz/md/git"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/autoupdate"
	"github.com/caic-xyz/caic/backend/internal/forge/forgemanager"
	"github.com/caic-xyz/caic/backend/internal/harness"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/repos"
	caicruntime "github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/api/v1conv"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/oauth/oauthclient"
)

type executorRegistry interface {
	RangeExecutors(fn func(relPath string, r *task.RepoExecutor) bool)
	Executor(relPath string) (*task.RepoExecutor, bool)
	RegisterExecutor(relPath string, r *task.RepoExecutor)
}

type serverHandlers struct {
	serverCtx          context.Context
	tailscaleAvailable bool
	forge              *forgemanager.Manager
	prefs              *preferences.Store
	repos              *repos.Service
	taskMgr            executorRegistry
	cacheSizes         *CacheSizeStore
	authStore          *auth.Store
	githubOAuth        *oauthclient.ProviderConfig
	gitlabOAuth        *oauthclient.ProviderConfig
	googleOAuth        *oauthclient.ProviderConfig
	voiceGateway       v1.VoiceGatewayMetadata
}

func (h *serverHandlers) getConfig(_ context.Context, _ *api.EmptyReq) (*v1.Config, error) {
	displayName, err := os.Hostname()
	if err != nil {
		slog.Warn("failed to get hostname", "err", err)
	}
	displayName, _, _ = strings.Cut(displayName, ".")
	cfg := &v1.Config{
		Version:              autoupdate.Version,
		DisplayName:          displayName,
		TailscaleAvailable:   h.tailscaleAvailable,
		USBAvailable:         runtime.GOOS == "linux",
		DisplayAvailable:     true,
		SudoAvailable:        true,
		GitHubTokenAvailable: h.forge.GitHubToken() != "" || h.githubOAuth != nil,
		VoiceGateway:         h.voiceGateway,
		GitHubAppEnabled:     h.forge.GitHubApp() != nil,
	}
	if h.authStore != nil {
		cfg.AuthProviders = h.authProviders()
	}
	return cfg, nil
}

// authProviders returns the list of configured OAuth provider names.
func (h *serverHandlers) authProviders() []string {
	var ps []string
	if h.githubOAuth != nil {
		ps = append(ps, "github")
	}
	if h.gitlabOAuth != nil {
		ps = append(ps, "gitlab")
	}
	if h.googleOAuth != nil {
		ps = append(ps, "google")
	}
	return ps
}

// getVersion returns the current server version and checks GitHub for the latest release.
func (h *serverHandlers) getVersion(ctx context.Context, _ *api.EmptyReq) (*v1.VersionResp, error) {
	current := autoupdate.Version
	gh := h.forge.GitHubClient()
	resp := &v1.VersionResp{
		Current:      current,
		AutoUpdateOn: gh != nil && current != "" && !strings.HasPrefix(current, "devel-"),
	}
	if gh != nil {
		latest, err := autoupdate.CheckLatest(ctx, gh)
		if err != nil {
			resp.CheckError = err.Error()
		} else {
			resp.Latest = latest
			resp.UpdateAvail = autoupdate.IsNewer(latest, current)
		}
	}
	return resp, nil
}

// triggerUpdate starts a background update check-and-install. Returns immediately.
func (h *serverHandlers) triggerUpdate(ctx context.Context, _ *api.EmptyReq) (*v1.UpdateResp, error) {
	gh := h.forge.GitHubClient()
	if gh == nil {
		return nil, api.InternalError("GitHub token not configured; cannot check for updates")
	}
	current := autoupdate.Version
	latest, err := autoupdate.CheckLatest(ctx, gh)
	if err != nil {
		return nil, api.InternalError("check latest version: " + err.Error())
	}
	if !autoupdate.IsNewer(latest, current) {
		return &v1.UpdateResp{Status: "already_up_to_date"}, nil
	}
	go func() {
		slog.InfoContext(h.serverCtx, "update triggered by user", "current", current, "latest", latest)
		if err := autoupdate.CheckAndUpdate(h.serverCtx, gh); err != nil {
			slog.WarnContext(h.serverCtx, "background update failed", "err", err)
		}
	}()
	return &v1.UpdateResp{Status: "started"}, nil
}

func (h *serverHandlers) getPreferences(ctx context.Context, _ *api.EmptyReq) (*v1.PreferencesResp, error) {
	prefs := h.prefs.Get(userIDFromCtx(ctx))
	recent := prefs.RecentRepos(time.Now())
	repoPrefs := make([]v1.RepoPrefsResp, len(recent))
	for i, r := range recent {
		repoPrefs[i] = v1.RepoPrefsResp{
			Path:       r.Path,
			BaseBranch: r.BaseBranch,
			Harness:    r.Harness,
			Model:      r.Model,
		}
	}
	cacheMappings := make([]v1.CacheMappingResp, len(prefs.Settings.CacheMappings))
	for i, m := range prefs.Settings.CacheMappings {
		cacheMappings[i] = v1.CacheMappingResp{
			HostPath:      m.HostPath,
			ContainerPath: m.ContainerPath,
			Enabled:       m.Enabled,
		}
	}
	customMounts := make([]v1.MountMappingResp, len(prefs.Settings.CustomMounts))
	for i, m := range prefs.Settings.CustomMounts {
		customMounts[i] = v1.MountMappingResp{
			HostPath:      m.HostPath,
			ContainerPath: m.ContainerPath,
			Enabled:       m.Enabled,
			ReadOnly:      m.ReadOnly,
		}
	}
	return &v1.PreferencesResp{
		Repositories: repoPrefs,
		Harness:      prefs.Harness,
		Models:       prefs.Models,
		Efforts:      map[string]map[string]string(prefs.Efforts),
		Settings: v1.UserSettings{
			AutoFixOnCIFailure: prefs.Settings.AutoFixOnCIFailure,
			AutoFixOnPROpen:    prefs.Settings.AutoFixOnPROpen,
			BaseImage:          prefs.Settings.BaseImage,
			ContainerPlatform:  v1.Platform(prefs.Settings.ContainerPlatform),
			MaxCPUs:            prefs.Settings.MaxCPUs,
			WellKnownCaches:    prefs.Settings.WellKnownCaches,
			CacheMappings:      cacheMappings,
			CustomMounts:       customMounts,
		},
	}, nil
}

func (h *serverHandlers) updatePreferences(ctx context.Context, req *v1.UpdatePreferencesReq) (*v1.PreferencesResp, error) {
	if err := h.prefs.Update(userIDFromCtx(ctx), func(p *preferences.Preferences) {
		p.Settings.AutoFixOnCIFailure = req.Settings.AutoFixOnCIFailure
		p.Settings.AutoFixOnPROpen = req.Settings.AutoFixOnPROpen
		p.Settings.BaseImage = req.Settings.BaseImage
		p.Settings.ContainerPlatform = md.Platform(req.Settings.ContainerPlatform)
		p.Settings.MaxCPUs = req.Settings.MaxCPUs
		p.Settings.WellKnownCaches = req.Settings.WellKnownCaches
		if req.Settings.CacheMappings != nil {
			p.Settings.CacheMappings = make([]preferences.CacheMapping, len(req.Settings.CacheMappings))
			for i, m := range req.Settings.CacheMappings {
				p.Settings.CacheMappings[i] = preferences.CacheMapping{
					HostPath:      m.HostPath,
					ContainerPath: m.ContainerPath,
					Enabled:       m.Enabled,
				}
			}
		}
		if req.Settings.CustomMounts != nil {
			p.Settings.CustomMounts = make([]preferences.MountMapping, len(req.Settings.CustomMounts))
			for i, m := range req.Settings.CustomMounts {
				p.Settings.CustomMounts[i] = preferences.MountMapping{
					HostPath:      m.HostPath,
					ContainerPath: m.ContainerPath,
					Enabled:       m.Enabled,
					ReadOnly:      m.ReadOnly,
				}
			}
		}
	}); err != nil {
		return nil, api.InternalError("save preferences: " + err.Error())
	}
	// Return the updated preferences.
	return h.getPreferences(ctx, nil)
}

func cacheMountsFromSettings(settings *preferences.Settings) []caicruntime.CacheMount {
	var caches []caicruntime.CacheMount
	names := slices.Sorted(maps.Keys(md.WellKnownCaches))
	for _, name := range names {
		if !settings.WellKnownCaches[name] {
			continue
		}
		for _, c := range md.WellKnownCaches[name] {
			caches = append(caches, caicruntime.CacheMount{
				Name:        c.Name,
				Description: c.Description,
				HostPath:    c.HostPath,
				MountPath:   c.ContainerPath,
				ReadOnly:    c.ReadOnly,
				Shallow:     c.Shallow,
			})
		}
	}
	for i, m := range settings.CacheMappings {
		if !m.Enabled {
			continue
		}
		caches = append(caches, caicruntime.CacheMount{
			Name:      fmt.Sprintf("custom-cache-%d", i),
			HostPath:  m.HostPath,
			MountPath: m.ContainerPath,
		})
	}
	return caches
}

func mountsFromSettings(settings *preferences.Settings) []caicruntime.Mount {
	mounts := make([]caicruntime.Mount, 0, len(settings.CustomMounts))
	for _, m := range settings.CustomMounts {
		if !m.Enabled {
			continue
		}
		mounts = append(mounts, caicruntime.Mount{
			HostPath:  m.HostPath,
			MountPath: m.ContainerPath,
			ReadOnly:  m.ReadOnly,
		})
	}
	return mounts
}

func (h *serverHandlers) listHarnesses(_ context.Context, _ *api.EmptyReq) (*[]v1.HarnessInfo, error) {
	// Collect unique harness backends from all executors.
	seen := make(map[harness.Name]agent.Backend)
	h.taskMgr.RangeExecutors(func(_ string, r *task.RepoExecutor) bool {
		maps.Copy(seen, r.Backends)
		return true
	})
	out := make([]v1.HarnessInfo, 0, len(seen))
	for h, b := range seen {
		out = append(out, v1.HarnessInfo{Name: string(h), Models: b.Models(), SupportsImages: b.SupportsImages(), SupportsCompact: b.SupportsCompact()})
	}
	slices.SortFunc(out, func(a, b v1.HarnessInfo) int {
		return strings.Compare(a.Name, b.Name)
	})
	return &out, nil
}

func (h *serverHandlers) listCaches(_ context.Context, _ *api.EmptyReq) (*v1.WellKnownCachesResp, error) {
	harnessMounts := make([]string, 0, len(md.HarnessMounts))
	for _, hp := range md.HarnessMounts {
		for _, p := range hp.HomePaths {
			harnessMounts = append(harnessMounts, "~/"+p)
		}
	}
	slices.Sort(harnessMounts)
	harnessMounts = slices.Compact(harnessMounts)

	wellKnown := make([]v1.WellKnownCache, 0, len(md.WellKnownCaches))
	for name, mounts := range md.WellKnownCaches {
		containerPaths := make([]string, len(mounts))
		for i, m := range mounts {
			containerPaths[i] = m.ContainerPath
		}
		wellKnown = append(wellKnown, v1.WellKnownCache{
			Name:        name,
			Description: mounts[0].Description,
			Mounts:      containerPaths,
		})
	}
	slices.SortFunc(wellKnown, func(a, b v1.WellKnownCache) int {
		return strings.Compare(a.Name, b.Name)
	})

	return &v1.WellKnownCachesResp{
		HarnessMounts: harnessMounts,
		WellKnown:     wellKnown,
	}, nil
}

func (h *serverHandlers) getCacheSizes(_ context.Context, _ *api.EmptyReq) (*v1.CacheSizesResp, error) {
	if h.cacheSizes == nil {
		return &v1.CacheSizesResp{}, nil
	}
	return &v1.CacheSizesResp{WellKnown: h.cacheSizes.Snapshot()}, nil
}

func (h *serverHandlers) listRepos(_ context.Context, _ *api.EmptyReq) (*[]v1.Repo, error) {
	return repoListFromSnapshot(h.repos.SnapshotWithCI()), nil
}

func (h *serverHandlers) handleListRepoBranches(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		writeError(w, api.BadRequest("repo is required"))
		return
	}
	info, ok := h.repos.InfoFor(repo)
	if !ok {
		writeError(w, api.NotFound("repo not found"))
		return
	}
	absPath := info.AbsPath
	ctx := r.Context()
	checkout := &git.Checkout{Root: absPath, Logger: slog.Default()}
	// Fetch local branches.
	localPairs, err := checkout.ListBranches(ctx, "")
	if err != nil {
		slog.WarnContext(ctx, "list local branches failed", "repo", repo, "err", err)
	}
	seen := make(map[string]struct{}, len(localPairs))
	branches := make([]v1.BranchInfo, 0, len(localPairs))
	for _, p := range localPairs {
		seen[p[0]] = struct{}{}
		branches = append(branches, v1.BranchInfo{Name: p[0]})
	}
	// Fetch remote branches from all remotes.
	remoteList, err := checkout.RunGit(ctx, "remote")
	if err != nil {
		slog.WarnContext(ctx, "list remotes failed", "repo", repo, "err", err)
	}
	for remote := range strings.SplitSeq(remoteList, "\n") {
		if remote == "" {
			continue
		}
		remotePairs, err := checkout.ListBranches(ctx, remote)
		if err != nil {
			slog.WarnContext(ctx, "list remote branches failed", "repo", repo, "remote", remote, "err", err)
			continue
		}
		for _, p := range remotePairs {
			if _, ok := seen[p[0]]; !ok {
				seen[p[0]] = struct{}{}
				branches = append(branches, v1.BranchInfo{Name: p[0], Remote: remote})
			}
		}
	}
	writeJSONResponse(w, &v1.RepoBranchesResp{Branches: branches}, nil)
}

func (h *serverHandlers) cloneRepo(ctx context.Context, req *v1.CloneRepoReq) (*v1.Repo, error) {
	info, err := h.repos.Clone(ctx, repos.CloneRequest{URL: req.URL, Path: req.Path, Depth: req.Depth})
	if err != nil {
		var repoErr *repos.Error
		if !errors.As(err, &repoErr) {
			return nil, api.InternalError(err.Error())
		}
		switch repoErr.Kind {
		case repos.ErrorBadRequest:
			return nil, api.BadRequest(repoErr.Message)
		case repos.ErrorConflict:
			return nil, api.Conflict(repoErr.Message)
		default:
			return nil, api.InternalError(repoErr.Message)
		}
	}
	return &v1.Repo{
		Path:       info.RelPath,
		Branch:     info.BaseBranch,
		BaseBranch: v1.BranchInfo{Name: info.BaseBranch, Remote: info.BaseBranchRemote},
		RemoteURL:  git.RemoteToHTTPS(info.Remote),
		Forge:      v1.Forge(info.ForgeKind),
	}, nil
}

func repoListFromSnapshot(snap []repos.InfoWithCI) *[]v1.Repo {
	out := make([]v1.Repo, len(snap))
	for i := range snap {
		out[i] = repoDTO(&snap[i])
	}
	return &out
}

func repoDTO(status *repos.InfoWithCI) v1.Repo {
	info := &status.Info
	repo := v1.Repo{
		Path:       info.RelPath,
		Branch:     info.BaseBranch,
		BaseBranch: v1.BranchInfo{Name: info.BaseBranch, Remote: info.BaseBranchRemote},
		RemoteURL:  git.RemoteToHTTPS(info.Remote),
		Forge:      v1.Forge(info.ForgeKind),
	}
	if status.HasCI {
		repo.CI = v1.CIStatus(status.CI.Status)
		repo.CIChecks = make([]v1.ForgeCheck, len(status.CI.Checks))
		for i := range status.CI.Checks {
			repo.CIChecks[i] = v1conv.ForgeCheck(&status.CI.Checks[i])
		}
	}
	return repo
}

// routes returns the handler for server configuration, preferences, and repo
// endpoints. Patterns are relative to the /api/caic/v1 version prefix, stripped
// at mount time. config and version are also registered publicly on the root
// mux (exempt from RequireUser); that exact-match registration takes precedence.
func (h *serverHandlers) routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /server/config", handle(h.getConfig))
	m.HandleFunc("GET /server/version", handle(h.getVersion))
	m.HandleFunc("GET /server/preferences", handle(h.getPreferences))
	m.HandleFunc("POST /server/preferences", handle(h.updatePreferences))
	m.HandleFunc("GET /server/harnesses", handle(h.listHarnesses))
	m.HandleFunc("GET /server/caches", handle(h.listCaches))
	m.HandleFunc("GET /server/cache-sizes", handle(h.getCacheSizes))
	m.HandleFunc("GET /server/repos", handle(h.listRepos))
	m.HandleFunc("POST /server/repos", handle(h.cloneRepo))
	m.HandleFunc("POST /server/update", handle(h.triggerUpdate))
	m.HandleFunc("GET /server/repos/branches", h.handleListRepoBranches)
	return m
}

// discoveryRoutes returns the public server discovery endpoints, accessible
// before login for auth provider discovery and frontend bootstrap.
func (h *serverHandlers) discoveryRoutes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /server-info/config", handle(h.getConfig))
	m.HandleFunc("GET /server-info/version", handle(h.getVersion))
	// Unmatched /server-info/ paths must not fall through to the SPA.
	m.Handle("/server-info/", http.NotFoundHandler())
	m.Handle("/server-info", http.NotFoundHandler())
	return m
}

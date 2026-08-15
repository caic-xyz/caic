// HTTP handlers for server configuration, preferences, repos, harnesses, caches, and voice token.

package server

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/caic-xyz/md"
	"github.com/caic-xyz/md/git"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/autoupdate"
	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgemgr"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/repo"
	caicruntime "github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/apiconv"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
	"github.com/caic-xyz/caic/oauth/oauthclient"
)

type serverHandlers struct {
	log                *slog.Logger
	serverCtx          context.Context
	runtimes           *caicruntime.Router
	tailscaleAvailable bool
	forgeMgr           *forgemgr.Manager
	prefs              *preferences.Store
	checkouts          *repo.Registry
	checkoutRoot       string
	repoStatus         *ci.RepoStatusStore
	taskMgr            *taskmgr.Manager
	cacheSizes         *CacheSizeStore
	authStore          *auth.Store
	githubOAuth        *oauthclient.ProviderConfig
	gitlabOAuth        *oauthclient.ProviderConfig
	googleOAuth        *oauthclient.ProviderConfig
	voiceGateway       v1.VoiceGatewayMetadata
}

func (h *serverHandlers) getConfig(ctx context.Context, _ *api.EmptyReq) (*v1.Config, error) {
	displayName, err := os.Hostname()
	if err != nil {
		h.log.WarnContext(ctx, "failed to get hostname", "err", err)
	}
	displayName, _, _ = strings.Cut(displayName, ".")
	cfg := &v1.Config{
		Version:              autoupdate.Version,
		DisplayName:          displayName,
		TailscaleAvailable:   h.tailscaleAvailable,
		USBAvailable:         runtime.GOOS == "linux",
		DisplayAvailable:     true,
		SudoAvailable:        true,
		GitHubTokenAvailable: h.forgeMgr.GitHubToken() != "" || h.githubOAuth != nil,
		VoiceGateway:         h.voiceGateway,
		GitHubAppEnabled:     h.forgeMgr.GitHubApp() != nil,
	}
	if h.authStore != nil {
		cfg.AuthProviders = h.authProviders()
	}
	cfg.Runtimes = make([]v1.RuntimeInfo, len(h.runtimes.Runtimes))
	for i, rt := range h.runtimes.Runtimes {
		cfg.Runtimes[i] = v1.RuntimeInfo{Name: string(rt.Name())}
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
	gh := h.forgeMgr.GitHubClient()
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
	gh := h.forgeMgr.GitHubClient()
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
		h.log.InfoContext(h.serverCtx, "update triggered by user", "current", current, "latest", latest)
		if err := autoupdate.CheckAndUpdate(h.serverCtx, gh); err != nil {
			h.log.WarnContext(h.serverCtx, "background update failed", "err", err)
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
		target, err := md.ResolveMountTarget(m.HostPath, m.ContainerPath)
		effectivePath := ""
		if err != nil {
			h.log.ErrorContext(ctx, "stored cache mapping is invalid", "index", i, "err", err)
		} else {
			effectivePath = md.ResolveContainerPath(target)
		}
		cacheMappings[i] = v1.CacheMappingResp{
			HostPath:              m.HostPath,
			ContainerPath:         m.ContainerPath,
			ResolvedContainerPath: effectivePath,
			Enabled:               m.Enabled,
		}
	}
	customMounts := make([]v1.MountMappingResp, len(prefs.Settings.CustomMounts))
	for i, m := range prefs.Settings.CustomMounts {
		target, err := md.ResolveMountTarget(m.HostPath, m.ContainerPath)
		effectivePath := ""
		if err != nil {
			h.log.ErrorContext(ctx, "stored custom mount is invalid", "index", i, "err", err)
		} else {
			effectivePath = md.ResolveContainerPath(target)
		}
		customMounts[i] = v1.MountMappingResp{
			HostPath:              m.HostPath,
			ContainerPath:         m.ContainerPath,
			ResolvedContainerPath: effectivePath,
			Enabled:               m.Enabled,
			ReadOnly:              m.ReadOnly,
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
			RuntimeName:        prefs.Settings.RuntimeName,
			WellKnownCaches:    prefs.Settings.WellKnownCaches,
			CacheMappings:      cacheMappings,
			CustomMounts:       customMounts,
		},
	}, nil
}

func (h *serverHandlers) updatePreferences(ctx context.Context, req *v1.UpdatePreferencesReq) (*v1.PreferencesResp, error) {
	if err := validatePreferenceSettings(&req.Settings); err != nil {
		return nil, err
	}
	if req.Settings.RuntimeName != "" {
		if _, ok := h.runtimes.ByName[caicruntime.Name(req.Settings.RuntimeName)]; !ok {
			return nil, api.BadRequest(fmt.Sprintf("unknown runtime %q", req.Settings.RuntimeName))
		}
	}
	if err := h.prefs.Update(userIDFromCtx(ctx), func(p *preferences.Preferences) {
		p.Settings.AutoFixOnCIFailure = req.Settings.AutoFixOnCIFailure
		p.Settings.AutoFixOnPROpen = req.Settings.AutoFixOnPROpen
		p.Settings.BaseImage = req.Settings.BaseImage
		p.Settings.ContainerPlatform = md.Platform(req.Settings.ContainerPlatform)
		p.Settings.MaxCPUs = req.Settings.MaxCPUs
		p.Settings.RuntimeName = req.Settings.RuntimeName
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

// validatePreferenceSettings verifies settings that require runtime-owned data.
func validatePreferenceSettings(settings *v1.UserSettings) error {
	for name := range settings.WellKnownCaches {
		if _, ok := md.WellKnownCaches[name]; !ok {
			return api.BadRequest("unknown cache: " + name)
		}
	}
	for i, m := range settings.CacheMappings {
		if _, err := md.ResolveMountTarget(m.HostPath, m.ContainerPath); err != nil {
			return api.BadRequest(fmt.Sprintf("cacheMappings[%d]: %s", i, err))
		}
	}
	for i, m := range settings.CustomMounts {
		if _, err := md.ResolveMountTarget(m.HostPath, m.ContainerPath); err != nil {
			return api.BadRequest(fmt.Sprintf("customMounts[%d]: %s", i, err))
		}
	}
	return nil
}

func cacheMountsFromSettings(settings *preferences.Settings) ([]caicruntime.CacheMount, error) {
	var caches []caicruntime.CacheMount
	names := slices.Sorted(maps.Keys(md.WellKnownCaches))
	for _, name := range names {
		if !settings.WellKnownCaches[name] {
			continue
		}
		for _, c := range md.WellKnownCaches[name] {
			caches = append(caches, caicruntime.CacheMount{
				Name:          c.Name,
				Description:   c.Description,
				HostPath:      c.HostPath,
				ContainerPath: c.ContainerPath,
				ReadOnly:      c.ReadOnly,
				Shallow:       c.Shallow,
			})
		}
	}
	for i, m := range settings.CacheMappings {
		if !m.Enabled {
			continue
		}
		target, err := md.ResolveMountTarget(m.HostPath, m.ContainerPath)
		if err != nil {
			return nil, fmt.Errorf("custom cache %d: %w", i, err)
		}
		caches = append(caches, caicruntime.CacheMount{
			Name:          fmt.Sprintf("custom-cache-%d", i),
			HostPath:      m.HostPath,
			ContainerPath: md.ResolveContainerPath(target),
		})
	}
	return caches, nil
}

func mountsFromSettings(settings *preferences.Settings) ([]caicruntime.Mount, error) {
	mounts := make([]caicruntime.Mount, 0, len(settings.CustomMounts))
	for i, m := range settings.CustomMounts {
		if !m.Enabled {
			continue
		}
		target, err := md.ResolveMountTarget(m.HostPath, m.ContainerPath)
		if err != nil {
			return nil, fmt.Errorf("custom mount %d: %w", i, err)
		}
		mounts = append(mounts, caicruntime.Mount{
			HostPath:      m.HostPath,
			ContainerPath: md.ResolveContainerPath(target),
			ReadOnly:      m.ReadOnly,
		})
	}
	return mounts, nil
}

func (h *serverHandlers) listHarnesses(_ context.Context, _ *api.EmptyReq) (*[]v1.HarnessInfo, error) {
	backends := h.taskMgr.Backends
	out := make([]v1.HarnessInfo, 0, len(backends))
	for h, b := range backends {
		name, err := apiconv.Harness(h)
		if err != nil {
			return nil, fmt.Errorf("convert available harness %q: %w", h, err)
		}
		inventory := b.ModelInventory()
		models := make([]v1.Model, 0, len(inventory.Models))
		for _, model := range inventory.Models {
			models = append(models, v1.Model{
				ID:            model.ID,
				EffortOptions: nonNilSlice(model.EffortOptions),
			})
		}
		out = append(out, v1.HarnessInfo{
			Name:            name,
			Models:          models,
			SupportsImages:  b.SupportsImages(),
			SupportsCompact: b.SupportsCompact(),
		})
	}
	slices.SortFunc(out, func(a, b v1.HarnessInfo) int {
		return strings.Compare(string(a.Name), string(b.Name))
	})
	return &out, nil
}

func nonNilSlice[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
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
	return &v1.CacheSizesResp{WellKnown: h.cacheSizes.Snapshot()}, nil
}

func (h *serverHandlers) listRepos(_ context.Context, _ *api.EmptyReq) (*[]v1.Repo, error) {
	return repoListFromSnapshot(h.log, h.checkouts.Checkouts(), h.repoStatus), nil
}

func (h *serverHandlers) handleListRepoBranches(w http.ResponseWriter, r *http.Request) {
	repoPath := r.URL.Query().Get("repo")
	if repoPath == "" {
		writeError(r.Context(), w, api.BadRequest("repo is required"))
		return
	}
	info, ok := h.checkouts.Checkout(repoPath)
	if !ok {
		writeError(r.Context(), w, api.NotFound("repo not found"))
		return
	}
	absPath := info.Dir
	ctx := r.Context()
	checkout := &git.Checkout{Root: absPath, Logger: h.log.With("repo", repoPath)}
	// Fetch local branches.
	localPairs, err := checkout.ListBranches(ctx, "")
	if err != nil {
		h.log.WarnContext(ctx, "list local branches failed", "repo", repoPath, "err", err)
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
		h.log.WarnContext(ctx, "list remotes failed", "repo", repoPath, "err", err)
	}
	for remote := range strings.SplitSeq(remoteList, "\n") {
		if remote == "" {
			continue
		}
		remotePairs, err := checkout.ListBranches(ctx, remote)
		if err != nil {
			h.log.WarnContext(ctx, "list remote branches failed", "repo", repoPath, "remote", remote, "err", err)
			continue
		}
		for _, p := range remotePairs {
			if _, ok := seen[p[0]]; !ok {
				seen[p[0]] = struct{}{}
				branches = append(branches, v1.BranchInfo{Name: p[0], Remote: remote})
			}
		}
	}
	writeJSONResponse(r.Context(), w, &v1.RepoBranchesResp{Branches: branches}, nil)
}

func (h *serverHandlers) cloneRepo(ctx context.Context, req *v1.CloneRepoReq) (*v1.Repo, error) {
	target, err := cloneTarget(h.checkoutRoot, req.URL, req.Path, h.checkouts)
	if err != nil {
		return nil, err
	}
	info, err := repo.Clone(ctx, h.log.With("repo", target.relPath), req.URL, target.dir, req.Depth)
	if err != nil {
		return nil, api.InternalError(err.Error())
	}
	info.RelPath = target.relPath
	if err := h.checkouts.RegisterCheckout(info); err != nil {
		if removeErr := os.RemoveAll(target.dir); removeErr != nil {
			return nil, api.InternalError(fmt.Sprintf("register checkout: %v; remove checkout: %v", err, removeErr))
		}
		return nil, api.Conflict(err.Error())
	}
	h.log.InfoContext(ctx, "cloned repo", "url", req.URL, "path", target.relPath)
	var remote string
	var kind forge.Kind
	if info.Repository != nil {
		remote = info.Repository.Remote
		kind = info.Repository.ForgeKind
	}
	forgeKind, err := apiconv.RepoForge(kind)
	if err != nil {
		return nil, api.InternalError(err.Error())
	}
	return &v1.Repo{
		Path:       info.RelPath,
		Branch:     info.BaseBranch,
		BaseBranch: v1.BranchInfo{Name: info.BaseBranch, Remote: info.BaseBranchRemote},
		RemoteURL:  git.RemoteToHTTPS(remote),
		Forge:      forgeKind,
	}, nil
}

type cloneTargetPath struct {
	relPath string
	dir     string
}

func cloneTarget(root, url, path string, checkouts *repo.Registry) (cloneTargetPath, error) {
	if root == "" {
		return cloneTargetPath{}, api.InternalError("checkout root is not configured")
	}
	if path == "" {
		path = strings.TrimSuffix(filepath.Base(url), ".git")
		if path == "" || path == "." || path == string(filepath.Separator) {
			return cloneTargetPath{}, api.BadRequest("cannot derive repo name from URL; specify path explicitly")
		}
	}
	dir := filepath.Join(root, path)
	relPath, err := filepath.Rel(root, dir)
	if err != nil || relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
		return cloneTargetPath{}, api.BadRequest("path escapes root directory")
	}
	if _, err := os.Stat(dir); err == nil {
		return cloneTargetPath{}, api.Conflict("directory already exists: " + relPath)
	} else if !os.IsNotExist(err) {
		return cloneTargetPath{}, api.InternalError("stat clone directory: " + err.Error())
	}
	if _, ok := checkouts.Checkout(relPath); ok {
		return cloneTargetPath{}, api.Conflict("repo already registered: " + relPath)
	}
	base := filepath.Base(relPath)
	for checkout := range checkouts.Checkouts() {
		if checkout.RelPath != "" && filepath.Base(checkout.RelPath) == base && checkout.RelPath != relPath {
			return cloneTargetPath{}, api.Conflict("repo basename conflicts with existing: " + checkout.RelPath)
		}
	}
	return cloneTargetPath{relPath: relPath, dir: dir}, nil
}

func repoListFromSnapshot(log *slog.Logger, snap iter.Seq[*repo.Checkout], repoStatus *ci.RepoStatusStore) *[]v1.Repo {
	var out []v1.Repo
	for checkout := range snap {
		out = append(out, repoDTO(log, checkout, repoStatus))
	}
	return &out
}

func repoDTO(log *slog.Logger, checkout *repo.Checkout, repoStatus *ci.RepoStatusStore) v1.Repo {
	var remote string
	var kind forge.Kind
	if checkout.Repository != nil {
		remote = checkout.Repository.Remote
		kind = checkout.Repository.ForgeKind
	}
	forgeKind, err := apiconv.RepoForge(kind)
	if err != nil {
		log.Error("convert repository forge", "repo", checkout.RelPath, "err", err)
	}
	dto := v1.Repo{
		Path:       checkout.RelPath,
		Branch:     checkout.BaseBranch,
		BaseBranch: v1.BranchInfo{Name: checkout.BaseBranch, Remote: checkout.BaseBranchRemote},
		RemoteURL:  git.RemoteToHTTPS(remote),
		Forge:      forgeKind,
	}
	if repoStatus != nil {
		if status, ok := repoStatus.StatusFor(checkout.RelPath); ok {
			ciStatus, err := apiconv.CIStatus(status.Status)
			if err != nil {
				log.Error("convert repository CI status", "repo", checkout.RelPath, "err", err)
			} else {
				dto.CI = ciStatus
			}
			dto.CIChecks = make([]v1.ForgeCheck, 0, len(status.Checks))
			for i := range status.Checks {
				check, err := apiconv.ForgeCheck(&status.Checks[i])
				if err != nil {
					log.Error("convert repository CI check", "repo", checkout.RelPath, "err", err)
					continue
				}
				dto.CIChecks = append(dto.CIChecks, check)
			}
			if len(dto.CIChecks) == 0 {
				dto.CIChecks = nil
			}
		}
	}
	return dto
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

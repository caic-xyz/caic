// HTTP handlers for server configuration, preferences, repos, and voice token.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/autoupdate"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/server/dto"
	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/md"
	"github.com/caic-xyz/md/gitutil"
)

func (s *Server) getConfig(_ context.Context, _ *dto.EmptyReq) (*v1.Config, error) {
	cfg := &v1.Config{
		Version:            autoupdate.Version,
		TailscaleAvailable: s.mdClient.TailscaleAPIKey != "",
		USBAvailable:       runtime.GOOS == "linux",
		DisplayAvailable:   true,
		GitHubAppEnabled:   s.forge.githubApp != nil,
	}
	if s.authEnabled() {
		cfg.AuthProviders = s.authProviders()
	}
	return cfg, nil
}

func (s *Server) getPreferences(ctx context.Context, _ *dto.EmptyReq) (*v1.PreferencesResp, error) {
	prefs := s.prefs.Get(userIDFromCtx(ctx))
	recent := prefs.RecentRepos(time.Now())
	repos := make([]v1.RepoPrefsResp, len(recent))
	for i, r := range recent {
		repos[i] = v1.RepoPrefsResp{
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
		}
	}
	return &v1.PreferencesResp{
		Repositories: repos,
		Harness:      prefs.Harness,
		Models:       prefs.Models,
		Settings: v1.UserSettings{
			AutoFixOnCIFailure: prefs.Settings.AutoFixOnCIFailure,
			AutoFixOnPROpen:    prefs.Settings.AutoFixOnPROpen,
			BaseImage:          prefs.Settings.BaseImage,
			UseDefaultCaches:   prefs.Settings.UseDefaultCaches,
			WellKnownCaches:    prefs.Settings.WellKnownCaches,
			CacheMappings:      cacheMappings,
		},
	}, nil
}

func (s *Server) updatePreferences(ctx context.Context, req *v1.UpdatePreferencesReq) (*v1.PreferencesResp, error) {
	if err := s.prefs.Update(userIDFromCtx(ctx), func(p *preferences.Preferences) {
		p.Settings.AutoFixOnCIFailure = req.Settings.AutoFixOnCIFailure
		p.Settings.AutoFixOnPROpen = req.Settings.AutoFixOnPROpen
		p.Settings.BaseImage = req.Settings.BaseImage
		p.Settings.UseDefaultCaches = req.Settings.UseDefaultCaches
		p.Settings.WellKnownCaches = req.Settings.WellKnownCaches
		if req.Settings.CacheMappings != nil {
			p.Settings.CacheMappings = make([]preferences.CacheMapping, len(req.Settings.CacheMappings))
			for i, m := range req.Settings.CacheMappings {
				p.Settings.CacheMappings[i] = preferences.CacheMapping{
					HostPath:      m.HostPath,
					ContainerPath: m.ContainerPath,
				}
			}
		}
	}); err != nil {
		return nil, dto.InternalError("save preferences: " + err.Error())
	}
	// Return the updated preferences.
	return s.getPreferences(ctx, nil)
}

func (s *Server) listHarnesses(_ context.Context, _ *dto.EmptyReq) (*[]v1.HarnessInfo, error) {
	// Collect unique harness backends from all runners.
	seen := make(map[agent.Harness]agent.Backend)
	for _, r := range s.runners {
		for h, b := range r.Backends {
			seen[h] = b
		}
	}
	out := make([]v1.HarnessInfo, 0, len(seen))
	for h, b := range seen {
		out = append(out, v1.HarnessInfo{Name: string(h), Models: b.Models(), SupportsImages: b.SupportsImages()})
	}
	slices.SortFunc(out, func(a, b v1.HarnessInfo) int {
		return strings.Compare(a.Name, b.Name)
	})
	return &out, nil
}

func (s *Server) listCaches(_ context.Context, _ *dto.EmptyReq) (*v1.WellKnownCachesResp, error) {
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

func (s *Server) listRepos(_ context.Context, _ *dto.EmptyReq) (*[]v1.Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reposLocked(), nil
}

func (s *Server) handleListRepoBranches(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		writeError(w, dto.BadRequest("repo is required"))
		return
	}
	absPath, ok := s.repoAbsPath(repo)
	if !ok {
		writeError(w, dto.NotFound("repo not found"))
		return
	}
	pairs, err := gitutil.ListBranches(r.Context(), absPath, "origin")
	if err != nil {
		slog.WarnContext(r.Context(), "list branches failed", "repo", repo, "err", err)
	}
	names := make([]string, len(pairs))
	for i, p := range pairs {
		names[i] = p[0]
	}
	writeJSONResponse(w, &v1.RepoBranchesResp{Branches: names}, nil)
}

func (s *Server) cloneRepo(ctx context.Context, req *v1.CloneRepoReq) (*v1.Repo, error) {
	// Derive target relative path.
	targetPath := req.Path
	if targetPath == "" {
		// Extract basename from URL, stripping .git suffix.
		base := filepath.Base(req.URL)
		base = strings.TrimSuffix(base, ".git")
		if base == "" || base == "." || base == "/" {
			return nil, dto.BadRequest("cannot derive repo name from URL; specify path explicitly")
		}
		targetPath = base
	}

	absTarget := filepath.Join(s.absRoot, targetPath)
	// Defense-in-depth: ensure the resolved path is under absRoot.
	if rel, err := filepath.Rel(s.absRoot, absTarget); err != nil || strings.HasPrefix(rel, "..") {
		return nil, dto.BadRequest("path escapes root directory")
	}

	// Check if directory already exists.
	if _, err := os.Stat(absTarget); err == nil {
		return nil, dto.Conflict("directory already exists: " + targetPath)
	}

	// Check if path already registered.
	if _, ok := s.runners[targetPath]; ok {
		return nil, dto.Conflict("repo already registered: " + targetPath)
	}

	// Determine clone depth.
	depth := req.Depth
	if depth == 0 {
		depth = 1
	}

	// Run git clone with timeout.
	cloneCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	args := []string{"clone", "--depth", strconv.Itoa(depth), "--recurse-submodules", "--shallow-submodules", req.URL, absTarget}
	cmd := exec.CommandContext(cloneCtx, "git", args...) //nolint:gosec // args are validated: depth is an int, URL is user-provided input, absTarget is validated above
	if out, err := cmd.CombinedOutput(); err != nil {
		// Clean up partial clone.
		_ = os.RemoveAll(absTarget)
		slog.Warn("git clone failed", "url", req.URL, "err", err, "out", string(out))
		return nil, dto.InternalError("git clone failed: " + err.Error())
	}

	// Discover repo info.
	branch, err := gitutil.DefaultBranch(ctx, absTarget, "origin")
	if err != nil {
		_ = os.RemoveAll(absTarget)
		return nil, dto.InternalError("cannot determine default branch: " + err.Error())
	}
	remote := gitutil.RemoteOriginURL(ctx, absTarget)

	// Create and init runner.
	runner := &task.Runner{
		BaseBranch: branch,
		Dir:        absTarget,
		LogDir:     s.logDir,
		Container:  s.backend,
	}
	if err := runner.Init(ctx); err != nil {
		_ = os.RemoveAll(absTarget)
		return nil, dto.InternalError("failed to init runner: " + err.Error())
	}

	var cloneForgeKind forge.Kind
	var cloneForgeOwner, cloneForgeRepo string
	if rawURL, err := forge.RemoteURL(ctx, absTarget); err == nil {
		cloneForgeKind, cloneForgeOwner, cloneForgeRepo, _ = forge.ParseRemoteURL(rawURL)
	}
	info := repoInfo{RelPath: targetPath, AbsPath: absTarget, BaseBranch: branch, Remote: remote, ForgeKind: cloneForgeKind, ForgeOwner: cloneForgeOwner, ForgeRepo: cloneForgeRepo}
	s.repos = append(s.repos, info)
	s.runners[targetPath] = runner
	slog.Info("cloned repo", "url", req.URL, "path", targetPath)

	return &v1.Repo{Path: targetPath, BaseBranch: branch, RemoteURL: gitutil.RemoteToHTTPS(remote), Forge: v1.Forge(cloneForgeKind)}, nil
}

// getVoiceToken returns a Gemini API credential for the Android voice client.
//
// Currently returns the raw GEMINI_API_KEY (ephemeral=false) because the
// v1alpha ephemeral endpoint produces lower-quality responses. The client uses
// the ephemeral field to decide the WebSocket URL and auth parameter.
//
// TODO(security): Switch back to ephemeral tokens once v1beta supports
// auth_tokens or v1alpha quality improves. See getVoiceTokenEphemeral.
func (s *Server) getVoiceToken(_ context.Context, _ *dto.EmptyReq) (*v1.VoiceTokenResp, error) {
	apiKey := s.geminiAPIKey
	if apiKey == "" {
		return nil, dto.InternalError("GEMINI_API_KEY not configured")
	}
	slog.Info("voice token", "keylen", len(apiKey), "mode", "raw_key")
	expireTime := time.Now().UTC().Add(30 * time.Minute).Format(time.RFC3339)
	return &v1.VoiceTokenResp{
		Token:     apiKey,
		ExpiresAt: expireTime,
	}, nil
}

// getVoiceTokenEphemeral creates a short-lived Gemini ephemeral token via
// POST /v1alpha/auth_tokens. Ephemeral tokens are v1alpha only; v1beta
// returns 404. The client must use v1alpha + BidiGenerateContentConstrained
// with ?access_token=.
//
// This path works but produces lower-quality voice responses than the v1beta
// BidiGenerateContent endpoint with a raw key. Kept for future use once Google
// stabilises v1beta ephemeral tokens.
//
// See https://ai.google.dev/gemini-api/docs/ephemeral-tokens
func (s *Server) getVoiceTokenEphemeral(ctx context.Context, _ *dto.EmptyReq) (*v1.VoiceTokenResp, error) { //nolint:unused // kept for future use
	apiKey := s.geminiAPIKey
	if apiKey == "" {
		return nil, dto.InternalError("GEMINI_API_KEY not configured")
	}
	slog.Info("voice token", "keylen", len(apiKey), "mode", "ephemeral")
	now := time.Now().UTC()
	expireTime := now.Add(30 * time.Minute).Format(time.RFC3339)
	newSessionExpire := now.Add(2 * time.Minute).Format(time.RFC3339)

	reqBody := CreateAuthTokenConfig{
		Uses:                 1,
		ExpireTime:           expireTime,
		NewSessionExpireTime: newSessionExpire,
	}
	bodyBytes, err := json.Marshal(&reqBody)
	if err != nil {
		return nil, dto.InternalError("failed to marshal token request").Wrap(err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://generativelanguage.googleapis.com/v1alpha/auth_tokens",
		bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, dto.InternalError("failed to create token request").Wrap(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, dto.InternalError("failed to fetch ephemeral token").Wrap(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, dto.InternalError(fmt.Sprintf("Gemini auth_tokens returned %d: %s", resp.StatusCode, string(body)))
	}

	var tokenResp AuthToken
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, dto.InternalError("failed to decode token response").Wrap(err)
	}

	tokenPrefix := tokenResp.Name
	if len(tokenPrefix) > 16 {
		tokenPrefix = tokenPrefix[:16]
	}
	slog.Info("voice token", "prefix", tokenPrefix, "len", len(tokenResp.Name))

	return &v1.VoiceTokenResp{
		Token:     tokenResp.Name,
		ExpiresAt: expireTime,
		Ephemeral: true,
	}, nil
}

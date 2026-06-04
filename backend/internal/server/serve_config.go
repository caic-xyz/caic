// HTTP handlers for server configuration, preferences, repos, and voice token.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/caic-xyz/md"
	"github.com/caic-xyz/md/gitutil"

	"github.com/caic-xyz/caic/backend/internal/agent"
	api "github.com/caic-xyz/caic/backend/internal/api"
	v1 "github.com/caic-xyz/caic/backend/internal/api/v1"
	"github.com/caic-xyz/caic/backend/internal/autoupdate"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	caicruntime "github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/voicegateway"
)

const voiceGatewayTokenAudience = "voice-gateway"

func (s *Server) getConfig(_ context.Context, _ *api.EmptyReq) (*v1.Config, error) {
	displayName, err := os.Hostname()
	if err != nil {
		slog.Warn("failed to get hostname", "err", err)
	}
	displayName, _, _ = strings.Cut(displayName, ".")
	cfg := &v1.Config{
		Version:              autoupdate.Version,
		DisplayName:          displayName,
		TailscaleAvailable:   s.tailscaleAvailable,
		USBAvailable:         runtime.GOOS == "linux",
		DisplayAvailable:     true,
		SudoAvailable:        true,
		GitHubTokenAvailable: s.forge.githubToken != "" || s.githubOAuth != nil,
		VoiceGateway:         s.voiceGatewayMetadata(),
		GitHubAppEnabled:     s.forge.githubApp != nil,
	}
	if s.authEnabled() {
		cfg.AuthProviders = s.authProviders()
	}
	return cfg, nil
}

func (s *Server) voiceGatewayMetadata() v1.VoiceGatewayMetadata {
	cfg := s.voiceGateway
	if cfg.Mode == "" {
		if s.voiceBridge != nil {
			cfg.Mode = VoiceGatewayModeEmbedded
		} else {
			cfg.Mode = VoiceGatewayModeDisabled
		}
	}
	switch cfg.Mode {
	case VoiceGatewayModeEmbedded:
		if s.voiceBridge == nil {
			return v1.VoiceGatewayMetadata{Mode: v1.VoiceGatewayModeDisabled}
		}
		return v1.VoiceGatewayMetadata{ //nolint:gosec // G101: token metadata field names, not credentials.
			Mode:               v1.VoiceGatewayModeEmbedded,
			MinGatewayProtocol: voicegateway.ProtocolVersion,
			AuthRequired:       false,
			TokenEndpoint:      "/api/v1/voice/token",
			TokenAudience:      voiceGatewayTokenAudience,
			Capabilities:       []string{"voice.gatewayGeminiLive"},
		}
	case VoiceGatewayModeExternal:
		return v1.VoiceGatewayMetadata{ //nolint:gosec // G101: token metadata field names, not credentials.
			Mode:               v1.VoiceGatewayModeExternal,
			URL:                cfg.URL,
			MinGatewayProtocol: voicegateway.ProtocolVersion,
			AuthRequired:       true,
			TokenEndpoint:      "/api/v1/voice/token",
			TokenAudience:      voiceGatewayTokenAudience,
			Capabilities:       []string{"voice.gatewayGeminiLive"},
		}
	default:
		return v1.VoiceGatewayMetadata{Mode: v1.VoiceGatewayModeDisabled}
	}
}

// getVersion returns the current server version and checks GitHub for the latest release.
func (s *Server) getVersion(ctx context.Context, _ *api.EmptyReq) (*v1.VersionResp, error) {
	current := autoupdate.Version
	gh := s.forge.githubClient()
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
func (s *Server) triggerUpdate(ctx context.Context, _ *api.EmptyReq) (*v1.UpdateResp, error) {
	gh := s.forge.githubClient()
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
		slog.Info("update triggered by user", "current", current, "latest", latest)
		if err := autoupdate.CheckAndUpdate(s.ctx, gh); err != nil {
			slog.Warn("background update failed", "err", err)
		}
	}()
	return &v1.UpdateResp{Status: "started"}, nil
}

func (s *Server) getPreferences(ctx context.Context, _ *api.EmptyReq) (*v1.PreferencesResp, error) {
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
	customMounts := make([]v1.MountMappingResp, len(prefs.Settings.CustomMounts))
	for i, m := range prefs.Settings.CustomMounts {
		customMounts[i] = v1.MountMappingResp{
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
			ContainerPlatform:  prefs.Settings.ContainerPlatform,
			MaxCPUs:            prefs.Settings.MaxCPUs,
			UseDefaultCaches:   prefs.Settings.UseDefaultCaches,
			WellKnownCaches:    prefs.Settings.WellKnownCaches,
			CacheMappings:      cacheMappings,
			CustomMounts:       customMounts,
		},
	}, nil
}

func (s *Server) updatePreferences(ctx context.Context, req *v1.UpdatePreferencesReq) (*v1.PreferencesResp, error) {
	if err := s.prefs.Update(userIDFromCtx(ctx), func(p *preferences.Preferences) {
		p.Settings.AutoFixOnCIFailure = req.Settings.AutoFixOnCIFailure
		p.Settings.AutoFixOnPROpen = req.Settings.AutoFixOnPROpen
		p.Settings.BaseImage = req.Settings.BaseImage
		p.Settings.ContainerPlatform = req.Settings.ContainerPlatform
		p.Settings.MaxCPUs = req.Settings.MaxCPUs
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
		if req.Settings.CustomMounts != nil {
			p.Settings.CustomMounts = make([]preferences.MountMapping, len(req.Settings.CustomMounts))
			for i, m := range req.Settings.CustomMounts {
				p.Settings.CustomMounts[i] = preferences.MountMapping{
					HostPath:      m.HostPath,
					ContainerPath: m.ContainerPath,
				}
			}
		}
	}); err != nil {
		return nil, api.InternalError("save preferences: " + err.Error())
	}
	// Return the updated preferences.
	return s.getPreferences(ctx, nil)
}

func cacheMountsFromSettings(settings *preferences.Settings) []caicruntime.CacheMount {
	var caches []caicruntime.CacheMount
	if settings.UseDefaultCaches {
		names := slices.Sorted(maps.Keys(md.WellKnownCaches))
		for _, name := range names {
			if enabled, ok := settings.WellKnownCaches[name]; ok && !enabled {
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
	}
	for _, m := range settings.CacheMappings {
		caches = append(caches, caicruntime.CacheMount{
			Name:      "custom:" + m.ContainerPath,
			HostPath:  m.HostPath,
			MountPath: m.ContainerPath,
		})
	}
	for _, m := range settings.CustomMounts {
		caches = append(caches, caicruntime.CacheMount{
			Name:      "custom-mount:" + m.ContainerPath,
			HostPath:  m.HostPath,
			MountPath: m.ContainerPath,
		})
	}
	return caches
}

func (s *Server) listHarnesses(_ context.Context, _ *api.EmptyReq) (*[]v1.HarnessInfo, error) {
	// Collect unique harness backends from all runners.
	seen := make(map[agent.Harness]agent.Backend)
	s.taskMgr.RangeRunners(func(_ string, r *task.Runner) bool {
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

func (s *Server) listCaches(_ context.Context, _ *api.EmptyReq) (*v1.WellKnownCachesResp, error) {
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

func (s *Server) listRepos(_ context.Context, _ *api.EmptyReq) (*[]v1.Repo, error) {
	return s.repoList(), nil
}

func (s *Server) handleListRepoBranches(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		writeError(w, api.BadRequest("repo is required"))
		return
	}
	absPath, ok := s.repoAbsPath(repo)
	if !ok {
		writeError(w, api.NotFound("repo not found"))
		return
	}
	ctx := r.Context()
	// Fetch local branches.
	localPairs, err := gitutil.ListBranches(ctx, absPath, "")
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
	remoteList, err := gitutil.RunGit(ctx, absPath, "remote")
	if err != nil {
		slog.WarnContext(ctx, "list remotes failed", "repo", repo, "err", err)
	}
	for remote := range strings.SplitSeq(remoteList, "\n") {
		if remote == "" {
			continue
		}
		remotePairs, err := gitutil.ListBranches(ctx, absPath, remote)
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

func (s *Server) cloneRepo(ctx context.Context, req *v1.CloneRepoReq) (*v1.Repo, error) {
	// Derive target relative path.
	targetPath := req.Path
	if targetPath == "" {
		// Extract basename from URL, stripping .git suffix.
		base := filepath.Base(req.URL)
		base = strings.TrimSuffix(base, ".git")
		if base == "" || base == "." || base == "/" {
			return nil, api.BadRequest("cannot derive repo name from URL; specify path explicitly")
		}
		targetPath = base
	}

	absTarget := filepath.Join(s.absRoot, targetPath)
	// Defense-in-depth: ensure the resolved path is under absRoot.
	if rel, err := filepath.Rel(s.absRoot, absTarget); err != nil || strings.HasPrefix(rel, "..") {
		return nil, api.BadRequest("path escapes root directory")
	} else {
		targetPath = rel
	}

	// Check if directory already exists.
	if _, err := os.Stat(absTarget); err == nil {
		return nil, api.Conflict("directory already exists: " + targetPath)
	}

	// Check if path already registered.
	if _, ok := s.taskMgr.Runner(targetPath); ok {
		return nil, api.Conflict("repo already registered: " + targetPath)
	}

	// Reject when the basename collides with an existing repo from a
	// different parent directory.
	bn := filepath.Base(targetPath)
	var basenameConflict string
	s.taskMgr.RangeRunners(func(rel string, _ *task.Runner) bool {
		if rel != "" && filepath.Base(rel) == bn && rel != targetPath {
			basenameConflict = rel
			return false
		}
		return true
	})
	if basenameConflict != "" {
		return nil, api.Conflict("repo basename conflicts with existing: " + basenameConflict)
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
		return nil, api.InternalError("git clone failed: " + err.Error())
	}

	// Discover repo info.
	remoteName, err := gitutil.DefaultRemote(ctx, absTarget)
	if err != nil {
		_ = os.RemoveAll(absTarget)
		return nil, api.InternalError("cannot determine default remote: " + err.Error())
	}
	branch, err := gitutil.DefaultBranch(ctx, absTarget, remoteName)
	if err != nil {
		_ = os.RemoveAll(absTarget)
		return nil, api.InternalError("cannot determine default branch: " + err.Error())
	}
	remote := gitutil.RemoteOriginURL(ctx, absTarget)
	info := RepoInfo{RelPath: targetPath, AbsPath: absTarget, BaseBranch: branch, BaseBranchRemote: remoteName, Remote: remote}
	runner, err := s.newRunner(ctx, &info)
	if err != nil {
		_ = os.RemoveAll(absTarget)
		return nil, api.InternalError("failed to init runner: " + err.Error())
	}
	if rawURL, err := forge.RemoteURL(ctx, absTarget); err == nil {
		info.ForgeKind, info.ForgeOwner, info.ForgeRepo, _ = forge.ParseRemoteURL(rawURL)
	}
	// Add to the registry first, then register the runner (see repoRegistry's
	// ordering invariant).
	s.repoReg.add(&info)
	s.taskMgr.RegisterRunner(targetPath, runner)
	slog.Info("cloned repo", "url", req.URL, "path", targetPath)

	return &v1.Repo{Path: targetPath, Branch: branch, BaseBranch: v1.BranchInfo{Name: branch, Remote: remoteName}, RemoteURL: gitutil.RemoteToHTTPS(remote), Forge: v1.Forge(info.ForgeKind)}, nil
}

// getVoiceToken returns a Gemini API credential for the Android voice client.
//
// Currently returns the raw GEMINI_API_KEY (ephemeral=false) because the
// v1alpha ephemeral endpoint produces lower-quality responses. The client uses
// the ephemeral field to decide the WebSocket URL and auth parameter.
//
// TODO(security): Switch back to ephemeral tokens once v1beta supports
// auth_tokens or v1alpha quality improves. See getVoiceTokenEphemeral.
func (s *Server) getVoiceToken(_ context.Context, _ *api.EmptyReq) (*v1.VoiceTokenResp, error) {
	apiKey := s.geminiAPIKey
	if apiKey == "" {
		return nil, api.InternalError("GEMINI_API_KEY not configured")
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
func (s *Server) getVoiceTokenEphemeral(ctx context.Context, _ *api.EmptyReq) (*v1.VoiceTokenResp, error) { //nolint:unused // kept for future use
	apiKey := s.geminiAPIKey
	if apiKey == "" {
		return nil, api.InternalError("GEMINI_API_KEY not configured")
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
		return nil, api.InternalError("failed to marshal token request").Wrap(err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://generativelanguage.googleapis.com/v1alpha/auth_tokens",
		bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, api.InternalError("failed to create token request").Wrap(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Goog-Api-Key", apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, api.InternalError("failed to fetch ephemeral token").Wrap(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, api.InternalError(fmt.Sprintf("Gemini auth_tokens returned %d: %s", resp.StatusCode, string(body)))
	}

	var tokenResp AuthToken
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, api.InternalError("failed to decode token response").Wrap(err)
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

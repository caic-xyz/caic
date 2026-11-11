// Tests release controls, server configuration, API parity, preferences, and runtime mappings.

package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgemgr"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/maruel/gomode/autoupdate"
)

func TestServerHandlers(t *testing.T) {
	t.Parallel()
	t.Run("image_refresh_requires_available_runtime", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		s.serverHandlers.imageRefresh = &ImageRefresh{Prefs: s.serverHandlers.prefs}
		_, err := s.serverHandlers.startImageRefresh(t.Context(), &v1.ImageRefreshReq{Runtime: "docker"})
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
			t.Fatalf("startImageRefresh error = %v, want bad request", err)
		}
		_, err = s.serverHandlers.getImageRefresh(t.Context(), &v1.ImageRefreshReq{Runtime: "docker"})
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
			t.Fatalf("getImageRefresh error = %v, want bad request", err)
		}
	})
	t.Run("image_refresh_status_uses_runtime_path", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		s.serverHandlers.imageRefresh = &ImageRefresh{Clients: map[string]ImageWarmer{"docker": &imageWarmerFake{}}}
		for _, tc := range []struct {
			runtime string
			want    int
		}{
			{runtime: "docker", want: http.StatusOK},
			{runtime: "podman", want: http.StatusBadRequest},
		} {
			t.Run(tc.runtime, func(t *testing.T) {
				t.Parallel()
				r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/server/runtimes/"+tc.runtime+"/image/refresh", nil)
				w := httptest.NewRecorder()
				s.serverHandlers.routes().ServeHTTP(w, r)
				if w.Code != tc.want {
					t.Fatalf("status = %d, want %d: %s", w.Code, tc.want, w.Body.String())
				}
			})
		}
	})
	t.Run("repo branches unknown repository", func(t *testing.T) {
		t.Parallel()

		s := newTestRouter(t, nil)
		w := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/server/repos/branches?repo=mistyped", nil)
		s.serverHandlers.handleListRepoBranches(w, r)

		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
		}
		err := decodeError(t, w)
		if err.Code != api.CodeNotFound {
			t.Errorf("code = %q, want %q", err.Code, api.CodeNotFound)
		}
		if err.Message != "repository not found" {
			t.Errorf("message = %q, want repository not found", err.Message)
		}
	})

	t.Run("repo branches describe task creation action", func(t *testing.T) {
		t.Parallel()
		dir := initCloneSourceRepo(t)
		runServerGit(t, dir, "remote", "add", "origin", dir)
		runServerGit(t, dir, "update-ref", "refs/remotes/origin/main", "main")
		runServerGit(t, dir, "branch", "available", "main")
		runServerGit(t, dir, "branch", "--set-upstream-to", "origin/main", "available")
		runServerGit(t, dir, "branch", "untracked", "main")
		runServerGit(t, dir, "update-ref", "refs/remotes/origin/remote-only", "main")

		s := newTestRouter(t, nil)
		registerRouterCheckout(t, s.checkouts, "repo", newRouterTestCheckout(dir))
		w := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/server/repos/branches?repo=repo", nil)
		s.serverHandlers.handleListRepoBranches(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		var resp v1.RepoBranchesResp
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		got := make(map[string]v1.BranchInfo, len(resp.Branches))
		for _, branch := range resp.Branches {
			got[branch.Name] = branch
		}
		if got["available"].Action != v1.BranchActionAdopt {
			t.Errorf("available action = %q, want adopt", got["available"].Action)
		}
		if got["untracked"].Action != v1.BranchActionBranchOff {
			t.Errorf("untracked action = %q, want branch_off", got["untracked"].Action)
		}
		if branch := got["remote-only"]; branch.Remote != "origin" || branch.Action != v1.BranchActionBranchOff {
			t.Errorf("remote-only = %+v, want origin branch_off", branch)
		}
	})

	t.Run("list_harnesses", func(t *testing.T) {
		t.Parallel()

		s := newTestRouter(t, map[harness.Name]agent.Backend{
			harness.Claude: &agenttest.FakeBackend{QuotaProviderID: agent.QuotaProviderClaudeCode},
			harness.Codex: &agenttest.FakeBackend{
				QuotaProviderID: agent.QuotaProviderCodex,
				Inventory:       agent.ModelInventory{Models: []agent.Model{{ID: "gpt-5", EffortOptions: []string{"low", "ultra"}}}},
			},
			harness.Pi: &agenttest.FakeBackend{},
		})

		got, err := s.serverHandlers.listHarnesses(t.Context(), &api.EmptyReq{})
		if err != nil {
			t.Fatalf("listHarnesses: %v", err)
		}
		if len(*got) != 3 {
			t.Fatalf("len(harnesses) = %d, want 3", len(*got))
		}
		if (*got)[0].QuotaGroup != v1.QuotaProviderClaudeCode || (*got)[1].QuotaGroup != v1.QuotaProviderCodex || (*got)[2].QuotaGroup != "" {
			t.Fatalf("quota groups = [%q %q %q], want [claudecode codex empty]", (*got)[0].QuotaGroup, (*got)[1].QuotaGroup, (*got)[2].QuotaGroup)
		}
		if (*got)[0].LogoURL != "/logos/claudecode.svg" || (*got)[1].LogoURL != "/logos/codex.svg" || (*got)[2].LogoURL != "/logos/pi.svg" {
			t.Fatalf("logo URLs = [%q %q %q], want [claudecode codex pi]", (*got)[0].LogoURL, (*got)[1].LogoURL, (*got)[2].LogoURL)
		}
		models := (*got)[1].Models
		if len(models) != 1 || models[0].ID != "gpt-5" || len(models[0].EffortOptions) != 2 {
			t.Fatalf("models = %#v, want gpt-5 with [low ultra]", models)
		}
		data, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("marshal harnesses: %v", err)
		}
		if strings.Contains(string(data), "null") {
			t.Fatalf("harnesses JSON = %s, want arrays instead of null", data)
		}
		if !strings.Contains(string(data), `"models":[{"id":"gpt-5"`) {
			t.Fatalf("harnesses JSON = %s, want gpt-5 in models", data)
		}
		if strings.Contains(string(data), `"modelCapabilities":`) || strings.Contains(string(data), `"modes":`) {
			t.Fatalf("harnesses JSON = %s, want models as the only model configuration", data)
		}
	})

	t.Run("list_harnesses_rejects_unsupported_quota_provider", func(t *testing.T) {
		t.Parallel()

		s := newTestRouter(t, map[harness.Name]agent.Backend{
			harness.Claude: &agenttest.FakeBackend{QuotaProviderID: agent.QuotaProvider("other")},
		})
		if _, err := s.serverHandlers.listHarnesses(t.Context(), &api.EmptyReq{}); err == nil {
			t.Fatal("listHarnesses() error = nil, want unsupported quota provider error")
		}
	})

	t.Run("refresh_harness_bypasses_its_model_cache", func(t *testing.T) {
		t.Parallel()
		openCode := &modelFetchBackend{
			FakeBackend: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "cached-model"}}}},
			harness:     harness.OpenCode,
			inventory:   agent.ModelInventory{Models: []agent.Model{{ID: "refreshed-model"}}},
		}
		codex := &modelFetchBackend{
			FakeBackend: &agenttest.FakeBackend{},
			harness:     harness.Codex,
			inventory:   agent.ModelInventory{Models: []agent.Model{{ID: "codex-model"}}},
		}
		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.OpenCode: openCode, harness.Codex: codex})
		s.serverHandlers.harnessModels = &HarnessModels{
			Log:         testLogger(),
			CacheDir:    t.TempDir(),
			Router:      s.serverHandlers.runtimes,
			TaskManager: s.taskMgr,
		}

		w := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/server/harnesses/opencode/refresh", strings.NewReader("{}"))
		s.serverHandlers.routes().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		if codex.target.SSHHost != "" {
			t.Fatalf("Codex fetch target = %q, want no fetch", codex.target.SSHHost)
		}
		var got v1.HarnessInfo
		if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if len(got.Models) != 1 || got.Models[0].ID != "refreshed-model" {
			t.Fatalf("refreshed harness = %#v, want refreshed-model", got)
		}
	})

	t.Run("refresh_harness_reports_fetch_failure_without_replacing_cache", func(t *testing.T) {
		t.Parallel()
		cached := agent.ModelInventory{Models: []agent.Model{{ID: "cached-model"}}}
		backend := &modelFetchBackend{
			FakeBackend: &agenttest.FakeBackend{Inventory: cached},
			harness:     harness.Dsh,
			fetchErr:    errors.New("native catalog unavailable"),
		}
		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Dsh: backend})
		cacheDir := t.TempDir()
		agent.OpenHarnessCache(cacheDir+"/harnesses.json").SetModelInventory(harness.Dsh, cached, "")
		s.serverHandlers.harnessModels = &HarnessModels{Log: testLogger(), CacheDir: cacheDir, Router: s.serverHandlers.runtimes, TaskManager: s.taskMgr}
		w := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/server/harnesses/dsh/refresh", strings.NewReader("{}"))
		s.serverHandlers.routes().ServeHTTP(w, r)
		var got struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if w.Code != http.StatusBadRequest || got.Error.Code != "BAD_REQUEST" || !strings.Contains(got.Error.Message, "native catalog unavailable") {
			t.Fatalf("refresh failure: status=%d error=%+v", w.Code, got.Error)
		}
		stored, fresh := agent.OpenHarnessCache(cacheDir+"/harnesses.json").ModelInventory(harness.Dsh, "")
		if !fresh || !reflect.DeepEqual(stored, cached) || !reflect.DeepEqual(backend.ModelInventory(), cached) {
			t.Fatalf("failed refresh replaced native inventory: cached=%+v backend=%+v", stored, backend.ModelInventory())
		}
	})

	t.Run("preferences", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)

		got, err := s.serverHandlers.getPreferences(t.Context(), &api.EmptyReq{})
		if err != nil {
			t.Fatalf("getPreferences: %v", err)
		}
		if got.Settings.PurgeDelay != 15*time.Second {
			t.Fatalf("default purge delay = %s, want %s", got.Settings.PurgeDelay, 15*time.Second)
		}

		want := 91 * time.Second
		got, err = s.serverHandlers.updatePreferences(t.Context(), &v1.UpdatePreferencesReq{
			Settings: v1.UserSettings{PurgeDelay: want, RuntimeSettings: map[string]v1.RuntimeSettings{"docker": {ContainerPlatform: "linux/amd64", MaxCPUs: 4}, "podman": {ContainerPlatform: "linux/arm64", MaxCPUs: 2}}},
		})
		if err != nil {
			t.Fatalf("updatePreferences: %v", err)
		}
		if got.Settings.PurgeDelay != want {
			t.Errorf("updated purge delay = %s, want %s", got.Settings.PurgeDelay, want)
		}
		if got.Settings.RuntimeSettings["docker"].MaxCPUs != 4 || got.Settings.RuntimeSettings["podman"].ContainerPlatform != "linux/arm64" {
			t.Fatalf("runtime settings not preserved: %+v", got.Settings.RuntimeSettings)
		}
	})
}

func TestSettingsMountsResolveContainerPaths(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		settings := &preferences.Settings{
			CacheMappings: []preferences.CacheMapping{{HostPath: "~/.cache/tool", Enabled: true}},
			CustomMounts:  []preferences.MountMapping{{HostPath: "~/.claude", Enabled: true}},
		}
		caches, err := cacheMountsFromSettings(settings)
		if err != nil {
			t.Fatal(err)
		}
		if len(caches) != 1 || caches[0].ContainerPath != "/home/user/.cache/tool" {
			t.Fatalf("caches = %+v, want md-resolved container path", caches)
		}
		mounts, err := mountsFromSettings(settings)
		if err != nil {
			t.Fatal(err)
		}
		if len(mounts) != 1 || mounts[0].ContainerPath != "/home/user/.claude" {
			t.Fatalf("mounts = %+v, want md-resolved container path", mounts)
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		settings := &preferences.Settings{CacheMappings: []preferences.CacheMapping{{HostPath: "cache", ContainerPath: "/cache", Enabled: true}}}
		if _, err := cacheMountsFromSettings(settings); err == nil || !strings.Contains(err.Error(), "host path must be absolute or home-relative") {
			t.Fatalf("cacheMountsFromSettings() error = %v, want invalid host path", err)
		}
		settings = &preferences.Settings{CustomMounts: []preferences.MountMapping{{HostPath: "cache", ContainerPath: "/cache", Enabled: true}}}
		if _, err := mountsFromSettings(settings); err == nil || !strings.Contains(err.Error(), "custom mount 0: host path must be absolute or home-relative") {
			t.Fatalf("mountsFromSettings() error = %v, want indexed invalid host path", err)
		}
	})
}

func TestValidatePreferenceSettings(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		settings v1.UserSettings
		want     string
	}{
		{
			name:     "unknown cache",
			settings: v1.UserSettings{WellKnownCaches: map[string]bool{"bogus": true}},
			want:     "unknown cache: bogus",
		},
		{
			name:     "invalid cache mapping",
			settings: v1.UserSettings{CacheMappings: []v1.CacheMappingResp{{HostPath: "", ContainerPath: "/cache"}}},
			want:     "cacheMappings[0]: host path is required",
		},
		{
			name:     "invalid custom mount",
			settings: v1.UserSettings{CustomMounts: []v1.MountMappingResp{{HostPath: "/host", ContainerPath: ""}}},
			want:     "customMounts[0]: container path is required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validatePreferenceSettings(&tc.settings)
			apiErr, ok := errors.AsType[*api.Error](err)
			if !ok {
				t.Fatalf("error = %T, want *api.Error", err)
			}
			if apiErr.Error() != tc.want {
				t.Errorf("error = %q, want %q", apiErr, tc.want)
			}
		})
	}
}

func TestServerAPIV1EnumParity(t *testing.T) {
	t.Parallel()

	t.Run("harnesses", func(t *testing.T) {
		t.Parallel()
		want := map[v1.Harness]harness.Name{
			v1.HarnessClaude:   harness.Claude,
			v1.HarnessCodex:    harness.Codex,
			v1.HarnessDsh:      harness.Dsh,
			v1.HarnessOpenCode: harness.OpenCode,
			v1.HarnessPi:       harness.Pi,
		}
		got := make(map[harness.Name]struct{}, len(want))
		for dtoHarness, agentHarness := range want {
			if string(dtoHarness) != string(agentHarness) {
				t.Errorf("DTO harness %q does not match agent harness %q", dtoHarness, agentHarness)
			}
			got[agentHarness] = struct{}{}
		}
		for _, agentHarness := range []harness.Name{harness.Claude, harness.Codex, harness.Dsh, harness.OpenCode, harness.Pi} {
			if _, ok := got[agentHarness]; !ok {
				t.Errorf("agent harness %q is missing from the v1 API", agentHarness)
			}
		}
	})

	t.Run("forges", func(t *testing.T) {
		t.Parallel()
		want := map[v1.Forge]forge.Kind{
			v1.ForgeGitHub: forge.KindGitHub,
			v1.ForgeGitLab: forge.KindGitLab,
		}
		got := make(map[forge.Kind]struct{}, len(want))
		for dtoForge, forgeKind := range want {
			if string(dtoForge) != string(forgeKind) {
				t.Errorf("DTO forge %q does not match forge kind %q", dtoForge, forgeKind)
			}
			got[forgeKind] = struct{}{}
		}
		for _, forgeKind := range []forge.Kind{forge.KindGitHub, forge.KindGitLab} {
			if _, ok := got[forgeKind]; !ok {
				t.Errorf("forge kind %q is missing from the v1 API", forgeKind)
			}
		}
	})
}

// updateTransport exercises the API-only HTTP boundary without public downloads.
type updateTransport struct {
	request func(*http.Request) (*http.Response, error)
}

func (tr updateTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return tr.request(r)
}

func TestServerUpdates(t *testing.T) {
	t.Parallel()
	t.Run("unavailable", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		v, err := s.serverHandlers.getVersion(t.Context(), &api.EmptyReq{})
		if err != nil || v.AutoUpdateOn || v.Latest != "" {
			t.Fatalf("version = %+v, %v", v, err)
		}
		_, err = s.serverHandlers.triggerUpdate(t.Context(), &api.EmptyReq{})
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.CodeUpdateUnavailable {
			t.Fatalf("update error = %v", err)
		}
	})
	t.Run("check failure", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		h := s.serverHandlers
		h.forgeMgr = forgemgr.New(h.log, "fixture-token", "", nil, forgemgr.NoOAuthTokenSource())
		u, err := autoupdate.New(autoupdate.Config{
			Repository: "caic-xyz/caic", CurrentVersion: autoupdate.Version(), Binary: "caic",
			ArchiveName: func(string) string { return "caic.tar.gz" },
			Client: &http.Client{Transport: updateTransport{request: func(*http.Request) (*http.Response, error) {
				return nil, errors.New("release API unavailable")
			}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		h.updater = u
		v, err := h.getVersion(t.Context(), &api.EmptyReq{})
		if err != nil || v.CheckError == "" || v.Latest != "" {
			t.Fatalf("version = %+v, %v", v, err)
		}
		_, err = h.triggerUpdate(t.Context(), &api.EmptyReq{})
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.CodeUpdateCheckFailed {
			t.Fatalf("update error = %v", err)
		}
	})
	t.Run("development identity survives display override", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		h := s.serverHandlers
		h.forgeMgr = forgemgr.New(h.log, "fixture-token", "", nil, forgemgr.NoOAuthTokenSource())
		// Display fixtures must never turn development binaries into eligible releases.
		h.version = "0.0.0"
		var requests atomic.Int32
		u, err := autoupdate.New(autoupdate.Config{
			Repository: "caic-xyz/caic", CurrentVersion: autoupdate.Version(), Binary: "caic",
			ArchiveName: func(string) string { return "caic.tar.gz" },
			Client: &http.Client{Transport: updateTransport{request: func(r *http.Request) (*http.Response, error) {
				requests.Add(1)
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"tag_name":"v99.0.0"}`)), Header: make(http.Header)}, nil
			}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		h.updater = u
		h.updateInstalled = func() { t.Error("failed update requested restart") }
		v, err := h.getVersion(t.Context(), &api.EmptyReq{})
		if err != nil || v.Current != "0.0.0" || v.Latest != "99.0.0" || v.AutoUpdateOn {
			t.Fatalf("version = %+v, %v", v, err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		resp, err := h.triggerUpdate(ctx, &api.EmptyReq{})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if resp.Status != "already_up_to_date" || requests.Load() != 2 {
			t.Fatalf("development update = %+v, requests = %d", resp, requests.Load())
		}
	})
}

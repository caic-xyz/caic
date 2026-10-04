// Tests application startup readiness, background restoration, and provider ownership.

package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/maruel/genai"
	"github.com/maruel/genai/providers"
	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/runtimetest"
	"github.com/caic-xyz/caic/backend/internal/server"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
	"github.com/caic-xyz/caic/backend/internal/usage"
)

type missingLogSystem struct {
	runtimetest.FakeSystem

	taskID string
}

func (s *missingLogSystem) List(context.Context) ([]runtime.Instance, error) {
	return []runtime.Instance{{ID: runtime.NewID(s.Name(), "md-agent-missing-log")}}, nil
}

func (s *missingLogSystem) Metadata(_ context.Context, _ runtime.ID, key runtime.MetadataKey) (string, error) {
	if key == runtime.MetadataTaskID {
		return s.taskID, nil
	}
	return "", nil
}

func TestRuntimeRestoreMissingLog(t *testing.T) {
	t.Parallel()
	cfg := closeSpyConfig(t, "")
	cfg.LLM.Disable = true
	id := ksid.NewID().String()
	cfg.Runtime.System = &missingLogSystem{taskID: id}
	// A corrupt purged history must not increase the runtime warning count or
	// turn a usable task list into a history error.
	dir := filepath.Join(cfg.Dirs.CacheDir, "tasks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	corruptPath := filepath.Join(dir, ksid.NewID().String()+".jsonl.zst")
	if err := os.WriteFile(corruptPath, []byte("corrupt compressed purged task log"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	a, err := New(t.Context(), slog.New(slog.NewJSONHandler(&logs, nil)), t.TempDir(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(a.taskMgr.Close(), a.metricsLog.Close()); err != nil {
			t.Error(err)
		}
		for _, c := range a.providerClosers {
			if err := c.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	if err := a.backgroundTasks[0](t.Context()); err != nil {
		t.Fatal(err)
	}
	if loading, loadErr := a.taskMgr.SettledStatus(); loading || loadErr != "" {
		t.Fatalf("successful history load reported runtime import failure: loading=%v error=%q", loading, loadErr)
	}
	if !strings.Contains(logs.String(), `"level":"ERROR","msg":"load live task logs failed; affected instances will not be imported"`) {
		t.Fatalf("missing runtime log failure was not logged at error level: %s", logs.String())
	}
	if !strings.Contains(logs.String(), `"level":"ERROR","msg":"skipping unreadable task log"`) || !strings.Contains(logs.String(), corruptPath) {
		t.Fatalf("corrupt purged task log was not logged at error level: %s", logs.String())
	}
	ctx, cancel := context.WithCancel(t.Context())
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.Server.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	})
	var warningID string
	for range 2 {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ln.Addr().String()+"/api/caic/v1/tasks/events", http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		client := http.Client{Timeout: 5 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		scanner := bufio.NewScanner(resp.Body)
		var warning *v1.Warning
		for scanner.Scan() {
			payload, ok := strings.CutPrefix(scanner.Text(), "data: ")
			if !ok {
				continue
			}
			var ev v1.TaskListEvent
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				t.Fatal(err)
			}
			if ev.Kind == "status" && (ev.Status.Loading || ev.Status.Error != "") {
				t.Fatalf("history status = %+v", ev.Status)
			}
			if ev.Kind == "warning" {
				warning = ev.Warning
				break
			}
		}
		if warning == nil || warning.Category != "runtime_restore_failed" || warning.ID == "" {
			t.Fatalf("runtime restoration warning = %+v, stream error = %v", warning, scanner.Err())
		}
		if warning.Message != "1 task could not be restored." {
			t.Fatalf("warning does not identify the failed task count: %+v", warning)
		}
		if warningID != "" && warning.ID != warningID {
			t.Fatalf("reconnection changed warning identity: %q != %q", warning.ID, warningID)
		}
		warningID = warning.ID
		if strings.Contains(warning.Message, id) || len(warning.Details) != 0 {
			t.Fatalf("public warning exposes task diagnostics: %+v", warning)
		}
	}
}

type slowImportSystem struct {
	runtimetest.FakeSystem

	started chan struct{}
	release <-chan struct{}
	delay   time.Duration
}

func (s *slowImportSystem) List(context.Context) ([]runtime.Instance, error) {
	return []runtime.Instance{{ID: runtime.NewID(s.Name(), "md-agent-startup")}}, nil
}

func (s *slowImportSystem) Metadata(ctx context.Context, _ runtime.ID, _ runtime.MetadataKey) (string, error) {
	if s.started != nil {
		select {
		case s.started <- struct{}{}:
		default:
		}
	}
	if s.release != nil {
		select {
		case <-s.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "", nil
}

func TestStartupReadiness(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	release := make(chan struct{})
	system := &slowImportSystem{started: make(chan struct{}, 1), release: release}
	cfg := closeSpyConfig(t, "")
	cfg.LLM.Disable = true
	cfg.Runtime.System = system
	type result struct {
		app *App
		err error
	}
	created := make(chan result, 1)
	go func() { a, err := New(ctx, slog.New(slog.DiscardHandler), t.TempDir(), cfg); created <- result{a, err} }()
	var a *App
	select {
	case res := <-created:
		if res.err != nil {
			t.Fatal(res.err)
		}
		a = res.app
	case <-system.started:
		cancel()
		<-created
		t.Fatal("startup waited for runtime task restoration")
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("startup did not complete")
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	})
	select {
	case <-system.started:
	case <-time.After(5 * time.Second):
		t.Fatal("background restoration did not start")
	}
	client := http.Client{Timeout: time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ln.Addr().String()+"/", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP status = %d", resp.StatusCode)
	}
	if loading, _ := a.taskMgr.SettledStatus(); !loading {
		t.Fatal("history completed before runtime restoration")
	}
	taskURL := "http://" + ln.Addr().String() + "/api/caic/v1/tasks/" + ksid.NewID().String()
	lookupCtx, cancelLookup := context.WithTimeout(ctx, 100*time.Millisecond)
	lookup, err := http.NewRequestWithContext(lookupCtx, http.MethodGet, taskURL, http.NoBody)
	if err != nil {
		cancelLookup()
		t.Fatal(err)
	}
	response, lookupErr := client.Do(lookup)
	cancelLookup()
	if lookupErr == nil {
		if err := response.Body.Close(); err != nil {
			t.Error(err)
		}
		t.Fatalf("unrestored task returned HTTP %d before import completed", response.StatusCode)
	}
	if !errors.Is(lookupErr, context.DeadlineExceeded) {
		t.Fatal(lookupErr)
	}
	createCtx, cancelCreate := context.WithCancel(ctx)
	cancelCreate()
	if _, err := a.taskMgr.Create(createCtx, taskmgr.CreateParams{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("creation during import = %v, want cancellation before allocation", err)
	}
	close(release)
	createCtx, cancelCreate = context.WithTimeout(ctx, 5*time.Second)
	defer cancelCreate()
	_, err = a.taskMgr.Create(createCtx, taskmgr.CreateParams{})
	var taskErr *taskmgr.Error
	if !errors.As(err, &taskErr) || taskErr.Code != taskmgr.CodeUnknownHarness {
		t.Fatalf("creation after import = %v, want normal parameter validation", err)
	}
	lookup, err = http.NewRequestWithContext(ctx, http.MethodGet, taskURL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	response, err = client.Do(lookup)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("missing task after import = HTTP %d", response.StatusCode)
	}
}

func BenchmarkAppStartupWithSlowImport(b *testing.B) {
	root := b.TempDir()
	cfg := &server.Config{
		Dirs:          server.DirsConfig{ConfigDir: b.TempDir(), CacheDir: b.TempDir()},
		Runtime:       server.RuntimeConfig{System: &slowImportSystem{delay: 20 * time.Millisecond}, SkipWarmup: true},
		Agent:         server.AgentConfig{Backends: agent.Backends{}},
		LLM:           server.LLMConfig{Disable: true},
		IPGeo:         server.IPGeoConfig{Allowlist: "0.0.0.0/0,::/0"},
		UsageFetchers: []usage.ProviderFetcher{},
	}
	log := slog.New(slog.DiscardHandler)
	b.ResetTimer()
	for range b.N {
		ctx, cancel := context.WithCancel(b.Context())
		a, err := New(ctx, log, root, cfg)
		if err != nil {
			cancel()
			b.Fatal(err)
		}
		b.StopTimer()
		cancel()
		var lc net.ListenConfig
		ln, err := lc.Listen(b.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			b.Fatal(err)
		}
		if err := a.Serve(ctx, ln); err != nil && !errors.Is(err, context.Canceled) {
			b.Fatal(err)
		}
		b.StartTimer()
	}
}

// closeSpyProvider is a genai.Provider stub recording whether Close was called.
type closeSpyProvider struct {
	genai.Provider

	closed bool
}

// Name and ModelID stand in for the provider methods initProvider logs.
func (p *closeSpyProvider) Name() string    { return "close-spy" }
func (p *closeSpyProvider) ModelID() string { return "close-spy-model" }

func (p *closeSpyProvider) Close() error {
	p.closed = true
	return nil
}

// registerCloseSpyProvider registers a provider whose factory returns spy. The
// factory requires the API key option that initProvider reads from the
// environment, so the caller must pin TEST_KEY.
func registerCloseSpyProvider(t *testing.T, spy *closeSpyProvider) string {
	const name = "test-provider-close"
	providers.All[name] = providers.Config{
		APIKeyEnvVar: "TEST_KEY",
		Factory: func(_ context.Context, opts ...genai.ProviderOption) (genai.Provider, error) {
			if !slices.ContainsFunc(opts, func(o genai.ProviderOption) bool {
				return o == genai.ProviderOptionAPIKey("test-key")
			}) {
				return nil, errors.New("API key is required")
			}
			return spy, nil
		},
	}
	t.Cleanup(func() { delete(providers.All, name) })
	return name
}

// closeSpyConfig builds a hermetic server config whose LLM is the spy provider.
func closeSpyConfig(t *testing.T, providerName string) *server.Config {
	// A fake runtime and a CIDR-only ipgeo allowlist keep New hermetic: no
	// containers, no model discovery, no database, and no named-origin fetches.
	return &server.Config{
		Dirs:          server.DirsConfig{ConfigDir: t.TempDir(), CacheDir: t.TempDir()},
		Runtime:       server.RuntimeConfig{System: &runtimetest.FakeSystem{}, SkipWarmup: true},
		Agent:         server.AgentConfig{Backends: agent.Backends{}},
		LLM:           server.LLMConfig{Provider: providerName},
		IPGeo:         server.IPGeoConfig{Allowlist: "0.0.0.0/0,::/0"},
		UsageFetchers: []usage.ProviderFetcher{},
	}
}

func TestApp(t *testing.T) {
	// Registry mutation requires a non-parallel test.
	t.Run("NewClosesProviderOnStartupFailure", func(t *testing.T) {
		t.Setenv("TEST_KEY", "test-key")
		spy := &closeSpyProvider{}
		cfg := closeSpyConfig(t, registerCloseSpyProvider(t, spy))
		// An unreadable IPGeo database makes New return an error after it has
		// registered the provider, exercising the startup-failure close path.
		cfg.IPGeo.DB = filepath.Join(t.TempDir(), "missing.mmdb")

		_, err := New(t.Context(), slog.New(slog.DiscardHandler), t.TempDir(), cfg)
		if err == nil || !strings.Contains(err.Error(), "ipgeo") {
			t.Fatalf("New() = %v, want ipgeo startup failure", err)
		}
		if !spy.closed {
			t.Error("provider was not closed after startup failure")
		}
	})

	t.Run("ServeClosesProviderAtShutdown", func(t *testing.T) {
		t.Setenv("TEST_KEY", "test-key")
		spy := &closeSpyProvider{}
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		srv, err := New(ctx, slog.New(slog.DiscardHandler), t.TempDir(), closeSpyConfig(t, registerCloseSpyProvider(t, spy)))
		if err != nil {
			t.Fatalf("New() = %v", err)
		}
		var lc net.ListenConfig
		ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- srv.Serve(ctx, ln) }()
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Serve() = %v", err)
		}
		if !spy.closed {
			t.Error("provider was not closed when serving stopped")
		}
	})
}

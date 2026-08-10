// Tests for Manager and standalone helpers.

package taskmgr

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/codex"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/logtest"
	"github.com/caic-xyz/caic/backend/internal/repo/repowork"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/mdruntime"
	"github.com/caic-xyz/caic/backend/internal/runtime/runtimetest"
	"github.com/caic-xyz/caic/backend/internal/task"
)

type testRuntimeInfo interface {
	runtime.Monitor
	runtime.Inventory
	runtime.PrivilegeInfo
}

type testRuntimeSystem struct {
	runtime.Lifecycle
	runtimetest.FakeInfo
}

func (*testRuntimeSystem) Name() runtime.Name { return "test-runtime" }

type testRuntimeInfoSystem struct {
	runtime.Lifecycle
	runtime.Monitor
	runtime.Inventory
	runtime.PrivilegeInfo
}

func (testRuntimeInfoSystem) Name() runtime.Name { return "test-runtime" }

type metadataErrorInfo struct {
	*runtimetest.FakeInfo

	key runtime.MetadataKey
	err error
}

func (f *metadataErrorInfo) Metadata(ctx context.Context, id runtime.ID, key runtime.MetadataKey) (string, error) {
	if key == f.key {
		return "", f.err
	}
	return f.FakeInfo.Metadata(ctx, id, key)
}

func newTestManager(t testing.TB, cfg Config) *Manager { //nolint:gocritic // Config mirrors New's value bag in tests.
	if cfg.Runtimes == nil {
		cfg.Runtimes = newTestRuntime(t, &runtimetest.FakeBackend{}, nil)
	}
	return New(cfg)
}

func newTestRuntime(t testing.TB, lc runtime.Lifecycle, info testRuntimeInfo) *runtime.Router {
	var sys runtime.System = &testRuntimeSystem{Lifecycle: lc}
	if info != nil {
		sys = testRuntimeInfoSystem{Lifecycle: lc, Monitor: info, Inventory: info, PrivilegeInfo: info}
	}
	router, err := runtime.NewRouter([]runtime.System{sys})
	if err != nil {
		t.Fatalf("runtime.NewRouter: %v", err)
	}
	return router
}

type fakeRelayReader struct {
	statusFn   func(context.Context, runtime.ConnectionTarget) (bool, string, error)
	readTailFn func(context.Context, runtime.ConnectionTarget, func([]byte) ([]agent.Message, error), int64) ([]agent.Message, int64, error)
	readLogFn  func(context.Context, runtime.ConnectionTarget, int) string
}

func (f fakeRelayReader) Status(ctx context.Context, target runtime.ConnectionTarget) (alive bool, diag string, err error) {
	return f.statusFn(ctx, target)
}

func (f fakeRelayReader) ReadTail(ctx context.Context, target runtime.ConnectionTarget, parseFn func([]byte) ([]agent.Message, error), maxBytes int64) (msgs []agent.Message, size int64, err error) {
	return f.readTailFn(ctx, target, parseFn, maxBytes)
}

func (f fakeRelayReader) ReadLog(ctx context.Context, target runtime.ConnectionTarget, maxBytes int) string {
	return f.readLogFn(ctx, target, maxBytes)
}

// blockingStopBackend blocks in Stop until release is closed (or the context is
// cancelled), widening the Stop/Purge race window. started and returned signal
// the call's entry and exit.
type blockingStopBackend struct {
	*runtimetest.FakeBackend

	started  chan struct{}
	returned chan struct{}
	release  chan struct{}
}

func (b *blockingStopBackend) Stop(ctx context.Context, id runtime.ID) error {
	close(b.started)
	defer close(b.returned)
	select {
	case <-b.release:
		return b.FakeBackend.Stop(ctx, id) // advances to StatusStopped
	case <-ctx.Done():
		return ctx.Err()
	}
}

// blockingReviveBackend blocks in Revive until release is closed, then fails
// without advancing the instance state.
type blockingReviveBackend struct {
	*runtimetest.FakeBackend

	release chan struct{}
}

func (b *blockingReviveBackend) Revive(ctx context.Context, id runtime.ID) error {
	select {
	case <-b.release:
		return errors.New("revive boom")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// reconnectInputBackend drives a real relay session for reconnect tests. It
// inherits metadata from the embedded fake and overrides AttachRelay/NewWire.
type reconnectInputBackend struct {
	*agenttest.FakeBackend

	mu sync.Mutex

	attachCalls int
	prompts     []agent.Prompt
	opts        *agent.Options
	cancel      context.CancelFunc
	session     *agent.Session
}

func (b *reconnectInputBackend) AttachRelay(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	cmdCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cmdCtx, "sleep", "60")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	c := &reconnectInputConn{backend: b, cancel: cancel}
	session := agent.NewSession(cmd, c, stdout, opts.MsgCh, nil)
	b.mu.Lock()
	b.attachCalls++
	b.opts = opts
	b.cancel = cancel
	b.session = session
	b.mu.Unlock()
	return session, nil
}

func (*reconnectInputBackend) NewWire() agent.WireFormat { return codex.New("", nil).NewWire() }

func (b *reconnectInputBackend) stop() {
	b.mu.Lock()
	cancel := b.cancel
	session := b.session
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if session != nil {
		_ = session.Wait()
	}
}

type reconnectInputConn struct {
	backend *reconnectInputBackend
	cancel  context.CancelFunc
}

func (c *reconnectInputConn) SendPrompt(p agent.Prompt) error {
	c.backend.mu.Lock()
	c.backend.prompts = append(c.backend.prompts, p)
	c.backend.mu.Unlock()
	return nil
}

func (*reconnectInputConn) SendRaw([]byte) error { return nil }

func (*reconnectInputConn) SendCompact(string) error { return errors.New("compact not supported") }

func (*reconnectInputConn) ReadMessages(r io.Reader, _ chan<- agent.Message) error {
	_, err := io.Copy(io.Discard, r)
	return err
}

func (c *reconnectInputConn) SendStop(context.Context) {
	c.cancel()
}

func (c *reconnectInputConn) Close() error {
	c.cancel()
	return nil
}

func textMessages(msgs []agent.Message) []string {
	texts := make([]string, 0, len(msgs))
	for _, msg := range msgs {
		if text, ok := msg.(*agent.TextMessage); ok {
			texts = append(texts, text.Text)
		}
	}
	return texts
}

func TestMergeLogAndRelayMessages(t *testing.T) {
	t.Parallel()
	t.Run("valid_exact_overlap", func(t *testing.T) {
		t.Parallel()
		merged := mergeLogAndRelayMessages(
			[]agent.Message{&agent.TextMessage{Text: "before"}, &agent.TextMessage{Text: "overlap"}},
			[]agent.Message{&agent.TextMessage{Text: "overlap"}, &agent.TextMessage{Text: "after"}},
		)
		texts := textMessages(merged)
		want := []string{"before", "overlap", "after"}
		if !slices.Equal(texts, want) {
			t.Fatalf("merged texts = %#v, want %#v", texts, want)
		}
	})
	t.Run("valid_usage_context_window_mismatch", func(t *testing.T) {
		t.Parallel()
		merged := mergeLogAndRelayMessages(
			[]agent.Message{
				&agent.TextMessage{Text: "before"},
				&agent.UsageMessage{Usage: agent.Usage{InputTokens: 1}, ContextWindow: 272000},
				&agent.TextMessage{Text: "overlap"},
			},
			[]agent.Message{
				&agent.UsageMessage{Usage: agent.Usage{InputTokens: 1}},
				&agent.TextMessage{Text: "overlap"},
				&agent.TextMessage{Text: "after"},
			},
		)
		texts := textMessages(merged)
		want := []string{"before", "overlap", "after"}
		if !slices.Equal(texts, want) {
			t.Fatalf("merged texts = %#v, want %#v", texts, want)
		}
		usage, ok := merged[1].(*agent.UsageMessage)
		if !ok || usage.ContextWindow != 272000 {
			t.Fatalf("merged[1] = %#v, want log usage with context window", merged[1])
		}
	})
	t.Run("valid_result_turn_count_mismatch", func(t *testing.T) {
		t.Parallel()
		merged := mergeLogAndRelayMessages(
			[]agent.Message{
				&agent.TextMessage{Text: "before"},
				&agent.ResultMessage{MessageType: "result", Subtype: "result", NumTurns: 16, Usage: agent.Usage{InputTokens: 1}},
			},
			[]agent.Message{
				&agent.ResultMessage{MessageType: "result", Subtype: "result", NumTurns: 1, Usage: agent.Usage{InputTokens: 1}},
				&agent.TextMessage{Text: "after"},
			},
		)
		resultCount := 0
		for _, msg := range merged {
			if _, ok := msg.(*agent.ResultMessage); ok {
				resultCount++
			}
		}
		if resultCount != 1 {
			t.Fatalf("merged result count = %d, want 1; merged = %#v", resultCount, merged)
		}
		texts := textMessages(merged)
		want := []string{"before", "after"}
		if !slices.Equal(texts, want) {
			t.Fatalf("merged texts = %#v, want %#v", texts, want)
		}
	})
	t.Run("valid_ignores_relay_diff_stat", func(t *testing.T) {
		t.Parallel()
		merged := mergeLogAndRelayMessages(
			[]agent.Message{&agent.TextMessage{Text: "before"}, &agent.TextMessage{Text: "overlap"}},
			[]agent.Message{&agent.DiffStatMessage{MessageType: "caic_diff_stat"}, &agent.TextMessage{Text: "overlap"}, &agent.TextMessage{Text: "after"}},
		)
		texts := textMessages(merged)
		want := []string{"before", "overlap", "after"}
		if !slices.Equal(texts, want) {
			t.Fatalf("merged texts = %#v, want %#v", texts, want)
		}
	})
	t.Run("valid_ignores_artificial_relay_init", func(t *testing.T) {
		t.Parallel()
		merged := mergeLogAndRelayMessages(
			[]agent.Message{
				&agent.InitMessage{Model: "openai-codex/gpt-5.5"},
				&agent.TextMessage{Text: "before"},
				&agent.UsageMessage{Usage: agent.Usage{InputTokens: 1}, ContextWindow: 272000},
				&agent.ThinkingDeltaMessage{Text: "overlap"},
			},
			[]agent.Message{
				&agent.UsageMessage{Usage: agent.Usage{InputTokens: 1}},
				&agent.InitMessage{Model: "openai-codex/gpt-5.5"},
				&agent.ThinkingDeltaMessage{Text: "overlap"},
				&agent.TextMessage{Text: "after"},
			},
		)
		texts := textMessages(merged)
		want := []string{"before", "after"}
		if !slices.Equal(texts, want) {
			t.Fatalf("merged texts = %#v, want %#v", texts, want)
		}
		initCount := 0
		for _, msg := range merged {
			if _, ok := msg.(*agent.InitMessage); ok {
				initCount++
			}
		}
		if initCount != 1 {
			t.Fatalf("merged init count = %d, want 1; merged = %#v", initCount, merged)
		}
	})
}

func TestNew(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		cfg := Config{ServerCtx: t.Context()}
		m := newTestManager(t, cfg)
		if m == nil {
			t.Fatal("New returned nil")
		}
		if m.Len() != 0 {
			t.Errorf("Len() = %d after New, want 0", m.Len())
		}
		r, ok := m.Workspace("")
		if !ok {
			t.Fatal("no-repo workspace not registered")
		}
		if r == nil {
			t.Fatal("no-repo workspace is nil")
		}
		select {
		case <-m.Changed():
			t.Error("Changed() should not be closed initially")
		default:
		}
	})
	t.Run("no-repo workspace is fully constructed", func(t *testing.T) {
		t.Parallel()
		backend := &mdruntime.Backend{}
		router, err := runtime.NewRouter([]runtime.System{&testRuntimeSystem{Lifecycle: backend}})
		if err != nil {
			t.Fatal(err)
		}
		cfg := Config{
			ServerCtx:  t.Context(),
			LogDir:     "/tmp/logs",
			CacheDir:   "/tmp/cache",
			Runtimes:   router,
			Backends:   map[harness.Name]agent.Backend{"fake": &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}},
			HarnessEnv: map[string][]string{string(harness.Codex): {"CODEX_HOME=/tmp/codex"}},
		}
		m := New(cfg)
		r, ok := m.Workspace("")
		if !ok {
			t.Fatal("no-repo workspace not registered")
		}
		if m.logDir != cfg.LogDir || m.cacheDir != cfg.CacheDir {
			t.Fatalf("manager dirs = log %q cache %q, want log %q cache %q", m.logDir, m.cacheDir, cfg.LogDir, cfg.CacheDir)
		}
		if r.Runtimes != cfg.Runtimes {
			t.Fatal("workspace instance backend was not wired")
		}
		if len(m.harnessEnv[string(harness.Codex)]) != 1 || m.harnessEnv[string(harness.Codex)][0] != "CODEX_HOME=/tmp/codex" {
			t.Fatalf("HarnessEnv = %#v, want configured codex env", m.harnessEnv)
		}
		if len(m.Backends()) == 0 {
			t.Fatal("manager backends were not initialized")
		}
	})
}

func TestManager(t *testing.T) {
	t.Parallel()

	t.Run("RegisterWorkspace", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			r := &repowork.Workspace{Dir: "/tmp/test", Log: logtest.Logger(t)}
			m.RegisterWorkspace("my/repo", r)
			got, ok := m.Workspace("my/repo")
			if !ok || got != r {
				t.Fatal("Workspace() did not return registered workspace")
			}
			if got.Log == nil {
				t.Fatal("registered workspace Log is nil")
			}
		})
	})

	t.Run("Close", func(t *testing.T) {
		t.Parallel()
		m := newTestManager(t, Config{ServerCtx: t.Context()})
		tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}}
		tk.RestoreMessages([]agent.Message{&agent.RateLimitMessage{
			Status:        agent.RateLimitStatusRejected,
			QuotaProvider: agent.QuotaProviderClaudeCode,
			QuotaWindow:   "five_hour",
			Utilization:   1,
		}})
		m.Insert(tk.ID.String(), NewEntry(tk, nil))
		_, live, unsubscribe := tk.Subscribe(m.serverCtx)
		t.Cleanup(unsubscribe)
		_, rateLimitLive, _ := tk.SubscribeRateLimits(m.serverCtx)

		if err := m.Close(); err != nil {
			t.Fatalf("Close() = %v, want nil", err)
		}

		select {
		case _, ok := <-live:
			if ok {
				t.Fatal("live task subscription remained open after Manager.Close")
			}
		case <-time.After(time.Second):
			t.Fatal("live task subscription did not close after Manager.Close")
		}
		select {
		case _, ok := <-rateLimitLive:
			if ok {
				t.Fatal("rate-limit subscription remained open after Manager.Close")
			}
		case <-time.After(time.Second):
			t.Fatal("rate-limit subscription did not close after Manager.Close")
		}
	})

	t.Run("CopiesBackends", func(t *testing.T) {
		t.Parallel()
		var firstCalls, replacementCalls int
		backends := map[harness.Name]agent.Backend{
			harness.Claude: &agenttest.FakeBackend{WireFactory: func() agent.WireFormat {
				firstCalls++
				return claudecode.New().NewWire()
			}},
		}
		m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: backends})
		backends[harness.Claude] = &agenttest.FakeBackend{WireFactory: func() agent.WireFormat {
			replacementCalls++
			return codex.New("", nil).NewWire()
		}}
		if _, err := m.resolveNativeParser(harness.Claude); err != nil {
			t.Fatalf("resolveNativeParser: %v", err)
		}
		if firstCalls != 1 || replacementCalls != 0 {
			t.Fatalf("wire construction calls = first %d replacement %d, want 1/0", firstCalls, replacementCalls)
		}
	})

	t.Run("RateLimitEvents", func(t *testing.T) {
		t.Parallel()
		m := newTestManager(t, Config{ServerCtx: t.Context()})
		now := time.Now().UTC()
		tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}}
		tk.RestoreMessages([]agent.Message{
			&agent.RateLimitMessage{
				Status:        agent.RateLimitStatusRejected,
				ResetsAt:      now.Add(time.Hour),
				QuotaProvider: agent.QuotaProviderClaudeCode,
				QuotaWindow:   "5h",
				Utilization:   1,
			},
			&agent.RateLimitMessage{
				Status:        agent.RateLimitStatusAllowedWarning,
				ResetsAt:      now.Add(7 * 24 * time.Hour),
				QuotaProvider: agent.QuotaProviderClaudeCode,
				QuotaWindow:   "7d",
				Utilization:   0.91,
			},
		})
		m.Insert(tk.ID.String(), NewEntry(tk, nil))

		quotas := m.QuotaTracker.Merge(nil, now)
		if len(quotas) != 1 || len(quotas[0].RateLimits) != 2 {
			t.Fatalf("tracked quotas = %#v, want two Claude Code quota windows", quotas)
		}
		if got := quotas[0].RateLimits[0]; got.Window != "5h" || got.UsedPct != 100 {
			t.Errorf("5h rate limit = %#v, want rejected update", got)
		}
		if got := quotas[0].RateLimits[1]; got.Window != "7d" || got.UsedPct != 91 {
			t.Errorf("7d rate limit = %#v, want warning update", got)
		}
	})

	t.Run("UnregisterWorkspace", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			r := &repowork.Workspace{Dir: "/tmp/test", Log: logtest.Logger(t)}
			m.RegisterWorkspace("my/repo", r)
			m.UnregisterWorkspace("my/repo")
			if _, ok := m.Workspace("my/repo"); ok {
				t.Error("Workspace() still returned workspace after UnregisterWorkspace")
			}
		})
		t.Run("valid_removes_only_matching", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			r1 := &repowork.Workspace{Dir: "/tmp/a", Log: logtest.Logger(t)}
			r2 := &repowork.Workspace{Dir: "/tmp/b", Log: logtest.Logger(t)}
			m.RegisterWorkspace("a", r1)
			m.RegisterWorkspace("b", r2)
			m.UnregisterWorkspace("a")
			if _, ok := m.Workspace("a"); ok {
				t.Error("Workspace(a) should be removed")
			}
			if got, ok := m.Workspace("b"); !ok || got != r2 {
				t.Error("Workspace(b) should still be registered")
			}
		})
	})

	t.Run("GetEntry", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
			}
			e := NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			got, ok := m.GetEntry(tk.ID.String())
			if !ok || got != e {
				t.Fatal("GetEntry did not return inserted entry")
			}
			if m.Len() != 1 {
				t.Errorf("Len() = %d, want 1", m.Len())
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			_, ok := m.GetEntry("nonexistent")
			if ok {
				t.Error("GetEntry should return false for nonexistent task")
			}
		})
	})

	// Regression: Range/RangeWorkspaces must invoke fn unlocked so callbacks may
	// call back into the Manager (e.g. server.toJSON → RepoWorkspace). Holding m.mu
	// during fn self-deadlocks the whole server (caught only by e2e before).
	t.Run("RangeCallbackCanCallManager", func(t *testing.T) {
		t.Parallel()
		m := newTestManager(t, Config{ServerCtx: t.Context()})
		tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
		m.Insert(tk.ID.String(), NewEntry(tk, nil))
		done := make(chan struct{})
		go func() {
			m.Range(func(_ string, _ *Entry) bool {
				_, _ = m.Workspace("") // re-enters m.mu; deadlocks if Range holds it
				return true
			})
			m.RangeWorkspaces(func(_ string, _ *repowork.Workspace) bool {
				_, _ = m.GetEntry(tk.ID.String())
				return true
			})
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Range/RangeWorkspaces deadlocked when callback called back into Manager")
		}
	})

	t.Run("Range", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			for range 5 {
				m.Insert(ksid.NewID().String(), NewEntry(&task.Task{
					ID:            ksid.NewID(),
					InitialPrompt: agent.Prompt{Text: "test"},
				}, nil))
			}
			var count int
			m.Range(func(id string, e *Entry) bool {
				count++
				return true
			})
			if count != 5 {
				t.Errorf("Range iterated %d entries, want 5", count)
			}
		})
		t.Run("valid_stops_on_false", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			for range 5 {
				m.Insert(ksid.NewID().String(), NewEntry(&task.Task{
					ID:            ksid.NewID(),
					InitialPrompt: agent.Prompt{Text: "test"},
				}, nil))
			}
			var count int
			m.Range(func(id string, e *Entry) bool {
				count++
				return false
			})
			if count != 1 {
				t.Errorf("Range iterated %d entries after stop, want 1", count)
			}
		})
	})

	t.Run("Changed", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_on_insert", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			oldCh := m.Changed()
			m.Insert(ksid.NewID().String(), NewEntry(&task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
			}, nil))
			select {
			case <-oldCh:
			case <-time.After(time.Second):
				t.Fatal("old Changed() channel not closed after Insert")
			}
		})
		t.Run("valid_new_channel_after_mutation", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			ch1 := m.Changed()
			m.NotifyTaskChange()
			ch2 := m.Changed()
			if ch1 == ch2 {
				t.Fatal("Changed() returned same channel after mutation")
			}
			select {
			case <-ch1:
			default:
				t.Error("old Changed() channel not closed after mutation")
			}
		})
	})

	t.Run("FindTasksByPR", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []task.RepoMount{{Name: "my/repo", Branch: "caic-1"}},
			}
			tk.SetPR("acme", "magic", 42)
			m.Insert(tk.ID.String(), NewEntry(tk, nil))
			found := m.FindTasksByPR("acme", "magic", 42)
			if len(found) != 1 {
				t.Fatalf("FindTasksByPR returned %d entries, want 1", len(found))
			}
			if found[0].Task() != tk {
				t.Error("FindTasksByPR returned wrong entry")
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []task.RepoMount{{Name: "my/repo", Branch: "caic-1"}},
			}
			tk.SetPR("acme", "magic", 42)
			m.Insert(tk.ID.String(), NewEntry(tk, nil))
			found := m.FindTasksByPR("other", "repo", 42)
			if len(found) != 0 {
				t.Errorf("FindTasksByPR returned %d entries for wrong owner, want 0", len(found))
			}
		})
	})

	t.Run("FindTasksMatchingBranch", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []task.RepoMount{{Name: "my/repo", Branch: "caic-1"}},
			}
			tk.SetPR("acme", "magic", 0)
			m.Insert(tk.ID.String(), NewEntry(tk, nil))
			found := m.FindTasksMatchingBranch("acme", "magic", "caic-1")
			if len(found) != 1 {
				t.Fatalf("FindTasksMatchingBranch returned %d entries, want 1", len(found))
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []task.RepoMount{{Name: "my/repo", Branch: "caic-1"}},
			}
			tk.SetPR("acme", "magic", 0)
			m.Insert(tk.ID.String(), NewEntry(tk, nil))
			found := m.FindTasksMatchingBranch("acme", "magic", "caic-2")
			if len(found) != 0 {
				t.Errorf("FindTasksMatchingBranch returned %d entries for wrong branch, want 0", len(found))
			}
		})
	})

	t.Run("FindTasksMonitoringBranch", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []task.RepoMount{{Name: "my/repo", Branch: "caic-1"}},
			}
			tk.SetPR("acme", "magic", 0)
			e := NewEntry(tk, nil)
			e.SetMonitorBranch("caic-1")
			m.Insert(tk.ID.String(), e)
			found := m.FindTasksMonitoringBranch("acme", "magic")
			if len(found) != 1 {
				t.Fatalf("FindTasksMonitoringBranch returned %d entries, want 1", len(found))
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []task.RepoMount{{Name: "my/repo", Branch: "caic-1"}},
			}
			tk.SetPR("acme", "magic", 0)
			m.Insert(tk.ID.String(), NewEntry(tk, nil))
			found := m.FindTasksMonitoringBranch("acme", "magic")
			if len(found) != 0 {
				t.Errorf("FindTasksMonitoringBranch returned %d entries without monitor branch, want 0", len(found))
			}
		})
	})

	t.Run("ListPendingBotTasks", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				ForgeIssue:    5,
				Repos:         []task.RepoMount{{Name: "repo/a"}},
			}
			tk.SetPR("acme", "magic", 0)
			tk.SetState(task.StateRunning)
			m.Insert(tk.ID.String(), NewEntry(tk, nil))
			pending := m.ListPendingBotTasks()
			if len(pending) != 1 {
				t.Fatalf("ListPendingBotTasks returned %d tasks, want 1", len(pending))
			}
			if pending[0].IssueNumber != 5 {
				t.Errorf("IssueNumber = %d, want 5", pending[0].IssueNumber)
			}
		})
		t.Run("valid_skips_no_forge_issue", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
			}
			tk.SetState(task.StateRunning)
			m.Insert(tk.ID.String(), NewEntry(tk, nil))
			pending := m.ListPendingBotTasks()
			if len(pending) != 0 {
				t.Errorf("ListPendingBotTasks returned %d tasks without ForgeIssue, want 0", len(pending))
			}
		})
		t.Run("valid_skips_terminal_states", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			for _, st := range []task.State{task.StateWaiting, task.StateStopped, task.StateCrashed, task.StateFailed, task.StatePurged} {
				tk := &task.Task{
					ID:            ksid.NewID(),
					InitialPrompt: agent.Prompt{Text: "test"},
					ForgeIssue:    1,
				}
				tk.SetState(st)
				m.Insert(tk.ID.String(), NewEntry(tk, nil))
			}
			pending := m.ListPendingBotTasks()
			if len(pending) != 0 {
				t.Errorf("ListPendingBotTasks returned %d tasks for terminal states, want 0", len(pending))
			}
		})
	})

	t.Run("resolveWorkspace", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_with_repo", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			r := &repowork.Workspace{Dir: "/tmp/test", Log: logtest.Logger(t)}
			m.RegisterWorkspace("my/repo", r)
			tk := &task.Task{
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []task.RepoMount{{Name: "my/repo"}},
			}
			got := m.resolveWorkspace(tk)
			if got != r {
				t.Error("resolveWorkspace returned wrong workspace")
			}
		})
		t.Run("valid_no_repo_fallback", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}}
			got := m.resolveWorkspace(tk)
			if got == nil {
				t.Fatal("resolveWorkspace returned nil for no-repo task")
			}
		})
	})

	t.Run("applyLoadedSessionMetadata", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_fills_missing_session", func(t *testing.T) {
			t.Parallel()
			tk := &task.Task{Model: "requested"}
			lt := &task.LoadedTask{SessionID: "thread-1", Model: "reported", AgentVersion: "1.2.3"}
			applyLoadedSessionMetadata(tk, lt)
			if got := tk.GetSessionID(); got != "thread-1" {
				t.Errorf("SessionID = %q, want thread-1", got)
			}
			snap := tk.Snapshot()
			if snap.Model != "reported" {
				t.Errorf("Model = %q, want reported", snap.Model)
			}
			if snap.AgentVersion != "1.2.3" {
				t.Errorf("AgentVersion = %q, want 1.2.3", snap.AgentVersion)
			}
		})
		t.Run("valid_preserves_live_session", func(t *testing.T) {
			t.Parallel()
			tk := &task.Task{}
			tk.RestoreMessages([]agent.Message{&agent.InitMessage{SessionID: "live"}})
			applyLoadedSessionMetadata(tk, &task.LoadedTask{SessionID: "persisted"})
			if got := tk.GetSessionID(); got != "live" {
				t.Errorf("SessionID = %q, want live", got)
			}
		})
	})

	t.Run("EffectiveBaseBranch", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_explicit", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			m.RegisterWorkspace("my/repo", &repowork.Workspace{
				BaseBranch: "develop",
				Log:        logtest.Logger(t),
			})
			tk := &task.Task{
				Repos: []task.RepoMount{{Name: "my/repo", BaseBranch: "main"}},
			}
			if got := m.EffectiveBaseBranch(tk); got != "main" {
				t.Errorf("EffectiveBaseBranch = %q, want %q", got, "main")
			}
		})
		t.Run("valid_workspace_default", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			m.RegisterWorkspace("my/repo", &repowork.Workspace{
				BaseBranch: "develop",
				Log:        logtest.Logger(t),
			})
			tk := &task.Task{
				Repos: []task.RepoMount{{Name: "my/repo"}},
			}
			if got := m.EffectiveBaseBranch(tk); got != "develop" {
				t.Errorf("EffectiveBaseBranch = %q, want %q", got, "develop")
			}
		})
		t.Run("valid_no_repo", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{}
			if got := m.EffectiveBaseBranch(tk); got != "" {
				t.Errorf("EffectiveBaseBranch = %q for no-repo, want empty", got)
			}
		})
	})

	t.Run("SetTaskMonitorBranch", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}}
			e := NewEntry(tk, nil)
			m.SetTaskMonitorBranch(e, "caic-1")
			if e.MonitorBranch() != "caic-1" {
				t.Errorf("MonitorBranch = %q, want %q", e.MonitorBranch(), "caic-1")
			}
		})
	})

	t.Run("WatchTaskCompletion", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_already_terminal", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
			}
			tk.SetState(task.StateStopped)
			m.Insert(tk.ID.String(), NewEntry(tk, nil))
			state, result, err := m.WatchTaskCompletion(t.Context(), tk.ID.String())
			if err != nil {
				t.Fatalf("WatchTaskCompletion error: %v", err)
			}
			if state != "stopped" {
				t.Errorf("state = %q, want %q", state, "stopped")
			}
			if result != "" {
				t.Errorf("result = %q, want empty", result)
			}
		})
		t.Run("error_not_found", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			_, _, err := m.WatchTaskCompletion(t.Context(), "nonexistent")
			if err == nil {
				t.Fatal("expected error for nonexistent task")
			}
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindNotFound {
				t.Fatalf("err = %v, want KindNotFound", err)
			}
		})
		t.Run("valid_waits_for_terminal", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
			}
			tk.SetState(task.StateRunning)
			m.Insert(tk.ID.String(), NewEntry(tk, nil))
			go func() {
				time.Sleep(50 * time.Millisecond)
				tk.SetState(task.StateStopped)
				m.NotifyTaskChange()
			}()
			state, _, err := m.WatchTaskCompletion(t.Context(), tk.ID.String())
			if err != nil {
				t.Fatalf("WatchTaskCompletion error: %v", err)
			}
			if state != "stopped" {
				t.Errorf("state = %q, want %q", state, "stopped")
			}
		})
		t.Run("error_context_cancelled", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
			}
			tk.SetState(task.StateRunning)
			m.Insert(tk.ID.String(), NewEntry(tk, nil))
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, _, err := m.WatchTaskCompletion(ctx, tk.ID.String())
			if err == nil {
				t.Fatal("expected error for cancelled context")
			}
		})
	})

	t.Run("LifecycleErrors", func(t *testing.T) {
		t.Parallel()
		// call returns the lifecycle error for a freshly-inserted task in the
		// given state. The not-found path is no longer the Manager's concern:
		// these methods take an *Entry, and the HTTP layer rejects unknown ids
		// before they are reached (see server.getTask).
		type tc struct {
			name  string
			state task.State
			want  ErrorKind
			call  func(m *Manager, e *Entry) error
		}
		stop := func(m *Manager, e *Entry) error { return m.Stop(t.Context(), e) }
		revive := func(m *Manager, e *Entry) error { return m.Revive(t.Context(), e) }
		syncOrigin := func(m *Manager, e *Entry) error {
			_, err := m.Sync(t.Context(), e, SyncTargetOrigin, false)
			return err
		}
		syncForceDefault := func(m *Manager, e *Entry) error {
			_, err := m.Sync(t.Context(), e, SyncTargetDefault, true)
			return err
		}
		fork := func(m *Manager, e *Entry) error {
			_, err := m.Fork(t.Context(), e, ForkParams{})
			return err
		}
		cases := []tc{
			{"stop_wrong_state", task.StateStopped, KindConflict, stop},
			{"revive_not_stopped", task.StateRunning, KindConflict, revive},
			{"sync_terminal_state", task.StateStopped, KindConflict, syncOrigin},
			{"sync_force_default", task.StateWaiting, KindBadRequest, syncForceDefault},
			{"fork_wrong_state", task.StateStopping, KindConflict, fork},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				m := newTestManager(t, Config{ServerCtx: t.Context()})
				tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}}
				tk.SetState(c.state)
				e := NewEntry(tk, nil)
				m.Insert(tk.ID.String(), e)
				err := c.call(m, e)
				if err == nil {
					t.Fatalf("expected *Error with Kind %v, got nil", c.want)
				}
				te, ok := errors.AsType[*Error](err)
				if !ok {
					t.Fatalf("error %v is not a *Error", err)
				}
				if te.Kind != c.want {
					t.Errorf("Kind = %v, want %v (err=%v)", te.Kind, c.want, err)
				}
			})
		}
	})

	t.Run("Create", func(t *testing.T) {
		t.Parallel()
		// newManagerWithRepo returns a Manager with one repo workspace that has a
		// fake backend for harness "fake".
		newManagerWithRepo := func(t *testing.T) *Manager {
			m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: map[harness.Name]agent.Backend{"fake": &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}})
			m.RegisterWorkspace("my/repo", &repowork.Workspace{Dir: "/tmp/my-repo", Log: logtest.Logger(t)})
			return m
		}
		t.Run("valid_sets_basename_mounted_path_and_max_cpus", func(t *testing.T) {
			t.Parallel()
			m := newManagerWithRepo(t)
			id, err := m.Create(t.Context(), CreateParams{
				Prompt:  agent.Prompt{Text: "hi"},
				Repos:   []CreateRepo{{Name: "my/repo", BaseBranch: "main"}},
				Harness: "fake",
				MaxCPUs: 7,
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			e, ok := m.GetEntry(id)
			if !ok {
				t.Fatal("entry not found after Create")
			}
			tk := e.Task()
			if tk.MaxCPUs != 7 {
				t.Errorf("MaxCPUs = %d, want 7", tk.MaxCPUs)
			}
			repos := tk.ReposSnapshot()
			if len(repos) != 1 {
				t.Fatalf("len(Repos) = %d, want 1", len(repos))
			}
			if got := repos[0].ContainerPath; got != "~/src/repo" {
				t.Errorf("ContainerPath = %q, want %q", got, "~/src/repo")
			}
		})
		t.Run("valid_sets_relative_mounted_path_for_basename_collision", func(t *testing.T) {
			t.Parallel()
			m := newManagerWithRepo(t)
			m.RegisterWorkspace("other/repo", &repowork.Workspace{Dir: "/tmp/other-repo", Log: logtest.Logger(t)})
			id, err := m.Create(t.Context(), CreateParams{
				Prompt:  agent.Prompt{Text: "hi"},
				Repos:   []CreateRepo{{Name: "my/repo", BaseBranch: "main"}},
				Harness: "fake",
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			e, ok := m.GetEntry(id)
			if !ok {
				t.Fatal("entry not found after Create")
			}
			tk := e.Task()
			repos := tk.ReposSnapshot()
			if len(repos) != 1 {
				t.Fatalf("len(Repos) = %d, want 1", len(repos))
			}
			if got := repos[0].ContainerPath; got != "~/src/my/repo" {
				t.Errorf("ContainerPath = %q, want %q", got, "~/src/my/repo")
			}
		})
		t.Run("valid_sudo_resolution_no_container", func(t *testing.T) {
			t.Parallel()
			// With no instance backend, Start fails in the background
			// goroutine and the task transitions to Failed. SudoPassword
			// short-circuits to "" because Runtime is empty; this exercises
			// the goroutine's sudo branch without an SSH round-trip.
			m := newManagerWithRepo(t)
			id, err := m.Create(t.Context(), CreateParams{
				Prompt:  agent.Prompt{Text: "hi"},
				Repos:   []CreateRepo{{Name: "my/repo"}},
				Harness: "fake",
				Sudo:    true,
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			e, _ := m.GetEntry(id)
			if !e.Task().Sudo {
				t.Error("Sudo flag not propagated to task")
			}
		})
		t.Run("valid_forge_issue_and_pr", func(t *testing.T) {
			t.Parallel()
			// The bot path passes ForgeIssue/ForgeOwner/ForgeRepo; Create records
			// the issue number and calls SetPR so ListPendingBotTasks works.
			m := newManagerWithRepo(t)
			id, err := m.Create(t.Context(), CreateParams{
				Prompt:     agent.Prompt{Text: "hi"},
				Repos:      []CreateRepo{{Name: "my/repo"}},
				Harness:    "fake",
				ForgeIssue: 17,
				ForgeOwner: "acme",
				ForgeRepo:  "magic",
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			e, _ := m.GetEntry(id)
			snap := e.Task().Snapshot()
			if snap.ForgeIssue != 17 {
				t.Errorf("ForgeIssue = %d, want 17", snap.ForgeIssue)
			}
			if snap.ForgeOwner != "acme" || snap.ForgeRepo != "magic" {
				t.Errorf("ForgeOwner/Repo = %q/%q, want acme/magic", snap.ForgeOwner, snap.ForgeRepo)
			}
		})
		t.Run("valid_no_forge_owner_leaves_pr_zero", func(t *testing.T) {
			t.Parallel()
			// HTTP creates leave the forge fields zero: SetPR must not be called.
			m := newManagerWithRepo(t)
			id, err := m.Create(t.Context(), CreateParams{
				Prompt:  agent.Prompt{Text: "hi"},
				Repos:   []CreateRepo{{Name: "my/repo"}},
				Harness: "fake",
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			e, _ := m.GetEntry(id)
			snap := e.Task().Snapshot()
			if snap.ForgeIssue != 0 || snap.ForgeOwner != "" || snap.ForgeRepo != "" {
				t.Errorf("forge fields not zero: issue=%d owner=%q repo=%q", snap.ForgeIssue, snap.ForgeOwner, snap.ForgeRepo)
			}
		})
		t.Run("error_unknown_repo", func(t *testing.T) {
			t.Parallel()
			m := newManagerWithRepo(t)
			_, err := m.Create(t.Context(), CreateParams{
				Prompt:  agent.Prompt{Text: "hi"},
				Repos:   []CreateRepo{{Name: "ghost"}},
				Harness: "fake",
			})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
		t.Run("error_images_unsupported", func(t *testing.T) {
			t.Parallel()
			m := newManagerWithRepo(t)
			_, err := m.Create(t.Context(), CreateParams{
				Prompt:  agent.Prompt{Text: "hi", Images: []agent.ImageData{{}}},
				Repos:   []CreateRepo{{Name: "my/repo"}},
				Harness: "fake",
			})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
	})

	t.Run("Fork", func(t *testing.T) {
		t.Parallel()
		// newForkManager returns a Manager with a source task that has a
		// instance, plus an workspace with a fake backend.
		newForkManager := func(t *testing.T) (*Manager, *Entry) {
			m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: map[harness.Name]agent.Backend{"fake": &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}})
			m.RegisterWorkspace("my/repo", &repowork.Workspace{Dir: "/tmp/my-repo", Log: logtest.Logger(t)})
			src := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "src"},
				Repos:         []task.RepoMount{{Name: "my/repo", Branch: "caic-1", GitRoot: "/tmp/my-repo"}},
				Harness:       "fake",
				MaxCPUs:       5,
				GitHubToken:   true,
			}
			src.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "md-agent-src"), runtime.ConnectionTarget{SSHHost: "md-agent-src"}, "", "", 0)
			src.SetState(task.StateWaiting)
			e := NewEntry(src, nil)
			m.Insert(src.ID.String(), e)
			return m, e
		}
		t.Run("valid_resolved_overrides_and_max_cpus", func(t *testing.T) {
			t.Parallel()
			m, src := newForkManager(t)
			id, err := m.Fork(t.Context(), src, ForkParams{
				Prompt:    agent.Prompt{Text: "fork"},
				Tailscale: true,
				USB:       true,
				Display:   true,
				Sudo:      true,
			})
			if err != nil {
				t.Fatalf("Fork: %v", err)
			}
			e, ok := m.GetEntry(id)
			if !ok {
				t.Fatal("fork entry not found")
			}
			tk := e.Task()
			if !tk.Tailscale || !tk.USB || !tk.Display || !tk.Sudo {
				t.Errorf("resolved overrides not applied: tailscale=%v usb=%v display=%v sudo=%v",
					tk.Tailscale, tk.USB, tk.Display, tk.Sudo)
			}
			if tk.MaxCPUs != 5 {
				t.Errorf("MaxCPUs = %d, want 5 (from source)", tk.MaxCPUs)
			}
			if tk.ForkedFromTaskID != src.Task().ID {
				t.Errorf("ForkedFromTaskID = %s, want %s", tk.ForkedFromTaskID, src.Task().ID)
			}
		})
		t.Run("valid_stopped_source", func(t *testing.T) {
			t.Parallel()
			m, src := newForkManager(t)
			src.Task().SetState(task.StateStopped)
			id, err := m.Fork(t.Context(), src, ForkParams{Prompt: agent.Prompt{Text: "fork"}})
			if err != nil {
				t.Fatalf("Fork: %v", err)
			}
			fork, ok := m.GetEntry(id)
			if !ok {
				t.Fatal("fork entry not found")
			}
			select {
			case <-fork.Done():
			case <-time.After(time.Second):
				t.Fatal("fork did not finish")
			}
			if got := src.Task().GetState(); got != task.StateStopped {
				t.Errorf("source state = %v, want %v", got, task.StateStopped)
			}
		})
		t.Run("valid_crashed_source", func(t *testing.T) {
			t.Parallel()
			m, src := newForkManager(t)
			src.Task().SetState(task.StateCrashed)
			id, err := m.Fork(t.Context(), src, ForkParams{Prompt: agent.Prompt{Text: "fork"}})
			if err != nil {
				t.Fatalf("Fork: %v", err)
			}
			fork, ok := m.GetEntry(id)
			if !ok {
				t.Fatal("fork entry not found")
			}
			select {
			case <-fork.Done():
			case <-time.After(time.Second):
				t.Fatal("fork did not finish")
			}
			if got := src.Task().GetState(); got != task.StateCrashed {
				t.Errorf("source state = %v, want %v", got, task.StateCrashed)
			}
		})
		t.Run("valid_metadata_matches_task", func(t *testing.T) {
			t.Parallel()
			m, src := newForkManager(t)
			id, err := m.Fork(t.Context(), src, ForkParams{Prompt: agent.Prompt{Text: "fork"}})
			if err != nil {
				t.Fatalf("Fork: %v", err)
			}
			e, _ := m.GetEntry(id)
			metadata := task.MakeMetadata(e.Task())
			if metadata[runtime.MetadataTaskID] != e.Task().ID.String() {
				t.Errorf("metadata[%s] = %q, want %q", runtime.MetadataTaskID, metadata[runtime.MetadataTaskID], e.Task().ID.String())
			}
		})
		t.Run("error_extra_repo_overlap", func(t *testing.T) {
			t.Parallel()
			m, src := newForkManager(t)
			_, err := m.Fork(t.Context(), src, ForkParams{
				Prompt:     agent.Prompt{Text: "fork"},
				ExtraRepos: []ForkRepo{{Name: "my/repo"}},
			})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
		t.Run("error_no_repo", func(t *testing.T) {
			t.Parallel()
			// Forking a task with no repos is invalid input (KindBadRequest,
			// 400), not a state conflict. Guards against the parity drift where
			// this returned 409.
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			src := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "src"},
				Harness:       "fake",
			}
			src.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "md-agent-src"), runtime.ConnectionTarget{SSHHost: "md-agent-src"}, "", "", 0)
			src.SetState(task.StateWaiting)
			e := NewEntry(src, nil)
			m.Insert(src.ID.String(), e)
			_, err := m.Fork(t.Context(), e, ForkParams{Prompt: agent.Prompt{Text: "fork"}})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
	})

	t.Run("Restart", func(t *testing.T) {
		t.Parallel()
		t.Run("error_empty_prompt_no_container", func(t *testing.T) {
			t.Parallel()
			// Empty prompt triggers the plan-file fallback; with no instance,
			// agent.ReadPlan fails and Restart returns KindBadRequest.
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetState(task.StateWaiting)
			e := NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			err := m.Restart(t.Context(), e, agent.Prompt{})
			te, ok := errors.AsType[*Error](err)
			if !ok {
				t.Fatalf("err %v is not a *Error", err)
			}
			if te.Kind != KindBadRequest {
				t.Errorf("Kind = %v, want KindBadRequest (err=%v)", te.Kind, err)
			}
		})
		t.Run("error_wrong_state", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetState(task.StateStopped)
			e := NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			err := m.Restart(t.Context(), e, agent.Prompt{Text: "go"})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
	})

	t.Run("SendInput", func(t *testing.T) {
		t.Parallel()
		t.Run("error_no_session_is_sentinel", func(t *testing.T) {
			t.Parallel()
			// A waiting task with no active session: SendInput fails and the
			// error must satisfy errors.Is(err, ErrNoSession) while still
			// carrying the underlying diagnostic message.
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetState(task.StateWaiting)
			e := NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			err := m.SendInput(t.Context(), e, agent.Prompt{Text: "go"})
			if err == nil {
				t.Fatal("expected error from SendInput with no session")
			}
			if !errors.Is(err, ErrNoSession) {
				t.Errorf("errors.Is(err, ErrNoSession) = false, err = %v", err)
			}
			if !strings.Contains(err.Error(), "no active session") {
				t.Errorf("err message %q lost underlying diagnostic", err.Error())
			}
		})
		t.Run("valid_reconnects_before_answering_restored_ask", func(t *testing.T) {
			t.Parallel()
			backend := &reconnectInputBackend{FakeBackend: &agenttest.FakeBackend{HarnessName: "reconnect", Images: true, ContextLimit: 200_000}}
			t.Cleanup(backend.stop)
			logDir := filepath.Join(t.TempDir(), "logs")
			m := newTestManager(t, Config{
				ServerCtx: t.Context(),
				LogDir:    logDir,
				Backends:  map[harness.Name]agent.Backend{"reconnect": backend},
			})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
				Harness:       "reconnect",
			}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(task.StateRunning)
			tk.RestoreMessages([]agent.Message{
				&agent.AskMessage{
					ToolUseID: "toolu-1",
					Questions: []agent.AskQuestion{{Question: "Which?"}},
				},
				&agent.PendingUserActionMessage{
					MessageType: agent.PendingUserActionMessageType,
					Action: agent.PendingUserAction{
						Kind:      agent.PendingUserActionAskUserQuestion,
						RequestID: "req-1",
						ToolUseID: "toolu-1",
						Ask: agent.PendingAskAction{
							Questions: []agent.AskQuestion{{Question: "Which?"}},
						},
					},
				},
				&agent.ResultMessage{MessageType: "result"},
			})
			logW, err := (&task.LogStore{LogDir: logDir}).Open(tk)
			if err != nil {
				t.Fatal(err)
			}
			if err := logW.Close(); err != nil {
				t.Fatal(err)
			}
			e := NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)

			if err := m.SendInput(t.Context(), e, agent.Prompt{Text: "A"}); err != nil {
				t.Fatal(err)
			}

			backend.mu.Lock()
			attachCalls := backend.attachCalls
			prompts := slices.Clone(backend.prompts)
			opts := backend.opts
			backend.mu.Unlock()
			if attachCalls != 1 {
				t.Fatalf("AttachRelay calls = %d, want 1", attachCalls)
			}
			if opts == nil {
				t.Fatal("AttachRelay opts = nil")
			}
			if len(opts.PendingUserActions) != 1 {
				t.Fatalf("PendingUserActions = %#v, want one restored ask", opts.PendingUserActions)
			}
			if len(prompts) != 1 || prompts[0].Text != "A" {
				t.Fatalf("prompts = %#v, want answer prompt", prompts)
			}
			if got := tk.GetState(); got != task.StateRunning {
				t.Fatalf("state = %s, want %s", got, task.StateRunning)
			}
		})
		t.Run("error_delivery_failure_is_not_no_session", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}, Harness: harness.Codex}
			tk.SetState(task.StateWaiting)

			cmdCtx, cmdCancel := context.WithCancel(t.Context())
			cmd := exec.CommandContext(cmdCtx, "sleep", "60")
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			s := agent.NewSession(cmd, agent.NewConn(stdin, io.Discard, codex.New("", nil).NewWire()), stdout, make(chan agent.Message, 256), nil)
			t.Cleanup(func() {
				cmdCancel()
				_ = s.Wait()
			})
			tk.AttachSession(&task.SessionHandle{Session: s})

			e := NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			err = m.SendInput(t.Context(), e, agent.Prompt{Text: "go"})
			if err == nil {
				t.Fatal("expected delivery error")
			}
			if errors.Is(err, ErrNoSession) {
				t.Fatalf("errors.Is(err, ErrNoSession) = true, err = %v", err)
			}
			taskErr, ok := errors.AsType[*Error](err)
			if !ok {
				t.Fatalf("err type = %T, want *Error", err)
			}
			if taskErr.Kind != KindConflict {
				t.Errorf("Kind = %s, want %s", taskErr.Kind, KindConflict)
			}
			if got := tk.GetState(); got != task.StateWaiting {
				t.Errorf("state = %s, want %s", got, task.StateWaiting)
			}
			if msgs := tk.Messages(); len(msgs) != 0 {
				t.Fatalf("messages = %d, want 0", len(msgs))
			}
		})
	})

	t.Run("Concurrency", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_concurrent_insert_and_range", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			var wg sync.WaitGroup
			for range 10 {
				tk := &task.Task{
					ID:            ksid.NewID(),
					InitialPrompt: agent.Prompt{Text: "test"},
				}
				id := tk.ID.String()
				wg.Go(func() {
					m.Insert(id, NewEntry(tk, nil))
				})
			}
			for range 5 {
				wg.Go(func() {
					m.Range(func(id string, e *Entry) bool { return true })
				})
			}
			wg.Wait()
			if m.Len() != 10 {
				t.Errorf("Len() = %d after concurrent inserts, want 10", m.Len())
			}
		})
		t.Run("valid_concurrent_notify_change", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			var wg sync.WaitGroup
			for range 50 {
				wg.Go(func() {
					m.NotifyTaskChange()
				})
			}
			wg.Wait()
		})
	})
	t.Run("ClearContext", func(t *testing.T) {
		t.Parallel()
		t.Run("error_wrong_state", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetState(task.StateStopped)
			e := NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			err := m.ClearContext(t.Context(), e)
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
		t.Run("error_no_workspace_backend", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetState(task.StateWaiting)
			e := NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			err := m.ClearContext(t.Context(), e)
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindInternal {
				t.Fatalf("err = %v, want KindInternal", err)
			}
		})
	})
	t.Run("Compact", func(t *testing.T) {
		t.Parallel()
		t.Run("error_no_session", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetState(task.StateWaiting)
			e := NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			err := m.Compact(t.Context(), e, "shorten")
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
	})
	t.Run("SudoPassword", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_no_sudo", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
				Sudo:          false,
			}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			if got := m.SudoPassword(t.Context(), tk); got != "" {
				t.Errorf("SudoPassword = %q, want empty for !Sudo", got)
			}
		})
		t.Run("valid_no_container", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
				Sudo:          true,
			}
			if got := m.SudoPassword(t.Context(), tk); got != "" {
				t.Errorf("SudoPassword = %q, want empty for empty Runtime", got)
			}
		})
		t.Run("valid_cached", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
				Sudo:          true,
				SudoPassword:  "cached-pw",
			}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			if got := m.SudoPassword(t.Context(), tk); got != "cached-pw" {
				t.Errorf("SudoPassword = %q, want cached-pw", got)
			}
		})
		t.Run("valid_fetches_then_caches", func(t *testing.T) {
			t.Parallel()
			fake := &runtimetest.FakeInfo{SudoResult: "fetched-pw"}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake)})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
				Sudo:          true,
			}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			if got := m.SudoPassword(t.Context(), tk); got != "fetched-pw" {
				t.Errorf("SudoPassword = %q, want fetched-pw", got)
			}
			if snap := tk.Snapshot(); snap.SudoPassword != "fetched-pw" {
				t.Error("password not cached on task after fetch")
			}
			// Change what the backend would return; a second call must serve the
			// cached value, proving it did not hit the backend again.
			fake.SudoResult = "second-pw"
			if got := m.SudoPassword(t.Context(), tk); got != "fetched-pw" {
				t.Errorf("cached SudoPassword = %q, want fetched-pw (cache bypassed)", got)
			}
		})
		t.Run("valid_fetch_error_returns_empty", func(t *testing.T) {
			t.Parallel()
			fake := &runtimetest.FakeInfo{SudoErr: errors.New("ssh boom")}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake)})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
				Sudo:          true,
			}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			if got := m.SudoPassword(t.Context(), tk); got != "" {
				t.Errorf("SudoPassword = %q, want empty on fetch error", got)
			}
			if snap := tk.Snapshot(); snap.SudoPassword != "" {
				t.Errorf("task SudoPassword cached %q on error, want empty", snap.SudoPassword)
			}
		})
	})
	t.Run("HandleRuntimeInstanceExit", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_transitions_to_stopped", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
				Repos:         []task.RepoMount{{Name: "repo/x", Branch: "caic-1"}},
			}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-dead"), runtime.ConnectionTarget{SSHHost: "ctr-dead"}, "", "", 0)
			tk.SetState(task.StateRunning)
			m.Insert(tk.ID.String(), NewEntry(tk, nil))
			m.handleRuntimeInstanceExit(runtime.NewID("test-runtime", "ctr-dead"))
			if got := tk.GetState(); got != task.StateStopped {
				t.Errorf("state = %v, want StateStopped", got)
			}
		})
		t.Run("valid_skips_purged", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
			}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-purged"), runtime.ConnectionTarget{SSHHost: "ctr-purged"}, "", "", 0)
			tk.SetState(task.StatePurged)
			m.Insert(tk.ID.String(), NewEntry(tk, nil))
			m.handleRuntimeInstanceExit(runtime.NewID("test-runtime", "ctr-purged"))
			if got := tk.GetState(); got != task.StatePurged {
				t.Errorf("state = %v (should stay Purged)", got)
			}
		})
		t.Run("valid_skips_purging", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
			}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-purging"), runtime.ConnectionTarget{SSHHost: "ctr-purging"}, "", "", 0)
			// A purge in progress: removing the instance emits the very "die"
			// event handled here. Acting on it would flap the task to Stopped
			// mid-purge and race the cleanup goroutine.
			tk.SetState(task.StatePurging)
			m.Insert(tk.ID.String(), NewEntry(tk, nil))
			m.handleRuntimeInstanceExit(runtime.NewID("test-runtime", "ctr-purging"))
			if got := tk.GetState(); got != task.StatePurging {
				t.Errorf("state = %v (should stay Purging)", got)
			}
		})
		t.Run("valid_skips_stopping", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
			}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-stopping"), runtime.ConnectionTarget{SSHHost: "ctr-stopping"}, "", "", 0)
			tk.SetState(task.StateStopping)
			m.Insert(tk.ID.String(), NewEntry(tk, nil))
			m.handleRuntimeInstanceExit(runtime.NewID("test-runtime", "ctr-stopping"))
			if got := tk.GetState(); got != task.StateStopping {
				t.Errorf("state = %v (should stay Stopping)", got)
			}
		})
		t.Run("valid_skips_stopped", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
			}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-stopped"), runtime.ConnectionTarget{SSHHost: "ctr-stopped"}, "", "", 0)
			tk.SetState(task.StateStopped)
			m.Insert(tk.ID.String(), NewEntry(tk, nil))
			m.handleRuntimeInstanceExit(runtime.NewID("test-runtime", "ctr-stopped"))
			if got := tk.GetState(); got != task.StateStopped {
				t.Errorf("state = %v (should stay Stopped)", got)
			}
		})
		t.Run("valid_skips_wrong_container", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
			}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-alive"), runtime.ConnectionTarget{SSHHost: "ctr-alive"}, "", "", 0)
			tk.SetState(task.StateRunning)
			m.Insert(tk.ID.String(), NewEntry(tk, nil))
			m.handleRuntimeInstanceExit(runtime.NewID("test-runtime", "ctr-other"))
			if got := tk.GetState(); got != task.StateRunning {
				t.Errorf("state = %v (should stay Running)", got)
			}
		})
	})
	t.Run("LoadMessagesOnDemand", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_no_loaded_task", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
			}
			e := NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			m.LoadMessagesOnDemand(e)
		})
		t.Run("uses_header_resolver", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "task.jsonl"), []byte(
				`{"type":"caic_meta","version":1,"prompt":"task","repos":[],"harness":"claude"}`+"\n"+
					`{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`+"\n",
			), 0o600); err != nil {
				t.Fatal(err)
			}
			logs, err := task.LoadLogs(dir)
			if err != nil {
				t.Fatal(err)
			}
			var wireCalls int
			m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: map[harness.Name]agent.Backend{
				harness.Claude: &agenttest.FakeBackend{WireFactory: func() agent.WireFormat {
					wireCalls++
					return claudecode.New().NewWire()
				}},
			}})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "task"}}
			entry := newPurgedEntry(tk, &task.Result{State: task.StatePurged}, logs[0])
			m.LoadMessagesOnDemand(entry)
			if wireCalls != 1 {
				t.Fatalf("NewWire calls = %d, want 1", wireCalls)
			}
			if len(logs[0].Msgs) != 1 {
				t.Fatalf("loaded messages = %d, want 1", len(logs[0].Msgs))
			}
			if snapshot := logs[0].ValidatedSnapshot(); snapshot == nil || !snapshot.EOFValidated {
				t.Fatalf("snapshot = %#v, want validated EOF proof", snapshot)
			}
		})
	})
	t.Run("LoadPurgedTasks", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_empty", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			err := m.LoadPurgedTasks(nil)
			if err != nil {
				t.Fatalf("LoadPurgedTasks(nil): %v", err)
			}
			if m.Len() != 0 {
				t.Errorf("Len() = %d after nil load, want 0", m.Len())
			}
		})
		t.Run("valid_filters_old", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			old := time.Now().Add(-30 * 24 * time.Hour).UTC()
			all := []*task.LoadedTask{
				{
					TaskID:            ksid.NewID().String(),
					Prompt:            "old",
					Harness:           "claude",
					LastStateUpdateAt: old,
					State:             task.StateStopped,
				},
			}
			err := m.LoadPurgedTasks(all)
			if err != nil {
				t.Fatalf("LoadPurgedTasks: %v", err)
			}
			if m.Len() != 0 {
				t.Errorf("Len() = %d after loading old task, want 0", m.Len())
			}
		})
		t.Run("valid_creates_entries", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			m.RegisterWorkspace("repo/a", &repowork.Workspace{Dir: "", Log: logtest.Logger(t)})
			now := time.Now().UTC()
			id := ksid.NewID()
			all := []*task.LoadedTask{
				{
					TaskID:            id.String(),
					Prompt:            "test task",
					Title:             "Test Title",
					Harness:           "claude",
					RuntimeName:       "docker",
					Repos:             []task.RepoMount{{Name: "repo/a", Branch: "caic-1"}},
					State:             task.StateStopped,
					Result:            &task.Result{State: task.StateStopped},
					StartedAt:         now.Add(-1 * time.Hour),
					LastStateUpdateAt: now,
					Tailscale:         true,
					USB:               true,
					Display:           true,
					Sudo:              true,
					GitHubToken:       true,
					BaseImage:         "ghcr.io/caic/base:v1",
					ContainerPlatform: "linux/amd64",
					MaxCPUs:           4,
					CacheMounts:       []runtime.CacheMount{{Name: "npm", HostPath: "~/.npm", ContainerPath: "/home/user/.npm", ReadOnly: true}},
					Mounts:            []runtime.Mount{{HostPath: "/host/work", ContainerPath: "/workspace/work", ReadOnly: true}},
					Model:             "model-1",
					Effort:            "high",
					AgentVersion:      "1.2.3",
					ForgePR:           42,
					ForgeOwner:        "acme",
					ForgeRepo:         "magic",
				},
			}
			err := m.LoadPurgedTasks(all)
			if err != nil {
				t.Fatalf("LoadPurgedTasks: %v", err)
			}
			if m.Len() != 1 {
				t.Fatalf("Len() = %d, want 1", m.Len())
			}
			e, ok := m.GetEntry(id.String())
			if !ok {
				t.Fatal("entry not found for expected ID")
			}
			tk := e.Task()
			if tk.Tailscale != true || tk.USB != true || tk.Display != true {
				t.Error("instance flags not restored")
			}
			snap := tk.Snapshot()
			if !snap.Sudo || !snap.GitHubToken {
				t.Errorf("privileged flags not restored: sudo=%v gitHubToken=%v", snap.Sudo, snap.GitHubToken)
			}
			if tk.RuntimeName != "docker" {
				t.Errorf("RuntimeName = %q, want docker", tk.RuntimeName)
			}
			if tk.Model != "model-1" || tk.Effort != "high" {
				t.Errorf("model/effort = %q/%q, want model-1/high", tk.Model, tk.Effort)
			}
			if tk.BaseImage != "ghcr.io/caic/base:v1" || tk.ContainerPlatform != "linux/amd64" || tk.MaxCPUs != 4 {
				t.Errorf("launch config = image %q platform %q cpus %d", tk.BaseImage, tk.ContainerPlatform, tk.MaxCPUs)
			}
			if len(tk.CacheMounts) != 1 || tk.CacheMounts[0].Name != "npm" || !tk.CacheMounts[0].ReadOnly {
				t.Errorf("CacheMounts = %+v", tk.CacheMounts)
			}
			if len(tk.Mounts) != 1 || tk.Mounts[0].HostPath != "/host/work" || !tk.Mounts[0].ReadOnly {
				t.Errorf("Mounts = %+v", tk.Mounts)
			}
			if tk.Title() != "Test Title" {
				t.Errorf("Title = %q, want \"Test Title\"", tk.Title())
			}
			if tk.GetPR() != 42 {
				t.Errorf("PR = %d, want 42", tk.GetPR())
			}
			if e.Result() == nil || e.Result().State != task.StateStopped {
				t.Errorf("Result = %v, want StateStopped", e.Result())
			}
		})
		t.Run("valid_promotes_missing_log_runtime", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			id := ksid.NewID()
			all := []*task.LoadedTask{{
				TaskID:            id.String(),
				Prompt:            "old task",
				Harness:           "claude",
				State:             task.StateStopped,
				Result:            &task.Result{State: task.StateStopped},
				LastStateUpdateAt: time.Now().UTC(),
			}}
			if err := m.LoadPurgedTasks(all); err != nil {
				t.Fatalf("LoadPurgedTasks: %v", err)
			}
			e, ok := m.GetEntry(id.String())
			if !ok {
				t.Fatal("entry not found")
			}
			if got := e.Task().RuntimeName; got != "test-runtime" {
				t.Fatalf("RuntimeName = %q, want test-runtime", got)
			}
			if got := all[0].RuntimeName; got != "test-runtime" {
				t.Fatalf("loaded RuntimeName = %q, want test-runtime", got)
			}
		})
		t.Run("valid_preloads_session_metadata", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			dir := t.TempDir()
			id := ksid.NewID()
			marshal := func(v any) string {
				b, err := json.Marshal(v)
				if err != nil {
					t.Fatal(err)
				}
				return string(b)
			}
			lines := []string{
				marshal(agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "pi task", Harness: harness.Pi, StartedAt: time.Now().UTC()}),
				marshal(agent.MetaSessionMessage{MessageType: "caic_session", SessionID: "ses-1", AgentVersion: "pi 1.2.3"}),
				`{"type":"text","text":"` + strings.Repeat("x", 70<<10) + `"}`,
				marshal(agent.MetaResultMessage{MessageType: "caic_result", State: task.StateStopped.String()}),
			}
			path := filepath.Join(dir, id.String()+"--.jsonl")
			if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			logs, err := task.LoadLogs(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(logs) != 1 {
				t.Fatalf("len(logs) = %d, want 1", len(logs))
			}
			if logs[0].SessionID != "ses-1" || logs[0].AgentVersion != "pi 1.2.3" {
				t.Fatalf("preloaded metadata = %q/%q, want ses-1/pi 1.2.3", logs[0].SessionID, logs[0].AgentVersion)
			}

			if err := m.LoadPurgedTasks(logs); err != nil {
				t.Fatalf("LoadPurgedTasks: %v", err)
			}
			e, ok := m.GetEntry(id.String())
			if !ok {
				t.Fatal("entry not found")
			}
			snap := e.Task().Snapshot()
			if snap.SessionID != "ses-1" {
				t.Errorf("SessionID = %q, want ses-1", snap.SessionID)
			}
			if snap.AgentVersion != "pi 1.2.3" {
				t.Errorf("AgentVersion = %q, want pi 1.2.3", snap.AgentVersion)
			}
		})
		t.Run("valid_running_becomes_failed", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			now := time.Now().UTC()
			id := ksid.NewID()
			all := []*task.LoadedTask{
				{
					TaskID:            id.String(),
					Prompt:            "was running",
					Repos:             []task.RepoMount{{Name: "repo/a"}},
					State:             task.StateRunning,
					LastStateUpdateAt: now,
				},
			}
			err := m.LoadPurgedTasks(all)
			if err != nil {
				t.Fatalf("LoadPurgedTasks: %v", err)
			}
			e, _ := m.GetEntry(id.String())
			if got := e.Task().GetState(); got != task.StateFailed {
				t.Errorf("state = %v, want StateFailed (running→failed)", got)
			}
		})
		t.Run("valid_max_per_repo", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			now := time.Now().UTC()
			all := make([]*task.LoadedTask, 0, 7)
			for i := range 7 {
				all = append(all, &task.LoadedTask{
					TaskID:            ksid.NewID().String(),
					Prompt:            "task",
					Repos:             []task.RepoMount{{Name: "repo/a"}},
					State:             task.StateStopped,
					LastStateUpdateAt: now.Add(time.Duration(i) * time.Second),
				})
			}
			err := m.LoadPurgedTasks(all)
			if err != nil {
				t.Fatalf("LoadPurgedTasks: %v", err)
			}
			if got := m.Len(); got != 5 {
				t.Errorf("Len() = %d, want 5 (maxPurgedPerRepo)", got)
			}
		})
		t.Run("valid_fallback_title", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			now := time.Now().UTC()
			id := ksid.NewID()
			all := []*task.LoadedTask{
				{
					TaskID:            id.String(),
					Prompt:            "this is the prompt",
					LastStateUpdateAt: now,
					State:             task.StateStopped,
				},
			}
			_ = m.LoadPurgedTasks(all)
			e, _ := m.GetEntry(id.String())
			if got := e.Task().Title(); got != "this is the prompt" {
				t.Errorf("Title = %q, want prompt fallback", got)
			}
		})
		t.Run("valid_no_result_fallback_to_failed", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			now := time.Now().UTC()
			id := ksid.NewID()
			all := []*task.LoadedTask{
				{
					TaskID:            id.String(),
					Prompt:            "test",
					LastStateUpdateAt: now,
				},
			}
			_ = m.LoadPurgedTasks(all)
			e, _ := m.GetEntry(id.String())
			if got := e.Result().State; got != task.StateFailed {
				t.Errorf("Result.State = %v, want StateFailed (fallback)", got)
			}
		})
		t.Run("valid_invalid_ksid_fallback", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			now := time.Now().UTC()
			all := []*task.LoadedTask{
				{
					TaskID:            "invalid",
					Prompt:            "test",
					LastStateUpdateAt: now,
					State:             task.StateStopped,
				},
			}
			_ = m.LoadPurgedTasks(all)
			if m.Len() != 1 {
				t.Errorf("Len() = %d, want 1 (new ksid fallback)", m.Len())
			}
		})
		t.Run("valid_skips_existing_live_tasks", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			now := time.Now().UTC()
			activeID := ksid.NewID()
			active := &task.Task{
				ID:            activeID,
				InitialPrompt: agent.Prompt{Text: "active"},
				Repos:         []task.RepoMount{{Name: "repo/a", Branch: "caic-live"}},
			}
			active.SetTitle("active")
			m.Insert(activeID.String(), NewEntry(active, nil))
			duplicateBranchID := ksid.NewID()
			keptID := ksid.NewID()
			all := []*task.LoadedTask{
				{
					TaskID:            activeID.String(),
					Prompt:            "replacement by id",
					Repos:             []task.RepoMount{{Name: "repo/a", Branch: "caic-old"}},
					LastStateUpdateAt: now,
					State:             task.StateStopped,
				},
				{
					TaskID:            duplicateBranchID.String(),
					Prompt:            "replacement by branch",
					Repos:             []task.RepoMount{{Name: "repo/a", Branch: "caic-live"}},
					LastStateUpdateAt: now,
					State:             task.StateStopped,
				},
				{
					TaskID:            keptID.String(),
					Prompt:            "kept",
					Repos:             []task.RepoMount{{Name: "repo/a", Branch: "caic-kept"}},
					LastStateUpdateAt: now,
					State:             task.StateStopped,
				},
			}
			if err := m.LoadPurgedTasks(all); err != nil {
				t.Fatalf("LoadPurgedTasks: %v", err)
			}
			if got := m.Len(); got != 2 {
				t.Fatalf("Len() = %d, want active + kept", got)
			}
			if _, ok := m.GetEntry(duplicateBranchID.String()); ok {
				t.Fatal("duplicate branch task was loaded")
			}
			activeEntry, _ := m.GetEntry(activeID.String())
			if got := activeEntry.Task().Title(); got != "active" {
				t.Errorf("active title = %q, want active", got)
			}
			if _, ok := m.GetEntry(keptID.String()); !ok {
				t.Fatal("kept task was not loaded")
			}
		})
	})
	t.Run("Sync", func(t *testing.T) {
		t.Parallel()
		t.Run("error_pending_no_container", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetState(task.StatePending)
			e := NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			_, err := m.Sync(t.Context(), e, SyncTargetOrigin, false)
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
		t.Run("error_purging", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetState(task.StatePurging)
			e := NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			_, err := m.Sync(t.Context(), e, SyncTargetOrigin, false)
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
		t.Run("error_provisioning_no_workspace", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetState(task.StateProvisioning)
			e := NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			_, err := m.Sync(t.Context(), e, SyncTargetOrigin, false)
			if err == nil {
				t.Fatal("expected error for provisioning task without instance")
			}
		})
		t.Run("error_force_not_supported", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetState(task.StateRunning)
			e := NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			_, err := m.Sync(t.Context(), e, SyncTargetDefault, true)
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
	})
	t.Run("Purge", func(t *testing.T) {
		t.Parallel()
		t.Run("error_wrong_state", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetState(task.StatePurged)
			e := NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			err := m.Purge(t.Context(), e)
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
		t.Run("valid_after_finished_crash", func(t *testing.T) {
			t.Parallel()
			fake := &runtimetest.FakeBackend{}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, fake, nil)})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(task.StateCrashed)
			entry := NewEntry(tk, nil)
			entry.Finish(&task.Result{State: task.StateCrashed, Err: errors.New("agent crashed")})
			m.Insert(tk.ID.String(), entry)

			if err := m.Purge(t.Context(), entry); err != nil {
				t.Fatalf("Purge: %v", err)
			}
			purgeDeadline := time.Now().Add(time.Second)
			for fake.Status("ctr-1") != runtimetest.StatusPurged {
				if time.Now().After(purgeDeadline) {
					t.Fatalf("instance status = %v, want purged", fake.Status("ctr-1"))
				}
				time.Sleep(time.Millisecond)
			}
			deadline := time.Now().Add(time.Second)
			for {
				if result := entry.Result(); result != nil && result.State == task.StatePurged {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("Result = %v, want StatePurged", entry.Result())
				}
				time.Sleep(time.Millisecond)
			}
		})
		t.Run("valid_wins_race_with_stop", func(t *testing.T) {
			t.Parallel()
			fake := &blockingStopBackend{
				FakeBackend: &runtimetest.FakeBackend{},
				started:     make(chan struct{}),
				returned:    make(chan struct{}),
				release:     make(chan struct{}),
			}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, fake, nil)})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
			}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(task.StateRunning)
			entry := NewEntry(tk, nil)
			m.Insert(tk.ID.String(), entry)

			if err := m.Stop(t.Context(), entry); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			select {
			case <-fake.started:
			case <-time.After(time.Second):
				t.Fatal("StopTask did not reach backend Stop")
			}

			if err := m.Purge(t.Context(), entry); err != nil {
				t.Fatalf("Purge: %v", err)
			}
			select {
			case <-entry.Done():
			case <-time.After(time.Second):
				t.Fatal("purge did not close done")
			}
			stopDoneChanged := m.Changed()
			close(fake.release)
			select {
			case <-fake.returned:
			case <-time.After(time.Second):
				t.Fatal("backend Stop did not return")
			}
			select {
			case <-stopDoneChanged:
			case <-time.After(time.Second):
				t.Fatal("StopTask completion did not notify")
			}

			deadline := time.Now().Add(time.Second)
			for tk.GetState() != task.StatePurged {
				if time.Now().After(deadline) {
					t.Fatalf("state = %v, want StatePurged after StopTask finishes", tk.GetState())
				}
				time.Sleep(time.Millisecond)
			}
			if result := entry.Result(); result == nil || result.State != task.StatePurged {
				t.Fatalf("Result = %v, want StatePurged", result)
			}
		})
	})
	t.Run("Stop", func(t *testing.T) {
		t.Parallel()
		t.Run("error_wrong_state", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetState(task.StateStopped)
			e := NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			err := m.Stop(t.Context(), e)
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
		t.Run("valid_stops_container_backend", func(t *testing.T) {
			t.Parallel()
			// Backend is the interface seam, so a fake stands in for Docker.
			fake := &runtimetest.FakeBackend{}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, fake, nil)})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
			}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(task.StateRunning)
			entry := NewEntry(tk, nil)
			m.Insert(tk.ID.String(), entry)

			if err := m.Stop(t.Context(), entry); err != nil {
				t.Fatalf("Stop: %v", err)
			}

			// Stop runs StopTask on a background goroutine; wait for the task to
			// settle rather than sleeping a fixed duration.
			deadline := time.Now().Add(2 * time.Second)
			for tk.GetState() != task.StateStopped {
				if time.Now().After(deadline) {
					t.Fatalf("state = %v, want StateStopped", tk.GetState())
				}
				time.Sleep(time.Millisecond)
			}
			if got := fake.Status("ctr-1"); got != runtimetest.StatusStopped {
				t.Errorf("instance ctr-1 status = %v, want stopped", got)
			}
		})
	})
	t.Run("Revive", func(t *testing.T) {
		t.Parallel()
		t.Run("error_wrong_state", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetState(task.StateRunning)
			e := NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			err := m.Revive(t.Context(), e)
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
		t.Run("valid_accepts_crashed", func(t *testing.T) {
			t.Parallel()
			releaseRevive := make(chan struct{})
			fake := &blockingReviveBackend{FakeBackend: &runtimetest.FakeBackend{}, release: releaseRevive}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, fake, nil)})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
			}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(task.StateCrashed)
			entry := NewEntry(tk, nil)
			m.Insert(tk.ID.String(), entry)

			if err := m.Revive(t.Context(), entry); err != nil {
				t.Fatalf("Revive: %v", err)
			}
			if got := tk.GetState(); got != task.StateProvisioning {
				t.Fatalf("state = %v, want provisioning", got)
			}
			close(releaseRevive)
			select {
			case <-entry.Done():
			case <-time.After(time.Second):
				t.Fatal("failed revive did not close done")
			}
		})
		t.Run("error_failure_closes_done_and_publishes_result", func(t *testing.T) {
			t.Parallel()
			releaseRevive := make(chan struct{})
			fake := &blockingReviveBackend{FakeBackend: &runtimetest.FakeBackend{}, release: releaseRevive}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, fake, nil)})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
			}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(task.StateStopped)
			entry := NewEntry(tk, nil)
			m.Insert(tk.ID.String(), entry)

			firstChanged := m.Changed()
			if err := m.Revive(t.Context(), entry); err != nil {
				t.Fatalf("Revive: %v", err)
			}
			select {
			case <-firstChanged:
			case <-time.After(time.Second):
				t.Fatal("initial revive transition did not notify")
			}
			failedChanged := m.Changed()
			close(releaseRevive)

			select {
			case <-entry.Done():
			case <-time.After(time.Second):
				t.Fatal("failed revive did not close done")
			}
			select {
			case <-failedChanged:
			case <-time.After(time.Second):
				t.Fatal("failed revive did not notify")
			}
			if got := tk.GetState(); got != task.StateFailed {
				t.Fatalf("state = %v, want StateFailed", got)
			}
			result := entry.Result()
			if result == nil || result.State != task.StateFailed || result.Err == nil {
				t.Fatalf("Result = %v, want failed result with error", result)
			}
		})
	})
	t.Run("SendInput_Images", func(t *testing.T) {
		t.Parallel()
		t.Run("error_images_unsupported", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: map[harness.Name]agent.Backend{"fake": &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}})
			m.RegisterWorkspace("repo/a", &repowork.Workspace{Dir: "/tmp/repo", Log: logtest.Logger(t)})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
				Repos:         []task.RepoMount{{Name: "repo/a"}},
				Harness:       "fake",
			}
			tk.SetState(task.StateWaiting)
			e := NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			err := m.SendInput(t.Context(), e, agent.Prompt{Text: "go", Images: []agent.ImageData{{}}})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
	})
	t.Run("watchSession", func(t *testing.T) {
		t.Run("valid_session_error_crashes_and_stops_instance", func(t *testing.T) {
			t.Parallel()
			cmd := exec.CommandContext(t.Context(), "sh", "-c", "exit 255")
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			msgCh := make(chan agent.Message, 1)
			dispatchDone := make(chan struct{})
			close(dispatchDone)
			s := agent.NewSession(cmd, agent.NewConn(stdin, io.Discard, codex.New("", nil).NewWire()), stdout, msgCh, nil)
			h := &task.SessionHandle{Session: s, MsgCh: msgCh, DispatchDone: dispatchDone}
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ssh-failed"), runtime.ConnectionTarget{SSHHost: "ssh-failed"}, "", "", 0)
			tk.SetState(task.StateRunning)
			tk.AttachSession(h)
			entry := NewEntry(tk, nil)
			runtimeBackend := &runtimetest.FakeBackend{}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, runtimeBackend, nil)})

			m.watchSession(entry, &repowork.Workspace{Dir: "", Log: logtest.Logger(t)}, h)

			select {
			case <-entry.Done():
			case <-time.After(time.Second):
				t.Fatal("watchSession did not finish")
			}
			if got := tk.GetState(); got != task.StateCrashed {
				t.Fatalf("state = %v, want crashed", got)
			}
			if got := runtimeBackend.Status("ssh-failed"); got != runtimetest.StatusStopped {
				t.Fatalf("instance ssh-failed status = %v, want stopped", got)
			}
		})
	})
	t.Run("ResolveNativeParser", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_with_backend", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: map[harness.Name]agent.Backend{"claude": &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}})
			m.RegisterWorkspace("repo/a", &repowork.Workspace{Dir: "", Log: logtest.Logger(t)})
			if _, err := m.resolveNativeParser("claude"); err != nil {
				t.Fatalf("resolveNativeParser: %v", err)
			}
		})
		t.Run("missing_backend", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			if _, err := m.resolveNativeParser("pi"); err == nil || !strings.Contains(err.Error(), "unknown harness") {
				t.Fatalf("resolveNativeParser error = %v, want unknown-harness error", err)
			}
		})
	})
	t.Run("LoadMessagesOnDemand_Purged", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_with_loaded_task", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
			}
			lt := &task.LoadedTask{TaskID: tk.ID.String()}
			e := newPurgedEntry(tk, &task.Result{State: task.StatePurged}, lt)
			m.Insert(tk.ID.String(), e)
			// LoadMessagesOnce triggers the fn but LoadMessages will fail
			// (no log file); this exercises the LoadMessagesOnce path.
			m.LoadMessagesOnDemand(e)
		})
	})
	t.Run("Create_Errors", func(t *testing.T) {
		t.Parallel()
		t.Run("error_unknown_harness", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: map[harness.Name]agent.Backend{}})
			m.RegisterWorkspace("repo/a", &repowork.Workspace{Dir: "/tmp/repo", Log: logtest.Logger(t)})
			_, err := m.Create(t.Context(), CreateParams{
				Prompt:  agent.Prompt{Text: "hi"},
				Repos:   []CreateRepo{{Name: "repo/a"}},
				Harness: "bogus",
			})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
		t.Run("error_unsupported_model", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: map[harness.Name]agent.Backend{"fake": &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}})
			m.RegisterWorkspace("repo/a", &repowork.Workspace{Dir: "/tmp/repo", Log: logtest.Logger(t)})
			_, err := m.Create(t.Context(), CreateParams{
				Prompt:  agent.Prompt{Text: "hi"},
				Repos:   []CreateRepo{{Name: "repo/a"}},
				Harness: "fake",
				Model:   "unsupported-model",
			})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
		t.Run("error_unknown_extra_repo", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: map[harness.Name]agent.Backend{"fake": &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}})
			m.RegisterWorkspace("repo/a", &repowork.Workspace{Dir: "/tmp/repo", Log: logtest.Logger(t)})
			_, err := m.Create(t.Context(), CreateParams{
				Prompt:  agent.Prompt{Text: "hi"},
				Repos:   []CreateRepo{{Name: "repo/a"}, {Name: "ghost"}},
				Harness: "fake",
			})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
	})
	t.Run("Fork_Errors", func(t *testing.T) {
		t.Parallel()

		// forkSetup creates a Manager with an workspace for "repo/a" and a source
		// task in StateWaiting. Returns the Manager and the source Entry.
		forkSetup := func(t *testing.T, sourceHarness harness.Name, backends map[harness.Name]agent.Backend) (*Manager, *Entry) {
			m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: backends})
			r := &repowork.Workspace{Dir: "/tmp/repo", Log: logtest.Logger(t)}
			m.RegisterWorkspace("repo/a", r)
			src := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "src"},
				Repos:         []task.RepoMount{{Name: "repo/a", Branch: "caic-1"}},
				Harness:       sourceHarness,
			}
			src.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "md-agent-src"), runtime.ConnectionTarget{SSHHost: "md-agent-src"}, "", "", 0)
			src.SetState(task.StateWaiting)
			e := NewEntry(src, nil)
			m.Insert(src.ID.String(), e)
			return m, e
		}

		defaultBackends := map[harness.Name]agent.Backend{"fake": &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}

		t.Run("error_unknown_harness", func(t *testing.T) {
			t.Parallel()
			m, e := forkSetup(t, "fake", defaultBackends)
			_, err := m.Fork(t.Context(), e, ForkParams{Prompt: agent.Prompt{Text: "fork"}, Harness: "bogus"})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
		t.Run("error_unsupported_model", func(t *testing.T) {
			t.Parallel()
			m, e := forkSetup(t, "fake", defaultBackends)
			_, err := m.Fork(t.Context(), e, ForkParams{Prompt: agent.Prompt{Text: "fork"}, Model: "unsupported"})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
		t.Run("error_model_with_new_harness", func(t *testing.T) {
			t.Parallel()
			backends := map[harness.Name]agent.Backend{
				"fake":  &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire},
				"fake2": &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m2"}}}, WireFactory: claudecode.New().NewWire},
			}
			m, e := forkSetup(t, "fake", backends)
			_, err := m.Fork(t.Context(), e, ForkParams{Prompt: agent.Prompt{Text: "fork"}, Harness: "fake2", Model: "unsupported"})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
		t.Run("error_no_container", func(t *testing.T) {
			t.Parallel()
			m, e := forkSetup(t, "fake", defaultBackends)
			// Overwrite the instance to empty.
			e.Task().SetRuntimeConnectionInfo("", runtime.ConnectionTarget{SSHHost: ""}, "", "", 0)
			_, err := m.Fork(t.Context(), e, ForkParams{Prompt: agent.Prompt{Text: "fork"}})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
		t.Run("error_wrong_state", func(t *testing.T) {
			t.Parallel()
			m, e := forkSetup(t, "fake", defaultBackends)
			e.Task().SetState(task.StateProvisioning)
			_, err := m.Fork(t.Context(), e, ForkParams{Prompt: agent.Prompt{Text: "fork"}})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
		t.Run("error_unknown_extra_repo", func(t *testing.T) {
			t.Parallel()
			m, e := forkSetup(t, "fake", defaultBackends)
			_, err := m.Fork(t.Context(), e, ForkParams{Prompt: agent.Prompt{Text: "fork"}, ExtraRepos: []ForkRepo{{Name: "ghost"}}})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
		t.Run("error_unknown_harness_when_model_set", func(t *testing.T) {
			t.Parallel()
			m, e := forkSetup(t, "bogus", defaultBackends)
			_, err := m.Fork(t.Context(), e, ForkParams{Prompt: agent.Prompt{Text: "fork"}, Model: "m1"})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
	})

	t.Run("AdoptInstances", func(t *testing.T) {
		t.Parallel()
		t.Run("rejects_duplicate_runtime_task_ids", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			fake := &runtimetest.FakeInfo{Meta: map[string]string{
				"duplicate-one\x00caic.id":      taskID.String(),
				"duplicate-one\x00caic.harness": string(harness.Claude),
				"duplicate-two\x00caic.id":      taskID.String(),
				"duplicate-two\x00caic.harness": string(harness.Claude),
			}}
			m := newTestManager(t, Config{
				ServerCtx: t.Context(),
				Runtimes:  newTestRuntime(t, &runtimetest.FakeBackend{}, fake),
				Backends:  map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{}},
			})
			m.RegisterWorkspace("repo/a", &repowork.Workspace{Dir: "/home/user/src/repo/a", Log: logtest.Logger(t)})
			instances := []runtime.Instance{
				{ID: runtime.NewID("test-runtime", "duplicate-one"), State: "exited", Repos: []runtime.Repo{{ContainerPath: "/home/user/src/repo/a", Branch: "caic-1"}}},
				{ID: runtime.NewID("test-runtime", "duplicate-two"), State: "exited", Repos: []runtime.Repo{{ContainerPath: "/home/user/src/repo/a", Branch: "caic-1"}}},
			}
			adopted, err := m.AdoptInstances(t.Context(), []AdoptRepo{{RelPath: "repo/a", AbsPath: "/home/user/src/repo/a"}}, instances, nil)
			if err == nil || !strings.Contains(err.Error(), "duplicate runtime task ID") {
				t.Fatalf("AdoptInstances error = %v, want duplicate-task-ID error", err)
			}
			if adopted != nil || m.Len() != 0 {
				t.Fatalf("duplicate adoption mutated manager: adopted=%#v len=%d", adopted, m.Len())
			}
		})
		t.Run("rejects_harness_metadata_error", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			fake := &metadataErrorInfo{
				FakeInfo: &runtimetest.FakeInfo{Meta: map[string]string{
					"metadata-error\x00caic.id": taskID.String(),
				}},
				key: runtime.MetadataHarness,
				err: errors.New("harness metadata unavailable"),
			}
			m := newTestManager(t, Config{
				ServerCtx: t.Context(),
				Runtimes:  newTestRuntime(t, &runtimetest.FakeBackend{}, fake),
				Backends:  map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{}},
			})
			m.RegisterWorkspace("repo/a", &repowork.Workspace{Dir: "/home/user/src/repo/a", Log: logtest.Logger(t)})
			_, err := m.AdoptInstances(t.Context(), []AdoptRepo{{RelPath: "repo/a", AbsPath: "/home/user/src/repo/a"}}, []runtime.Instance{{
				ID:    runtime.NewID("test-runtime", "metadata-error"),
				State: "exited",
				Repos: []runtime.Repo{{GitRoot: "/home/user/src/repo/a", Branch: "caic-1", ContainerPath: "/home/user/src/repo/a"}},
			}}, []*task.LoadedTask{{
				TaskID:  taskID.String(),
				Harness: harness.Claude,
				Repos:   []task.RepoMount{{Name: "repo/a", Branch: "caic-1"}},
			}})
			if err == nil || !strings.Contains(err.Error(), "harness metadata unavailable") {
				t.Fatalf("AdoptInstances error = %v, want harness metadata error", err)
			}
		})
		t.Run("valid_matches_only_primary_repo", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			resetAt := time.Now().Add(time.Hour).UTC()
			fake := &runtimetest.FakeInfo{Meta: map[string]string{
				"md-caic-caic-5\x00caic.id":      taskID.String(),
				"md-caic-caic-5\x00caic.harness": string(harness.Claude),
			}}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake), Backends: map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}})
			m.RegisterWorkspace("caic-xyz/caic", &repowork.Workspace{Dir: "/home/user/src/caic-xyz/caic", Log: logtest.Logger(t)})
			m.RegisterWorkspace("caic-xyz/md", &repowork.Workspace{Dir: "/home/user/src/caic-xyz/md", Log: logtest.Logger(t)})

			adopted, err := m.AdoptInstances(t.Context(), []AdoptRepo{
				{RelPath: "caic-xyz/caic", AbsPath: "/home/user/src/caic-xyz/caic"},
				{RelPath: "caic-xyz/md", AbsPath: "/home/user/src/caic-xyz/md"},
			}, []runtime.Instance{
				{
					ID:    runtime.NewID("test-runtime", "md-caic-caic-5"),
					State: "exited",
					Repos: []runtime.Repo{
						{GitRoot: "/home/user/src/caic-xyz/caic", Branch: "caic-5", ContainerPath: "/home/user/src/caic-xyz/caic"},
						{GitRoot: "/home/user/src/caic-xyz/md", Branch: "caic-0", ContainerPath: "/home/user/src/caic-xyz/md"},
					},
				},
			}, []*task.LoadedTask{{
				TaskID:  taskID.String(),
				Harness: harness.Claude,
				Repos:   []task.RepoMount{{Name: "caic-xyz/caic", Branch: "caic-5"}},
				Msgs: []agent.Message{&agent.RateLimitMessage{
					Status:        agent.RateLimitStatusRejected,
					ResetsAt:      resetAt,
					QuotaProvider: agent.QuotaProviderClaudeCode,
					QuotaWindow:   "five_hour",
					Utilization:   1,
				}},
			}})
			if err != nil {
				t.Fatalf("AdoptInstances: %v", err)
			}
			if len(adopted) != 1 {
				t.Fatalf("adopted len = %d, want 1", len(adopted))
			}
			if adopted[0].RelPath != "caic-xyz/caic" {
				t.Errorf("adopted RelPath = %q, want caic-xyz/caic", adopted[0].RelPath)
			}
			if adopted[0].Branch != "caic-5" {
				t.Errorf("adopted Branch = %q, want caic-5", adopted[0].Branch)
			}
			if m.Len() != 1 {
				t.Errorf("manager Len = %d, want 1", m.Len())
			}
			quotas := m.QuotaTracker.Merge(nil, time.Now())
			if len(quotas) != 1 || len(quotas[0].RateLimits) != 1 {
				t.Fatalf("tracked quotas = %#v, want one restored rate limit", quotas)
			}
			if got := quotas[0].RateLimits[0]; got.UsedPct != 100 || !got.ResetsAt.Equal(resetAt) {
				t.Errorf("tracked rate limit = %#v, want 100%% used with reset %v", got, resetAt)
			}
		})
		t.Run("error_does_not_match_log_by_repo_only", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			otherTaskID := ksid.NewID()
			fake := &runtimetest.FakeInfo{Meta: map[string]string{
				"repo-only-match\x00caic.id":      taskID.String(),
				"repo-only-match\x00caic.harness": string(harness.Claude),
			}}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake), Backends: map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}})
			m.RegisterWorkspace("repo/a", &repowork.Workspace{Dir: "/home/user/src/repo/a", Log: logtest.Logger(t)})

			_, err := m.AdoptInstances(t.Context(), []AdoptRepo{{RelPath: "repo/a", AbsPath: "/home/user/src/repo/a"}}, []runtime.Instance{{
				ID: runtime.NewID("test-runtime", "repo-only-match"), State: "exited",
				Repos: []runtime.Repo{{GitRoot: "/home/user/src/repo/a", Branch: "caic-1", ContainerPath: "/home/user/src/repo/a"}},
			}}, []*task.LoadedTask{{
				TaskID:  otherTaskID.String(),
				Repos:   []task.RepoMount{{Name: "repo/a", Branch: "caic-1"}},
				Harness: harness.Claude,
			}})
			if err == nil || !strings.Contains(err.Error(), "task log "+taskID.String()+" not found") {
				t.Fatalf("AdoptInstances error = %v, want missing exact task log", err)
			}
		})
		t.Run("error_rejects_unknown_log_harness", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			fake := &runtimetest.FakeInfo{Meta: map[string]string{
				"unknown-harness\x00caic.id":      taskID.String(),
				"unknown-harness\x00caic.harness": "unknown",
			}}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake), Backends: map[harness.Name]agent.Backend{}})
			m.RegisterWorkspace("repo/a", &repowork.Workspace{Dir: "/home/user/src/repo/a", Log: logtest.Logger(t)})

			_, err := m.AdoptInstances(t.Context(), []AdoptRepo{{RelPath: "repo/a", AbsPath: "/home/user/src/repo/a"}}, []runtime.Instance{{
				ID: runtime.NewID("test-runtime", "unknown-harness"), State: "exited",
				Repos: []runtime.Repo{{GitRoot: "/home/user/src/repo/a", Branch: "caic-1", ContainerPath: "/home/user/src/repo/a"}},
			}}, []*task.LoadedTask{{
				TaskID:  taskID.String(),
				Repos:   []task.RepoMount{{Name: "repo/a", Branch: "caic-1"}},
				Harness: "unknown",
			}})
			if err == nil || !strings.Contains(err.Error(), `unknown harness "unknown"`) {
				t.Fatalf("AdoptInstances error = %v, want unknown-harness error", err)
			}
		})
		t.Run("valid_adopts_qualified_no_repo_instance", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			instanceID := runtime.NewID("test-runtime", "md-agent-no-repo")
			fake := &runtimetest.FakeInfo{Meta: map[string]string{
				"md-agent-no-repo\x00caic.id":      taskID.String(),
				"md-agent-no-repo\x00caic.harness": string(harness.Claude),
			}}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake), Backends: map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}})

			adopted, err := m.AdoptInstances(t.Context(), nil, []runtime.Instance{{ID: instanceID, State: "exited"}}, []*task.LoadedTask{{
				TaskID: taskID.String(), Harness: harness.Claude,
			}})
			if err != nil {
				t.Fatalf("AdoptInstances: %v", err)
			}
			if len(adopted) != 1 {
				t.Fatalf("adopted len = %d, want 1", len(adopted))
			}
			if adopted[0].Task.ID != taskID {
				t.Fatalf("adopted task ID = %s, want %s", adopted[0].Task.ID, taskID)
			}
			if adopted[0].RelPath != "" {
				t.Fatalf("adopted RelPath = %q, want no repo", adopted[0].RelPath)
			}
		})
		t.Run("valid_restores_launch_config_from_log", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			fake := &runtimetest.FakeInfo{Meta: map[string]string{
				"restore-config\x00caic.id":      taskID.String(),
				"restore-config\x00caic.harness": string(harness.Claude),
			}}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake), Backends: map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}})
			m.RegisterWorkspace("repo/a", &repowork.Workspace{Dir: "/home/user/src/repo/a", Log: logtest.Logger(t)})

			logDir := t.TempDir()
			meta, err := json.Marshal(agent.MetaMessage{
				MessageType:       "caic_meta",
				Version:           1,
				Prompt:            "restore config",
				Repos:             []agent.MetaRepo{{Name: "repo/a", Branch: "caic-9"}},
				Harness:           harness.Claude,
				BaseImage:         "ghcr.io/caic/base:v1",
				ContainerPlatform: "linux/amd64",
				MaxCPUs:           5,
				CacheMounts:       []agent.MetaCacheMount{{Name: "npm", HostPath: "~/.npm", ContainerPath: "/home/user/.npm", ReadOnly: true}},
				Mounts:            []agent.MetaMount{{HostPath: "/host/work", ContainerPath: "/workspace/work", ReadOnly: true}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(logDir, taskID.String()+"-repo-a-caic-9.jsonl"), append(meta, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			logs, err := task.LoadLogs(logDir)
			if err != nil {
				t.Fatal(err)
			}

			adopted, err := m.AdoptInstances(t.Context(), []AdoptRepo{{RelPath: "repo/a", AbsPath: "/home/user/src/repo/a"}}, []runtime.Instance{
				{ID: runtime.NewID("test-runtime", "restore-config"), State: "exited", Repos: []runtime.Repo{{GitRoot: "/home/user/src/repo/a", Branch: "caic-9", ContainerPath: "/home/user/src/repo/a"}}},
			}, logs)
			if err != nil {
				t.Fatalf("AdoptInstances: %v", err)
			}
			if len(adopted) != 1 {
				t.Fatalf("adopted len = %d, want 1", len(adopted))
			}
			got := adopted[0].Task
			if got.BaseImage != "ghcr.io/caic/base:v1" || got.ContainerPlatform != "linux/amd64" || got.MaxCPUs != 5 {
				t.Fatalf("launch config = image %q platform %q cpus %d", got.BaseImage, got.ContainerPlatform, got.MaxCPUs)
			}
			if len(got.CacheMounts) != 1 || got.CacheMounts[0].Name != "npm" || !got.CacheMounts[0].ReadOnly {
				t.Errorf("CacheMounts = %+v", got.CacheMounts)
			}
			if len(got.Mounts) != 1 || got.Mounts[0].HostPath != "/host/work" || !got.Mounts[0].ReadOnly {
				t.Errorf("Mounts = %+v", got.Mounts)
			}
		})
		t.Run("valid_merges_local_log_with_relay_tail", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			fake := &runtimetest.FakeInfo{Meta: map[string]string{
				"merge-tail\x00caic.id":      taskID.String(),
				"merge-tail\x00caic.harness": string(harness.Claude),
			}}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake), Backends: map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}})
			m.relay = fakeRelayReader{
				statusFn: func(context.Context, runtime.ConnectionTarget) (bool, string, error) {
					return true, "alive", nil
				},
				readTailFn: func(context.Context, runtime.ConnectionTarget, func([]byte) ([]agent.Message, error), int64) ([]agent.Message, int64, error) {
					return []agent.Message{&agent.TextMessage{Text: "during restart"}}, 128, nil
				},
				readLogFn: func(context.Context, runtime.ConnectionTarget, int) string { return "" },
			}
			m.RegisterWorkspace("caic-xyz/caic", &repowork.Workspace{Dir: "/home/user/src/caic-xyz/caic", Log: logtest.Logger(t)})

			logDir := t.TempDir()
			meta, err := json.Marshal(agent.MetaMessage{
				MessageType: "caic_meta",
				Version:     1,
				Prompt:      "merge history",
				Repos:       []agent.MetaRepo{{Name: "caic-xyz/caic", Branch: "caic-12"}},
				Harness:     harness.Claude,
			})
			if err != nil {
				t.Fatal(err)
			}
			diskMsg := `{"type":"assistant","message":{"model":"m","id":"msg_01","role":"assistant","content":[{"type":"text","text":"before restart"}],"usage":{}},"session_id":"s","uuid":"u1"}`
			if err := os.WriteFile(filepath.Join(logDir, taskID.String()+".jsonl"), []byte(string(meta)+"\n"+diskMsg+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			logs, err := task.LoadLogs(logDir)
			if err != nil {
				t.Fatal(err)
			}

			adopted, err := m.AdoptInstances(t.Context(), []AdoptRepo{
				{RelPath: "caic-xyz/caic", AbsPath: "/home/user/src/caic-xyz/caic"},
			}, []runtime.Instance{
				{
					ID:    runtime.NewID("test-runtime", "merge-tail"),
					State: "running",
					Repos: []runtime.Repo{{
						GitRoot:       "/home/user/src/caic-xyz/caic",
						Branch:        "caic-12",
						ContainerPath: "/home/user/src/caic-xyz/caic",
					}},
				},
			}, logs)
			if err != nil {
				t.Fatalf("AdoptInstances: %v", err)
			}
			if len(adopted) != 1 {
				t.Fatalf("adopted len = %d, want 1", len(adopted))
			}
			texts := textMessages(adopted[0].Task.Messages())
			if !slices.Contains(texts, "before restart") || !slices.Contains(texts, "during restart") {
				t.Fatalf("messages = %#v, want disk history plus relay tail", texts)
			}
		})
		t.Run("missing_local_log_refuses_live_reconnect", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			backend := &reconnectInputBackend{FakeBackend: &agenttest.FakeBackend{HarnessName: "reconnect", Images: true, ContextLimit: 200_000}}
			t.Cleanup(backend.stop)
			fake := &runtimetest.FakeInfo{Meta: map[string]string{
				"ask-tail\x00caic.id":      taskID.String(),
				"ask-tail\x00caic.harness": "reconnect",
			}}
			m := newTestManager(t, Config{
				ServerCtx: t.Context(),
				LogDir:    filepath.Join(t.TempDir(), "logs"),
				Runtimes:  newTestRuntime(t, &runtimetest.FakeBackend{}, fake),
				Backends:  map[harness.Name]agent.Backend{"reconnect": backend},
			})
			m.relay = fakeRelayReader{
				statusFn: func(context.Context, runtime.ConnectionTarget) (bool, string, error) {
					return true, "alive", nil
				},
				readTailFn: func(context.Context, runtime.ConnectionTarget, func([]byte) ([]agent.Message, error), int64) ([]agent.Message, int64, error) {
					return []agent.Message{
						&agent.AskMessage{
							ToolUseID: "toolu-1",
							Questions: []agent.AskQuestion{{Question: "Which?"}},
						},
						&agent.PendingUserActionMessage{
							MessageType: agent.PendingUserActionMessageType,
							Action: agent.PendingUserAction{
								Kind:      agent.PendingUserActionAskUserQuestion,
								RequestID: "req-1",
								ToolUseID: "toolu-1",
								Ask: agent.PendingAskAction{
									Questions: []agent.AskQuestion{{Question: "Which?"}},
								},
							},
						},
						&agent.ResultMessage{MessageType: "result"},
					}, 599440, nil
				},
				readLogFn: func(context.Context, runtime.ConnectionTarget, int) string { return "" },
			}
			m.RegisterWorkspace("repo/a", &repowork.Workspace{Dir: "/home/user/src/repo/a", Log: logtest.Logger(t)})

			adopted, err := m.AdoptInstances(t.Context(), []AdoptRepo{
				{RelPath: "repo/a", AbsPath: "/home/user/src/repo/a"},
			}, []runtime.Instance{
				{
					ID:          runtime.NewID("test-runtime", "ask-tail"),
					AgentTarget: runtime.ConnectionTarget{SSHHost: "ask-tail"},
					State:       "running",
					Repos: []runtime.Repo{{
						GitRoot:       "/home/user/src/repo/a",
						Branch:        "caic-10",
						ContainerPath: "/home/user/src/repo/a",
					}},
				},
			}, []*task.LoadedTask{{
				TaskID:  taskID.String(),
				Harness: "reconnect",
				Repos:   []task.RepoMount{{Name: "repo/a", Branch: "caic-10"}},
			}})
			if err != nil {
				t.Fatalf("AdoptInstances: %v", err)
			}
			if len(adopted) != 1 {
				t.Fatalf("adopted len = %d, want 1", len(adopted))
			}

			backend.mu.Lock()
			attachCalls := backend.attachCalls
			opts := backend.opts
			backend.mu.Unlock()
			if attachCalls != 0 {
				t.Fatalf("AttachRelay calls = %d, want 0", attachCalls)
			}
			if opts != nil {
				t.Fatalf("AttachRelay opts = %#v, want nil", opts)
			}
			if adopted[0].Task.HasSession() {
				t.Fatal("adopted task attached without authoritative local log")
			}
			if adopted[0].Task.LogPath() != "" {
				t.Fatalf("replacement LogPath = %q, want empty", adopted[0].Task.LogPath())
			}
		})
		t.Run("valid_dead_relay_exit_error_crashes_adopted_task", func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
			defer cancel()
			taskID := ksid.NewID()
			fake := &runtimetest.FakeInfo{Meta: map[string]string{
				"dead-relay\x00caic.id":      taskID.String(),
				"dead-relay\x00caic.harness": string(harness.Claude),
			}}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake), Backends: map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}})
			m.RegisterWorkspace("caic-xyz/caic", &repowork.Workspace{Dir: "/home/user/src/caic-xyz/caic", Log: logtest.Logger(t)})

			logDir := t.TempDir()
			meta, err := json.Marshal(agent.MetaMessage{
				MessageType: "caic_meta",
				Version:     1,
				Prompt:      "dead relay task",
				Repos:       []agent.MetaRepo{{Name: "caic-xyz/caic", Branch: "caic-7"}},
				Harness:     harness.Claude,
			})
			if err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(logDir, taskID.String()+".jsonl")
			body := string(meta) + "\n" + `{"type":"caic_exit","exit_code":2,"error":"Unknown option: --approve"}` + "\n"
			if err := os.WriteFile(logPath, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			logs, err := task.LoadLogs(logDir)
			if err != nil {
				t.Fatal(err)
			}

			adopted, err := m.AdoptInstances(ctx, []AdoptRepo{
				{RelPath: "caic-xyz/caic", AbsPath: "/home/user/src/caic-xyz/caic"},
			}, []runtime.Instance{
				{
					ID:    runtime.NewID("test-runtime", "dead-relay"),
					State: "running",
					Repos: []runtime.Repo{{
						GitRoot:       "/home/user/src/caic-xyz/caic",
						Branch:        "caic-7",
						ContainerPath: "/home/user/src/caic-xyz/caic",
					}},
				},
			}, logs)
			if err != nil {
				t.Fatalf("AdoptInstances: %v", err)
			}
			if len(adopted) != 1 {
				t.Fatalf("adopted len = %d, want 1", len(adopted))
			}
			if got := adopted[0].Task.GetState(); got != task.StateCrashed {
				t.Errorf("state = %v, want crashed", got)
			}
			if adopted[0].Entry.Result() == nil {
				t.Fatal("entry result is nil")
			}
			if err := adopted[0].Entry.Result().Err; err == nil || !strings.Contains(err.Error(), "Unknown option: --approve") {
				t.Fatalf("result err = %v, want relay stderr", err)
			}
			persisted, err := os.ReadFile(logPath) //nolint:gosec // test-controlled temp path
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(persisted), `"type":"caic_result"`) || !strings.Contains(string(persisted), `"state":"crashed"`) {
				t.Fatalf("log missing crashed caic_result trailer:\n%s", persisted)
			}
		})
		t.Run("valid_dead_relay_tail_exit_error_crashes_adopted_task", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			fake := &runtimetest.FakeInfo{Meta: map[string]string{
				"dead-relay-tail\x00caic.id":      taskID.String(),
				"dead-relay-tail\x00caic.harness": string(harness.Claude),
			}}
			runtimeBackend := &runtimetest.FakeBackend{}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, runtimeBackend, fake), Backends: map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}})
			m.relay = fakeRelayReader{
				statusFn: func(context.Context, runtime.ConnectionTarget) (bool, string, error) {
					return false, "dead", nil
				},
				readTailFn: func(context.Context, runtime.ConnectionTarget, func([]byte) ([]agent.Message, error), int64) ([]agent.Message, int64, error) {
					return []agent.Message{&agent.ExitMessage{ExitCode: 2, Error: "Unknown option: --approve"}}, 128, nil
				},
				readLogFn: func(context.Context, runtime.ConnectionTarget, int) string { return "relay exited" },
			}
			m.RegisterWorkspace("caic-xyz/caic", &repowork.Workspace{Dir: "/home/user/src/caic-xyz/caic", Log: logtest.Logger(t)})

			adopted, err := m.AdoptInstances(t.Context(), []AdoptRepo{
				{RelPath: "caic-xyz/caic", AbsPath: "/home/user/src/caic-xyz/caic"},
			}, []runtime.Instance{
				{
					ID:    runtime.NewID("test-runtime", "dead-relay-tail"),
					State: "running",
					Repos: []runtime.Repo{{
						GitRoot:       "/home/user/src/caic-xyz/caic",
						Branch:        "caic-8",
						ContainerPath: "/home/user/src/caic-xyz/caic",
					}},
				},
			}, []*task.LoadedTask{{
				TaskID:  taskID.String(),
				Harness: harness.Claude,
				Repos:   []task.RepoMount{{Name: "caic-xyz/caic", Branch: "caic-8"}},
			}})
			if err != nil {
				t.Fatalf("AdoptInstances: %v", err)
			}
			if len(adopted) != 1 {
				t.Fatalf("adopted len = %d, want 1", len(adopted))
			}
			if got := adopted[0].Task.GetState(); got != task.StateCrashed {
				t.Fatalf("state = %v, want crashed", got)
			}
			if err := adopted[0].Entry.Result().Err; err == nil || !strings.Contains(err.Error(), "Unknown option: --approve") {
				t.Fatalf("result err = %v, want relay stderr", err)
			}
		})
		t.Run("valid_stale_tail_exit_error_does_not_crash_adopted_task", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			fake := &runtimetest.FakeInfo{Meta: map[string]string{
				"stale-tail\x00caic.id":      taskID.String(),
				"stale-tail\x00caic.harness": string(harness.Claude),
			}}
			runtimeBackend := &runtimetest.FakeBackend{}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, runtimeBackend, fake), Backends: map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}})
			m.relay = fakeRelayReader{
				statusFn: func(context.Context, runtime.ConnectionTarget) (bool, string, error) {
					return false, "dead", nil
				},
				readTailFn: func(context.Context, runtime.ConnectionTarget, func([]byte) ([]agent.Message, error), int64) ([]agent.Message, int64, error) {
					return []agent.Message{
						&agent.InitMessage{SessionID: "new-session"},
						&agent.ResultMessage{MessageType: "result", Subtype: "success", Result: "done"},
						&agent.ExitMessage{ExitCode: 2, Error: "stale crash"},
					}, 256, nil
				},
				readLogFn: func(context.Context, runtime.ConnectionTarget, int) string { return "relay exited" },
			}
			m.RegisterWorkspace("caic-xyz/caic", &repowork.Workspace{Dir: "/home/user/src/caic-xyz/caic", Log: logtest.Logger(t)})

			adopted, err := m.AdoptInstances(t.Context(), []AdoptRepo{
				{RelPath: "caic-xyz/caic", AbsPath: "/home/user/src/caic-xyz/caic"},
			}, []runtime.Instance{
				{
					ID:    runtime.NewID("test-runtime", "stale-tail"),
					State: "running",
					Repos: []runtime.Repo{{
						GitRoot:       "/home/user/src/caic-xyz/caic",
						Branch:        "caic-9",
						ContainerPath: "/home/user/src/caic-xyz/caic",
					}},
				},
			}, []*task.LoadedTask{{
				TaskID:  taskID.String(),
				Harness: harness.Claude,
				Repos:   []task.RepoMount{{Name: "caic-xyz/caic", Branch: "caic-9"}},
			}})
			if err != nil {
				t.Fatalf("AdoptInstances: %v", err)
			}
			if len(adopted) != 1 {
				t.Fatalf("adopted len = %d, want 1", len(adopted))
			}
			if got := adopted[0].Task.GetState(); got != task.StateStopped {
				t.Fatalf("state = %v, want stopped", got)
			}
			if got := adopted[0].Task.LastExitError(); got != "" {
				t.Fatalf("LastExitError = %q, want stale error cleared", got)
			}
			if adopted[0].Entry.Result() != nil {
				t.Fatalf("entry result = %#v, want nil", adopted[0].Entry.Result())
			}
		})
		t.Run("valid_stale_crashed_trailer_does_not_crash_adopted_task", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			fake := &runtimetest.FakeInfo{Meta: map[string]string{
				"stale-trailer\x00caic.id":      taskID.String(),
				"stale-trailer\x00caic.harness": string(harness.Claude),
			}}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake), Backends: map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}})
			m.RegisterWorkspace("caic-xyz/caic", &repowork.Workspace{Dir: "/home/user/src/caic-xyz/caic", Log: logtest.Logger(t)})

			logDir := t.TempDir()
			meta, err := json.Marshal(agent.MetaMessage{
				MessageType: "caic_meta",
				Version:     1,
				Prompt:      "clean task with stale crash trailer",
				Repos:       []agent.MetaRepo{{Name: "caic-xyz/caic", Branch: "caic-10"}},
				Harness:     harness.Claude,
			})
			if err != nil {
				t.Fatal(err)
			}
			result := `{"type":"result","subtype":"success","is_error":false,"duration_ms":1,"num_turns":1,"result":"done"}`
			staleExit := `{"type":"caic_exit","exit_code":2,"error":"stale crash"}`
			staleTrailer := `{"type":"caic_result","state":"crashed","error":"agent session crashed"}`
			logs := string(meta) + "\n" + result + "\n" + staleExit + "\n" + staleTrailer + "\n"
			if err := os.WriteFile(filepath.Join(logDir, taskID.String()+".jsonl"), []byte(logs), 0o600); err != nil {
				t.Fatal(err)
			}
			loaded, err := task.LoadLogs(logDir)
			if err != nil {
				t.Fatal(err)
			}

			adopted, err := m.AdoptInstances(t.Context(), []AdoptRepo{
				{RelPath: "caic-xyz/caic", AbsPath: "/home/user/src/caic-xyz/caic"},
			}, []runtime.Instance{
				{
					ID:    runtime.NewID("test-runtime", "stale-trailer"),
					State: "exited",
					Repos: []runtime.Repo{{
						GitRoot:       "/home/user/src/caic-xyz/caic",
						Branch:        "caic-10",
						ContainerPath: "/home/user/src/caic-xyz/caic",
					}},
				},
			}, loaded)
			if err != nil {
				t.Fatalf("AdoptInstances: %v", err)
			}
			if len(adopted) != 1 {
				t.Fatalf("adopted len = %d, want 1", len(adopted))
			}
			if got := adopted[0].Task.GetState(); got != task.StateStopped {
				t.Fatalf("state = %v, want stopped", got)
			}
			if got := adopted[0].Task.LastExitError(); got != "" {
				t.Fatalf("LastExitError = %q, want stale error cleared", got)
			}
		})
		t.Run("valid_loads_legacy_codex_session_metadata", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			fake := &runtimetest.FakeInfo{Meta: map[string]string{
				"md-caic-caic-6\x00caic.id":      taskID.String(),
				"md-caic-caic-6\x00caic.harness": string(harness.Codex),
			}}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake), Backends: map[harness.Name]agent.Backend{harness.Codex: codex.New("", nil)}})
			m.RegisterWorkspace("caic-xyz/caic", &repowork.Workspace{Dir: "/home/user/src/caic-xyz/caic", Log: logtest.Logger(t)})

			logDir := t.TempDir()
			meta, err := json.Marshal(agent.MetaMessage{
				MessageType: "caic_meta",
				Version:     1,
				Prompt:      "legacy codex task",
				Repos:       []agent.MetaRepo{{Name: "caic-xyz/caic", Branch: "caic-6"}},
				Harness:     harness.Codex,
			})
			if err != nil {
				t.Fatal(err)
			}
			init := `{"method":"thread/started","params":{"thread":{"id":"thread-from-started","cliVersion":"1.0","createdAt":1,"cwd":"/repo","modelProvider":"openai","path":"/repo","preview":"","source":"user","status":{"type":"idle"},"updatedAt":2}}}`
			if err := os.WriteFile(filepath.Join(logDir, taskID.String()+".jsonl"), []byte(string(meta)+"\n"+init+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			logs, err := task.LoadLogs(logDir)
			if err != nil {
				t.Fatal(err)
			}

			adopted, err := m.AdoptInstances(t.Context(), []AdoptRepo{
				{RelPath: "caic-xyz/caic", AbsPath: "/home/user/src/caic-xyz/caic"},
			}, []runtime.Instance{
				{
					ID:    runtime.NewID("test-runtime", "md-caic-caic-6"),
					State: "exited",
					Repos: []runtime.Repo{
						{GitRoot: "/home/user/src/caic-xyz/caic", Branch: "caic-6", ContainerPath: "/home/user/src/caic-xyz/caic"},
					},
				},
			}, logs)
			if err != nil {
				t.Fatalf("AdoptInstances: %v", err)
			}
			if len(adopted) != 1 {
				t.Fatalf("adopted len = %d, want 1", len(adopted))
			}
			if got := adopted[0].Task.GetSessionID(); got != "thread-from-started" {
				t.Errorf("SessionID = %q, want thread-from-started", got)
			}
		})
	})

	t.Run("watchStats", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		started := make(chan []runtime.ID, 1)
		fake := &runtimetest.FakeInfo{
			WatchStarted: started,
			Stats:        []runtime.StatsSample{{InstanceID: "ctr-1", Stats: runtime.Stats{CPUPerc: 2.5, MemUsed: 200, DiskUsed: -1}}},
		}
		m := newTestManager(t, Config{ServerCtx: ctx, Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake)})
		tk := &task.Task{
			ID:            ksid.NewID(),
			InitialPrompt: agent.Prompt{Text: "x"},
		}
		tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
		tk.SetState(task.StateRunning)
		m.Insert(tk.ID.String(), NewEntry(tk, nil))

		go m.watchStats(ctx)
		select {
		case ids := <-started:
			if !slices.Equal(ids, []runtime.ID{"test-runtime:ctr-1"}) {
				t.Fatalf("watch ids = %v, want [ctr-1]", ids)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for stats stream")
		}

		deadline := time.After(2 * time.Second)
		ticker := time.NewTicker(10 * time.Millisecond)
		t.Cleanup(ticker.Stop)
		for {
			history, _, unsub := tk.SubscribeStats(t.Context())
			unsub()
			if len(history) > 0 {
				if history[0].CPUPerc != 2.5 || history[0].MemUsed != 200 || history[0].DiskUsed != -1 {
					t.Fatalf("streamed stats = %+v", history[0])
				}
				return
			}
			select {
			case <-deadline:
				t.Fatal("timed out waiting for stats push")
			case <-ticker.C:
			}
		}
	})

	t.Run("watchRuntimeEvents", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_dispatches_death", func(t *testing.T) {
			t.Parallel()
			events := make(chan runtime.Event, 1)
			fake := &runtimetest.FakeInfo{Events: events}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake)})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
				Repos:         []task.RepoMount{{Name: "repo/x", Branch: "caic-1"}},
			}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-dead"), runtime.ConnectionTarget{SSHHost: "ctr-dead"}, "", "", 0)
			tk.SetState(task.StateRunning)
			m.Insert(tk.ID.String(), NewEntry(tk, nil))

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			m.watchRuntimeEvents(ctx)

			events <- runtime.Event{InstanceID: "ctr-dead"}

			// The watcher dispatches on its own goroutine; wait for the state
			// transition rather than sleeping a fixed duration.
			deadline := time.Now().Add(2 * time.Second)
			for tk.GetState() != task.StateStopped {
				if time.Now().After(deadline) {
					t.Fatalf("state = %v, want StateStopped after instance death", tk.GetState())
				}
				time.Sleep(time.Millisecond)
			}
		})
	})
}

func TestAllocateBranches(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Config{ServerCtx: t.Context()})
	// Fake dirs: Init's branch scan fails and is ignored, leaving nextID at 0, so
	// the first ReserveBranchName on each repo yields caic-0.
	m.RegisterWorkspace("acme/app", &repowork.Workspace{Dir: "/tmp/app", Log: logtest.Logger(t)})
	m.RegisterWorkspace("caic-xyz/caic", &repowork.Workspace{Dir: "/tmp/caic", Log: logtest.Logger(t)})

	// Forking a 2-repo task carries both repos on their source branches. Every
	// repo — primary and non-primary alike — is reallocated to its own fresh
	// branch (from its own workspace), so the fork never shares a branch with the
	// still-checked-out source instance. Index 0 is not special.
	mounts := []task.RepoMount{
		{Name: "acme/app", Branch: "caic-2"},
		{Name: "caic-xyz/caic", Branch: "caic-18"},
	}
	tk := &task.Task{Repos: slices.Clone(mounts)}
	if err := m.allocateBranches(t.Context(), tk, mounts, len(mounts)); err != nil {
		t.Fatal(err)
	}
	for i, r := range tk.ReposSnapshot() {
		if r.Branch == mounts[i].Branch {
			t.Errorf("repo %d (%s) not reallocated, still on source branch %q", i, r.Name, r.Branch)
		}
		if r.Branch != "caic-0" {
			t.Errorf("repo %d branch = %q, want caic-0", i, r.Branch)
		}
	}

	// A repo with no registered workspace is a hard error.
	bad := &task.Task{Repos: []task.RepoMount{{Name: "ghost/repo", Branch: "caic-1"}}}
	if err := m.allocateBranches(t.Context(), bad, bad.ReposSnapshot(), 1); err == nil {
		t.Fatal("allocateForkBranches error = nil, want error for unregistered repo")
	}
}

func TestCountResultMessages(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		msgs := []agent.Message{
			&agent.TextMessage{Text: "hello"},
			&agent.ResultMessage{MessageType: "result"},
			&agent.TextMessage{Text: "world"},
			&agent.ResultMessage{MessageType: "result"},
		}
		if n := countResultMessages(msgs); n != 2 {
			t.Errorf("countResultMessages = %d, want 2", n)
		}
	})
	t.Run("valid_empty", func(t *testing.T) {
		t.Parallel()
		if n := countResultMessages(nil); n != 0 {
			t.Errorf("countResultMessages(nil) = %d, want 0", n)
		}
	})
}

func TestLastResultText(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}}
		tk.RestoreMessages([]agent.Message{
			&agent.ResultMessage{MessageType: "result", Result: "first"},
			&agent.ResultMessage{MessageType: "result", Result: "last"},
		})
		if got := lastResultText(tk); got != "last" {
			t.Errorf("lastResultText = %q, want %q", got, "last")
		}
	})
	t.Run("valid_no_result", func(t *testing.T) {
		t.Parallel()
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}}
		if got := lastResultText(tk); got != "" {
			t.Errorf("lastResultText = %q, want empty", got)
		}
	})
}

func TestNeedsTitleRegen(t *testing.T) {
	t.Parallel()
	resolver := func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
		return nil, errors.New("unexpected load")
	}
	t.Run("valid_no_log", func(t *testing.T) {
		t.Parallel()
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}}
		if !needsTitleRegen(tk, nil, resolver) {
			t.Error("needsTitleRegen should return true when lt is nil")
		}
	})
	t.Run("valid_no_title_in_log", func(t *testing.T) {
		t.Parallel()
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}}
		lt := &task.LoadedTask{TaskID: "test", Prompt: "test"}
		if !needsTitleRegen(tk, lt, resolver) {
			t.Error("needsTitleRegen should return true when lt.Title is empty")
		}
	})
	t.Run("valid_more_results_in_memory", func(t *testing.T) {
		t.Parallel()
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}}
		tk.RestoreMessages([]agent.Message{
			&agent.ResultMessage{MessageType: "result"},
			&agent.ResultMessage{MessageType: "result"},
		})
		lt := &task.LoadedTask{
			TaskID: "test",
			Title:  "existing title",
			Msgs: []agent.Message{
				&agent.ResultMessage{MessageType: "result"},
			},
		}
		if !needsTitleRegen(tk, lt, resolver) {
			t.Error("needsTitleRegen should return true when restoredResults > logResults")
		}
	})
	t.Run("valid_same_count", func(t *testing.T) {
		t.Parallel()
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}}
		tk.RestoreMessages([]agent.Message{
			&agent.ResultMessage{MessageType: "result"},
		})
		lt := &task.LoadedTask{
			TaskID: "test",
			Title:  "existing title",
			Msgs: []agent.Message{
				&agent.ResultMessage{MessageType: "result"},
			},
		}
		if needsTitleRegen(tk, lt, resolver) {
			t.Error("needsTitleRegen should return false when counts match")
		}
	})
	t.Run("valid_large_log_skips", func(t *testing.T) {
		t.Parallel()
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}}
		lt := &task.LoadedTask{
			TaskID:  "test",
			Title:   "existing title",
			LogSize: 200 << 20, // 200 MiB, above maxLogSize
		}
		if needsTitleRegen(tk, lt, resolver) {
			t.Error("needsTitleRegen should return false for large logs")
		}
	})
}

func TestRefreshAdoptedDiffStat(t *testing.T) {
	t.Parallel()
	t.Run("valid_waiting_fetches_branch_diff", func(t *testing.T) {
		t.Parallel()
		fake := &runtimetest.FakeBackend{DiffOutput: "5\t1\tmain.go\n"}
		workspace := &repowork.Workspace{
			Dir:        "/repo",
			RepoName:   "repo",
			GitTimeout: time.Minute,
			Runtimes:   newTestRuntime(t, fake, nil),
			Log:        logtest.Logger(t),
		}
		tk := &task.Task{Repos: []task.RepoMount{{GitRoot: "/repo", Branch: "caic-0"}}}
		tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
		tk.SetState(task.StateWaiting)

		refreshAdoptedDiffStat(t.Context(), workspace, tk)

		// A populated DiffStat is the observable proof the fetch-then-diff path ran.
		ds := tk.Snapshot().DiffStat
		if len(ds) != 1 || ds[0].Path != "main.go" || ds[0].Added != 5 || ds[0].Deleted != 1 {
			t.Errorf("DiffStat = %+v, want [{main.go 5 1}]", ds)
		}
	})
	t.Run("valid_running_skips_branch_diff", func(t *testing.T) {
		t.Parallel()
		fake := &runtimetest.FakeBackend{DiffOutput: "5\t1\tmain.go\n"}
		workspace := &repowork.Workspace{
			Dir:        "/repo",
			RepoName:   "repo",
			GitTimeout: time.Minute,
			Runtimes:   newTestRuntime(t, fake, nil),
			Log:        logtest.Logger(t),
		}
		tk := &task.Task{Repos: []task.RepoMount{{GitRoot: "/repo", Branch: "caic-0"}}}
		tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
		tk.SetState(task.StateRunning)

		refreshAdoptedDiffStat(t.Context(), workspace, tk)

		// An empty DiffStat is the observable proof the diff path was skipped.
		if ds := tk.Snapshot().DiffStat; len(ds) != 0 {
			t.Errorf("DiffStat = %+v, want empty", ds)
		}
	})
}

func TestErrTaskNotFound(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		if errTaskNotFound.Error() != "task not found" {
			t.Errorf("errTaskNotFound = %q, want %q", errTaskNotFound, "task not found")
		}
	})
	t.Run("kind", func(t *testing.T) {
		t.Parallel()
		te, ok := errors.AsType[*Error](error(errTaskNotFound))
		if !ok {
			t.Fatal("errTaskNotFound is not a *Error")
		}
		if te.Kind != KindNotFound {
			t.Errorf("Kind = %v, want KindNotFound", te.Kind)
		}
	})
}

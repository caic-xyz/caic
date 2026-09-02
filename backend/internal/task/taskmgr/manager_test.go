// Tests task manager registry and lifecycle behavior.

package taskmgr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/mdruntime"
	"github.com/caic-xyz/caic/backend/internal/runtime/runtimetest"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

type testRuntimeInfo interface {
	runtime.Monitor
	runtime.Inventory
	runtime.PrivilegeInfo
}

func registerCheckout(t testing.TB, registry *repo.Registry, relPath string, checkout *repo.Checkout) {
	checkout.RelPath = relPath
	if err := registry.RegisterCheckout(checkout); err != nil {
		t.Fatal(err)
	}
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
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.LogStore == nil {
		cfg.LogStore = taskslog.NewStore(testLogger(), t.TempDir())
	}
	if cfg.Runtimes == nil {
		cfg.Runtimes = newTestRuntime(t, &runtimetest.FakeBackend{}, nil)
	}
	if cfg.Checkouts == nil {
		cfg.Checkouts = repo.NewRegistry()
	}
	if cfg.RuntimeStartTimeout == 0 {
		cfg.RuntimeStartTimeout = time.Hour
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func awaitTaskCleanup(t *testing.T, m *Manager, id string) {
	t.Cleanup(func() {
		e, ok := m.GetEntry(id)
		if !ok {
			t.Errorf("task entry %q not found", id)
			return
		}
		select {
		case <-e.Done():
		case <-time.After(time.Second):
			t.Errorf("task %q did not finish", id)
		}
	})
}

func newTestRuntime(t testing.TB, lc runtime.Lifecycle, info testRuntimeInfo) *runtime.Router {
	var sys runtime.System = &testRuntimeSystem{Lifecycle: lc}
	if info != nil {
		sys = testRuntimeInfoSystem{Lifecycle: lc, Monitor: info, Inventory: info, PrivilegeInfo: info}
	}
	router, err := runtime.NewRouter(slog.New(slog.DiscardHandler), []runtime.System{sys})
	if err != nil {
		t.Fatalf("runtime.NewRouter: %v", err)
	}
	return router
}

type fakeRelayReader struct {
	statusFn   func(context.Context, runtime.ConnectionTarget) (bool, string, error)
	readTailFn func(context.Context, runtime.ConnectionTarget, *agent.LogRecordParser, int64) ([]agent.TimedMessage, int64, error)
	readLogFn  func(context.Context, runtime.ConnectionTarget, int) string
}

func relayParsed(msgs ...agent.Message) []agent.TimedMessage {
	parsed := make([]agent.TimedMessage, len(msgs))
	for i, msg := range msgs {
		parsed[i] = agent.TimedMessage{Message: msg}
	}
	return parsed
}

func (f fakeRelayReader) Status(ctx context.Context, target runtime.ConnectionTarget) (alive bool, diag string, err error) {
	return f.statusFn(ctx, target)
}

func (f fakeRelayReader) ReadTail(ctx context.Context, target runtime.ConnectionTarget, parser *agent.LogRecordParser, maxBytes int64) (msgs []agent.TimedMessage, size int64, err error) {
	return f.readTailFn(ctx, target, parser, maxBytes)
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

// blockedStopBackend ignores cancellation until the test releases it.
type blockedStopBackend struct {
	*runtimetest.FakeBackend

	started chan struct{}
	release chan struct{}
}

func (b *blockedStopBackend) Stop(context.Context, runtime.ID) error {
	close(b.started)
	<-b.release
	return nil
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
	session := agent.NewSession(ctx, cmd, c, stdout, opts.MsgCh, opts.Logger)
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

func (*reconnectInputConn) ReadMessages(r io.Reader, _ chan<- agent.TimedMessage) error {
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

func mergeLogAndRelayMessages(h harness.Name, logMessages, relayMessages []agent.Message) []agent.Message {
	logEntries := make([]agent.TimedMessage, len(logMessages))
	for i, message := range logMessages {
		logEntries[i].Message = message
	}
	relayEntries := make([]agent.TimedMessage, len(relayMessages))
	for i, message := range relayMessages {
		relayEntries[i].Message = message
	}
	merged := newLogRelayMessageMerger(logEntries, h).merge(relayEntries)
	messages := make([]agent.Message, len(merged))
	for i, entry := range merged {
		messages[i] = entry.Message
	}
	return messages
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

func BenchmarkMergeLogAndRelayTimeline(b *testing.B) {
	const logCount = 4_000
	const overlap = 1_000
	logEntries := make([]agent.TimedMessage, logCount)
	for i := range logEntries {
		logEntries[i] = agent.TimedMessage{
			Message:      &agent.TextMessage{Text: fmt.Sprintf("message-%d", i)},
			ProducerTime: time.UnixMilli(int64(i + 1)),
		}
	}
	relayEntries := make([]agent.TimedMessage, overlap+1)
	for i := range overlap {
		logEntry := logEntries[logCount-overlap+i]
		relayEntries[i] = agent.TimedMessage{
			Message:      &agent.TextMessage{Text: fmt.Sprintf("message-%d", logCount-overlap+i)},
			ProducerTime: logEntry.ProducerTime,
		}
	}
	relayEntries[overlap] = agent.TimedMessage{
		Message:      &agent.TextMessage{Text: "new-message"},
		ProducerTime: time.UnixMilli(logCount + 1),
	}
	b.ReportAllocs()

	for b.Loop() {
		if got := newLogRelayMessageMerger(logEntries, harness.Codex).merge(relayEntries); len(got) != logCount+1 {
			b.Fatalf("merged %d entries, want %d", len(got), logCount+1)
		}
	}
}

func TestMergeLogAndRelayMessages(t *testing.T) {
	t.Parallel()
	t.Run("valid_exact_overlap", func(t *testing.T) {
		t.Parallel()
		merged := mergeLogAndRelayMessages(
			harness.Codex,
			[]agent.Message{&agent.TextMessage{Text: "before"}, &agent.TextMessage{Text: "overlap"}},
			[]agent.Message{&agent.TextMessage{Text: "overlap"}, &agent.TextMessage{Text: "after"}},
		)
		texts := textMessages(merged)
		want := []string{"before", "overlap", "after"}
		if !slices.Equal(texts, want) {
			t.Fatalf("merged texts = %#v, want %#v", texts, want)
		}
	})
	t.Run("valid_codex_overlap_with_init", func(t *testing.T) {
		t.Parallel()
		startupA := &agent.RawMessage{MessageType: "startup_a", Raw: []byte(`{"startup":"a"}`)}
		startupB := &agent.RawMessage{MessageType: "startup_b", Raw: []byte(`{"startup":"b"}`)}
		init := &agent.InitMessage{SessionID: "session", Model: "gpt-5.6-sol"}
		prompt := &agent.UserInputMessage{Text: "repair the pull request"}
		thinking := &agent.ThinkingMessage{Text: "Inspecting repository"}
		status := &agent.TextMessage{Text: "The pull request contains a binary."}
		after := &agent.ToolUseMessage{ToolUseID: "tool", Name: "Bash"}
		merged := mergeLogAndRelayMessages(
			harness.Codex,
			[]agent.Message{
				&agent.LogMessage{Line: "starting container"},
				startupA,
				startupB,
				init,
				prompt,
				thinking,
				status,
			},
			[]agent.Message{startupA, startupB, init, prompt, thinking, status, after},
		)

		if len(merged) != 8 {
			t.Fatalf("merged %d messages, want 8: %#v", len(merged), merged)
		}
		if merged[7] != after {
			t.Fatalf("last merged message = %#v, want %#v", merged[7], after)
		}
	})
	t.Run("valid_overlap_preserves_aligned_times", func(t *testing.T) {
		t.Parallel()
		before := &agent.TextMessage{Text: "before"}
		overlap := &agent.TextMessage{Text: "overlap"}
		after := &agent.TextMessage{Text: "after"}
		relayOverlapAt := time.UnixMilli(2_000)
		relayAfterAt := time.UnixMilli(3_000)

		entries := newLogRelayMessageMerger(
			[]agent.TimedMessage{
				{Message: before, ProducerTime: time.UnixMilli(1_000)},
				{Message: overlap, ProducerTime: time.UnixMilli(2_000)},
			},
			harness.Codex,
		).merge(
			[]agent.TimedMessage{
				{Message: &agent.TextMessage{Text: "overlap"}, ProducerTime: relayOverlapAt},
				{Message: after, ProducerTime: relayAfterAt},
			},
		)

		messages := make([]agent.Message, len(entries))
		for i, entry := range entries {
			messages[i] = entry.Message
		}
		if got := textMessages(messages); !slices.Equal(got, []string{"before", "overlap", "after"}) {
			t.Fatalf("merged texts = %#v", got)
		}
		for i, want := range []int64{1_000, 2_000, 3_000} {
			if got := entries[i].ProducerTime.UnixMilli(); got != want {
				t.Fatalf("entry %d time = %d, want %d", i, got, want)
			}
		}
	})
	t.Run("valid_usage_context_window_mismatch", func(t *testing.T) {
		t.Parallel()
		merged := mergeLogAndRelayMessages(
			harness.Codex,
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
			harness.Codex,
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
			harness.Codex,
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
			harness.Pi,
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
	t.Run("requires construction dependencies", func(t *testing.T) {
		t.Parallel()
		runtimes := newTestRuntime(t, &runtimetest.FakeBackend{}, nil)
		cacheDir := t.TempDir()
		for _, tc := range []struct {
			name string
			cfg  Config
			want string
		}{
			{name: "server context", want: "task manager server context is required"},
			{name: "task log store", cfg: Config{ServerCtx: t.Context()}, want: "task manager task log store is required"},
			{name: "runtime router", cfg: Config{ServerCtx: t.Context(), LogStore: taskslog.NewStore(testLogger(), cacheDir)}, want: "task manager runtime router is required"},
			{name: "checkout registry", cfg: Config{ServerCtx: t.Context(), LogStore: taskslog.NewStore(testLogger(), cacheDir), Runtimes: runtimes}, want: "task manager checkout registry is required"},
			{name: "runtime start timeout", cfg: Config{ServerCtx: t.Context(), LogStore: taskslog.NewStore(testLogger(), cacheDir), Runtimes: runtimes, Checkouts: repo.NewRegistry()}, want: "task manager runtime start timeout is required"},
			{name: "logger", cfg: Config{ServerCtx: t.Context(), LogStore: taskslog.NewStore(testLogger(), cacheDir), Runtimes: runtimes, Checkouts: repo.NewRegistry(), RuntimeStartTimeout: time.Hour}, want: "task manager logger is required"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				if _, err := New(tc.cfg); err == nil || err.Error() != tc.want {
					t.Fatalf("New() error = %v, want %q", err, tc.want)
				}
			})
		}
	})
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
		if _, ok := m.Checkouts.Checkout(""); ok {
			t.Fatal("registry contains no-repo checkout")
		}
		select {
		case <-m.Changed():
			t.Error("Changed() should not be closed initially")
		default:
		}
	})
	t.Run("no-repo checkout is fully constructed", func(t *testing.T) {
		t.Parallel()
		backend := &mdruntime.Backend{}
		router, err := runtime.NewRouter(slog.New(slog.DiscardHandler), []runtime.System{&testRuntimeSystem{Lifecycle: backend}})
		if err != nil {
			t.Fatal(err)
		}
		cacheDir := t.TempDir()
		cfg := Config{
			ServerCtx:           t.Context(),
			Log:                 slog.New(slog.DiscardHandler),
			LogStore:            taskslog.NewStore(testLogger(), filepath.Join(cacheDir, "tasks")),
			Runtimes:            router,
			Backends:            map[harness.Name]agent.Backend{"fake": &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}},
			HarnessEnv:          map[string][]string{string(harness.Codex): {"CODEX_HOME=/tmp/codex"}},
			Checkouts:           repo.NewRegistry(),
			RuntimeStartTimeout: time.Hour,
		}
		m, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if m.logStore != cfg.LogStore {
			t.Fatal("manager does not share the configured task log store")
		}
		if len(m.harnessEnv[string(harness.Codex)]) != 1 || m.harnessEnv[string(harness.Codex)][0] != "CODEX_HOME=/tmp/codex" {
			t.Fatalf("HarnessEnv = %#v, want configured codex env", m.harnessEnv)
		}
		if len(m.Backends) == 0 {
			t.Fatal("manager backends were not initialized")
		}
	})
}

func TestManager(t *testing.T) {
	t.Parallel()

	t.Run("RegisterCheckout", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			r := &repo.Checkout{Dir: "/tmp/test"}
			registerCheckout(t, m.Checkouts, "my/repo", r)
			got, ok := m.Checkouts.Checkout("my/repo")
			if !ok || got != r {
				t.Fatal("Checkout() did not return registered checkout")
			}
		})
	})

	t.Run("Close", func(t *testing.T) {
		t.Parallel()
		m := newTestManager(t, Config{ServerCtx: t.Context()})
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
		tk.SeedTimeline([]agent.Message{&agent.RateLimitMessage{
			Status:        agent.RateLimitStatusRejected,
			QuotaProvider: agent.QuotaProviderClaudeCode,
			QuotaWindow:   "five_hour",
			Utilization:   1,
		}})
		m.Insert(tk.ID.String(), m.NewEntry(tk, nil))
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

	t.Run("Close waits for lifecycle", func(t *testing.T) {
		t.Parallel()
		backend := &blockedStopBackend{
			FakeBackend: &runtimetest.FakeBackend{},
			started:     make(chan struct{}),
			release:     make(chan struct{}, 1),
		}
		m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, backend, nil)})
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
		tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
		tk.SetState(taskslog.StateRunning)
		entry := m.NewEntry(tk, nil)
		m.Insert(tk.ID.String(), entry)
		if err := entry.Lifecycle.Stop(t.Context()); err != nil {
			t.Fatalf("Stop() = %v", err)
		}
		select {
		case <-backend.started:
		case <-time.After(time.Second):
			t.Fatal("Stop did not reach the runtime backend")
		}

		closed := make(chan error, 1)
		go func() { closed <- m.Close() }()
		select {
		case err := <-closed:
			t.Fatalf("Close() returned before lifecycle stopped: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		backend.release <- struct{}{}
		select {
		case err := <-closed:
			if err != nil {
				t.Fatalf("Close() = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Close() did not wait for lifecycle completion")
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
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
		tk.SeedTimeline([]agent.Message{
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
		m.Insert(tk.ID.String(), m.NewEntry(tk, nil))

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

	t.Run("UnregisterCheckout", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			r := &repo.Checkout{Dir: "/tmp/test"}
			registerCheckout(t, m.Checkouts, "my/repo", r)
			m.Checkouts.UnregisterCheckout("my/repo")
			if _, ok := m.Checkouts.Checkout("my/repo"); ok {
				t.Error("Checkout() still returned checkout after Remove")
			}
		})
		t.Run("valid_removes_only_matching", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			r1 := &repo.Checkout{Dir: "/tmp/a"}
			r2 := &repo.Checkout{Dir: "/tmp/b"}
			registerCheckout(t, m.Checkouts, "a", r1)
			registerCheckout(t, m.Checkouts, "b", r2)
			m.Checkouts.UnregisterCheckout("a")
			if _, ok := m.Checkouts.Checkout("a"); ok {
				t.Error("Checkout(a) should be removed")
			}
			if got, ok := m.Checkouts.Checkout("b"); !ok || got != r2 {
				t.Error("Checkout(b) should still be registered")
			}
		})
	})

	t.Run("GetEntry", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
			e := m.NewEntry(tk, nil)
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

	t.Run("NewEntry", func(t *testing.T) {
		t.Parallel()
		m := newTestManager(t, Config{ServerCtx: t.Context()})
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
		e := m.NewEntry(tk, nil)
		if e.Task() != tk {
			t.Fatal("Task() returned wrong pointer")
		}
		if e.LoadedTask() != nil || e.Result() != nil {
			t.Fatal("new entry has mutable state")
		}
		select {
		case <-e.Done():
			t.Fatal("new entry is already done")
		default:
		}
		m.Insert(tk.ID.String(), e)
		if got, want := e.Lifecycle, e.Lifecycle; got != want {
			t.Fatal("Lifecycle allocated a new coordinator")
		}
	})

	// Regression: Manager.Range and Registry.Checkouts must iterate
	// unlocked so callers may re-enter their owner. Holding either lock during
	// iteration self-deadlocks the whole server (caught only by e2e before).
	t.Run("RangeCanReenterOwners", func(t *testing.T) {
		t.Parallel()
		m := newTestManager(t, Config{ServerCtx: t.Context()})
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
		m.Insert(tk.ID.String(), m.NewEntry(tk, nil))
		done := make(chan struct{})
		go func() {
			m.Range(func(_ string, _ *Entry) bool {
				_, _ = m.Checkouts.Checkout("repo")
				return true
			})
			registerCheckout(t, m.Checkouts, "repo", &repo.Checkout{})
			for range m.Checkouts.Checkouts() {
				_, _ = m.GetEntry(tk.ID.String())
			}
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Range/Checkouts deadlocked when iteration called back into Manager")
		}
	})

	t.Run("Range", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			for range 5 {
				m.Insert(ksid.NewID().String(), m.NewEntry(mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", ""), nil))
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
				m.Insert(ksid.NewID().String(), m.NewEntry(mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", ""), nil))
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
			m.Insert(ksid.NewID().String(), m.NewEntry(mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", ""), nil))
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
			tk.Repos = []taskslog.RepoMount{{Name: "my/repo", Branch: "caic-1"}}
			tk.SetPR("acme", "magic", 42)
			m.Insert(tk.ID.String(), m.NewEntry(tk, nil))
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
			tk.Repos = []taskslog.RepoMount{{Name: "my/repo", Branch: "caic-1"}}
			tk.SetPR("acme", "magic", 42)
			m.Insert(tk.ID.String(), m.NewEntry(tk, nil))
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
			tk.Repos = []taskslog.RepoMount{{Name: "my/repo", Branch: "caic-1"}}
			tk.SetPR("acme", "magic", 0)
			m.Insert(tk.ID.String(), m.NewEntry(tk, nil))
			found := m.FindTasksMatchingBranch("acme", "magic", "caic-1")
			if len(found) != 1 {
				t.Fatalf("FindTasksMatchingBranch returned %d entries, want 1", len(found))
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
			tk.Repos = []taskslog.RepoMount{{Name: "my/repo", Branch: "caic-1"}}
			tk.SetPR("acme", "magic", 0)
			m.Insert(tk.ID.String(), m.NewEntry(tk, nil))
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
			tk.Repos = []taskslog.RepoMount{{Name: "my/repo", Branch: "caic-1"}}
			tk.SetPR("acme", "magic", 0)
			e := m.NewEntry(tk, nil)
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
			tk.Repos = []taskslog.RepoMount{{Name: "my/repo", Branch: "caic-1"}}
			tk.SetPR("acme", "magic", 0)
			m.Insert(tk.ID.String(), m.NewEntry(tk, nil))
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
			tk.ForgeIssue = 5
			tk.Repos = []taskslog.RepoMount{{Name: "repo/a"}}
			tk.SetPR("acme", "magic", 0)
			tk.SetState(taskslog.StateRunning)
			m.Insert(tk.ID.String(), m.NewEntry(tk, nil))
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
			tk.SetState(taskslog.StateRunning)
			m.Insert(tk.ID.String(), m.NewEntry(tk, nil))
			pending := m.ListPendingBotTasks()
			if len(pending) != 0 {
				t.Errorf("ListPendingBotTasks returned %d tasks without ForgeIssue, want 0", len(pending))
			}
		})
		t.Run("valid_skips_terminal_states", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			for _, st := range []taskslog.State{taskslog.StateWaiting, taskslog.StateStopped, taskslog.StateCrashed, taskslog.StateFailed, taskslog.StatePurged} {
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
				tk.ForgeIssue = 1
				tk.SetState(st)
				m.Insert(tk.ID.String(), m.NewEntry(tk, nil))
			}
			pending := m.ListPendingBotTasks()
			if len(pending) != 0 {
				t.Errorf("ListPendingBotTasks returned %d tasks for terminal states, want 0", len(pending))
			}
		})
	})

	t.Run("resolveCheckout", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_with_repo", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			r := &repo.Checkout{Dir: "/tmp/test"}
			registerCheckout(t, m.Checkouts, "my/repo", r)
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
			tk.Repos = []taskslog.RepoMount{{Name: "my/repo"}}
			got := m.resolveCheckout(tk)
			if got != r {
				t.Error("resolveCheckout returned wrong checkout")
			}
		})
		t.Run("valid_no_repo_fallback", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
			got := m.resolveCheckout(tk)
			if got != nil {
				t.Fatal("resolveCheckout returned a checkout for no-repo task")
			}
		})
	})

	t.Run("applyLoadedSessionMetadata", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_fills_missing_session", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "requested")
			lt := &taskslog.LoadedTask{SessionID: "thread-1", Model: "reported", AgentVersion: "1.2.3"}
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
			tk.SeedTimeline([]agent.Message{&agent.InitMessage{SessionID: "live"}})
			applyLoadedSessionMetadata(tk, &taskslog.LoadedTask{SessionID: "persisted"})
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
			registerCheckout(t, m.Checkouts, "my/repo", &repo.Checkout{
				BaseBranch: "develop",
			})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
			tk.Repos = []taskslog.RepoMount{{Name: "my/repo", BaseBranch: "main"}}
			if got := m.EffectiveBaseBranch(tk); got != "main" {
				t.Errorf("EffectiveBaseBranch = %q, want %q", got, "main")
			}
		})
		t.Run("valid_checkout_default", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			registerCheckout(t, m.Checkouts, "my/repo", &repo.Checkout{
				BaseBranch: "develop",
			})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
			tk.Repos = []taskslog.RepoMount{{Name: "my/repo"}}
			if got := m.EffectiveBaseBranch(tk); got != "develop" {
				t.Errorf("EffectiveBaseBranch = %q, want %q", got, "develop")
			}
		})
		t.Run("valid_no_repo", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
			e := m.NewEntry(tk, nil)
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
			tk.SetState(taskslog.StateStopped)
			m.Insert(tk.ID.String(), m.NewEntry(tk, nil))
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
			tk.SetState(taskslog.StateRunning)
			m.Insert(tk.ID.String(), m.NewEntry(tk, nil))
			go func() {
				time.Sleep(50 * time.Millisecond)
				tk.SetState(taskslog.StateStopped)
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
			tk.SetState(taskslog.StateRunning)
			m.Insert(tk.ID.String(), m.NewEntry(tk, nil))
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
			state taskslog.State
			want  ErrorKind
			call  func(m *Manager, e *Entry) error
		}
		stop := func(m *Manager, e *Entry) error { return e.Lifecycle.Stop(t.Context()) }
		revive := func(m *Manager, e *Entry) error { return e.Lifecycle.Revive() }
		syncOrigin := func(m *Manager, e *Entry) error {
			_, err := e.Lifecycle.Sync(t.Context(), SyncTargetOrigin, false)
			return err
		}
		syncForceDefault := func(m *Manager, e *Entry) error {
			_, err := e.Lifecycle.Sync(t.Context(), SyncTargetDefault, true)
			return err
		}
		fork := func(m *Manager, e *Entry) error {
			_, err := e.Lifecycle.Fork(t.Context(), ForkParams{})
			return err
		}
		cases := []tc{
			{"stop_wrong_state", taskslog.StateStopped, KindConflict, stop},
			{"revive_not_stopped", taskslog.StateRunning, KindConflict, revive},
			{"sync_terminal_state", taskslog.StateStopped, KindConflict, syncOrigin},
			{"sync_force_default", taskslog.StateWaiting, KindBadRequest, syncForceDefault},
			{"fork_wrong_state", taskslog.StateStopping, KindConflict, fork},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				m := newTestManager(t, Config{ServerCtx: t.Context()})
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
				tk.SetState(c.state)
				e := m.NewEntry(tk, nil)
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
		// newManagerWithRepo returns a Manager with one repo checkout that has a
		// fake backend for harness "fake".
		newManagerWithRepo := func(t *testing.T) *Manager {
			m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: map[harness.Name]agent.Backend{"fake": &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}})
			registerCheckout(t, m.Checkouts, "my/repo", &repo.Checkout{Dir: "/tmp/my-repo"})
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
			awaitTaskCleanup(t, m, id)
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
			registerCheckout(t, m.Checkouts, "other/repo", &repo.Checkout{Dir: "/tmp/other-repo"})
			id, err := m.Create(t.Context(), CreateParams{
				Prompt:  agent.Prompt{Text: "hi"},
				Repos:   []CreateRepo{{Name: "my/repo", BaseBranch: "main"}},
				Harness: "fake",
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			awaitTaskCleanup(t, m, id)
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
			awaitTaskCleanup(t, m, id)
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
			awaitTaskCleanup(t, m, id)
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
			awaitTaskCleanup(t, m, id)
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
		// instance, plus an checkout with a fake backend.
		newForkManager := func(t *testing.T) (*Manager, *Entry) {
			m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: map[harness.Name]agent.Backend{"fake": &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}})
			registerCheckout(t, m.Checkouts, "my/repo", &repo.Checkout{Dir: "/tmp/my-repo"})
			src := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "src"}, "fake", "")
			src.Repos = []taskslog.RepoMount{{Name: "my/repo", Branch: "caic-1", GitRoot: "/tmp/my-repo"}}
			src.MaxCPUs = 5
			src.GitHubToken = true
			src.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "md-agent-src"), runtime.ConnectionTarget{SSHHost: "md-agent-src"}, "", "", 0)
			src.SetState(taskslog.StateWaiting)
			e := m.NewEntry(src, nil)
			m.Insert(src.ID.String(), e)
			return m, e
		}
		t.Run("valid_resolved_overrides_and_max_cpus", func(t *testing.T) {
			t.Parallel()
			m, src := newForkManager(t)
			id, err := src.Lifecycle.Fork(t.Context(), ForkParams{
				Prompt:    agent.Prompt{Text: "fork"},
				Tailscale: true,
				USB:       true,
				Display:   true,
				Sudo:      true,
			})
			if err != nil {
				t.Fatalf("Fork: %v", err)
			}
			awaitTaskCleanup(t, m, id)
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
		t.Run("does_not_corrupt_source_log_path", func(t *testing.T) {
			// Regression test: the fork goroutine must open and track its own
			// log through forkEntry's AgentRuntime, not the source's. Using the
			// source's AgentRuntime makes AgentRuntime.LogPathStore record the
			// fork's log path against the source entry, so the source later
			// fails to reopen its own log (e.g. to write its terminal result
			// trailer on stop/purge).
			t.Parallel()
			m, src := newForkManager(t)
			if got := src.LogPath.Get(); got != "" {
				t.Fatalf("source log path = %q before fork, want empty", got)
			}
			id, err := src.Lifecycle.Fork(t.Context(), ForkParams{Prompt: agent.Prompt{Text: "fork"}})
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
			if got := src.LogPath.Get(); got != "" {
				t.Errorf("source log path = %q after fork, want empty (fork must not write to the source entry's log path)", got)
			}
		})
		t.Run("valid_stopped_source", func(t *testing.T) {
			t.Parallel()
			m, src := newForkManager(t)
			src.Task().SetState(taskslog.StateStopped)
			id, err := src.Lifecycle.Fork(t.Context(), ForkParams{Prompt: agent.Prompt{Text: "fork"}})
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
			if got := src.Task().GetState(); got != taskslog.StateStopped {
				t.Errorf("source state = %v, want %v", got, taskslog.StateStopped)
			}
		})
		t.Run("valid_crashed_source", func(t *testing.T) {
			t.Parallel()
			m, src := newForkManager(t)
			src.Task().SetState(taskslog.StateCrashed)
			id, err := src.Lifecycle.Fork(t.Context(), ForkParams{Prompt: agent.Prompt{Text: "fork"}})
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
			if got := src.Task().GetState(); got != taskslog.StateCrashed {
				t.Errorf("source state = %v, want %v", got, taskslog.StateCrashed)
			}
		})
		t.Run("valid_metadata_matches_task", func(t *testing.T) {
			t.Parallel()
			m, src := newForkManager(t)
			id, err := src.Lifecycle.Fork(t.Context(), ForkParams{Prompt: agent.Prompt{Text: "fork"}})
			if err != nil {
				t.Fatalf("Fork: %v", err)
			}
			awaitTaskCleanup(t, m, id)
			e, _ := m.GetEntry(id)
			metadata := task.MakeMetadata(e.Task())
			if metadata[runtime.MetadataTaskID] != e.Task().ID.String() {
				t.Errorf("metadata[%s] = %q, want %q", runtime.MetadataTaskID, metadata[runtime.MetadataTaskID], e.Task().ID.String())
			}
		})
		t.Run("error_extra_repo_overlap", func(t *testing.T) {
			t.Parallel()
			_, src := newForkManager(t)
			_, err := src.Lifecycle.Fork(t.Context(), ForkParams{
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
			src := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "src"}, "fake", "")
			src.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "md-agent-src"), runtime.ConnectionTarget{SSHHost: "md-agent-src"}, "", "", 0)
			src.SetState(taskslog.StateWaiting)
			e := m.NewEntry(src, nil)
			m.Insert(src.ID.String(), e)
			_, err := e.Lifecycle.Fork(t.Context(), ForkParams{Prompt: agent.Prompt{Text: "fork"}})
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetState(taskslog.StateWaiting)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			err := e.Lifecycle.Restart(t.Context(), agent.Prompt{})
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetState(taskslog.StateStopped)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			err := e.Lifecycle.Restart(t.Context(), agent.Prompt{Text: "go"})
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetState(taskslog.StateWaiting)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			err := e.Lifecycle.SendInput(t.Context(), agent.Prompt{Text: "go"})
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
			cacheDir := t.TempDir()
			logDir := filepath.Join(cacheDir, "tasks")
			m := newTestManager(t, Config{
				ServerCtx: t.Context(),
				LogStore:  taskslog.NewStore(testLogger(), logDir),
				Backends:  map[harness.Name]agent.Backend{"reconnect": backend},
			})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "reconnect", "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(taskslog.StateRunning)
			tk.SeedTimeline([]agent.Message{
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
			logW, path, err := (taskslog.NewStore(testLogger(), logDir)).Open(tk.LogFilename(), tk.LogHeader())
			if err != nil {
				t.Fatal(err)
			}
			if err := logW.Close(); err != nil {
				t.Fatal(err)
			}
			e := m.NewEntry(tk, nil)
			e.LogPath.Set(path)
			m.Insert(tk.ID.String(), e)

			if err := e.Lifecycle.SendInput(t.Context(), agent.Prompt{Text: "A"}); err != nil {
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
			if got := tk.GetState(); got != taskslog.StateRunning {
				t.Fatalf("state = %s, want %s", got, taskslog.StateRunning)
			}
		})
		t.Run("error_delivery_failure_is_not_no_session", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, harness.Codex, "")
			tk.SetState(taskslog.StateWaiting)

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
			log := slog.New(slog.DiscardHandler)
			s := agent.NewSession(t.Context(), cmd, agent.NewConn(t.Context(), log, stdin, agent.DiscardLogSink{Version: agent.LogVersionV1}, codex.New("", nil).NewWire()), stdout, make(chan agent.TimedMessage, 256), log)
			t.Cleanup(func() {
				cmdCancel()
				_ = s.Wait()
			})
			tk.AttachSession(&task.SessionHandle{Session: s})

			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			err = e.Lifecycle.SendInput(t.Context(), agent.Prompt{Text: "go"})
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
			if got := tk.GetState(); got != taskslog.StateWaiting {
				t.Errorf("state = %s, want %s", got, taskslog.StateWaiting)
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
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
				id := tk.ID.String()
				wg.Go(func() {
					m.Insert(id, m.NewEntry(tk, nil))
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetState(taskslog.StateStopped)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			err := e.Lifecycle.ClearContext()
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
		t.Run("error_no_checkout_backend", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetState(taskslog.StateWaiting)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			err := e.Lifecycle.ClearContext()
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetState(taskslog.StateWaiting)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			err := e.Lifecycle.Compact(t.Context(), "shorten")
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.Sudo = false
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			if got := m.SudoPassword(t.Context(), tk); got != "" {
				t.Errorf("SudoPassword = %q, want empty for !Sudo", got)
			}
		})
		t.Run("valid_no_container", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.Sudo = true
			if got := m.SudoPassword(t.Context(), tk); got != "" {
				t.Errorf("SudoPassword = %q, want empty for empty Runtime", got)
			}
		})
		t.Run("valid_cached", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.Sudo = true
			tk.SudoPassword = "cached-pw"
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			if got := m.SudoPassword(t.Context(), tk); got != "cached-pw" {
				t.Errorf("SudoPassword = %q, want cached-pw", got)
			}
		})
		t.Run("valid_fetches_then_caches", func(t *testing.T) {
			t.Parallel()
			fake := &runtimetest.FakeInfo{SudoResult: "fetched-pw"}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake)})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.Sudo = true
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.Sudo = true
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.Repos = []taskslog.RepoMount{{Name: "repo/x", Branch: "caic-1"}}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-dead"), runtime.ConnectionTarget{SSHHost: "ctr-dead"}, "", "", 0)
			tk.SetState(taskslog.StateRunning)
			m.Insert(tk.ID.String(), m.NewEntry(tk, nil))
			m.handleRuntimeInstanceExit(t.Context(), runtime.NewID("test-runtime", "ctr-dead"))
			if got := tk.GetState(); got != taskslog.StateStopped {
				t.Errorf("state = %v, want StateStopped", got)
			}
		})
		t.Run("valid_skips_purged", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-purged"), runtime.ConnectionTarget{SSHHost: "ctr-purged"}, "", "", 0)
			tk.SetState(taskslog.StatePurged)
			m.Insert(tk.ID.String(), m.NewEntry(tk, nil))
			m.handleRuntimeInstanceExit(t.Context(), runtime.NewID("test-runtime", "ctr-purged"))
			if got := tk.GetState(); got != taskslog.StatePurged {
				t.Errorf("state = %v (should stay Purged)", got)
			}
		})
		t.Run("valid_skips_purging", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-purging"), runtime.ConnectionTarget{SSHHost: "ctr-purging"}, "", "", 0)
			// A purge in progress: removing the instance emits the very "die"
			// event handled here. Acting on it would flap the task to Stopped
			// mid-purge and race the cleanup goroutine.
			tk.SetState(taskslog.StatePurging)
			m.Insert(tk.ID.String(), m.NewEntry(tk, nil))
			m.handleRuntimeInstanceExit(t.Context(), runtime.NewID("test-runtime", "ctr-purging"))
			if got := tk.GetState(); got != taskslog.StatePurging {
				t.Errorf("state = %v (should stay Purging)", got)
			}
		})
		t.Run("valid_skips_stopping", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-stopping"), runtime.ConnectionTarget{SSHHost: "ctr-stopping"}, "", "", 0)
			tk.SetState(taskslog.StateStopping)
			m.Insert(tk.ID.String(), m.NewEntry(tk, nil))
			m.handleRuntimeInstanceExit(t.Context(), runtime.NewID("test-runtime", "ctr-stopping"))
			if got := tk.GetState(); got != taskslog.StateStopping {
				t.Errorf("state = %v (should stay Stopping)", got)
			}
		})
		t.Run("valid_skips_stopped", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-stopped"), runtime.ConnectionTarget{SSHHost: "ctr-stopped"}, "", "", 0)
			tk.SetState(taskslog.StateStopped)
			m.Insert(tk.ID.String(), m.NewEntry(tk, nil))
			m.handleRuntimeInstanceExit(t.Context(), runtime.NewID("test-runtime", "ctr-stopped"))
			if got := tk.GetState(); got != taskslog.StateStopped {
				t.Errorf("state = %v (should stay Stopped)", got)
			}
		})
		t.Run("valid_skips_wrong_container", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-alive"), runtime.ConnectionTarget{SSHHost: "ctr-alive"}, "", "", 0)
			tk.SetState(taskslog.StateRunning)
			m.Insert(tk.ID.String(), m.NewEntry(tk, nil))
			m.handleRuntimeInstanceExit(t.Context(), runtime.NewID("test-runtime", "ctr-other"))
			if got := tk.GetState(); got != taskslog.StateRunning {
				t.Errorf("state = %v (should stay Running)", got)
			}
		})
	})
	t.Run("HistorySource", func(t *testing.T) {
		t.Parallel()
		t.Run("error_no_log", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			if _, err := m.HistorySource(e); !errors.Is(err, taskslog.ErrNoLog) {
				t.Fatalf("HistorySource error = %v, want ErrNoLog", err)
			}
		})
		t.Run("valid_uses_header_resolver", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "task.jsonl"), []byte(
				`{"type":"caic_meta","version":1,"prompt":"task","repos":[],"harness":"claude"}`+"\n"+
					`{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`+"\n",
			), 0o600); err != nil {
				t.Fatal(err)
			}
			logs, err := taskslog.NewStore(testLogger(), dir).LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}
			var wireCalls int
			m := newTestManager(t, Config{
				ServerCtx: t.Context(),
				Backends: map[harness.Name]agent.Backend{
					harness.Claude: &agenttest.FakeBackend{WireFactory: func() agent.WireFormat {
						wireCalls++
						return claudecode.New().NewWire()
					}},
				},
			})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "task"}, "", "")
			entry := newTestPurgedEntry(t, tk, &taskslog.Result{State: taskslog.StatePurged}, logs[0])
			source, err := m.HistorySource(entry)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for parsed, streamErr := range source.StreamMessages(t.Context()) {
				if streamErr != nil {
					t.Fatal(streamErr)
				}
				if _, ok := parsed.Message.(*agent.TextMessage); ok {
					found = true
				}
			}
			if !found {
				t.Fatal("text message not found")
			}
			if wireCalls != 1 {
				t.Fatalf("NewWire calls = %d, want 1", wireCalls)
			}
			if source.Timeline != nil {
				t.Fatalf("history source retained %d messages", len(source.Timeline))
			}
		})
	})
	t.Run("BackwardMessages", func(t *testing.T) {
		t.Parallel()
		t.Run("live_uses_memory_without_log", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "live"}, "", "")
			want := &agent.TextMessage{Text: "live result"}
			tk.SeedTimeline([]agent.Message{want})
			entry := m.NewEntry(tk, nil)

			var got agent.Message
			for message, err := range m.BackwardMessages(t.Context(), entry) {
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := message.(*agent.TextMessage); ok {
					got = message
					break
				}
			}
			if got != want {
				t.Fatalf("BackwardMessages match = %#v, want %#v", got, want)
			}
		})

		t.Run("restored_uses_disk_without_hydrating_task", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "task.jsonl")
			if err := os.WriteFile(path, []byte(
				`{"type":"caic_meta","version":1,"prompt":"task","repos":[],"harness":"claude"}`+"\n"+
					`{"type":"assistant","message":{"content":[{"type":"text","text":"disk result"}]}}`+"\n"+
					`{"type":"caic_result","state":"purged"}`+"\n",
			), 0o600); err != nil {
				t.Fatal(err)
			}
			logs, err := taskslog.NewStore(testLogger(), dir).LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}
			m := newTestManager(t, Config{
				ServerCtx: t.Context(),
				Backends: map[harness.Name]agent.Backend{
					harness.Claude: &agenttest.FakeBackend{WireFactory: claudecode.New().NewWire},
				},
			})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "task"}, "", "")
			entry := m.NewEntry(tk, logs[0])

			var got agent.Message
			for message, err := range m.BackwardMessages(t.Context(), entry) {
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := message.(*agent.TextMessage); ok {
					got = message
					break
				}
			}
			text, ok := got.(*agent.TextMessage)
			if !ok || text.Text != "disk result" {
				t.Fatalf("BackwardMessages match = %#v, want disk result", got)
			}
			if len(tk.Messages()) != 0 {
				t.Fatalf("restored task retained %d messages", len(tk.Messages()))
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
		t.Run("valid_creates_entries", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			registerCheckout(t, m.Checkouts, "repo/a", &repo.Checkout{Dir: t.TempDir()})
			now := time.Now().UTC()
			id := ksid.NewID()
			all := []*taskslog.LoadedTask{
				{
					TaskID:            id.String(),
					Prompt:            "test task",
					Title:             "Test Title",
					Harness:           "claude",
					RuntimeName:       "docker",
					Repos:             []taskslog.RepoMount{{Name: "repo/a", Branch: "caic-1"}},
					State:             taskslog.StateStopped,
					LastTrailer:       &taskslog.Result{State: taskslog.StateStopped},
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
			if e.Lifecycle == nil {
				t.Fatal("persisted entry has no lifecycle")
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
			if e.Result() == nil || e.Result().State != taskslog.StateStopped {
				t.Errorf("Result = %v, want StateStopped", e.Result())
			}
		})
		t.Run("valid_promotes_missing_log_runtime", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			id := ksid.NewID()
			all := []*taskslog.LoadedTask{{
				TaskID:            id.String(),
				Prompt:            "old task",
				Harness:           "claude",
				State:             taskslog.StateStopped,
				LastTrailer:       &taskslog.Result{State: taskslog.StateStopped},
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
				marshal(agent.MetaResultMessage{MessageType: "caic_result", State: taskslog.StateStopped.String()}),
			}
			path := filepath.Join(dir, id.String()+"--.jsonl")
			if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			logs, err := taskslog.NewStore(testLogger(), dir).LoadUnsettled()
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
			all := []*taskslog.LoadedTask{
				{
					TaskID:            id.String(),
					Prompt:            "was running",
					Repos:             []taskslog.RepoMount{{Name: "repo/a"}},
					State:             taskslog.StateRunning,
					LastStateUpdateAt: now,
				},
			}
			err := m.LoadPurgedTasks(all)
			if err != nil {
				t.Fatalf("LoadPurgedTasks: %v", err)
			}
			e, _ := m.GetEntry(id.String())
			if got := e.Task().GetState(); got != taskslog.StateFailed {
				t.Errorf("state = %v, want StateFailed (running→failed)", got)
			}
		})
		t.Run("valid_fallback_title", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			now := time.Now().UTC()
			id := ksid.NewID()
			all := []*taskslog.LoadedTask{
				{
					TaskID:            id.String(),
					Prompt:            "this is the prompt",
					LastStateUpdateAt: now,
					State:             taskslog.StateStopped,
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
			all := []*taskslog.LoadedTask{
				{
					TaskID:            id.String(),
					Prompt:            "test",
					LastStateUpdateAt: now,
				},
			}
			_ = m.LoadPurgedTasks(all)
			e, _ := m.GetEntry(id.String())
			if got := e.Result().State; got != taskslog.StateFailed {
				t.Errorf("Result.State = %v, want StateFailed (fallback)", got)
			}
		})
		t.Run("valid_invalid_ksid_fallback", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			now := time.Now().UTC()
			all := []*taskslog.LoadedTask{
				{
					TaskID:            "invalid",
					Prompt:            "test",
					LastStateUpdateAt: now,
					State:             taskslog.StateStopped,
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
			active := mustNewTask(t, activeID, agent.Prompt{Text: "active"}, "", "")
			active.Repos = []taskslog.RepoMount{{Name: "repo/a", Branch: "caic-live"}}
			active.SetTitle("active")
			m.Insert(activeID.String(), m.NewEntry(active, nil))
			duplicateBranchID := ksid.NewID()
			keptID := ksid.NewID()
			all := []*taskslog.LoadedTask{
				{
					TaskID:            activeID.String(),
					Prompt:            "replacement by id",
					Repos:             []taskslog.RepoMount{{Name: "repo/a", Branch: "caic-old"}},
					LastStateUpdateAt: now,
					State:             taskslog.StateStopped,
				},
				{
					TaskID:            duplicateBranchID.String(),
					Prompt:            "replacement by branch",
					Repos:             []taskslog.RepoMount{{Name: "repo/a", Branch: "caic-live"}},
					LastStateUpdateAt: now,
					State:             taskslog.StateStopped,
				},
				{
					TaskID:            keptID.String(),
					Prompt:            "kept",
					Repos:             []taskslog.RepoMount{{Name: "repo/a", Branch: "caic-kept"}},
					LastStateUpdateAt: now,
					State:             taskslog.StateStopped,
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetState(taskslog.StatePending)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			_, err := e.Lifecycle.Sync(t.Context(), SyncTargetOrigin, false)
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
		t.Run("error_purging", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetState(taskslog.StatePurging)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			_, err := e.Lifecycle.Sync(t.Context(), SyncTargetOrigin, false)
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
		t.Run("error_provisioning_no_checkout", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetState(taskslog.StateProvisioning)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			_, err := e.Lifecycle.Sync(t.Context(), SyncTargetOrigin, false)
			if err == nil {
				t.Fatal("expected error for provisioning task without instance")
			}
		})
		t.Run("error_force_not_supported", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetState(taskslog.StateRunning)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			_, err := e.Lifecycle.Sync(t.Context(), SyncTargetDefault, true)
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetState(taskslog.StatePurged)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			err := e.Lifecycle.Purge(t.Context())
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
		t.Run("valid_after_finished_crash", func(t *testing.T) {
			t.Parallel()
			fake := &runtimetest.FakeBackend{}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, fake, nil)})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(taskslog.StateCrashed)
			entry := m.NewEntry(tk, nil)
			entry.Finish(&taskslog.Result{State: taskslog.StateCrashed, Err: errors.New("agent crashed")})
			m.Insert(tk.ID.String(), entry)

			if err := entry.Lifecycle.Purge(t.Context()); err != nil {
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
				if result := entry.Result(); result != nil && result.State == taskslog.StatePurged {
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(taskslog.StateRunning)
			entry := m.NewEntry(tk, nil)
			m.Insert(tk.ID.String(), entry)

			if err := entry.Lifecycle.Stop(t.Context()); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			select {
			case <-fake.started:
			case <-time.After(time.Second):
				t.Fatal("StopTask did not reach backend Stop")
			}

			if err := entry.Lifecycle.Purge(t.Context()); err != nil {
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
			for tk.GetState() != taskslog.StatePurged {
				if time.Now().After(deadline) {
					t.Fatalf("state = %v, want StatePurged after StopTask finishes", tk.GetState())
				}
				time.Sleep(time.Millisecond)
			}
			if result := entry.Result(); result == nil || result.State != taskslog.StatePurged {
				t.Fatalf("Result = %v, want StatePurged", result)
			}
		})
	})
	t.Run("Stop", func(t *testing.T) {
		t.Parallel()
		t.Run("error_wrong_state", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetState(taskslog.StateStopped)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			err := e.Lifecycle.Stop(t.Context())
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(taskslog.StateRunning)
			entry := m.NewEntry(tk, nil)
			m.Insert(tk.ID.String(), entry)

			if err := entry.Lifecycle.Stop(t.Context()); err != nil {
				t.Fatalf("Stop: %v", err)
			}

			// Stop runs StopTask on a background goroutine; wait for the task to
			// settle rather than sleeping a fixed duration.
			deadline := time.Now().Add(2 * time.Second)
			for tk.GetState() != taskslog.StateStopped {
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetState(taskslog.StateRunning)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			err := e.Lifecycle.Revive()
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(taskslog.StateCrashed)
			entry := m.NewEntry(tk, nil)
			m.Insert(tk.ID.String(), entry)

			if err := entry.Lifecycle.Revive(); err != nil {
				t.Fatalf("Revive: %v", err)
			}
			if got := tk.GetState(); got != taskslog.StateProvisioning {
				t.Fatalf("state = %v, want provisioning", got)
			}
			close(releaseRevive)
			select {
			case <-entry.Done():
			case <-time.After(time.Second):
				t.Fatal("failed revive did not close done")
			}
		})
		t.Run("error_failure_closes_done", func(t *testing.T) {
			t.Parallel()
			releaseRevive := make(chan struct{})
			fake := &blockingReviveBackend{FakeBackend: &runtimetest.FakeBackend{}, release: releaseRevive}
			m := newTestManager(t, Config{
				ServerCtx: t.Context(),
				Runtimes:  newTestRuntime(t, fake, nil),
				Backends: map[harness.Name]agent.Backend{
					harness.Claude: &agenttest.FakeBackend{WireFactory: claudecode.New().NewWire},
				},
			})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, harness.Claude, "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(taskslog.StateStopped)
			log, path, err := m.logStore.Open(tk.LogFilename(), tk.LogHeader())
			if err != nil {
				t.Fatal(err)
			}
			if err := m.logStore.WriteResultTrailer(log, tk.Title(), &taskslog.Result{State: taskslog.StateStopped}); err != nil {
				t.Fatal(err)
			}
			if err := log.Close(); err != nil {
				t.Fatal(err)
			}
			entry := m.NewEntry(tk, nil)
			entry.LogPath.Set(path)
			m.Insert(tk.ID.String(), entry)

			firstChanged := m.Changed()
			if err := entry.Lifecycle.Revive(); err != nil {
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
			if got := tk.GetState(); got != taskslog.StateFailed {
				t.Fatalf("state = %v, want StateFailed", got)
			}
			result := entry.Result()
			if result == nil || result.State != taskslog.StateFailed || result.Err == nil {
				t.Fatalf("Result = %v, want failed result with error", result)
			}
		})
	})
	t.Run("SendInput_Images", func(t *testing.T) {
		t.Parallel()
		t.Run("error_images_unsupported", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: map[harness.Name]agent.Backend{"fake": &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}})
			registerCheckout(t, m.Checkouts, "repo/a", &repo.Checkout{Dir: "/tmp/repo"})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "fake", "")
			tk.Repos = []taskslog.RepoMount{{Name: "repo/a"}}
			tk.SetState(taskslog.StateWaiting)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID.String(), e)
			err := e.Lifecycle.SendInput(t.Context(), agent.Prompt{Text: "go", Images: []agent.ImageData{{}}})
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
			msgCh := make(chan agent.TimedMessage, 1)
			dispatchDone := make(chan struct{})
			close(dispatchDone)
			log := slog.New(slog.DiscardHandler)
			s := agent.NewSession(t.Context(), cmd, agent.NewConn(t.Context(), log, stdin, agent.DiscardLogSink{Version: agent.LogVersionV1}, codex.New("", nil).NewWire()), stdout, msgCh, log)
			h := &task.SessionHandle{Session: s, MsgCh: msgCh, DispatchDone: dispatchDone}
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ssh-failed"), runtime.ConnectionTarget{SSHHost: "ssh-failed"}, "", "", 0)
			tk.SetState(taskslog.StateRunning)
			tk.AttachSession(h)
			runtimeBackend := &runtimetest.FakeBackend{}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, runtimeBackend, nil)})
			entry := m.NewEntry(tk, nil)
			m.Insert(tk.ID.String(), entry)

			entry.Lifecycle.watchSession(h)

			select {
			case <-entry.Done():
			case <-time.After(time.Second):
				t.Fatal("watchSession did not finish")
			}
			if got := tk.GetState(); got != taskslog.StateCrashed {
				t.Fatalf("state = %v, want crashed", got)
			}
			if got := runtimeBackend.Status("ssh-failed"); got != runtimetest.StatusStopped {
				t.Fatalf("instance ssh-failed status = %v, want stopped", got)
			}
		})
		t.Run("server_shutdown_preserves_task_and_instance", func(t *testing.T) {
			t.Parallel()
			serverCtx, cancelServer := context.WithCancel(t.Context())
			t.Cleanup(cancelServer)
			cmd := exec.CommandContext(t.Context(), "sh", "-c", "sleep 30")
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
			msgCh := make(chan agent.TimedMessage, 1)
			dispatchDone := make(chan struct{})
			close(dispatchDone)
			log := slog.New(slog.DiscardHandler)
			s := agent.NewSession(t.Context(), cmd, agent.NewConn(t.Context(), log, stdin, agent.DiscardLogSink{Version: agent.LogVersionV1}, codex.New("", nil).NewWire()), stdout, msgCh, log)
			h := &task.SessionHandle{Session: s, MsgCh: msgCh, DispatchDone: dispatchDone}
			runtimeBackend := &runtimetest.FakeBackend{}
			instanceID, err := runtimeBackend.Launch(t.Context(), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetRuntimeConnectionInfo(instanceID, runtime.ConnectionTarget{SSHHost: "ssh-restart"}, "", "", 0)
			tk.SetState(taskslog.StateRunning)
			tk.AttachSession(h)
			m := newTestManager(t, Config{ServerCtx: serverCtx, Runtimes: newTestRuntime(t, runtimeBackend, nil)})
			entry := m.NewEntry(tk, nil)
			m.Insert(tk.ID.String(), entry)

			entry.Lifecycle.watchSession(h)
			cancelServer()
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entry.Done():
				t.Fatal("server shutdown finished the task")
			case <-time.After(100 * time.Millisecond):
			}
			if got := tk.GetState(); got != taskslog.StateRunning {
				t.Fatalf("state = %v, want running", got)
			}
			if got := runtimeBackend.Status(instanceID); got != runtimetest.StatusRunning {
				t.Fatalf("instance %s status = %v, want running", instanceID, got)
			}
		})
	})
	t.Run("ResolveNativeParser", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_with_backend", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: map[harness.Name]agent.Backend{"claude": &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}})
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
	t.Run("Create_Errors", func(t *testing.T) {
		t.Parallel()
		t.Run("error_unknown_harness", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: map[harness.Name]agent.Backend{}})
			registerCheckout(t, m.Checkouts, "repo/a", &repo.Checkout{Dir: "/tmp/repo"})
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
			registerCheckout(t, m.Checkouts, "repo/a", &repo.Checkout{Dir: "/tmp/repo"})
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
			registerCheckout(t, m.Checkouts, "repo/a", &repo.Checkout{Dir: "/tmp/repo"})
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

		// forkSetup creates a Manager with an checkout for "repo/a" and a source
		// task in StateWaiting. Returns the source Entry.
		forkSetup := func(t *testing.T, sourceHarness harness.Name, backends map[harness.Name]agent.Backend) *Entry {
			m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: backends})
			r := &repo.Checkout{Dir: "/tmp/repo"}
			registerCheckout(t, m.Checkouts, "repo/a", r)
			src := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "src"}, sourceHarness, "")
			src.Repos = []taskslog.RepoMount{{Name: "repo/a", Branch: "caic-1"}}
			src.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "md-agent-src"), runtime.ConnectionTarget{SSHHost: "md-agent-src"}, "", "", 0)
			src.SetState(taskslog.StateWaiting)
			e := m.NewEntry(src, nil)
			m.Insert(src.ID.String(), e)
			return e
		}

		defaultBackends := map[harness.Name]agent.Backend{"fake": &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}

		t.Run("error_unknown_harness", func(t *testing.T) {
			t.Parallel()
			e := forkSetup(t, "fake", defaultBackends)
			_, err := e.Lifecycle.Fork(t.Context(), ForkParams{Prompt: agent.Prompt{Text: "fork"}, Harness: "bogus"})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
		t.Run("error_unsupported_model", func(t *testing.T) {
			t.Parallel()
			e := forkSetup(t, "fake", defaultBackends)
			_, err := e.Lifecycle.Fork(t.Context(), ForkParams{Prompt: agent.Prompt{Text: "fork"}, Model: "unsupported"})
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
			e := forkSetup(t, "fake", backends)
			_, err := e.Lifecycle.Fork(t.Context(), ForkParams{Prompt: agent.Prompt{Text: "fork"}, Harness: "fake2", Model: "unsupported"})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
		t.Run("error_no_container", func(t *testing.T) {
			t.Parallel()
			e := forkSetup(t, "fake", defaultBackends)
			// Overwrite the instance to empty.
			e.Task().SetRuntimeConnectionInfo("", runtime.ConnectionTarget{SSHHost: ""}, "", "", 0)
			_, err := e.Lifecycle.Fork(t.Context(), ForkParams{Prompt: agent.Prompt{Text: "fork"}})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
		t.Run("error_wrong_state", func(t *testing.T) {
			t.Parallel()
			e := forkSetup(t, "fake", defaultBackends)
			e.Task().SetState(taskslog.StateProvisioning)
			_, err := e.Lifecycle.Fork(t.Context(), ForkParams{Prompt: agent.Prompt{Text: "fork"}})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
		t.Run("error_unknown_extra_repo", func(t *testing.T) {
			t.Parallel()
			e := forkSetup(t, "fake", defaultBackends)
			_, err := e.Lifecycle.Fork(t.Context(), ForkParams{Prompt: agent.Prompt{Text: "fork"}, ExtraRepos: []ForkRepo{{Name: "ghost"}}})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
		t.Run("error_unknown_harness_when_model_set", func(t *testing.T) {
			t.Parallel()
			e := forkSetup(t, "bogus", defaultBackends)
			_, err := e.Lifecycle.Fork(t.Context(), ForkParams{Prompt: agent.Prompt{Text: "fork"}, Model: "m1"})
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
			registerCheckout(t, m.Checkouts, "repo/a", &repo.Checkout{Dir: "/home/user/src/repo/a"})
			instances := []runtime.Instance{
				{ID: runtime.NewID("test-runtime", "duplicate-one"), State: "exited", Repos: []runtime.Repo{{ContainerPath: "/home/user/src/repo/a", Branch: "caic-1"}}},
				{ID: runtime.NewID("test-runtime", "duplicate-two"), State: "exited", Repos: []runtime.Repo{{ContainerPath: "/home/user/src/repo/a", Branch: "caic-1"}}},
			}
			adopted, err := m.ImportInstances(t.Context(), instances, nil)
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
			registerCheckout(t, m.Checkouts, "repo/a", &repo.Checkout{Dir: "/home/user/src/repo/a"})
			_, err := m.ImportInstances(t.Context(), []runtime.Instance{{
				ID:    runtime.NewID("test-runtime", "metadata-error"),
				State: "exited",
				Repos: []runtime.Repo{{GitRoot: "/home/user/src/repo/a", Branch: "caic-1", ContainerPath: "/home/user/src/repo/a"}},
			}}, []*taskslog.LoadedTask{{
				TaskID:  taskID.String(),
				Harness: harness.Claude,
				Repos:   []taskslog.RepoMount{{Name: "repo/a", Branch: "caic-1"}},
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
			registerCheckout(t, m.Checkouts, "caic-xyz/caic", &repo.Checkout{Dir: "/home/user/src/caic-xyz/caic"})
			registerCheckout(t, m.Checkouts, "caic-xyz/md", &repo.Checkout{Dir: "/home/user/src/caic-xyz/md"})

			adopted, err := m.ImportInstances(t.Context(),

				[]runtime.Instance{
					{
						ID:    runtime.NewID("test-runtime", "md-caic-caic-5"),
						State: "exited",
						Repos: []runtime.Repo{
							{GitRoot: "/home/user/src/caic-xyz/caic", Branch: "caic-5", ContainerPath: "/home/user/src/caic-xyz/caic"},
							{GitRoot: "/home/user/src/caic-xyz/md", Branch: "caic-0", ContainerPath: "/home/user/src/caic-xyz/md"},
						},
					},
				}, []*taskslog.LoadedTask{{
					TaskID:  taskID.String(),
					Harness: harness.Claude,
					Repos:   []taskslog.RepoMount{{Name: "caic-xyz/caic", Branch: "caic-5"}},
					Timeline: []agent.TimedMessage{{Message: &agent.RateLimitMessage{
						Status:        agent.RateLimitStatusRejected,
						ResetsAt:      resetAt,
						QuotaProvider: agent.QuotaProviderClaudeCode,
						QuotaWindow:   "five_hour",
						Utilization:   1,
					}}},
				}})
			if err != nil {
				t.Fatalf("AdoptInstances: %v", err)
			}
			if len(adopted) != 1 {
				t.Fatalf("adopted len = %d, want 1", len(adopted))
			}
			primary := adopted[0].Task().Primary()
			if primary == nil || primary.Name != "caic-xyz/caic" {
				t.Errorf("primary repo = %#v, want caic-xyz/caic", primary)
			}
			if primary == nil || primary.Branch != "caic-5" {
				t.Errorf("primary branch = %#v, want caic-5", primary)
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
			registerCheckout(t, m.Checkouts, "repo/a", &repo.Checkout{Dir: "/home/user/src/repo/a"})

			_, err := m.ImportInstances(t.Context(), []runtime.Instance{{
				ID: runtime.NewID("test-runtime", "repo-only-match"), State: "exited",
				Repos: []runtime.Repo{{GitRoot: "/home/user/src/repo/a", Branch: "caic-1", ContainerPath: "/home/user/src/repo/a"}},
			}}, []*taskslog.LoadedTask{{
				TaskID:  otherTaskID.String(),
				Repos:   []taskslog.RepoMount{{Name: "repo/a", Branch: "caic-1"}},
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
			registerCheckout(t, m.Checkouts, "repo/a", &repo.Checkout{Dir: "/home/user/src/repo/a"})

			_, err := m.ImportInstances(t.Context(), []runtime.Instance{{
				ID: runtime.NewID("test-runtime", "unknown-harness"), State: "exited",
				Repos: []runtime.Repo{{GitRoot: "/home/user/src/repo/a", Branch: "caic-1", ContainerPath: "/home/user/src/repo/a"}},
			}}, []*taskslog.LoadedTask{{
				TaskID:  taskID.String(),
				Repos:   []taskslog.RepoMount{{Name: "repo/a", Branch: "caic-1"}},
				Harness: "unknown",
			}})

			if err == nil || !strings.Contains(err.Error(), `unknown harness "unknown"`) {
				t.Fatalf("AdoptInstances error = %v, want unknown-harness error", err)
			}
		})
		t.Run("aborts_before_registration_or_reconnect_on_malformed_control", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			backend := &reconnectInputBackend{FakeBackend: &agenttest.FakeBackend{HarnessName: harness.Claude, Images: true, ContextLimit: 200_000}}
			t.Cleanup(backend.stop)
			fake := &runtimetest.FakeInfo{Meta: map[string]string{
				"md-agent-semantic-error\x00caic.id":      taskID.String(),
				"md-agent-semantic-error\x00caic.harness": string(harness.Claude),
			}}
			m := newTestManager(t, Config{
				ServerCtx: t.Context(),
				Runtimes:  newTestRuntime(t, &runtimetest.FakeBackend{}, fake),
				Backends:  map[harness.Name]agent.Backend{harness.Claude: backend},
			})
			m.relay = fakeRelayReader{
				statusFn: func(context.Context, runtime.ConnectionTarget) (bool, string, error) { return true, "alive", nil },
				readTailFn: func(context.Context, runtime.ConnectionTarget, *agent.LogRecordParser, int64) ([]agent.TimedMessage, int64, error) {
					return nil, 0, nil
				},
				readLogFn: func(context.Context, runtime.ConnectionTarget, int) string { return "" },
			}

			meta, err := json.Marshal(agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "semantic error", Harness: harness.Claude})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(m.logStore.LogDir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(m.logStore.LogDir, taskID.String()+".jsonl")
			data := append([]byte(nil), meta...)
			data = append(data, []byte(`
{"type":"caic_result","state":123}
`)...)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			logs, err := m.logStore.LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}

			adopted, err := m.ImportInstances(t.Context(), []runtime.Instance{{
				ID:    runtime.NewID("test-runtime", "md-agent-semantic-error"),
				State: "running",
			}}, logs)

			if err == nil || !strings.Contains(err.Error(), "task log "+taskID.String()+" not found") {
				t.Fatalf("AdoptInstances error = %v, want malformed-control log rejection", err)
			}
			if adopted != nil || m.Len() != 0 {
				t.Fatalf("malformed control mutated manager: adopted=%#v len=%d", adopted, m.Len())
			}
			after, readErr := os.ReadFile(path) //nolint:gosec // path is test-controlled.
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(after, data) {
				t.Fatal("malformed control appended to the task log")
			}
			backend.mu.Lock()
			attachCalls := backend.attachCalls
			backend.mu.Unlock()
			if attachCalls != 0 {
				t.Fatalf("relay reconnect calls = %d, want 0", attachCalls)
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

			adopted, err := m.ImportInstances(t.Context(), []runtime.Instance{{ID: instanceID, State: "exited"}}, []*taskslog.LoadedTask{{
				TaskID: taskID.String(), Harness: harness.Claude, Prompt: "test",
			}})
			if err != nil {
				t.Fatalf("AdoptInstances: %v", err)
			}
			if len(adopted) != 1 {
				t.Fatalf("adopted len = %d, want 1", len(adopted))
			}
			if adopted[0].Task().ID != taskID {
				t.Fatalf("adopted task ID = %s, want %s", adopted[0].Task().ID, taskID)
			}
			if adopted[0].Task().Primary() != nil {
				t.Fatalf("primary repo = %#v, want none", adopted[0].Task().Primary())
			}
		})
		t.Run("valid_restores_branch_diff_for_exited_instance", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			instanceID := runtime.NewID("test-runtime", "restore-diff")
			info := &runtimetest.FakeInfo{Meta: map[string]string{
				"restore-diff\x00caic.id":      taskID.String(),
				"restore-diff\x00caic.harness": string(harness.Claude),
			}}
			runtimeBackend := &runtimetest.FakeBackend{DiffOutput: "10\t2\tfrontend/src/App.tsx\n5\t1\tfrontend/src/App.test.tsx\n"}
			m := newTestManager(t, Config{
				ServerCtx: t.Context(),
				Runtimes:  newTestRuntime(t, runtimeBackend, info),
				Backends:  map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{WireFactory: claudecode.New().NewWire}},
			})
			registerCheckout(t, m.Checkouts, "repo/a", &repo.Checkout{Dir: "/home/user/src/repo/a"})

			adopted, err := m.ImportInstances(t.Context(), []runtime.Instance{{
				ID:    instanceID,
				State: "exited",
				Repos: []runtime.Repo{{GitRoot: "/home/user/src/repo/a", Branch: "caic-9", ContainerPath: "/home/user/src/repo/a"}},
			}}, []*taskslog.LoadedTask{{
				TaskID: taskID.String(), Harness: harness.Claude, Prompt: "restore diff",
				Repos: []taskslog.RepoMount{{Name: "repo/a", BaseBranch: "main", Branch: "caic-9"}},
			}})
			if err != nil {
				t.Fatalf("AdoptInstances: %v", err)
			}
			if len(adopted) != 1 {
				t.Fatalf("adopted len = %d, want 1", len(adopted))
			}
			want := agent.DiffStat{
				{Path: "frontend/src/App.tsx", Added: 10, Deleted: 2},
				{Path: "frontend/src/App.test.tsx", Added: 5, Deleted: 1},
			}
			if got := adopted[0].Task().LiveDiffStat(); !slices.Equal(got, want) {
				t.Fatalf("LiveDiffStat = %+v, want %+v", got, want)
			}
		})
		t.Run("valid_restores_launch_config_from_log", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			fake := &runtimetest.FakeInfo{Meta: map[string]string{
				"restore-config\x00caic.id":      taskID.String(),
				"restore-config\x00caic.harness": string(harness.Claude),
			}}
			cacheDir := t.TempDir()
			m := newTestManager(t, Config{ServerCtx: t.Context(), LogStore: taskslog.NewStore(testLogger(), filepath.Join(cacheDir, "tasks")), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake), Backends: map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}})
			registerCheckout(t, m.Checkouts, "repo/a", &repo.Checkout{Dir: "/home/user/src/repo/a"})

			logDir := filepath.Join(cacheDir, "tasks")
			if err := os.MkdirAll(logDir, 0o750); err != nil {
				t.Fatal(err)
			}
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
			logs, err := taskslog.NewStore(testLogger(), logDir).LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}

			adopted, err := m.ImportInstances(t.Context(), []runtime.Instance{
				{ID: runtime.NewID("test-runtime", "restore-config"), State: "exited", Repos: []runtime.Repo{{GitRoot: "/home/user/src/repo/a", Branch: "caic-9", ContainerPath: "/home/user/src/repo/a"}}},
			}, logs)
			if err != nil {
				t.Fatalf("AdoptInstances: %v", err)
			}
			if len(adopted) != 1 {
				t.Fatalf("adopted len = %d, want 1", len(adopted))
			}
			got := adopted[0].Task()
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
			cacheDir := t.TempDir()
			m := newTestManager(t, Config{ServerCtx: t.Context(), LogStore: taskslog.NewStore(testLogger(), filepath.Join(cacheDir, "tasks")), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake), Backends: map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}})
			m.relay = fakeRelayReader{
				statusFn: func(context.Context, runtime.ConnectionTarget) (bool, string, error) {
					return true, "alive", nil
				},
				readTailFn: func(context.Context, runtime.ConnectionTarget, *agent.LogRecordParser, int64) ([]agent.TimedMessage, int64, error) {
					return relayParsed(&agent.TextMessage{Text: "during restart"}), 128, nil
				},
				readLogFn: func(context.Context, runtime.ConnectionTarget, int) string { return "" },
			}
			registerCheckout(t, m.Checkouts, "caic-xyz/caic", &repo.Checkout{Dir: "/home/user/src/caic-xyz/caic"})

			logDir := filepath.Join(cacheDir, "tasks")
			if err := os.MkdirAll(logDir, 0o750); err != nil {
				t.Fatal(err)
			}
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
			logs, err := taskslog.NewStore(testLogger(), logDir).LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}

			adopted, err := m.ImportInstances(t.Context(),

				[]runtime.Instance{
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
			if got := adopted[0].Task().RelayOffsetValue(); got != 128 {
				t.Fatalf("RelayOffset = %d, want 128", got)
			}
			texts := textMessages(adopted[0].Task().Messages())
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
				Runtimes:  newTestRuntime(t, &runtimetest.FakeBackend{}, fake),
				Backends:  map[harness.Name]agent.Backend{"reconnect": backend},
			})
			m.relay = fakeRelayReader{
				statusFn: func(context.Context, runtime.ConnectionTarget) (bool, string, error) {
					return true, "alive", nil
				},
				readTailFn: func(context.Context, runtime.ConnectionTarget, *agent.LogRecordParser, int64) ([]agent.TimedMessage, int64, error) {
					return relayParsed(
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
					), 599440, nil
				},
				readLogFn: func(context.Context, runtime.ConnectionTarget, int) string { return "" },
			}
			registerCheckout(t, m.Checkouts, "repo/a", &repo.Checkout{Dir: "/home/user/src/repo/a"})

			adopted, err := m.ImportInstances(t.Context(),

				[]runtime.Instance{
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
				}, []*taskslog.LoadedTask{{
					TaskID:     taskID.String(),
					Harness:    "reconnect",
					LogVersion: agent.LogVersionV1,
					Repos:      []taskslog.RepoMount{{Name: "repo/a", Branch: "caic-10"}},
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
			if adopted[0].Task().HasSession() {
				t.Fatal("adopted task attached without authoritative local log")
			}
			if adopted[0].LogPath.Get() != "" {
				t.Fatalf("replacement LogPath = %q, want empty", adopted[0].LogPath.Get())
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
			cacheDir := t.TempDir()
			m := newTestManager(t, Config{ServerCtx: t.Context(), LogStore: taskslog.NewStore(testLogger(), filepath.Join(cacheDir, "tasks")), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake), Backends: map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}})
			registerCheckout(t, m.Checkouts, "caic-xyz/caic", &repo.Checkout{Dir: "/home/user/src/caic-xyz/caic"})

			logDir := filepath.Join(cacheDir, "tasks")
			if err := os.MkdirAll(logDir, 0o750); err != nil {
				t.Fatal(err)
			}
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
			logs, err := taskslog.NewStore(testLogger(), logDir).LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}

			adopted, err := m.ImportInstances(ctx,

				[]runtime.Instance{
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
			if got := adopted[0].Task().GetState(); got != taskslog.StateCrashed {
				t.Errorf("state = %v, want crashed", got)
			}
			if adopted[0].Result() == nil {
				t.Fatal("entry result is nil")
			}
			if err := adopted[0].Result().Err; err == nil || !strings.Contains(err.Error(), "Unknown option: --approve") {
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
				readTailFn: func(context.Context, runtime.ConnectionTarget, *agent.LogRecordParser, int64) ([]agent.TimedMessage, int64, error) {
					return relayParsed(&agent.ExitMessage{ExitCode: 2, Error: "Unknown option: --approve"}), 128, nil
				},
				readLogFn: func(context.Context, runtime.ConnectionTarget, int) string { return "relay exited" },
			}
			registerCheckout(t, m.Checkouts, "caic-xyz/caic", &repo.Checkout{Dir: "/home/user/src/caic-xyz/caic"})

			adopted, err := m.ImportInstances(t.Context(),

				[]runtime.Instance{
					{
						ID:    runtime.NewID("test-runtime", "dead-relay-tail"),
						State: "running",
						Repos: []runtime.Repo{{
							GitRoot:       "/home/user/src/caic-xyz/caic",
							Branch:        "caic-8",
							ContainerPath: "/home/user/src/caic-xyz/caic",
						}},
					},
				}, []*taskslog.LoadedTask{{
					TaskID:     taskID.String(),
					Harness:    harness.Claude,
					LogVersion: agent.LogVersionV1,
					Repos:      []taskslog.RepoMount{{Name: "caic-xyz/caic", Branch: "caic-8"}},
				}})
			if err != nil {
				t.Fatalf("AdoptInstances: %v", err)
			}
			if len(adopted) != 1 {
				t.Fatalf("adopted len = %d, want 1", len(adopted))
			}
			if got := adopted[0].Task().GetState(); got != taskslog.StateCrashed {
				t.Fatalf("state = %v, want crashed", got)
			}
			if err := adopted[0].Result().Err; err == nil || !strings.Contains(err.Error(), "Unknown option: --approve") {
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
				readTailFn: func(context.Context, runtime.ConnectionTarget, *agent.LogRecordParser, int64) ([]agent.TimedMessage, int64, error) {
					return relayParsed(
						&agent.InitMessage{SessionID: "new-session"},
						&agent.ResultMessage{MessageType: "result", Subtype: "success", Result: "done"},
						&agent.ExitMessage{ExitCode: 2, Error: "stale crash"},
					), 256, nil
				},
				readLogFn: func(context.Context, runtime.ConnectionTarget, int) string { return "relay exited" },
			}
			registerCheckout(t, m.Checkouts, "caic-xyz/caic", &repo.Checkout{Dir: "/home/user/src/caic-xyz/caic"})

			adopted, err := m.ImportInstances(t.Context(),

				[]runtime.Instance{
					{
						ID:    runtime.NewID("test-runtime", "stale-tail"),
						State: "running",
						Repos: []runtime.Repo{{
							GitRoot:       "/home/user/src/caic-xyz/caic",
							Branch:        "caic-9",
							ContainerPath: "/home/user/src/caic-xyz/caic",
						}},
					},
				}, []*taskslog.LoadedTask{{
					TaskID:     taskID.String(),
					Harness:    harness.Claude,
					LogVersion: agent.LogVersionV1,
					Repos:      []taskslog.RepoMount{{Name: "caic-xyz/caic", Branch: "caic-9"}},
				}})
			if err != nil {
				t.Fatalf("AdoptInstances: %v", err)
			}
			if len(adopted) != 1 {
				t.Fatalf("adopted len = %d, want 1", len(adopted))
			}
			if got := adopted[0].Task().GetState(); got != taskslog.StateStopped {
				t.Fatalf("state = %v, want stopped", got)
			}
			if got := adopted[0].Task().LastExitError(); got != "" {
				t.Fatalf("LastExitError = %q, want stale error cleared", got)
			}
			if adopted[0].Result() != nil {
				t.Fatalf("entry result = %#v, want nil", adopted[0].Result())
			}
		})
		t.Run("valid_stale_crashed_trailer_does_not_crash_adopted_task", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			fake := &runtimetest.FakeInfo{Meta: map[string]string{
				"stale-trailer\x00caic.id":      taskID.String(),
				"stale-trailer\x00caic.harness": string(harness.Claude),
			}}
			cacheDir := t.TempDir()
			m := newTestManager(t, Config{ServerCtx: t.Context(), LogStore: taskslog.NewStore(testLogger(), filepath.Join(cacheDir, "tasks")), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake), Backends: map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}})
			registerCheckout(t, m.Checkouts, "caic-xyz/caic", &repo.Checkout{Dir: "/home/user/src/caic-xyz/caic"})

			logDir := filepath.Join(cacheDir, "tasks")
			if err := os.MkdirAll(logDir, 0o750); err != nil {
				t.Fatal(err)
			}
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
			loaded, err := taskslog.NewStore(testLogger(), logDir).LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}

			adopted, err := m.ImportInstances(t.Context(),

				[]runtime.Instance{
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
			if got := adopted[0].Task().GetState(); got != taskslog.StateStopped {
				t.Fatalf("state = %v, want stopped", got)
			}
			if got := adopted[0].Task().LastExitError(); got != "" {
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
			cacheDir := t.TempDir()
			m := newTestManager(t, Config{ServerCtx: t.Context(), LogStore: taskslog.NewStore(testLogger(), filepath.Join(cacheDir, "tasks")), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake), Backends: map[harness.Name]agent.Backend{harness.Codex: codex.New("", nil)}})
			registerCheckout(t, m.Checkouts, "caic-xyz/caic", &repo.Checkout{Dir: "/home/user/src/caic-xyz/caic"})

			logDir := filepath.Join(cacheDir, "tasks")
			if err := os.MkdirAll(logDir, 0o750); err != nil {
				t.Fatal(err)
			}
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
			logs, err := taskslog.NewStore(testLogger(), logDir).LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}

			adopted, err := m.ImportInstances(t.Context(),

				[]runtime.Instance{
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
			if got := adopted[0].Task().GetSessionID(); got != "thread-from-started" {
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
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
		tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
		tk.SetState(taskslog.StateRunning)
		m.Insert(tk.ID.String(), m.NewEntry(tk, nil))

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
		t.Run("buffers_before_import", func(t *testing.T) {
			t.Parallel()
			events := make(chan runtime.Event, 1)
			fake := &runtimetest.FakeInfo{Events: events}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake)})
			t.Cleanup(func() { _ = m.Close() })
			if err := m.BeginImport(); err != nil {
				t.Fatal(err)
			}

			events <- runtime.Event{InstanceID: "ctr-import", Kind: runtime.EventDie}
			deadline := time.Now().Add(time.Second)
			for {
				m.eventMu.Lock()
				buffered := len(m.pendingRuntimeEvents)
				m.eventMu.Unlock()
				if buffered == 1 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("runtime event was not buffered")
				}
				time.Sleep(time.Millisecond)
			}

			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-import"), runtime.ConnectionTarget{SSHHost: "ctr-import"}, "", "", 0)
			tk.SetState(taskslog.StateRunning)
			m.Insert(tk.ID.String(), m.NewEntry(tk, nil))
			if _, err := m.ImportInstances(t.Context(), []runtime.Instance{}, nil); err != nil {
				t.Fatal(err)
			}
			if got := tk.GetState(); got != taskslog.StateStopped {
				t.Fatalf("state = %v, want StateStopped", got)
			}
		})
		t.Run("unavailable_at_startup", func(t *testing.T) {
			t.Parallel()
			fake := &runtimetest.FakeInfo{WatchErr: errors.New("unavailable")}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake)})
			if err := m.BeginImport(); err == nil {
				t.Fatal("BeginImport succeeded, want runtime event watch error")
			}
			m.eventMu.Lock()
			importing := m.importing
			m.eventMu.Unlock()
			if importing {
				t.Fatal("startup import remained fenced after event subscription failed")
			}
		})
		t.Run("valid_dispatches_death", func(t *testing.T) {
			t.Parallel()
			events := make(chan runtime.Event, 1)
			fake := &runtimetest.FakeInfo{Events: events}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake)})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.Repos = []taskslog.RepoMount{{Name: "repo/x", Branch: "caic-1"}}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-dead"), runtime.ConnectionTarget{SSHHost: "ctr-dead"}, "", "", 0)
			tk.SetState(taskslog.StateRunning)
			m.Insert(tk.ID.String(), m.NewEntry(tk, nil))

			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			go m.watchRuntimeEvents(ctx, nil)

			events <- runtime.Event{InstanceID: "ctr-dead", Kind: runtime.EventDie}

			// Wait for the state transition rather than sleeping a fixed duration.
			deadline := time.Now().Add(2 * time.Second)
			for tk.GetState() != taskslog.StateStopped {
				if time.Now().After(deadline) {
					t.Fatalf("state = %v, want StateStopped after instance death", tk.GetState())
				}
				time.Sleep(time.Millisecond)
			}
		})
		t.Run("lifecycle_kinds", func(t *testing.T) {
			t.Parallel()
			for _, tt := range []struct {
				name    string
				harness harness.Name
				initial taskslog.State
				kind    runtime.EventKind
				want    taskslog.State
			}{
				{"start_restores_stopped_task", "fake", taskslog.StateStopped, runtime.EventStart, taskslog.StateWaiting},
				{"destroy_keeps_purged_task", "", taskslog.StatePurged, runtime.EventDestroy, taskslog.StatePurged},
			} {
				t.Run(tt.name, func(t *testing.T) {
					t.Parallel()
					m := newTestManager(t, Config{ServerCtx: t.Context()})
					tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, tt.harness, "")
					instanceID := runtime.NewID("test-runtime", runtime.InstanceID("ctr-"+tt.name))
					tk.SetRuntimeConnectionInfo(instanceID, runtime.ConnectionTarget{SSHHost: string(instanceID)}, "", "", 0)
					tk.SetState(tt.initial)
					m.Insert(tk.ID.String(), m.NewEntry(tk, nil))
					m.handleRuntimeEvent(t.Context(), runtime.Event{InstanceID: instanceID, Kind: tt.kind})
					if got := tk.GetState(); got != tt.want {
						t.Fatalf("state = %v, want %v", got, tt.want)
					}
				})
			}
			t.Run("oom_prevents_later_death_from_hiding_cause", func(t *testing.T) {
				t.Parallel()
				m := newTestManager(t, Config{ServerCtx: t.Context()})
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
				tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-oom"), runtime.ConnectionTarget{SSHHost: "ctr-oom"}, "", "", 0)
				tk.SetState(taskslog.StateRunning)
				m.Insert(tk.ID.String(), m.NewEntry(tk, nil))
				m.handleRuntimeEvent(t.Context(), runtime.Event{InstanceID: runtime.NewID("test-runtime", "ctr-oom"), Kind: runtime.EventOOM})
				m.handleRuntimeEvent(t.Context(), runtime.Event{InstanceID: runtime.NewID("test-runtime", "ctr-oom"), Kind: runtime.EventDie})
				if got := tk.GetState(); got != taskslog.StateCrashed {
					t.Fatalf("state = %v, want StateCrashed", got)
				}
			})
			t.Run("unknown_is_ignored", func(t *testing.T) {
				t.Parallel()
				m := newTestManager(t, Config{ServerCtx: t.Context()})
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
				tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-unknown"), runtime.ConnectionTarget{SSHHost: "ctr-unknown"}, "", "", 0)
				tk.SetState(taskslog.StateRunning)
				m.Insert(tk.ID.String(), m.NewEntry(tk, nil))
				m.handleRuntimeEvent(t.Context(), runtime.Event{InstanceID: runtime.NewID("test-runtime", "ctr-unknown"), Kind: "unknown"})
				if got := tk.GetState(); got != taskslog.StateRunning {
					t.Fatalf("state = %v, want StateRunning", got)
				}
			})
		})
	})
}

// TestLoadUnsettledTasks covers LoadUnsettledTasks: the plain (uncompressed)
// scan is loaded uncapped, so live and just-terminal tasks are not lost, and
// running tasks are coerced to failed.
func TestLoadUnsettledTasks(t *testing.T) {
	t.Parallel()
	t.Run("UncappedPerRepo", func(t *testing.T) {
		t.Parallel()
		m := newTestManager(t, Config{ServerCtx: t.Context()})
		now := time.Now().UTC()
		all := make([]*taskslog.LoadedTask, 0, 7)
		for i := range 7 {
			all = append(all, &taskslog.LoadedTask{
				TaskID:            ksid.NewID().String(),
				Prompt:            "unsettled",
				Repos:             []taskslog.RepoMount{{Name: "repo/a"}},
				State:             taskslog.StateStopped,
				LastStateUpdateAt: now.Add(time.Duration(i) * time.Second),
			})
		}
		if err := m.LoadUnsettledTasks(all); err != nil {
			t.Fatalf("LoadUnsettledTasks: %v", err)
		}
		if got := m.Len(); got != 7 {
			t.Errorf("Len() = %d, want 7 (unsettled is uncapped)", got)
		}
	})
	t.Run("RunningBecomesFailed", func(t *testing.T) {
		t.Parallel()
		m := newTestManager(t, Config{ServerCtx: t.Context()})
		id := ksid.NewID()
		if err := m.LoadUnsettledTasks([]*taskslog.LoadedTask{{
			TaskID:            id.String(),
			Prompt:            "was running",
			Repos:             []taskslog.RepoMount{{Name: "repo/a"}},
			State:             taskslog.StateRunning,
			LastStateUpdateAt: time.Now().UTC(),
		}}); err != nil {
			t.Fatalf("LoadUnsettledTasks: %v", err)
		}
		e, ok := m.GetEntry(id.String())
		if !ok {
			t.Fatal("entry not found")
		}
		if got := e.Task().GetState(); got != taskslog.StateFailed {
			t.Errorf("state = %v, want StateFailed (running coerced)", got)
		}
	})
}

func TestAllocateBranches(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Config{ServerCtx: t.Context()})
	// Fake dirs: Init's branch scan fails and is ignored, leaving nextID at 0, so
	// the first ReserveBranchName on each repo yields caic-0.
	registerCheckout(t, m.Checkouts, "acme/app", &repo.Checkout{Dir: "/tmp/app"})
	registerCheckout(t, m.Checkouts, "caic-xyz/caic", &repo.Checkout{Dir: "/tmp/caic"})

	// Forking a 2-repo task carries both repos on their source branches. Every
	// repo — primary and non-primary alike — is reallocated to its own fresh
	// branch (from its own checkout), so the fork never shares a branch with the
	// still-checked-out source instance. Index 0 is not special.
	mounts := []taskslog.RepoMount{
		{Name: "acme/app", Branch: "caic-2"},
		{Name: "caic-xyz/caic", Branch: "caic-18"},
	}
	tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
	tk.Repos = slices.Clone(mounts)
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

	// A repo with no registered checkout is a hard error.
	bad := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
	bad.Repos = []taskslog.RepoMount{{Name: "ghost/repo", Branch: "caic-1"}}
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
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
		tk.SeedTimeline([]agent.Message{
			&agent.ResultMessage{MessageType: "result", Result: "first"},
			&agent.ResultMessage{MessageType: "result", Result: "last"},
		})
		if got := lastResultText(tk); got != "last" {
			t.Errorf("lastResultText = %q, want %q", got, "last")
		}
	})
	t.Run("valid_no_result", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
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
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
		if !needsTitleRegen(tk, nil, resolver) {
			t.Error("needsTitleRegen should return true when lt is nil")
		}
	})
	t.Run("valid_no_title_in_log", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
		lt := &taskslog.LoadedTask{TaskID: "test", Prompt: "test"}
		if !needsTitleRegen(tk, lt, resolver) {
			t.Error("needsTitleRegen should return true when lt.Title is empty")
		}
	})
	t.Run("valid_more_results_in_memory", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
		tk.SeedTimeline([]agent.Message{
			&agent.ResultMessage{MessageType: "result"},
			&agent.ResultMessage{MessageType: "result"},
		})
		lt := &taskslog.LoadedTask{
			TaskID: "test",
			Title:  "existing title",
			Timeline: []agent.TimedMessage{
				{Message: &agent.ResultMessage{MessageType: "result"}},
			},
		}
		if !needsTitleRegen(tk, lt, resolver) {
			t.Error("needsTitleRegen should return true when restoredResults > logResults")
		}
	})
	t.Run("valid_same_count", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
		tk.SeedTimeline([]agent.Message{
			&agent.ResultMessage{MessageType: "result"},
		})
		lt := &taskslog.LoadedTask{
			TaskID: "test",
			Title:  "existing title",
			Timeline: []agent.TimedMessage{
				{Message: &agent.ResultMessage{MessageType: "result"}},
			},
		}
		if needsTitleRegen(tk, lt, resolver) {
			t.Error("needsTitleRegen should return false when counts match")
		}
	})
	t.Run("valid_large_log_skips", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
		lt := &taskslog.LoadedTask{
			TaskID:  "test",
			Title:   "existing title",
			LogSize: 200 << 20, // 200 MiB, above maxLogSize
		}
		if needsTitleRegen(tk, lt, resolver) {
			t.Error("needsTitleRegen should return false for large logs")
		}
	})
}

// errTaskNotFound is a test fixture for the not-found error shape.
// Production builds not-found errors with notFoundf.
var errTaskNotFound = &Error{Kind: KindNotFound, Msg: "task not found"}

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

// TestSettledLoadState verifies the background settled-history pass state
// machine exposed to the task-list stream.
func TestSettledLoadState(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Config{ServerCtx: t.Context()})
	t.Cleanup(func() { _ = m.Close() })

	// A fresh Manager has not completed a pass, so it reports loading.
	if loading, err := m.SettledStatus(); !loading || err != "" {
		t.Fatalf("initial SettledStatus = (%v, %q), want (true, \"\")", loading, err)
	}

	m.CompleteSettledLoad(errors.New("prior failure"))
	if loading, _ := m.SettledStatus(); loading {
		t.Fatal("after CompleteSettledLoad: SettledLoading = true, want false")
	}
	if _, got := m.SettledStatus(); got != "prior failure" {
		t.Fatalf("after CompleteSettledLoad(err): SettledError = %q, want %q", got, "prior failure")
	}

	m.CompleteSettledLoad(nil)
	if loading, _ := m.SettledStatus(); loading {
		t.Fatal("after clean CompleteSettledLoad: SettledLoading = true, want false")
	}
	if _, got := m.SettledStatus(); got != "" {
		t.Fatalf("after clean CompleteSettledLoad: SettledError = %q, want empty", got)
	}

	// A clean completion clears a prior error, so the state cannot report a
	// stale failure across a retry or second pass.
	m.CompleteSettledLoad(errors.New("stale failure"))
	if _, got := m.SettledStatus(); got != "stale failure" {
		t.Fatalf("CompleteSettledLoad(err): SettledError = %q, want %q", got, "stale failure")
	}
	m.CompleteSettledLoad(nil)
	if _, got := m.SettledStatus(); got != "" {
		t.Fatalf("CompleteSettledLoad(nil) after failure: SettledError = %q, want empty", got)
	}
}

func TestRegisteredLogPaths(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logStore := taskslog.NewStore(testLogger(), dir)
	m := newTestManager(t, Config{ServerCtx: t.Context(), LogStore: logStore})

	meta, err := json.Marshal(agent.MetaMessage{
		MessageType: "caic_meta",
		Version:     int(agent.LogVersionV1),
		Harness:     harness.Claude,
		Prompt:      "history",
	})
	if err != nil {
		t.Fatal(err)
	}
	trailer, err := json.Marshal(agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "alpha.jsonl")
	data := make([]byte, 0, len(meta)+len(trailer)+2)
	data = append(data, meta...)
	data = append(data, '\n')
	data = append(data, trailer...)
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	logs, err := logStore.LoadUnsettled()
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("LoadUnsettled = %d logs, want 1", len(logs))
	}
	if err := m.LoadUnsettledTasks(logs); err != nil {
		t.Fatal(err)
	}
	if paths := m.RegisteredLogPaths(); len(paths) != 1 {
		t.Fatalf("RegisteredLogPaths = %v, want exactly %q", paths, path)
	}
	if _, ok := m.RegisteredLogPaths()[filepath.Clean(path)]; !ok {
		t.Errorf("RegisteredLogPaths missing %q: %v", path, m.RegisteredLogPaths())
	}
}

// TestLoadersConcurrentWithNotify guards the loader notification path against
// the data race the background pass would otherwise take: the loaders notify
// task watchers while the SSE handlers (and other lifecycle code) call
// NotifyTaskChange/Changed on the running server. Run with -race.
func TestLoadersConcurrentWithNotify(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Config{ServerCtx: t.Context()})

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			lt := &taskslog.LoadedTask{
				TaskID: fmt.Sprintf("load-%d", i),
				State:  taskslog.StatePurged,
			}
			if err := m.LoadPurgedTasks([]*taskslog.LoadedTask{lt}); err != nil {
				t.Error(err)
				return
			}
			if err := m.LoadUnsettledTasks([]*taskslog.LoadedTask{
				{TaskID: fmt.Sprintf("unsettled-%d", i), State: taskslog.StateRunning},
			}); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			m.NotifyTaskChange()
			_ = m.Changed()
		}
	}()
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}

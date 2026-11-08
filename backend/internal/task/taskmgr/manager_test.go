// Tests task manager registry, pending prompt delivery, and lifecycle behavior.

package taskmgr

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/taskslog/data/inputqueue"

	taskhistory "github.com/caic-xyz/caic/backend/internal/task/history"

	logdata "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
	"github.com/klauspost/compress/zstd"

	"github.com/maruel/genai"
	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/backends"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/codex"
	"github.com/caic-xyz/caic/backend/internal/agent/dsh"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/agent/opencode"
	"github.com/caic-xyz/caic/backend/internal/agent/pi"
	"github.com/caic-xyz/caic/backend/internal/gittest"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/mdruntime"
	"github.com/caic-xyz/caic/backend/internal/runtime/runtimetest"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/task/taskrun"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	"github.com/caic-xyz/caic/metrics"
	"github.com/maruel/gomode/mcp"
	"github.com/maruel/gomode/mcp/mcptest"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

type titleProvider struct {
	genai.Provider

	calls atomic.Int32
}

func (p *titleProvider) GenSync(context.Context, genai.Messages, ...genai.GenOption) (genai.Result, error) {
	p.calls.Add(1)
	return genai.Result{Replies: []genai.Reply{{Text: "Generated title"}}}, nil
}

type endedStatsInfo struct {
	runtimetest.FakeInfo
}

func (f *endedStatsInfo) WatchStats(ctx context.Context, ids []runtime.ID) (iter.Seq2[runtime.StatsSample, error], error) {
	select {
	case f.WatchStarted <- slices.Clone(ids):
	case <-ctx.Done():
	}
	return func(func(runtime.StatsSample, error) bool) {}, nil
}

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
	testRuntimeBackend
	runtimetest.FakeInfo
}

func (*testRuntimeSystem) Name() runtime.Name { return "test-runtime" }

type testRuntimeBackend interface {
	runtime.Files
	runtime.Lifecycle
	runtime.Repository
}

type testRuntimeInfoSystem struct {
	testRuntimeBackend
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

type fakeTaskMCPScoper struct {
	taskID   ksid.ID
	registry mcp.Registry
}

func (f *fakeTaskMCPScoper) ForTask(id ksid.ID) mcp.Registry {
	f.taskID = id
	return f.registry
}

// newColdQueryManager registers a cold entry over the task logs in dir. The
// returned counter reports ordered log decodes, which each cold query performs
// at most once per log identity.
func newColdQueryManager(t *testing.T, dir string) (*Manager, *Entry, *int) {
	scans := new(int)
	agentBackends := agent.Backends{harness.Claude: &agenttest.FakeBackend{WireFactory: func() agent.WireFormat {
		(*scans)++
		return claudecode.New().NewWire()
	}}}
	store := taskhistory.NewStore(testLogger(), taskslog.NewStore(dir), agentBackends)
	logs, err := store.LoadUnsettled()
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("loaded %d task logs, want 1", len(logs))
	}
	m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: agentBackends, LogStore: store})
	entry := m.NewEntry(mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "task"}, "", ""), logs[0])
	return m, entry, scans
}

// writeColdQueryLog writes one plain task log with a Claude header and the given records.
func writeColdQueryLog(t *testing.T, path string, lines ...string) {
	data := `{"version":3,"prompt":"task","repos":[],"harness":"claude","t":"caic_meta"}` + "\n" + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// appendColdQueryLog adds one record to a plain task log.
func appendColdQueryLog(t *testing.T, path, line string) {
	f, err := os.OpenFile(filepath.Clean(path), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteString(line + "\n")
	if err := errors.Join(writeErr, f.Close()); err != nil {
		t.Fatal(err)
	}
}

func newTestManager(t testing.TB, cfg Config) *Manager { //nolint:gocritic // Config mirrors New's value bag in tests.
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Rollup == nil {
		cfg.Rollup = task.DiscardRollup{}
	}
	if cfg.Backends == nil {
		cfg.Backends = backends.Default("", nil)
	}
	if cfg.LogStore == nil {
		cfg.LogStore = taskhistory.NewStore(testLogger(), taskslog.NewStore(t.TempDir()), cfg.Backends)
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
	if cfg.EnsureSessionTimeout == 0 {
		cfg.EnsureSessionTimeout = 30 * time.Millisecond
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

// recordTestTaskLog constructs the durable history owned by a registered fixture.
func recordTestTaskLog(t testing.TB, m *Manager, e *Entry) {
	log, path, err := m.logStore.Open(e.Task().LogFilename(), e.Task().LogHeader())
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	e.LogPath.Set(path)
}

func awaitTaskCleanup(t *testing.T, m *Manager, id ksid.ID) {
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

func checkPublishedTaskLog(t *testing.T, m *Manager, create func() (ksid.ID, error)) {
	// Hold insertion after its registry notification, before the creator can
	// start asynchronous runtime work. Inspect exactly what readers can see.
	m.quotaWatchMu.Lock()
	_, changed := m.ChangeSnapshot()
	var workers sync.WaitGroup
	var createErr error
	done := make(chan struct{})
	workers.Go(func() {
		_, createErr = create()
		close(done)
	})
	t.Cleanup(func() {
		m.quotaWatchMu.Unlock()
		workers.Wait()
		if createErr != nil {
			t.Errorf("create task: %v", createErr)
		}
	})
	select {
	case <-changed:
	case <-done:
		t.Fatalf("creation returned before publication: %v", createErr)
	case <-time.After(time.Second):
		t.Fatal("task was not published")
	}
	for _, e := range m.Entries() {
		if e.Task().InitialPrompt.Text != "publication" {
			continue
		}
		path := e.LogPath.Get()
		if path == "" {
			t.Fatal("published task has no history log path")
		}
		loaded, err := taskhistory.NewStore(slog.New(slog.DiscardHandler), taskslog.NewStore(filepath.Dir(path)), backends.Default("", nil)).LoadSource(path)
		if err != nil {
			t.Fatalf("read published history: %v", err)
		}
		if loaded.TaskID != e.Task().ID.String() || loaded.Primary() == nil || loaded.Primary().Branch == "" {
			t.Fatalf("published log has incomplete identity: %#v", loaded)
		}
		return
	}
	t.Fatal("published task missing")
}

func BenchmarkManagerCreate(b *testing.B) {
	m := newTestManager(b, Config{ServerCtx: b.Context(), Backends: agent.Backends{"fake": &agenttest.FakeBackend{}}})
	b.Cleanup(func() {
		if err := m.Close(); err != nil {
			b.Error(err)
		}
	})
	registerCheckout(b, m.Checkouts, "my/repo", &repo.Checkout{Dir: b.TempDir()})
	p := CreateParams{Prompt: agent.Prompt{Text: strings.Repeat("Implement the requested change. ", 8)}, Repos: []CreateRepo{{Name: "my/repo"}}, Harness: "fake"}
	b.ReportAllocs()
	for b.Loop() {
		id, err := m.Create(b.Context(), p)
		if err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		e, _ := m.GetEntry(id)
		e.Lifecycle.wg.Wait()
		m.mu.Lock()
		delete(m.tasks, id)
		m.mu.Unlock()
		b.StartTimer()
	}
}

func newTestRuntime(t testing.TB, backend testRuntimeBackend, info testRuntimeInfo) *runtime.Router {
	var sys runtime.System = &testRuntimeSystem{testRuntimeBackend: backend}
	if info != nil {
		sys = testRuntimeInfoSystem{testRuntimeBackend: backend, Monitor: info, Inventory: info, PrivilegeInfo: info}
	}
	router, err := runtime.NewRouter([]runtime.System{sys}, metrics.Nop{})
	if err != nil {
		t.Fatalf("runtime.NewRouter: %v", err)
	}
	return router
}

type fakeRelayReader struct {
	statusFn   func(context.Context, runtime.ConnectionTarget) (bool, string, error)
	readTailFn func(context.Context, runtime.ConnectionTarget, int64) (agent.RelaySnapshot, int64, error)
	readLogFn  func(context.Context, runtime.ConnectionTarget, int) string
}

// deadRelayFixture isolates history-import tests from real SSH probes.
func deadRelayFixture() fakeRelayReader {
	return fakeRelayReader{
		statusFn: func(context.Context, runtime.ConnectionTarget) (bool, string, error) { return false, "dead", nil },
		readTailFn: func(context.Context, runtime.ConnectionTarget, int64) (agent.RelaySnapshot, int64, error) {
			return agent.RelaySnapshot{}, 0, nil
		},
		readLogFn: func(context.Context, runtime.ConnectionTarget, int) string { return "" },
	}
}

func (f fakeRelayReader) Status(ctx context.Context, target runtime.ConnectionTarget) (alive bool, diag string, err error) {
	return f.statusFn(ctx, target)
}

func (f fakeRelayReader) ReadTail(ctx context.Context, target runtime.ConnectionTarget, maxBytes int64) (timeline agent.RelaySnapshot, size int64, err error) {
	return f.readTailFn(ctx, target, maxBytes)
}

func (f fakeRelayReader) ReadLog(ctx context.Context, target runtime.ConnectionTarget, maxBytes int) string {
	return f.readLogFn(ctx, target, maxBytes)
}

func parsedRelayFixture(records ...string) (agent.RelaySnapshot, int64, error) {
	var timeline agent.RelaySnapshot
	var generation string
	for _, line := range records {
		frame, err := taskslog.DecodeFrame([]byte(line))
		parsed := agent.ParsedRecord{Provenance: agent.FrameProvenance(&frame)}
		if err != nil {
			return agent.RelaySnapshot{}, 0, err
		}
		if parsed.Provenance.Generation != "" {
			generation = parsed.Provenance.Generation
		}
		timeline.Encoded = append(timeline.Encoded, line...)
		timeline.Encoded = append(timeline.Encoded, '\n')
		if parsed.Provenance.Source == agent.RecordSourceRelay {
			timeline.RelayRecords = append(timeline.RelayRecords, agent.RelayRecordBoundary{
				Generation: generation, RelayEnd: int64(len(timeline.Encoded)), ByteEnd: len(timeline.Encoded),
				Fingerprint: sha256.Sum256([]byte(line)),
			})
		}
	}
	return timeline, int64(len(timeline.Encoded)), nil
}

// waitingLaunchBackend holds startup until the test releases it.
type waitingLaunchBackend struct {
	*runtimetest.FakeBackend

	release <-chan struct{}
}

func (b *waitingLaunchBackend) Launch(context.Context, []runtime.Repo, *runtime.StartOptions) (runtime.ID, error) {
	<-b.release
	return "", errors.New("startup released")
}

// blockingStopBackend blocks in Stop until release is closed (or the context is
// cancelled), widening the Stop/Purge race window. started and returned signal
// the call's entry and exit. onPurge observes the entry when the runtime purge
// runs, which is where a purge overtaking a stop becomes visible.
type blockingStopBackend struct {
	*runtimetest.FakeBackend

	started  chan struct{}
	returned chan struct{}
	release  chan struct{}
	onPurge  func()
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

func (b *blockingStopBackend) Purge(ctx context.Context, id runtime.ID) error {
	if b.onPurge != nil {
		b.onPurge()
	}
	return b.FakeBackend.Purge(ctx, id)
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

// blockingPurgeBackend reports when Purge starts and blocks until release is
// closed, allowing tests to observe the task while cleanup is in progress.
type blockingPurgeBackend struct {
	*runtimetest.FakeBackend

	started chan struct{}
	release chan struct{}
}

func (b *blockingPurgeBackend) Purge(ctx context.Context, id runtime.ID) error {
	close(b.started)
	select {
	case <-b.release:
		return b.FakeBackend.Purge(ctx, id)
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

	attachCalls     int
	prompts         []agent.Prompt
	opts            *agent.Options
	attached        chan struct{}
	exitImmediately bool
	cancel          context.CancelFunc
	session         *agent.Session
}

func (b *reconnectInputBackend) AttachRelay(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	cmdCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cmdCtx, "python3", "-c", "import sys; sys.stdin.buffer.read()")
	if b.exitImmediately {
		cmd = exec.CommandContext(cmdCtx, "true")
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	c := &reconnectInputConn{Conn: agent.NewConn(cmdCtx, opts.Logger, stdin, opts.Records, nil), backend: b, cancel: cancel}
	session := agent.NewSession(ctx, cmd, c, stdout, opts.Logger)
	b.mu.Lock()
	b.attachCalls++
	b.opts = opts
	b.cancel = cancel
	b.session = session
	if b.attached != nil {
		select {
		case b.attached <- struct{}{}:
		default:
		}
	}
	b.mu.Unlock()
	return session, nil
}

func (b *reconnectInputBackend) NewWire() agent.WireFormat {
	if b.HarnessName == harness.Claude {
		return claudecode.New().NewWire()
	}
	return codex.New("", nil).NewWire()
}
func (b *reconnectInputBackend) Start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	return b.AttachRelay(ctx, opts)
}

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
	agent.Conn

	backend *reconnectInputBackend
	cancel  context.CancelFunc
}

func (c *reconnectInputConn) SendPrompt(p agent.Prompt, raw []byte) error {
	c.backend.mu.Lock()
	c.backend.prompts = append(c.backend.prompts, p)
	c.backend.mu.Unlock()
	return c.Conn.SendPrompt(p, raw)
}

func (c *reconnectInputConn) SendStop(context.Context) {
	c.cancel()
}

func (c *reconnectInputConn) Cancel() { c.cancel() }

func textMessages(msgs []agent.Message) []string {
	texts := make([]string, 0, len(msgs))
	for _, msg := range msgs {
		if text, ok := msg.(*agent.TextMessage); ok {
			texts = append(texts, text.Text)
		}
	}
	return texts
}

// BenchmarkManagerOfflineImport measures startup inventory, physical reconciliation,
// persistence and task projection for a long session with an offline relay suffix.
// SSH reads a local relay fixture through a subprocess shim; log I/O remains real.
func BenchmarkManagerOfflineImport(b *testing.B) {
	shimDir := b.TempDir()
	shim := `#!/usr/bin/python3
import os, shlex, subprocess, sys
args = sys.argv[1:]
while args[0] == "-o":
    args = args[2:]
command = " ".join(args[1:]).replace("RELAY_OUTPUT_PATH", shlex.quote(os.environ["CAIC_BENCH_RELAY_OUTPUT"]))
sys.exit(subprocess.run(command, shell=True).returncode)
`
	shim = strings.ReplaceAll(shim, "RELAY_OUTPUT_PATH", agent.RelayOutputPath)
	if err := os.WriteFile(filepath.Join(shimDir, "ssh"), []byte(shim), 0o700); err != nil { //nolint:gosec // Executable benchmark SSH boundary.
		b.Fatal(err)
	}
	b.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, size := range []int{1 << 20, 10 << 20} {
		b.Run(fmt.Sprintf("MiB%d", size>>20), func(b *testing.B) {
			id := ksid.NewID()
			store := taskhistory.NewStore(testLogger(), taskslog.NewStore(b.TempDir()), backends.Default("", nil))
			tk := mustNewTask(b, id, agent.Prompt{Text: "initial"}, harness.OpenCode, "")
			meta := tk.LogHeader()
			meta.Version = int(agent.LogVersionV3)
			header, err := taskslog.EncodeHeader((*logdata.MetaMessage)(meta))
			if err != nil {
				b.Fatal(err)
			}
			generation := `{"t":"relay_generation","generation":"benchmark"}`
			chunk := `{"t":"agent","ts":1.000,"msg":{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"` + strings.Repeat("x", 2048) + `"}}}}}`
			result := `{"t":"agent","ts":2.000,"msg":{"jsonrpc":"2.0","id":9,"result":{"stopReason":"end_turn"}}}`
			request := `{"t":"input","ts":3.000,"msg":{"jsonrpc":"2.0","id":17,"method":"session/prompt","params":{"sessionId":"s","prompt":[{"type":"text","text":"initial"}]}}}`
			failure := `{"t":"agent","ts":4.000,"msg":{"jsonrpc":"2.0","id":17,"error":{"code":-32000,"message":"offline prompt rejected"}}}`
			var fixture, remote bytes.Buffer
			fixture.Write(header)
			fmt.Fprintf(&fixture, "\n%s\n", generation)
			fmt.Fprintf(&remote, "%s\n", generation)
			for fixture.Len() < size {
				fmt.Fprintf(&fixture, "%s\n%s\n", chunk, result)
				fmt.Fprintf(&remote, "%s\n%s\n", chunk, result)
			}
			fmt.Fprintf(&fixture, "%s\n", request)
			fmt.Fprintf(&remote, "%s\n", failure)
			remotePath := filepath.Join(b.TempDir(), "output.jsonl")
			if err := os.WriteFile(remotePath, remote.Bytes(), 0o600); err != nil {
				b.Fatal(err)
			}
			b.Setenv("CAIC_BENCH_RELAY_OUTPUT", remotePath)
			path := filepath.Join(store.Journal.LogDir, tk.LogFilename())
			b.ReportAllocs()
			b.SetBytes(int64(fixture.Len()))
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				if err := os.WriteFile(path, fixture.Bytes(), 0o600); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				m := newTestManager(b, Config{ServerCtx: b.Context(), LogStore: store,
					Runtimes: newTestRuntime(b, &runtimetest.FakeBackend{}, &runtimetest.FakeInfo{Meta: map[string]string{
						"md-agent-benchmark\x00caic.id": id.String(), "md-agent-benchmark\x00caic.harness": string(harness.OpenCode),
					}}), Backends: map[harness.Name]agent.Backend{harness.OpenCode: opencode.New("", nil)}})
				m.relay = fakeRelayReader{
					statusFn: func(context.Context, runtime.ConnectionTarget) (bool, string, error) { return false, "dead", nil },
					readTailFn: func(ctx context.Context, _ runtime.ConnectionTarget, maxBytes int64) (agent.RelaySnapshot, int64, error) {
						return agent.ReadRelayTail(ctx, "benchmark", maxBytes)
					},
					readLogFn: func(context.Context, runtime.ConnectionTarget, int) string { return "" },
				}
				logs, err := store.LoadForTaskIDs([]string{id.String()})
				if err != nil {
					b.Fatal(err)
				}
				entries, err := m.ImportInstances(b.Context(), []runtime.Instance{{ID: runtime.NewID("test-runtime", "md-agent-benchmark"), State: "running"}}, logs)
				if err != nil || len(entries) != 1 {
					b.Fatalf("import = %v, %v", entries, err)
				}
				if err := m.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkUnionRuntimeIDs(b *testing.B) {
	for _, n := range []int{1, 5, 20} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			a := make([]runtime.ID, n)
			other := make([]runtime.ID, n)
			for i := range n {
				a[i] = runtime.ID(fmt.Sprintf("docker:a-%d", i))
				other[i] = runtime.ID(fmt.Sprintf("docker:a-%d", i+n/2))
			}
			b.ReportAllocs()
			for b.Loop() {
				if got := unionRuntimeIDs(a, other); len(got) == 0 {
					b.Fatal("empty union")
				}
			}
		})
	}
}

func TestUnionRuntimeIDs(t *testing.T) {
	t.Parallel()
	got := unionRuntimeIDs(
		[]runtime.ID{"docker:three", "docker:one", "docker:one"},
		[]runtime.ID{"docker:two", "docker:three"},
	)
	want := []runtime.ID{"docker:one", "docker:three", "docker:two"}
	if !slices.Equal(got, want) {
		t.Fatalf("union = %v, want %v", got, want)
	}
}

func TestRelaySnapshotAppend(t *testing.T) {
	t.Parallel()
	fingerprint := func(value string) [32]byte { return sha256.Sum256([]byte(value)) }
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name       string
			generation string
			log        []agent.RelayRecordBoundary
			snapshot   agent.RelaySnapshot
			want       string
		}{
			{name: "empty_snapshot", log: []agent.RelayRecordBoundary{{Generation: "g", RelayEnd: 20}}},
			{name: "first_output", snapshot: agent.RelaySnapshot{RelayRecords: []agent.RelayRecordBoundary{{RelayEnd: 6, ByteEnd: 6}}, Encoded: []byte("first\n")}, want: "first\n"},
			{name: "snapshot_starts_before_overlap", generation: "g",
				log: []agent.RelayRecordBoundary{{Generation: "g", RelayEnd: 20}, {Generation: "g", RelayEnd: 30}},
				snapshot: agent.RelaySnapshot{RelayRecords: []agent.RelayRecordBoundary{
					{Generation: "g", RelayEnd: 10, ByteEnd: 6}, {Generation: "g", RelayEnd: 20, ByteEnd: 13},
					{Generation: "g", RelayEnd: 30, ByteEnd: 21}, {Generation: "g", RelayEnd: 40, ByteEnd: 27},
				}, Encoded: []byte("older\nbefore\noverlap\nafter\n")}, want: "after\n"},
			{name: "adjacent_identical_records_have_distinct_offsets", generation: "g",
				log: []agent.RelayRecordBoundary{{Generation: "g", RelayEnd: 10}, {Generation: "g", RelayEnd: 20}},
				snapshot: agent.RelaySnapshot{RelayRecords: []agent.RelayRecordBoundary{
					{Generation: "g", RelayEnd: 20, ByteEnd: 7}, {Generation: "g", RelayEnd: 30, ByteEnd: 14},
				}, Encoded: bytes.Repeat([]byte("repeat\n"), 2)}, want: "repeat\n"},
			{name: "generation_reset", generation: "old", log: []agent.RelayRecordBoundary{{Generation: "old", RelayEnd: 100}},
				snapshot: agent.RelaySnapshot{RelayRecords: []agent.RelayRecordBoundary{{Generation: "new", RelayEnd: 4, ByteEnd: 4}, {Generation: "new", RelayEnd: 10, ByteEnd: 10}}, Encoded: []byte("new\nafter\n")}, want: "new\nafter\n"},
			{name: "unmarked_generation_reset", log: []agent.RelayRecordBoundary{{RelayEnd: 100, Fingerprint: fingerprint("old")}},
				snapshot: agent.RelaySnapshot{RelayRecords: []agent.RelayRecordBoundary{{Generation: "new", RelayEnd: 4, ByteEnd: 4}}, Encoded: []byte("new\n")}, want: "new\n"},
			{name: "unmarked_exact_bytes", log: []agent.RelayRecordBoundary{{Fingerprint: fingerprint("before")}, {Fingerprint: fingerprint("overlap")}},
				snapshot: agent.RelaySnapshot{RelayRecords: []agent.RelayRecordBoundary{{ByteEnd: 6, Fingerprint: fingerprint("older")}, {ByteEnd: 13, Fingerprint: fingerprint("before")}, {ByteEnd: 21, Fingerprint: fingerprint("overlap")}, {ByteEnd: 27, Fingerprint: fingerprint("after")}}, Encoded: []byte("older\nbefore\noverlap\nafter\n")}, want: "after\n"},
			{name: "unmarked_discontinuous_prefix", log: []agent.RelayRecordBoundary{{Fingerprint: fingerprint("unmatched")}, {Fingerprint: fingerprint("before")}, {Fingerprint: fingerprint("overlap")}},
				snapshot: agent.RelaySnapshot{RelayRecords: []agent.RelayRecordBoundary{{ByteEnd: 6, Fingerprint: fingerprint("older")}, {ByteEnd: 13, Fingerprint: fingerprint("before")}, {ByteEnd: 21, Fingerprint: fingerprint("overlap")}, {ByteEnd: 27, Fingerprint: fingerprint("after")}}, Encoded: []byte("older\nbefore\noverlap\nafter\n")}, want: "after\n"},
			{name: "unmarked_single_record_at_source_start", log: []agent.RelayRecordBoundary{{Fingerprint: fingerprint("overlap")}},
				snapshot: agent.RelaySnapshot{RelayRecords: []agent.RelayRecordBoundary{{ByteEnd: 8, Fingerprint: fingerprint("overlap")}, {ByteEnd: 14, Fingerprint: fingerprint("after")}}, Encoded: []byte("overlap\nafter\n")}, want: "after\n"},
			{name: "unmarked_to_seeded_offsets", log: []agent.RelayRecordBoundary{{Fingerprint: fingerprint("before")}, {Fingerprint: fingerprint("overlap")}},
				snapshot: agent.RelaySnapshot{RelayRecords: []agent.RelayRecordBoundary{{Generation: "g", RelayEnd: 20, ByteEnd: 7, Fingerprint: fingerprint("before")}, {Generation: "g", RelayEnd: 30, ByteEnd: 15, Fingerprint: fingerprint("overlap")}, {Generation: "g", RelayEnd: 40, ByteEnd: 21, Fingerprint: fingerprint("after")}}, Encoded: []byte("before\noverlap\nafter\n")}, want: "after\n"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				got, err := relaySnapshotAppend(tc.log, tc.generation, tc.snapshot)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != tc.want {
					t.Fatalf("append = %q, want %q", got, tc.want)
				}
			})
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name       string
			generation string
			log        []agent.RelayRecordBoundary
			snapshot   agent.RelaySnapshot
		}{
			{name: "empty_log_truncated_snapshot", snapshot: agent.RelaySnapshot{RelayRecords: []agent.RelayRecordBoundary{{RelayEnd: 100, ByteEnd: 6}}, Encoded: []byte("after\n")}},
			{name: "new_generation_truncated_snapshot", generation: "old", log: []agent.RelayRecordBoundary{{Generation: "old", RelayEnd: 20}}, snapshot: agent.RelaySnapshot{RelayRecords: []agent.RelayRecordBoundary{{Generation: "new", RelayEnd: 100, ByteEnd: 6}}, Encoded: []byte("after\n")}},
			{name: "marked_no_overlap", generation: "g", log: []agent.RelayRecordBoundary{{Generation: "g", RelayEnd: 20}}, snapshot: agent.RelaySnapshot{RelayRecords: []agent.RelayRecordBoundary{{Generation: "g", RelayEnd: 100, ByteEnd: 6}}, Encoded: []byte("after\n")}},
			{name: "marked_ambiguous_overlap", generation: "g", log: []agent.RelayRecordBoundary{{Generation: "g", RelayEnd: 20}}, snapshot: agent.RelaySnapshot{RelayRecords: []agent.RelayRecordBoundary{{Generation: "g", RelayEnd: 20, ByteEnd: 1}, {Generation: "g", RelayEnd: 20, ByteEnd: 2}}, Encoded: []byte("xx")}},
			{name: "unmarked_no_exact_overlap", log: []agent.RelayRecordBoundary{{Fingerprint: fingerprint("before")}}, snapshot: agent.RelaySnapshot{RelayRecords: []agent.RelayRecordBoundary{{ByteEnd: 6, Fingerprint: fingerprint("after")}}, Encoded: []byte("after\n")}},
			{name: "unmarked_ambiguous_repeated_overlap", log: []agent.RelayRecordBoundary{{Fingerprint: fingerprint("repeat")}}, snapshot: agent.RelaySnapshot{RelayRecords: []agent.RelayRecordBoundary{{ByteEnd: 7, Fingerprint: fingerprint("repeat")}, {ByteEnd: 14, Fingerprint: fingerprint("repeat")}}, Encoded: bytes.Repeat([]byte("repeat\n"), 2)}},
			{name: "unmarked_short_match_inside_both_sources", log: []agent.RelayRecordBoundary{{Fingerprint: fingerprint("before")}, {Fingerprint: fingerprint("overlap")}}, snapshot: agent.RelaySnapshot{RelayRecords: []agent.RelayRecordBoundary{{ByteEnd: 6, Fingerprint: fingerprint("older")}, {ByteEnd: 14, Fingerprint: fingerprint("overlap")}}, Encoded: []byte("older\noverlap\n")}},
			{name: "invalid_byte_boundary", generation: "g", log: []agent.RelayRecordBoundary{{Generation: "g", RelayEnd: 20}}, snapshot: agent.RelaySnapshot{RelayRecords: []agent.RelayRecordBoundary{{Generation: "g", RelayEnd: 20, ByteEnd: 100}}, Encoded: []byte("after\n")}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				if got, err := relaySnapshotAppend(tc.log, tc.generation, tc.snapshot); err == nil || got != nil {
					t.Fatalf("append = %q, err = %v; want reconciliation error", got, err)
				}
			})
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
			{name: "runtime router", cfg: Config{ServerCtx: t.Context(), LogStore: taskhistory.NewStore(testLogger(), taskslog.NewStore(cacheDir), backends.Default("", nil))}, want: "task manager runtime router is required"},
			{name: "checkout registry", cfg: Config{ServerCtx: t.Context(), LogStore: taskhistory.NewStore(testLogger(), taskslog.NewStore(cacheDir), backends.Default("", nil)), Runtimes: runtimes}, want: "task manager checkout registry is required"},
			{name: "usage rollup", cfg: Config{ServerCtx: t.Context(), LogStore: taskhistory.NewStore(testLogger(), taskslog.NewStore(cacheDir), backends.Default("", nil)), Runtimes: runtimes, Checkouts: repo.NewRegistry()}, want: "task manager usage rollup sink is required"},
			{name: "runtime start timeout", cfg: Config{ServerCtx: t.Context(), LogStore: taskhistory.NewStore(testLogger(), taskslog.NewStore(cacheDir), backends.Default("", nil)), Runtimes: runtimes, Checkouts: repo.NewRegistry(), Rollup: task.DiscardRollup{}}, want: "task manager runtime start timeout is required"},
			{name: "logger", cfg: Config{ServerCtx: t.Context(), LogStore: taskhistory.NewStore(testLogger(), taskslog.NewStore(cacheDir), backends.Default("", nil)), Runtimes: runtimes, Checkouts: repo.NewRegistry(), Rollup: task.DiscardRollup{}, RuntimeStartTimeout: time.Hour}, want: "task manager logger is required"},
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
		router, err := runtime.NewRouter([]runtime.System{&testRuntimeSystem{testRuntimeBackend: backend}}, metrics.Nop{})
		if err != nil {
			t.Fatal(err)
		}
		cacheDir := t.TempDir()
		cfg := Config{
			ServerCtx:           t.Context(),
			Log:                 slog.New(slog.DiscardHandler),
			LogStore:            taskhistory.NewStore(testLogger(), taskslog.NewStore(filepath.Join(cacheDir, "tasks")), backends.Default("", nil)),
			Runtimes:            router,
			Backends:            map[harness.Name]agent.Backend{"fake": &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}}},
			HarnessEnv:          map[string][]string{string(harness.Codex): {"CODEX_HOME=/tmp/codex"}},
			Checkouts:           repo.NewRegistry(),
			Rollup:              task.DiscardRollup{},
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
	t.Run("TaskChanges", func(t *testing.T) {
		t.Parallel()
		m := newTestManager(t, Config{ServerCtx: t.Context()})
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "watched"}, "", "")
		m.Insert(tk.ID, m.NewEntry(tk, nil))
		version, changed := m.ChangeSnapshot()
		tk.SetTitle("Renamed")
		m.NotifyTaskUpdate(tk.ID)
		select {
		case <-changed:
		default:
			t.Fatal("mutation did not signal")
		}
		version, changed, events, gap := m.TaskChanges(version)
		if gap || len(events) != 1 || events[0].TaskID != tk.ID || !events[0].Catalog || !events[0].Summary {
			t.Fatalf("rename=%+v gap=%v", events, gap)
		}
		tk.SetState(task.StateRunning)
		m.NotifyTaskUpdate(tk.ID)
		tk.SetState(task.StateWaiting)
		m.NotifyTaskUpdate(tk.ID)
		select {
		case <-changed:
		default:
			t.Fatal("state change lost during rearm")
		}
		version, _, events, gap = m.TaskChanges(version)
		if gap || len(events) != 2 || events[0].Catalog || events[1].Catalog || events[0].Version >= events[1].Version {
			t.Fatalf("distinct occurrences=%+v gap=%v", events, gap)
		}
		m.NotifyTaskUpdate(tk.ID)
		version, _, events, gap = m.TaskChanges(version)
		if gap || len(events) != 1 || events[0].Catalog || events[0].Summary || events[0].Notifications {
			t.Fatalf("detail-only activity invalidates unrelated projections=%+v", events)
		}
		for range 513 {
			m.NotifyTaskUpdate(tk.ID)
		}
		_, _, events, gap = m.TaskChanges(version)
		if !gap || len(events) != 512 {
			t.Fatalf("overflow count=%d gap=%v", len(events), gap)
		}
	})
	t.Run("ErrorOnlyInvalidation", func(t *testing.T) {
		t.Parallel()
		m := newTestManager(t, Config{ServerCtx: t.Context()})
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "attention"}, "", "")
		e := m.NewEntry(tk, nil)
		m.Insert(tk.ID, e)
		version, _ := m.ChangeSnapshot()
		result := &task.Result{State: tk.GetState(), Err: errors.New("intent rejected")}
		e.Finish(result)
		m.NotifyTaskUpdate(tk.ID)
		version, _, changes, _ := m.TaskChanges(version)
		if len(changes) != 1 || !changes[0].Summary || changes[0].Catalog || changes[0].Notifications {
			t.Fatalf("error-only finish=%+v", changes)
		}
		e.clearResult(result)
		m.NotifyTaskUpdate(tk.ID)
		_, _, changes, _ = m.TaskChanges(version)
		if len(changes) != 1 || !changes[0].Summary || changes[0].Catalog || changes[0].Notifications {
			t.Fatalf("error clear=%+v", changes)
		}
	})
	t.Run("LoadedInvalidationBaseline", func(t *testing.T) {
		t.Parallel()
		m := newTestManager(t, Config{ServerCtx: t.Context()})
		version, _ := m.ChangeSnapshot()
		id := ksid.NewID()
		if err := m.LoadPurgedTasks([]*taskhistory.LoadedTask{{TaskID: id.String(), Prompt: "restored", State: task.StatePurged, Harness: harness.Claude, Title: "restored"}}); err != nil {
			t.Fatal(err)
		}
		version, _, changes, _ := m.TaskChanges(version)
		if len(changes) != 1 || !changes[0].Catalog {
			t.Fatalf("load catalog=%+v", changes)
		}
		m.NotifyTaskUpdate(id)
		_, _, changes, _ = m.TaskChanges(version)
		if len(changes) != 1 || changes[0].Catalog || changes[0].Summary || changes[0].Notifications {
			t.Fatalf("quiet loaded detail=%+v", changes)
		}
	})
	t.Run("drainInputQueue", func(t *testing.T) {
		t.Run("restart_edit_and_deliver", func(t *testing.T) {
			t.Parallel()
			store := taskhistory.NewStore(testLogger(), taskslog.NewStore(t.TempDir()), backends.Default("", nil))
			m := newTestManager(t, Config{ServerCtx: t.Context(), LogStore: store})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "initial"}, harness.Claude, "")
			tk.SetState(task.StateRunning)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID, e)
			prompt := agent.Prompt{Text: "first", Images: []logdata.ImageData{{MediaType: "image/png", Data: "aGVsbG8="}}}
			if _, err := e.Lifecycle.QueueInput(t.Context(), prompt); err != nil {
				t.Fatal(err)
			}
			original := prompt.Images[0]
			prompt.Images[0].Data = "caller changed its slice"
			if got := e.PendingInput().Prompt.Images[0]; got != original {
				t.Fatalf("caller mutated pending attachment: %+v", got)
			}
			prompt.Images[0] = original
			if _, err := e.Lifecycle.QueueInput(t.Context(), agent.Prompt{Text: "second"}); err != nil {
				t.Fatal(err)
			}
			m.drainInputQueue(t.Context())
			if got := e.PendingInput(); got == nil {
				t.Fatalf("running pending input = %+v", got)
			}
			// Reconstruct both the manager and task, preserving only their durable identities.
			restored := newTestManager(t, Config{ServerCtx: t.Context(), LogStore: store})
			rt := mustNewTask(t, tk.ID, agent.Prompt{Text: "initial"}, harness.Claude, "")
			rt.SetState(task.StateAsking)
			re := restored.NewEntry(rt, nil)
			restored.Insert(rt.ID, re)
			queue := re.PendingInput()
			if queue == nil || queue.Prompt.Text != "first\nsecond" || len(queue.Prompt.Images) != 1 {
				t.Fatalf("restored queue: %+v", queue)
			}
			// Runtime adoption replaces the cold entry after its pending input was exposed.
			re = restored.NewEntry(rt, nil)
			restored.Insert(rt.ID, re)
			if got := re.PendingInput(); got == nil || got.ID != queue.ID {
				t.Fatalf("adoption lost pending input: %+v", got)
			}
			img, ok := re.PendingInputImage(queue.ID, 0)
			if !ok || img != prompt.Images[0] {
				t.Fatalf("restored attachment = %+v, found %v", img, ok)
			}
			for _, index := range []int{-1, 1} {
				if _, ok := re.PendingInputImage(queue.ID, index); ok {
					t.Fatalf("invalid attachment index %d found", index)
				}
			}
			if _, ok := restored.NewEntry(mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "other"}, harness.Claude, ""), nil).PendingInputImage(queue.ID, 0); ok {
				t.Fatal("cross-task attachment lookup succeeded")
			}
			other := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "other"}, harness.Claude, "")
			if err := restored.NewEntry(other, nil).Lifecycle.CancelQueuedInput(queue.ID); err == nil {
				t.Fatal("cross-task cancellation succeeded")
			}
			if err := re.Lifecycle.CancelQueuedInput(queue.ID); err != nil {
				t.Fatal(err)
			}
			prompt.Text = "edited"
			if _, err := re.Lifecycle.QueueInput(t.Context(), prompt); err != nil {
				t.Fatal(err)
			}
			restored.drainInputQueue(t.Context())
			if re.PendingInput() == nil {
				t.Fatal("asking state drained the queue")
			}
			b := &reconnectInputBackend{FakeBackend: &agenttest.FakeBackend{HarnessName: harness.Claude, Images: true}}
			t.Cleanup(b.stop)
			startQueueSession(t, re, b)
			rt.SetState(task.StateWaiting)
			restored.drainInputQueue(t.Context())
			awaitQueueCondition(t, restored, func() bool { return re.PendingInput() == nil })
			if rt.GetState() != task.StateRunning {
				t.Fatalf("delivery state=%v", rt.GetState())
			}
			b.mu.Lock()
			got := append([]agent.Prompt(nil), b.prompts...)
			b.mu.Unlock()
			if !reflect.DeepEqual(got, []agent.Prompt{prompt}) {
				t.Fatalf("delivered prompts = %+v", got)
			}
			loaded, err := store.Journal.LoadInputQueue()
			if err != nil || len(loaded) != 0 {
				t.Fatalf("delivered prompts remained on disk: %+v %v", loaded, err)
			}
		})
		t.Run("purge_discards_queued_attachments_before_restart", func(t *testing.T) {
			t.Parallel()
			store := taskhistory.NewStore(testLogger(), taskslog.NewStore(t.TempDir()), backends.Default("", nil))
			m := newTestManager(t, Config{ServerCtx: t.Context(), LogStore: store})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "initial"}, harness.Claude, "")
			tk.SetState(task.StateRunning)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID, e)
			recordTestTaskLog(t, m, e)
			if _, err := e.Lifecycle.QueueInput(t.Context(), agent.Prompt{Text: "discard me", Images: []logdata.ImageData{{MediaType: "image/png", Data: "aGVsbG8="}}}); err != nil {
				t.Fatal(err)
			}
			tk.SetState(task.StateStopped)
			m.background.Go(func() { m.watchInputQueue(m.serverCtx) })
			t.Cleanup(func() { m.cancelServerCtx(); m.background.Wait() })
			if err := e.Lifecycle.Purge(t.Context(), 0); err != nil {
				t.Fatal(err)
			}
			awaitQueueCondition(t, m, func() bool { return tk.GetState() == task.StatePurged })
			if err := e.Lifecycle.Close(); err != nil {
				t.Fatal(err)
			}
			awaitQueueCondition(t, m, func() bool { return e.PendingInput() == nil })
			restored := newTestManager(t, Config{ServerCtx: t.Context(), LogStore: store})
			re := restored.NewEntry(tk, nil)
			restored.Insert(tk.ID, re)
			if got := re.PendingInput(); got != nil {
				t.Fatalf("purged attachments restored: %+v", got)
			}
		})
		t.Run("startup_discards_missing_tasks_but_retains_stopped_tasks", func(t *testing.T) {
			t.Parallel()
			store := taskhistory.NewStore(testLogger(), taskslog.NewStore(t.TempDir()), backends.Default("", nil))
			m := newTestManager(t, Config{ServerCtx: t.Context(), LogStore: store})
			ids := make([]ksid.ID, 0, 2)
			for range 2 {
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "initial"}, harness.Claude, "")
				tk.SetState(task.StateRunning)
				e := m.NewEntry(tk, nil)
				m.Insert(tk.ID, e)
				if _, err := e.Lifecycle.QueueInput(t.Context(), agent.Prompt{Text: "retained work"}); err != nil {
					t.Fatal(err)
				}
				ids = append(ids, tk.ID)
			}
			restored := newTestManager(t, Config{ServerCtx: t.Context(), LogStore: store})
			for _, stage := range []string{"loading", "failed"} {
				if stage == "failed" {
					restored.CompleteRestoration(errors.New("partial history scan"))
				}
				restored.drainInputQueue(t.Context())
				q, err := store.Journal.LoadInputQueue()
				if err != nil || len(q) != 2 {
					t.Fatalf("%s restoration discarded unknown tasks: queue=%+v err=%v", stage, q, err)
				}
			}
			tk := mustNewTask(t, ids[1], agent.Prompt{Text: "initial"}, harness.Claude, "")
			tk.SetState(task.StateStopped)
			restored.Insert(tk.ID, restored.NewEntry(tk, nil))
			restored.CompleteRestoration(nil)
			restored.drainInputQueue(t.Context())
			q, err := store.Journal.LoadInputQueue()
			if err != nil || len(q) != 1 || q[0].TaskID != tk.ID.String() {
				t.Fatalf("startup queue=%+v err=%v", q, err)
			}
		})
		t.Run("in_flight_send_isolated_from_other_tasks", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			blocked := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "initial"}, harness.Claude, "")
			blocked.SetState(task.StateRunning)
			e := m.NewEntry(blocked, nil)
			m.Insert(blocked.ID, e)
			conn := newGatedQueueSession(t, e)
			if _, err := e.Lifecycle.QueueInput(t.Context(), agent.Prompt{Text: "blocked prompt"}); err != nil {
				t.Fatal(err)
			}
			pending := e.PendingInput()
			if pending == nil || e.PendingInput() != pending {
				t.Fatal("pending input was not returned directly")
			}
			blocked.SetState(task.StateWaiting)
			m.drainInputQueue(t.Context())
			select {
			case <-conn.started:
			case <-time.After(5 * time.Second):
				t.Fatal("delivery did not start")
			}
			q := e.PendingInput()
			if q == nil || !q.Delivering {
				t.Fatalf("in-flight queue=%+v", q)
			}
			if pending.Delivering || pending.Error != "" || pending.Delivered {
				t.Errorf("delivery mutated retained pending input: %+v", pending)
			}
			if err := e.Lifecycle.CancelQueuedInput(q.ID); err == nil {
				t.Fatal("in-flight cancellation succeeded")
			}
			b := &reconnectInputBackend{FakeBackend: &agenttest.FakeBackend{HarnessName: harness.Claude}}
			t.Cleanup(b.stop)
			healthy := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "other"}, harness.Claude, "")
			healthy.SetState(task.StateWaiting)
			he := m.NewEntry(healthy, nil)
			m.Insert(healthy.ID, he)
			startQueueSession(t, he, b)
			if _, err := he.Lifecycle.QueueInput(t.Context(), agent.Prompt{Text: "healthy prompt"}); err != nil {
				t.Fatal(err)
			}
			m.drainInputQueue(t.Context())
			awaitQueueCondition(t, m, func() bool { return he.PendingInput() == nil })
			if got := e.PendingInput(); got == nil || !got.Delivering {
				t.Fatalf("stalled send changed unexpectedly: %+v", got)
			}
			close(conn.release)
			awaitQueueCondition(t, m, func() bool { return e.PendingInput() == nil })
			if !q.Delivering {
				t.Fatal("delivery completion mutated retained in-flight input")
			}
		})
		t.Run("failed_import_reconnect_preserves_prompt_gate", func(t *testing.T) {
			t.Parallel()
			for _, state := range []task.State{task.StateAsking, task.StateHasPlan} {
				for _, stage := range []string{"attach", "readiness"} {
					t.Run(string(state)+"/"+stage, func(t *testing.T) {
						t.Parallel()
						store := taskhistory.NewStore(testLogger(), taskslog.NewStore(t.TempDir()), backends.Default("", nil))
						original := newTestManager(t, Config{ServerCtx: t.Context(), LogStore: store})
						tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "initial"}, harness.Claude, "")
						tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "restore-gate"), runtime.ConnectionTarget{SSHHost: "restore-gate"}, "", "", 0)
						tk.SetSessionMetadata(agent.SessionMetadata{SessionID: "saved-session"})
						tk.SetState(state)
						e := original.NewEntry(tk, nil)
						original.Insert(tk.ID, e)
						recordTestTaskLog(t, original, e)
						if _, err := e.Lifecycle.QueueInput(t.Context(), agent.Prompt{Text: "deferred work"}); err != nil {
							t.Fatal(err)
						}
						var backend agent.Backend = &agenttest.FakeBackend{HarnessName: harness.Claude}
						if stage == "readiness" {
							b := &reconnectInputBackend{FakeBackend: &agenttest.FakeBackend{HarnessName: harness.Claude}, exitImmediately: true}
							t.Cleanup(b.stop)
							backend = b
						}
						restored := newTestManager(t, Config{ServerCtx: t.Context(), LogStore: store, Backends: agent.Backends{harness.Claude: backend}})
						re := restored.NewEntry(tk, nil)
						restored.Insert(tk.ID, re)
						re.Lifecycle.reconnectImportedSession()
						re.Lifecycle.wg.Wait()
						if got := tk.GetState(); got != state {
							t.Fatalf("failed %s reconnect released %s gate: %s", stage, state, got)
						}
						restored.drainInputQueue(t.Context())
						restored.background.Wait()
						q := re.PendingInput()
						if q == nil || q.Delivering || q.Error != "" {
							t.Fatalf("unresolved %s attempted deferred input: %+v", state, q)
						}
						persisted, err := store.Journal.LoadInputQueue()
						if err != nil || len(persisted) != 1 || persisted[0].Text != "deferred work" {
							t.Fatalf("retained queue=%+v err=%v", persisted, err)
						}
					})
				}
			}
		})
		t.Run("accepted_prompt_with_failed_removal_is_not_replayed", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "initial"}, harness.Claude, "")
			tk.SetState(task.StateRunning)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID, e)
			conn := newGatedQueueSession(t, e)
			if _, err := e.Lifecycle.QueueInput(t.Context(), agent.Prompt{Text: "deliver once"}); err != nil {
				t.Fatal(err)
			}
			tk.SetState(task.StateWaiting)
			m.drainInputQueue(t.Context())
			select {
			case <-conn.started:
			case <-t.Context().Done():
				t.Fatal(t.Context().Err())
			}
			path := filepath.Join(m.logStore.Journal.LogDir, "input-queue.json")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			close(conn.release)
			awaitQueueCondition(t, m, func() bool { return e.PendingInput().Error != "" })
			q := e.PendingInput()
			if !q.Delivered || q.Delivering {
				t.Fatalf("accepted prompt state=%+v", q)
			}
			tk.SetState(task.StateWaiting)
			m.drainInputQueue(t.Context())
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := e.Lifecycle.CancelQueuedInput(q.ID); err != nil {
				t.Fatal(err)
			}
		})
		t.Run("delivery_failure_remains_visible", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "initial"}, harness.Claude, "")
			tk.SetState(task.StateRunning)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID, e)
			if _, err := e.Lifecycle.QueueInput(t.Context(), agent.Prompt{Text: "retry me"}); err != nil {
				t.Fatal(err)
			}
			tk.SetState(task.StateWaiting)
			m.drainInputQueue(t.Context())
			awaitQueueCondition(t, m, func() bool { return e.PendingInput().Error != "" })
			q := e.PendingInput()
			if q == nil || q.Error == "" {
				t.Fatalf("failed delivery disappeared: %+v", q)
			}
			if err := e.Lifecycle.CancelQueuedInput(q.ID); err != nil {
				t.Fatal(err)
			}
		})
	})
	t.Parallel()

	t.Run("TaskListComplete", func(t *testing.T) {
		t.Parallel()
		m := newTestManager(t, Config{ServerCtx: t.Context()})
		t.Cleanup(func() {
			if err := m.Close(); err != nil {
				t.Error(err)
			}
		})
		if err := m.Start(&fakeTaskMCPScoper{}); err != nil {
			t.Fatal(err)
		}
		m.CompleteRestoration(nil)
		if m.TaskListComplete() {
			t.Fatal("runtime import fence certified missing tasks")
		}
		if _, err := m.ImportInstances(t.Context(), nil, nil); err != nil {
			t.Fatal(err)
		}
		if !m.TaskListComplete() {
			t.Fatal("completed runtime import did not certify membership")
		}
	})

	t.Run("Start", func(t *testing.T) {
		t.Run("valid_scopes_enabled_task", func(t *testing.T) {
			t.Parallel()
			info := &runtimetest.FakeInfo{Events: make(chan runtime.Event)}
			m := newTestManager(t, Config{
				ServerCtx: t.Context(),
				Runtimes:  newTestRuntime(t, &runtimetest.FakeBackend{}, info),
			})
			t.Cleanup(func() {
				if err := m.Close(); err != nil {
					t.Error(err)
				}
			})
			scoper := &fakeTaskMCPScoper{registry: mcptest.FakeRegistry{}}
			if err := m.Start(scoper); err != nil {
				t.Fatalf("Start() error: %v", err)
			}
			if !m.TaskMCPAvailable() {
				t.Fatal("TaskMCPAvailable() = false, want true")
			}

			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
			tk.CaicMCP = true
			entry := m.NewEntry(tk, nil)
			tools, err := entry.Lifecycle.agentRuntime.MCPRegistry.Tools(t.Context())
			if err != nil || len(tools) != 1 || tools[0].Name != "echo" {
				t.Fatalf("owned entry MCP registry tools=%#v err=%v", tools, err)
			}
			if scoper.taskID != tk.ID {
				t.Errorf("scoper task ID = %s, want %s", scoper.taskID, tk.ID)
			}
		})

		t.Run("error_missing_scoper", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			if err := m.Start(nil); err == nil {
				t.Fatal("Start(nil) succeeded")
			}
			if m.TaskMCPAvailable() {
				t.Fatal("TaskMCPAvailable() = true, want false")
			}
		})
	})

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
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			t.Run("closes subscriptions", func(t *testing.T) {
				t.Parallel()
				m := newTestManager(t, Config{ServerCtx: t.Context()})
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
				tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{&agent.RateLimitMessage{
					Status:        agent.RateLimitStatusRejected,
					QuotaProvider: agent.QuotaProviderClaudeCode,
					QuotaWindow:   "five_hour",
					Utilization:   1,
				}}))
				m.Insert(tk.ID, m.NewEntry(tk, nil))
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

			t.Run("interrupts queued transport before joining producers", func(t *testing.T) {
				t.Parallel()
				m := newTestManager(t, Config{ServerCtx: t.Context()})
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "initial"}, harness.Claude, "")
				tk.SetState(task.StateRunning)
				e := m.NewEntry(tk, nil)
				m.Insert(tk.ID, e)
				reader, writer := io.Pipe()
				b := &blockedInputBackend{
					FakeBackend: &agenttest.FakeBackend{HarnessName: harness.Claude, WireFactory: claudecode.New().NewWire},
					stdin:       &blockedQueueInput{PipeWriter: writer, started: make(chan struct{})},
				}
				startQueueSession(t, e, b)
				if _, err := e.Lifecycle.QueueInput(t.Context(), agent.Prompt{Text: "blocked prompt"}); err != nil {
					t.Fatal(err)
				}
				tk.SetState(task.StateWaiting)
				m.drainInputQueue(t.Context())
				select {
				case <-b.stdin.started:
				case <-time.After(5 * time.Second):
					t.Fatal("queued delivery did not enter the transport writer")
				}
				closed := make(chan error, 1)
				var workers sync.WaitGroup
				workers.Go(func() { closed <- m.Close() })
				t.Cleanup(func() {
					if err := reader.Close(); err != nil {
						t.Error(err)
					}
					workers.Wait()
				})
				select {
				case err := <-closed:
					if err != nil {
						t.Fatalf("Close() = %v", err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("Close waited for queued delivery before interrupting its transport")
				}
			})

			t.Run("waits for lifecycle", func(t *testing.T) {
				t.Parallel()
				backend := &blockedStopBackend{
					FakeBackend: &runtimetest.FakeBackend{},
					started:     make(chan struct{}),
					release:     make(chan struct{}, 1),
				}
				m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, backend, nil)})
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "")
				tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
				tk.SetState(task.StateRunning)
				entry := m.NewEntry(tk, nil)
				m.Insert(tk.ID, entry)
				recordTestTaskLog(t, m, entry)
				if err := entry.Lifecycle.Stop(t.Context()); err != nil {
					t.Fatalf("Stop() = %v", err)
				}
				select {
				case <-backend.started:
				case <-time.After(time.Second):
					t.Fatal("Stop did not reach the runtime backend")
				}
				intent, err := m.logStore.LoadForTaskIDs([]string{tk.ID.String()})
				if err != nil || len(intent) != 1 || intent[0].State != task.StateStopping {
					t.Fatalf("stop intent=%v err=%v", intent, err)
				}

				closed := make(chan error, 1)
				var workers sync.WaitGroup
				workers.Go(func() { closed <- m.Close() })
				t.Cleanup(func() {
					select {
					case backend.release <- struct{}{}:
					default:
					}
					workers.Wait()
				})
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
		})
	})

	t.Run("CopiesBackends", func(t *testing.T) {
		t.Parallel()
		var firstCalls, replacementCalls int
		agents := map[harness.Name]agent.Backend{
			harness.Claude: &agenttest.FakeBackend{WireFactory: func() agent.WireFormat {
				firstCalls++
				return claudecode.New().NewWire()
			}},
		}
		m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: agents})
		agents[harness.Claude] = &agenttest.FakeBackend{WireFactory: func() agent.WireFormat {
			replacementCalls++
			return codex.New("", nil).NewWire()
		}}
		if _, err := m.ResolveParser(harness.Claude); err != nil {
			t.Fatalf("ResolveParser: %v", err)
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
		tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
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
		}))
		m.Insert(tk.ID, m.NewEntry(tk, nil))

		quotas := m.QuotaTracker.Merge(nil, now)
		if len(quotas) != 1 || len(quotas[0].RateLimits) != 2 {
			t.Fatalf("tracked quotas = %#v, want two Claude Code quota windows", quotas)
		}
		if got := quotas[0].RateLimits[0]; got.Window != "5h" || got.Utilization != 1 {
			t.Errorf("5h rate limit = %#v, want rejected update", got)
		}
		if got := quotas[0].RateLimits[1]; got.Window != "7d" || got.Utilization != 0.91 {
			t.Errorf("7d rate limit = %#v, want warning update", got)
		}
	})

	t.Run("RateLimitStatusOnlyUtilization", func(t *testing.T) {
		t.Parallel()
		m := newTestManager(t, Config{ServerCtx: t.Context()})
		now := time.Now().UTC()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
		tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
			// A status-only event must not replace the provider's real value with 0%.
			// -1 marks an unreported utilization.
			&agent.RateLimitMessage{
				Status:        agent.RateLimitStatusAllowed,
				ResetsAt:      now.Add(time.Hour),
				QuotaProvider: agent.QuotaProviderClaudeCode,
				QuotaWindow:   "5h",
				Utilization:   -1,
			},
			// A rejected status still reports a full window.
			&agent.RateLimitMessage{
				Status:        agent.RateLimitStatusRejected,
				ResetsAt:      now.Add(7 * 24 * time.Hour),
				QuotaProvider: agent.QuotaProviderClaudeCode,
				QuotaWindow:   "7d",
				Utilization:   -1,
			},
		}))
		m.Insert(tk.ID, m.NewEntry(tk, nil))

		quotas := m.QuotaTracker.Merge(nil, now)
		if len(quotas) != 1 || len(quotas[0].RateLimits) != 1 {
			t.Fatalf("tracked quotas = %#v, want only the rejected 7d window", quotas)
		}
		if got := quotas[0].RateLimits[0]; got.Window != "7d" || got.Utilization != 1 {
			t.Errorf("tracked rate limit = %#v, want rejected 7d update", got)
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
			m.Insert(tk.ID, e)
			got, ok := m.GetEntry(tk.ID)
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
			_, ok := m.GetEntry(ksid.NewID())
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
		m.Insert(tk.ID, e)
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
		m.Insert(tk.ID, m.NewEntry(tk, nil))
		done := make(chan struct{})
		var workers sync.WaitGroup
		workers.Go(func() {
			m.Range(func(_ ksid.ID, _ *Entry) bool {
				_, _ = m.Checkouts.Checkout("repo")
				return true
			})
			registerCheckout(t, m.Checkouts, "repo", &repo.Checkout{})
			for range m.Checkouts.Checkouts() {
				_, _ = m.GetEntry(tk.ID)
			}
			close(done)
		})
		select {
		case <-done:
			workers.Wait()
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
				m.Insert(ksid.NewID(), m.NewEntry(mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", ""), nil))
			}
			var count int
			m.Range(func(id ksid.ID, e *Entry) bool {
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
				m.Insert(ksid.NewID(), m.NewEntry(mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", ""), nil))
			}
			var count int
			m.Range(func(id ksid.ID, e *Entry) bool {
				count++
				return false
			})
			if count != 1 {
				t.Errorf("Range iterated %d entries after stop, want 1", count)
			}
		})
	})

	t.Run("Entries", func(t *testing.T) {
		t.Parallel()
		m := newTestManager(t, Config{ServerCtx: t.Context()})
		for range 5 {
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
			m.Insert(tk.ID, m.NewEntry(tk, nil))
		}
		var count int
		for id, entry := range m.Entries() {
			if _, ok := m.GetEntry(id); !ok || entry == nil {
				t.Fatalf("Entries() yielded missing entry %q", id)
			}
			count++
		}
		if count != 5 {
			t.Errorf("Entries() yielded %d entries, want 5", count)
		}
	})

	t.Run("Changed", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_on_insert", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			oldCh := m.Changed()
			m.Insert(ksid.NewID(), m.NewEntry(mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", ""), nil))
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
		t.Run("valid_change_snapshot_tracks_version", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			version, oldCh := m.ChangeSnapshot()
			m.NotifyTaskChange()
			nextVersion, nextCh := m.ChangeSnapshot()
			if nextVersion != version+1 {
				t.Fatalf("change version = %d, want %d", nextVersion, version+1)
			}
			if oldCh == nextCh {
				t.Fatal("ChangeSnapshot returned the same channel after mutation")
			}
			select {
			case <-oldCh:
			default:
				t.Fatal("ChangeSnapshot's old channel is not closed after mutation")
			}
		})
	})

	t.Run("FindTasksByPR", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
			tk.Repos = []repo.Mount{{Name: "my/repo", Branch: "caic-1"}}
			tk.SetPR("acme", "magic", 42)
			m.Insert(tk.ID, m.NewEntry(tk, nil))
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
			tk.Repos = []repo.Mount{{Name: "my/repo", Branch: "caic-1"}}
			tk.SetPR("acme", "magic", 42)
			m.Insert(tk.ID, m.NewEntry(tk, nil))
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
			tk.Repos = []repo.Mount{{Name: "my/repo", Branch: "caic-1"}}
			tk.SetPR("acme", "magic", 0)
			m.Insert(tk.ID, m.NewEntry(tk, nil))
			found := m.FindTasksMatchingBranch("acme", "magic", "caic-1")
			if len(found) != 1 {
				t.Fatalf("FindTasksMatchingBranch returned %d entries, want 1", len(found))
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
			tk.Repos = []repo.Mount{{Name: "my/repo", Branch: "caic-1"}}
			tk.SetPR("acme", "magic", 0)
			m.Insert(tk.ID, m.NewEntry(tk, nil))
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
			tk.Repos = []repo.Mount{{Name: "my/repo", Branch: "caic-1"}}
			tk.SetPR("acme", "magic", 0)
			e := m.NewEntry(tk, nil)
			e.SetMonitorBranch("caic-1")
			m.Insert(tk.ID, e)
			found := m.FindTasksMonitoringBranch("acme", "magic")
			if len(found) != 1 {
				t.Fatalf("FindTasksMonitoringBranch returned %d entries, want 1", len(found))
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
			tk.Repos = []repo.Mount{{Name: "my/repo", Branch: "caic-1"}}
			tk.SetPR("acme", "magic", 0)
			m.Insert(tk.ID, m.NewEntry(tk, nil))
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
			tk.Repos = []repo.Mount{{Name: "repo/a"}}
			tk.SetPR("acme", "magic", 0)
			tk.SetState(task.StateRunning)
			m.Insert(tk.ID, m.NewEntry(tk, nil))
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
			tk.SetState(task.StateRunning)
			m.Insert(tk.ID, m.NewEntry(tk, nil))
			pending := m.ListPendingBotTasks()
			if len(pending) != 0 {
				t.Errorf("ListPendingBotTasks returned %d tasks without ForgeIssue, want 0", len(pending))
			}
		})
		t.Run("valid_skips_terminal_states", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			for _, st := range []task.State{task.StateWaiting, task.StateStopped, task.StateCrashed, task.StateFailed, task.StatePurged} {
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
				tk.ForgeIssue = 1
				tk.SetState(st)
				m.Insert(tk.ID, m.NewEntry(tk, nil))
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
			tk.Repos = []repo.Mount{{Name: "my/repo"}}
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
		t.Run("persisted_order_and_context_boundary", func(t *testing.T) {
			t.Parallel()
			for _, compressed := range []bool{false, true} {
				for _, cleared := range []bool{false, true} {
					t.Run(fmt.Sprintf("compressed=%t/cleared=%t", compressed, cleared), func(t *testing.T) {
						t.Parallel()
						id := ksid.NewID()
						dir := t.TempDir()
						path := filepath.Join(dir, id.String()+"-metadata.jsonl")
						raw := []byte(`{"t":"caic_meta","version":3,"harness":"claude","model":"requested","prompt":"test"}
{"t":"session","session_id":"initial","model":"initial","reported_effort":"low","agent_version":"1","auto_compact_token_limit":100}
{"t":"agent","ts":1.000,"msg":{"type":"system","subtype":"init","session_id":"native","model":"active","effort":"high","claude_code_version":"2"}}
{"t":"session","session_id":"restored","model":"stale","reported_effort":"low","agent_version":"3","auto_compact_token_limit":300}
`)
						want := agent.SessionMetadata{SessionID: "restored", ReportedModel: "active", ReportedEffort: "high", AgentVersion: "3", AutoCompactTokenLimit: 300}
						if cleared {
							raw = append(raw, []byte("{\"t\":\"context_cleared\"}\n")...)
							want.SessionID = ""
						}
						raw = append(raw, []byte("{\"t\":\"result\",\"state\":\"purged\"}\n")...)
						if compressed {
							path += ".zst"
							encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
							if err != nil {
								t.Fatal(err)
							}
							raw = encoder.EncodeAll(raw, nil)
							if err := encoder.Close(); err != nil {
								t.Fatal(err)
							}
						}
						if err := os.WriteFile(path, raw, 0o600); err != nil {
							t.Fatal(err)
						}
						store := taskhistory.NewStore(testLogger(), taskslog.NewStore(dir), backends.Default("", nil))
						for _, source := range []string{"scan", "cache"} {
							var logs []*taskhistory.LoadedTask
							var err error
							if compressed {
								logs, err = store.LoadSettled()
							} else {
								logs, err = store.LoadUnsettled()
							}
							if err != nil || len(logs) != 1 {
								t.Fatalf("%s inventory: %d tasks, %v", source, len(logs), err)
							}
							lt := logs[0]
							retained := mustNewTask(t, id, agent.Prompt{Text: "test"}, harness.Claude, "requested")
							retained.SetSessionMetadata(lt.SessionMetadata)
							if got := retained.Snapshot().SessionMetadata; got != want {
								t.Fatalf("%s retained metadata = %+v, want %+v", source, got, want)
							}
							if err := lt.LoadMessages(); err != nil {
								t.Fatal(err)
							}
							adopted := mustNewTask(t, id, agent.Prompt{Text: "test"}, harness.Claude, "requested")
							adopted.SeedTimelineParts(lt.Timeline, nil)
							applyLoadedSessionMetadata(adopted, lt)
							if snap := adopted.Snapshot(); snap.SessionMetadata != want || snap.RequestedModel != "requested" {
								t.Fatalf("%s adopted metadata = %+v, requested model = %q, want %+v and requested", source, snap.SessionMetadata, snap.RequestedModel, want)
							}
						}
					})
				}
			}
		})
		t.Run("valid_fills_missing_session", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "requested")
			lt := &taskhistory.LoadedTask{SessionID: "thread-1", ReportedModel: "reported", AgentVersion: "1.2.3", RequestedModel: "requested"}
			applyLoadedSessionMetadata(tk, lt)
			if got := tk.GetSessionID(); got != "thread-1" {
				t.Errorf("SessionID = %q, want thread-1", got)
			}
			if tk.RequestedModel != "requested" {
				t.Errorf("requested Model = %q, want requested", tk.RequestedModel)
			}
			snap := tk.Snapshot()
			if snap.RequestedModel != "requested" || snap.ReportedModel != "reported" {
				t.Errorf("settings = (%q, %q), want (requested, reported)", snap.RequestedModel, snap.ReportedModel)
			}
			if snap.AgentVersion != "1.2.3" {
				t.Errorf("AgentVersion = %q, want 1.2.3", snap.AgentVersion)
			}
		})
		t.Run("valid_preserves_live_session", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
			tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{&agent.InitMessage{SessionID: "live"}}))
			applyLoadedSessionMetadata(tk, &taskhistory.LoadedTask{SessionID: "persisted"})
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
			tk.Repos = []repo.Mount{{Name: "my/repo", BaseBranch: "main"}}
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
			tk.Repos = []repo.Mount{{Name: "my/repo"}}
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
			tk.SetState(task.StateStopped)
			m.Insert(tk.ID, m.NewEntry(tk, nil))
			state, result, err := m.WatchTaskCompletion(t.Context(), tk.ID)
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
			_, _, err := m.WatchTaskCompletion(t.Context(), ksid.NewID())
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
			tk.SetState(task.StateRunning)
			m.Insert(tk.ID, m.NewEntry(tk, nil))
			var workers sync.WaitGroup
			workers.Go(func() {
				time.Sleep(50 * time.Millisecond)
				tk.SetState(task.StateStopped)
				m.NotifyTaskChange()
			})
			t.Cleanup(workers.Wait)
			state, _, err := m.WatchTaskCompletion(t.Context(), tk.ID)
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
			tk.SetState(task.StateRunning)
			m.Insert(tk.ID, m.NewEntry(tk, nil))
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, _, err := m.WatchTaskCompletion(ctx, tk.ID)
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
			_, err := e.Lifecycle.Fork(t.Context(), &ForkParams{})
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
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
				tk.SetState(c.state)
				e := m.NewEntry(tk, nil)
				m.Insert(tk.ID, e)
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
			m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: map[harness.Name]agent.Backend{"fake": &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}}}})
			registerCheckout(t, m.Checkouts, "my/repo", &repo.Checkout{Dir: "/tmp/my-repo"})
			return m
		}
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			t.Run("canceled manager rejects prepared publication", func(t *testing.T) {
				t.Parallel()
				m := newTestManager(t, Config{ServerCtx: t.Context()})
				t.Cleanup(func() {
					if err := m.Close(); err != nil {
						t.Error(err)
					}
				})
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "late creation"}, harness.Claude, "")
				e := m.NewEntry(tk, nil)
				m.Cancel()
				if err := m.publishEntry(t.Context(), e, 0, false); !errors.Is(err, context.Canceled) {
					t.Fatalf("publishEntry() = %v, want cancellation", err)
				}
				for range m.Entries() {
					t.Fatal("canceled manager published the task")
				}
				if _, err := os.Stat(e.LogPath.Get()); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("prepared journal was not rolled back: %v", err)
				}
			})
		})
		t.Run("log_initialized_before_publication", func(t *testing.T) {
			t.Parallel()
			m := newManagerWithRepo(t)
			t.Cleanup(func() {
				if err := m.Close(); err != nil {
					t.Error(err)
				}
			})
			checkPublishedTaskLog(t, m, func() (ksid.ID, error) {
				return m.Create(t.Context(), CreateParams{Prompt: agent.Prompt{Text: "publication"}, Repos: []CreateRepo{{Name: "my/repo"}}, Harness: "fake"})
			})
		})
		t.Run("error_log_initialization_does_not_publish", func(t *testing.T) {
			t.Parallel()
			m := newManagerWithRepo(t)
			t.Cleanup(func() {
				if err := m.Close(); err != nil {
					t.Error(err)
				}
			})
			if err := os.Remove(m.logStore.Journal.LogDir); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(m.logStore.Journal.LogDir, []byte("blocked"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, changed := m.ChangeSnapshot()
			_, err := m.Create(t.Context(), CreateParams{Prompt: agent.Prompt{Text: "fail"}, Repos: []CreateRepo{{Name: "my/repo"}}, Harness: "fake"})
			if te, ok := errors.AsType[*Error](err); !ok || te.Kind != KindInternal {
				t.Fatalf("Create = %v, want internal initialization error", err)
			}
			if m.Len() != 0 {
				t.Fatal("failed creation published a task")
			}
			select {
			case <-changed:
				t.Fatal("failed creation notified task readers")
			default:
			}
		})
		t.Run("concurrent_local_branch_adoption", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			initTaskRepo(t, dir, []string{"branch", "local-work"}, []string{"branch", "--set-upstream-to", "origin/main", "local-work"})
			release := make(chan struct{})
			m := newTestManager(t, Config{
				ServerCtx: t.Context(),
				Runtimes:  newTestRuntime(t, &waitingLaunchBackend{FakeBackend: &runtimetest.FakeBackend{}, release: release}, nil),
				Backends:  agent.Backends{"fake": &agenttest.FakeBackend{}},
			})
			registerCheckout(t, m.Checkouts, "my/repo", &repo.Checkout{Dir: dir, GitTimeout: time.Second})
			t.Cleanup(func() {
				close(release)
				if err := m.Close(); err != nil {
					t.Error(err)
				}
			})
			start := make(chan struct{})
			var workers sync.WaitGroup
			ids := make([]ksid.ID, 2)
			errs := make([]error, 2)
			for i := range ids {
				workers.Go(func() {
					<-start
					ids[i], errs[i] = m.Create(t.Context(), CreateParams{Prompt: agent.Prompt{Text: "adopt"}, Repos: []CreateRepo{{Name: "my/repo", BaseBranch: "local-work"}}, Harness: "fake"})
				})
			}
			close(start)
			workers.Wait()
			branches := make([]string, 0, len(errs))
			for i, err := range errs {
				if err != nil {
					t.Fatal(err)
				}
				e, _ := m.GetEntry(ids[i])
				branches = append(branches, e.Task().Primary().Branch)
			}
			slices.Sort(branches)
			if !reflect.DeepEqual(branches, []string{"caic-0", "local-work"}) {
				t.Fatalf("concurrent branches = %v", branches)
			}
		})
		t.Run("valid_sets_basename_mounted_path_and_max_cpus", func(t *testing.T) {
			t.Parallel()
			m := newManagerWithRepo(t)
			id, err := m.Create(t.Context(), CreateParams{
				Prompt:          agent.Prompt{Text: "hi"},
				Repos:           []CreateRepo{{Name: "my/repo"}},
				Harness:         "fake",
				MaxCPUs:         7,
				ResumeSessionID: "session:not-a-guid",
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
			if got := tk.GetSessionID(); got != "session:not-a-guid" {
				t.Errorf("SessionID = %q, want session:not-a-guid", got)
			}
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
				Repos:   []CreateRepo{{Name: "my/repo"}},
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
			if te.Code != CodeUnknownRepository {
				t.Errorf("Code = %q, want %q", te.Code, CodeUnknownRepository)
			}
		})
		t.Run("error_images_unsupported", func(t *testing.T) {
			t.Parallel()
			m := newManagerWithRepo(t)
			_, err := m.Create(t.Context(), CreateParams{
				Prompt:  agent.Prompt{Text: "hi", Images: []logdata.ImageData{{}}},
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
			m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: map[harness.Name]agent.Backend{"fake": &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}}}})
			registerCheckout(t, m.Checkouts, "my/repo", &repo.Checkout{Dir: "/tmp/my-repo"})
			src := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "src"}, "fake", "")
			src.Repos = []repo.Mount{{Name: "my/repo", Branch: "caic-1", GitRoot: "/tmp/my-repo"}}
			src.MaxCPUs = 5
			src.GitHubToken = true
			src.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "md-agent-src"), runtime.ConnectionTarget{SSHHost: "md-agent-src"}, "", "", 0)
			src.SetState(task.StateWaiting)
			e := m.NewEntry(src, nil)
			m.Insert(src.ID, e)
			return m, e
		}
		for _, delegated := range []bool{false, true} {
			t.Run(fmt.Sprintf("log_initialized_before_publication/delegated=%t", delegated), func(t *testing.T) {
				t.Parallel()
				m, src := newForkManager(t)
				t.Cleanup(func() {
					if err := m.Close(); err != nil {
						t.Error(err)
					}
				})
				checkPublishedTaskLog(t, m, func() (ksid.ID, error) {
					p := &ForkParams{Prompt: agent.Prompt{Text: "publication"}}
					if delegated {
						return src.Lifecycle.ForkDelegated(t.Context(), p)
					}
					return src.Lifecycle.Fork(t.Context(), p)
				})
			})
		}
		t.Run("valid_resolved_overrides_and_max_cpus", func(t *testing.T) {
			t.Parallel()
			m, src := newForkManager(t)
			id, err := src.Lifecycle.Fork(t.Context(), &ForkParams{
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
		t.Run("valid_delegated_fork_sets_parent_with_CAIC_MCP_disabled", func(t *testing.T) {
			t.Parallel()
			m, src := newForkManager(t)
			src.Task().CaicMCP = true
			id, err := src.Lifecycle.ForkDelegated(t.Context(), &ForkParams{Prompt: agent.Prompt{Text: "child"}})
			if err != nil {
				t.Fatalf("ForkDelegated: %v", err)
			}
			awaitTaskCleanup(t, m, id)
			entry, ok := m.GetEntry(id)
			if !ok {
				t.Fatal("child entry not found")
			}
			child := entry.Task()
			if child.ParentTaskID != src.Task().ID {
				t.Errorf("ParentTaskID = %s, want %s", child.ParentTaskID, src.Task().ID)
			}
			if child.CaicMCP {
				t.Error("delegated child CAIC MCP = true, want false")
			}
		})
		t.Run("delegated_fork_rejects_eleventh_non_purged_child", func(t *testing.T) {
			t.Parallel()
			m, src := newForkManager(t)
			for range maxDelegatedTasksPerParent {
				child := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "child"}, "", "")
				child.ParentTaskID = src.Task().ID
				m.Insert(child.ID, m.NewEntry(child, nil))
			}
			_, err := src.Lifecycle.ForkDelegated(t.Context(), &ForkParams{Prompt: agent.Prompt{Text: "one too many"}})
			if err == nil || !strings.Contains(err.Error(), "10 non-purged child tasks") {
				t.Fatalf("ForkDelegated() error = %v, want child-task limit", err)
			}
		})
		t.Run("parent_stop_does_not_change_child", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			parent := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "parent"}, harness.Claude, "")
			parent.SetState(task.StateWaiting)
			child := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "child"}, harness.Claude, "")
			child.ParentTaskID = parent.ID
			child.SetState(task.StateWaiting)
			parentEntry := m.NewEntry(parent, nil)
			m.Insert(parent.ID, parentEntry)
			m.Insert(child.ID, m.NewEntry(child, nil))

			recordTestTaskLog(t, m, parentEntry)
			if err := parentEntry.Lifecycle.Stop(t.Context()); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			if got := child.GetState(); got != task.StateWaiting {
				t.Errorf("child state = %s, want %s", got, task.StateWaiting)
			}
		})
		t.Run("parent_purge_does_not_change_child", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			parent := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "parent"}, harness.Claude, "")
			parent.SetState(task.StateStopped)
			child := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "child"}, harness.Claude, "")
			child.ParentTaskID = parent.ID
			child.SetState(task.StateWaiting)
			parentEntry := m.NewEntry(parent, nil)
			m.Insert(parent.ID, parentEntry)
			m.Insert(child.ID, m.NewEntry(child, nil))

			t.Cleanup(func() {
				if err := parentEntry.Lifecycle.Close(); err != nil {
					t.Errorf("Close: %v", err)
				}
			})
			recordTestTaskLog(t, m, parentEntry)
			if err := parentEntry.Lifecycle.Purge(t.Context(), 0); err != nil {
				t.Fatalf("Purge: %v", err)
			}
			if got := child.GetState(); got != task.StateWaiting {
				t.Errorf("child state = %s, want %s", got, task.StateWaiting)
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
			version, _ := m.ChangeSnapshot()
			id, err := src.Lifecycle.Fork(t.Context(), &ForkParams{Prompt: agent.Prompt{Text: "fork"}})
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
			fork.Lifecycle.wg.Wait()
			_, _, changes, _ := m.TaskChanges(version)
			found := 0
			for _, change := range changes {
				if change.TaskID == id && change.Summary && change.Notifications {
					found++
				}
			}
			if found < 2 {
				t.Fatalf("fork outcome missing alert invalidation: %+v", changes)
			}
			if got := src.LogPath.Get(); got != "" {
				t.Errorf("source log path = %q after fork, want empty (fork must not write to the source entry's log path)", got)
			}
		})
		t.Run("valid_stopped_source", func(t *testing.T) {
			t.Parallel()
			m, src := newForkManager(t)
			src.Task().SetState(task.StateStopped)
			id, err := src.Lifecycle.Fork(t.Context(), &ForkParams{Prompt: agent.Prompt{Text: "fork"}})
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
			id, err := src.Lifecycle.Fork(t.Context(), &ForkParams{Prompt: agent.Prompt{Text: "fork"}})
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
			id, err := src.Lifecycle.Fork(t.Context(), &ForkParams{Prompt: agent.Prompt{Text: "fork"}})
			if err != nil {
				t.Fatalf("Fork: %v", err)
			}
			awaitTaskCleanup(t, m, id)
			e, _ := m.GetEntry(id)
			metadata := taskrun.MakeMetadata(e.Task())
			if metadata[runtime.MetadataTaskID] != e.Task().ID.String() {
				t.Errorf("metadata[%s] = %q, want %q", runtime.MetadataTaskID, metadata[runtime.MetadataTaskID], e.Task().ID.String())
			}
		})
		t.Run("error_extra_repo_overlap", func(t *testing.T) {
			t.Parallel()
			_, src := newForkManager(t)
			_, err := src.Lifecycle.Fork(t.Context(), &ForkParams{
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
			src.SetState(task.StateWaiting)
			e := m.NewEntry(src, nil)
			m.Insert(src.ID, e)
			_, err := e.Lifecycle.Fork(t.Context(), &ForkParams{Prompt: agent.Prompt{Text: "fork"}})
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
			// agent.ReadPlan fails and Restart returns KindInternal.
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetState(task.StateWaiting)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID, e)
			err := e.Lifecycle.Restart(t.Context(), agent.Prompt{})
			te, ok := errors.AsType[*Error](err)
			if !ok {
				t.Fatalf("err %v is not a *Error", err)
			}
			if te.Kind != KindInternal {
				t.Errorf("Kind = %v, want KindInternal (err=%v)", te.Kind, err)
			}
		})
		t.Run("error_wrong_state", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetState(task.StateStopped)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID, e)
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
			tk.SetState(task.StateWaiting)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID, e)
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
			backend := &reconnectInputBackend{FakeBackend: &agenttest.FakeBackend{HarnessName: "reconnect", Images: true}}
			t.Cleanup(backend.stop)
			cacheDir := t.TempDir()
			logDir := filepath.Join(cacheDir, "tasks")
			m := newTestManager(t, Config{
				ServerCtx: t.Context(),
				LogStore:  taskhistory.NewStore(testLogger(), taskslog.NewStore(logDir), backends.Default("", nil)),
				Backends:  map[harness.Name]agent.Backend{"reconnect": backend},
			})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "reconnect", "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(task.StateRunning)
			tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
				&agent.AskMessage{
					ToolUseID: "toolu-1",
					Questions: []logdata.AskQuestion{{Question: "Which?"}},
				},
				&agent.PendingUserActionMessage{
					MessageType: agent.PendingUserActionMessageType,
					Action: logdata.PendingUserAction{
						Kind:      agent.PendingUserActionAskUserQuestion,
						RequestID: "req-1",
						ToolUseID: "toolu-1",
						Ask: logdata.PendingAskAction{
							Questions: []logdata.AskQuestion{{Question: "Which?"}},
						},
					},
				},
				&agent.ResultMessage{MessageType: "result"},
			}))
			logW, path, err := (taskhistory.NewStore(testLogger(), taskslog.NewStore(logDir), backends.Default("", nil))).Open(tk.LogFilename(), tk.LogHeader())
			if err != nil {
				t.Fatal(err)
			}
			if err := logW.AppendRecord(agent.PhysicalRecord{Encoded: []byte(`{"t":"input","ts":1.000,"msg":{"id":7,"method":"thread/start","params":{}}}` + "\n" + `{"t":"agent","ts":1.001,"msg":{"id":7,"result":{"thread":{"id":"saved-thread"}}}}` + "\n")}); err != nil {
				t.Fatal(err)
			}
			if err := logW.Close(); err != nil {
				t.Fatal(err)
			}
			e := m.NewEntry(tk, nil)
			e.LogPath.Set(path)
			m.Insert(tk.ID, e)
			e.Finish(&task.Result{State: task.StateFailed, Err: errors.New("old reconnect failure")})

			if err := e.Lifecycle.SendInput(t.Context(), agent.Prompt{Text: "A"}); err != nil {
				t.Fatal(err)
			}

			if e.Result() != nil {
				t.Fatal("successful input reconnect retained stale failure")
			}
			select {
			case <-e.Done():
				t.Fatal("successful input reconnect is terminal")
			default:
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
		t.Run("error_reconnect_preserves_failure", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: map[harness.Name]agent.Backend{harness.Codex: &agenttest.FakeBackend{HarnessName: harness.Codex}}})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, harness.Codex, "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetSessionMetadata(agent.SessionMetadata{SessionID: "saved-session"})
			tk.SetState(task.StateWaiting)
			e := m.NewEntry(tk, nil)
			recordTestTaskLog(t, m, e)
			old := &task.Result{State: task.StateFailed, Err: errors.New("old reconnect failure")}
			e.Finish(old)
			m.Insert(tk.ID, e)
			if err := e.Lifecycle.SendInput(t.Context(), agent.Prompt{Text: "retry"}); err == nil {
				t.Fatal("failed reconnect accepted input")
			}
			if e.Result() != old {
				t.Fatal("failed input reconnect cleared old failure")
			}
			select {
			case <-e.Done():
			default:
				t.Fatal("failed reconnect lost terminal state")
			}
		})
		t.Run("error_delivery_failure_is_not_no_session", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, harness.Codex, "")
			tk.SetState(task.StateWaiting)

			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID, e)
			backend := &localProcessBackend{FakeBackend: &agenttest.FakeBackend{WireFactory: codex.New("", nil).NewWire}, command: []string{"sleep", "60"}}
			startQueueSession(t, e, backend)
			err := e.Lifecycle.SendInput(t.Context(), agent.Prompt{Text: "go"})
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
			if tk.HasAcceptedInputEvidence() {
				t.Fatal("rejected encoded prompt became accepted input")
			}
			msgs := tk.Messages()
			if len(msgs) != 3 {
				t.Fatalf("failure evidence=%#v", msgs)
			}
			failure, ok := msgs[2].(*agent.SystemMessage)
			if !ok || failure.Subtype != agent.SystemSubtypeInputDeliveryFailed || !strings.Contains(failure.Detail, "no thread ID") {
				t.Fatalf("failure evidence=%#v", msgs[2])
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
				id := tk.ID
				wg.Go(func() {
					m.Insert(id, m.NewEntry(tk, nil))
				})
			}
			for range 5 {
				wg.Go(func() {
					m.Range(func(id ksid.ID, e *Entry) bool { return true })
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
			tk.SetState(task.StateStopped)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID, e)
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
			tk.SetState(task.StateWaiting)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID, e)
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
			tk.SetState(task.StateWaiting)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID, e)
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
			rb := &runtimetest.FakeBackend{}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, rb, &observedRuntimeInfo{FakeInfo: &runtimetest.FakeInfo{}, backend: rb})})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.Repos = []repo.Mount{{Name: "repo/x", Branch: "caic-1"}}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-dead"), runtime.ConnectionTarget{SSHHost: "ctr-dead"}, "", "", 0)
			tk.SetState(task.StateRunning)
			m.Insert(tk.ID, m.NewEntry(tk, nil))
			m.handleRuntimeInstanceExit(t.Context(), runtime.NewID("test-runtime", "ctr-dead"))
			if got := tk.GetState(); got != task.StateStopped {
				t.Errorf("state = %v, want StateStopped", got)
			}
		})
		t.Run("valid_skips_purged", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-purged"), runtime.ConnectionTarget{SSHHost: "ctr-purged"}, "", "", 0)
			tk.SetState(task.StatePurged)
			m.Insert(tk.ID, m.NewEntry(tk, nil))
			m.handleRuntimeInstanceExit(t.Context(), runtime.NewID("test-runtime", "ctr-purged"))
			if got := tk.GetState(); got != task.StatePurged {
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
			tk.SetState(task.StatePurging)
			m.Insert(tk.ID, m.NewEntry(tk, nil))
			m.handleRuntimeInstanceExit(t.Context(), runtime.NewID("test-runtime", "ctr-purging"))
			if got := tk.GetState(); got != task.StatePurging {
				t.Errorf("state = %v (should stay Purging)", got)
			}
		})
		t.Run("valid_skips_stopping", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-stopping"), runtime.ConnectionTarget{SSHHost: "ctr-stopping"}, "", "", 0)
			tk.SetState(task.StateStopping)
			m.Insert(tk.ID, m.NewEntry(tk, nil))
			m.handleRuntimeInstanceExit(t.Context(), runtime.NewID("test-runtime", "ctr-stopping"))
			if got := tk.GetState(); got != task.StateStopping {
				t.Errorf("state = %v (should stay Stopping)", got)
			}
		})
		t.Run("valid_skips_stopped", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-stopped"), runtime.ConnectionTarget{SSHHost: "ctr-stopped"}, "", "", 0)
			tk.SetState(task.StateStopped)
			m.Insert(tk.ID, m.NewEntry(tk, nil))
			m.handleRuntimeInstanceExit(t.Context(), runtime.NewID("test-runtime", "ctr-stopped"))
			if got := tk.GetState(); got != task.StateStopped {
				t.Errorf("state = %v (should stay Stopped)", got)
			}
		})
		t.Run("valid_skips_wrong_container", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-alive"), runtime.ConnectionTarget{SSHHost: "ctr-alive"}, "", "", 0)
			tk.SetState(task.StateRunning)
			m.Insert(tk.ID, m.NewEntry(tk, nil))
			m.handleRuntimeInstanceExit(t.Context(), runtime.NewID("test-runtime", "ctr-other"))
			if got := tk.GetState(); got != task.StateRunning {
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
			m.Insert(tk.ID, e)
			if _, err := m.HistorySource(e); !errors.Is(err, taskslog.ErrNoLog) {
				t.Fatalf("HistorySource error = %v, want ErrNoLog", err)
			}
		})
		t.Run("valid_uses_header_resolver", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "task.jsonl"), []byte(
				`{"version":3,"prompt":"task","repos":[],"harness":"claude","t":"caic_meta"}`+"\n"+
					`{"t":"agent","ts":1.000,"msg":{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}}`+"\n",
			), 0o600); err != nil {
				t.Fatal(err)
			}
			logs, err := taskhistory.NewStore(testLogger(), taskslog.NewStore(dir), backends.Default("", nil)).LoadUnsettled()
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
			entry := newTestPurgedEntry(t, tk, &task.Result{State: task.StatePurged}, logs[0])
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
	t.Run("LastAgentMessage", func(t *testing.T) {
		t.Parallel()
		t.Run("error/canceled live snapshot", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "live"}, "", "")
			tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{&agent.TextMessage{Text: "first"}, &agent.TextMessage{Text: "second"}}))
			entry := m.NewEntry(tk, nil)
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			cancel()
			if _, err := m.LastAgentMessage(ctx, entry); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled query = %v", err)
			}
		})
		t.Run("error/canceled while another query holds the gate", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			entry := m.NewEntry(mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "task"}, "", ""), nil)
			entry.historyGate <- struct{}{}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := entry.acquireHistory(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("acquireHistory while the gate is held = %v", err)
			}
			<-entry.historyGate
		})
		t.Run("live_uses_memory_without_log", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "live"}, "", "")
			root := agent.MessageSource{ThreadID: "root", TurnID: "turn", RootThreadID: "root"}
			child := agent.MessageSource{ThreadID: "child", TurnID: "turn", RootThreadID: "root"}
			last := &agent.TextMessage{Source: root, Text: "root answer"}
			tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
				&agent.TextMessage{Source: root, Text: "earlier"},
				&agent.TextMessage{Source: child, Text: "child answer"},
				last,
				&agent.TextMessage{Source: root},
				&agent.ResultMessage{Source: root},
			}))
			entry := m.NewEntry(tk, nil)
			got, err := m.LastAgentMessage(t.Context(), entry)
			if err != nil {
				t.Fatal(err)
			}
			if got != last {
				t.Fatalf("LastAgentMessage = %#v", got)
			}
		})
		t.Run("restored_scan_is_reused_until_the_log_changes", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "task.jsonl")
			writeColdQueryLog(t, path,
				`{"t":"agent","ts":0.500,"msg":{"type":"assistant","message":{"content":[{"type":"text","text":"earlier disk result"}]}}}`,
				`{"t":"agent","ts":1.000,"msg":{"type":"assistant","message":{"content":[{"type":"text","text":"disk result"}]}}}`,
				`{"state":"purged","t":"result"}`)
			m, entry, scans := newColdQueryManager(t, dir)
			for range 3 {
				got, err := m.LastAgentMessage(t.Context(), entry)
				if err != nil {
					t.Fatal(err)
				}
				text, ok := got.(*agent.TextMessage)
				if !ok || text.Text != "disk result" {
					t.Fatalf("LastAgentMessage = %#v", got)
				}
			}
			if *scans != 1 {
				t.Fatalf("identical queries decoded the log %d times, want 1", *scans)
			}
			appendColdQueryLog(t, path, `{"t":"agent","ts":2.000,"msg":{"type":"assistant","message":{"content":[{"type":"text","text":"appended result"}]}}}`)
			got, err := m.LastAgentMessage(t.Context(), entry)
			if err != nil {
				t.Fatal(err)
			}
			if text, ok := got.(*agent.TextMessage); !ok || text.Text != "appended result" {
				t.Fatalf("LastAgentMessage after append = %#v", got)
			}
			if *scans != 2 {
				t.Fatalf("changed log was not decoded again: scans = %d", *scans)
			}
			if len(entry.Task().Messages()) != 0 {
				t.Fatal("cold query retained the transcript on the task")
			}
		})
	})
	t.Run("ToolInput", func(t *testing.T) {
		t.Parallel()
		t.Run("live_uses_memory_without_log", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "live"}, "", "")
			root := agent.MessageSource{ThreadID: "root", TurnID: "turn", RootThreadID: "root"}
			child := agent.MessageSource{ThreadID: "child", TurnID: "turn", RootThreadID: "root"}
			tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
				&agent.ToolUseMessage{Source: root, ToolUseID: "local", Input: json.RawMessage(`{"owner":"root"}`)},
				&agent.ToolUseMessage{Source: child, ToolUseID: "local", Input: json.RawMessage(`{"owner":"child"}`)},
			}))
			entry := m.NewEntry(tk, nil)
			if _, err := m.ToolInput(t.Context(), entry, &ToolInputQuery{ToolUseID: "local"}); !errors.Is(err, ErrToolUseAmbiguous) {
				t.Fatalf("ambiguous query = %v", err)
			}
			got, err := m.ToolInput(t.Context(), entry, &ToolInputQuery{ToolUseID: "local", Source: root})
			if err != nil {
				t.Fatal(err)
			}
			if string(got.Input) != `{"owner":"root"}` {
				t.Fatalf("scoped input = %s", got.Input)
			}
			if _, err := m.ToolInput(t.Context(), entry, &ToolInputQuery{ToolUseID: "absent"}); !errors.Is(err, ErrToolUseNotFound) {
				t.Fatalf("absent query = %v", err)
			}
		})
		t.Run("restored_scan_is_reused", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			records := make([]string, 0, historyMemoTools+2)
			for i := range historyMemoTools + 2 {
				records = append(records, fmt.Sprintf(`{"t":"agent","ts":1.000,"msg":{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tool-%d","name":"Bash","input":{"command":"c%d"}}]}}}`, i, i))
			}
			path := filepath.Join(dir, "task.jsonl")
			writeColdQueryLog(t, path, append(records, `{"state":"purged","t":"result"}`)...)
			m, entry, scans := newColdQueryManager(t, dir)
			for range 3 {
				got, err := m.ToolInput(t.Context(), entry, &ToolInputQuery{ToolUseID: "tool-0"})
				if err != nil {
					t.Fatal(err)
				}
				if string(got.Input) != `{"command":"c0"}` {
					t.Fatalf("input = %s", got.Input)
				}
			}
			if *scans != 1 {
				t.Fatalf("identical queries decoded the log %d times, want 1", *scans)
			}
			for i := range historyMemoTools + 2 {
				if _, err := m.ToolInput(t.Context(), entry, &ToolInputQuery{ToolUseID: fmt.Sprintf("tool-%d", i)}); err != nil {
					t.Fatal(err)
				}
			}
			before := *scans
			// Only the newest historyMemoTools answers survive the count bound.
			for i := 2; i < historyMemoTools+2; i++ {
				if _, err := m.ToolInput(t.Context(), entry, &ToolInputQuery{ToolUseID: fmt.Sprintf("tool-%d", i)}); err != nil {
					t.Fatal(err)
				}
			}
			if *scans != before {
				t.Fatalf("retained answers were recomputed: scans = %d, want %d", *scans, before)
			}
			if _, err := m.ToolInput(t.Context(), entry, &ToolInputQuery{ToolUseID: "tool-0"}); err != nil {
				t.Fatal(err)
			}
			if *scans != before+1 {
				t.Fatalf("evicted answer scans = %d, want %d", *scans, before+1)
			}
			appendColdQueryLog(t, path, `{"t":"agent","ts":2.000,"msg":{"type":"assistant","message":{"content":[{"type":"text","text":"later"}]}}}`)
			last, err := m.LastAgentMessage(t.Context(), entry)
			if err != nil {
				t.Fatal(err)
			}
			if text, ok := last.(*agent.TextMessage); !ok || text.Text != "later" {
				t.Fatalf("LastAgentMessage = %#v", last)
			}
		})
		t.Run("oversized_input_is_not_retained", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			huge := fmt.Sprintf(`{"command":%q}`, strings.Repeat("x", historyMemoToolBytes+1))
			writeColdQueryLog(t, filepath.Join(dir, "task.jsonl"),
				fmt.Sprintf(`{"t":"agent","ts":1.000,"msg":{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tool-1","name":"Write","input":%s}]}}}`, huge),
				`{"state":"purged","t":"result"}`)
			m, entry, scans := newColdQueryManager(t, dir)
			for range 2 {
				got, err := m.ToolInput(t.Context(), entry, &ToolInputQuery{ToolUseID: "tool-1"})
				if err != nil {
					t.Fatal(err)
				}
				if len(got.Input) != len(huge) {
					t.Fatalf("input length = %d, want %d", len(got.Input), len(huge))
				}
			}
			if len(entry.historyMemo.tools) != 0 {
				t.Fatalf("retained %d oversized answers, want 0", len(entry.historyMemo.tools))
			}
			if *scans != 2 {
				t.Fatalf("oversized answer scans = %d, want 2", *scans)
			}
		})
		t.Run("retained_outcomes_are_bounded", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			records := make([]string, 0, historyMemoTools+2)
			for i := range historyMemoTools + 2 {
				records = append(records, fmt.Sprintf(`{"t":"agent","ts":1.000,"msg":{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tool-%d","name":"Bash","input":{"command":"%d%s"}}]}}}`, i, i, strings.Repeat("x", historyMemoToolBytes/3)))
			}
			writeColdQueryLog(t, filepath.Join(dir, "task.jsonl"), append(records, `{"state":"purged","t":"result"}`)...)
			m, entry, _ := newColdQueryManager(t, dir)
			for i := range historyMemoTools + 2 {
				if _, err := m.ToolInput(t.Context(), entry, &ToolInputQuery{ToolUseID: fmt.Sprintf("tool-%d", i)}); err != nil {
					t.Fatal(err)
				}
			}
			retained := 0
			for _, tool := range entry.historyMemo.tools {
				retained += len(tool.input.Input)
			}
			if retained > historyMemoToolBytes {
				t.Fatalf("retained %d bytes, want at most %d", retained, historyMemoToolBytes)
			}
			if len(entry.historyMemo.tools) == 0 || len(entry.historyMemo.tools) == historyMemoTools+2 {
				t.Fatalf("retained %d answers, want some but not all", len(entry.historyMemo.tools))
			}
			if newest := fmt.Sprintf("tool-%d", historyMemoTools+1); entry.historyMemo.tools[0].key.ToolUseID != newest {
				t.Fatalf("newest retained answer = %q, want %q", entry.historyMemo.tools[0].key.ToolUseID, newest)
			}
		})
		t.Run("absent_tool_verdict_is_retained", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeColdQueryLog(t, filepath.Join(dir, "task.jsonl"),
				`{"t":"agent","ts":1.000,"msg":{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tool-1","name":"Bash","input":{"command":"c"}}]}}}`,
				`{"state":"purged","t":"result"}`)
			m, entry, scans := newColdQueryManager(t, dir)
			for range 2 {
				if _, err := m.ToolInput(t.Context(), entry, &ToolInputQuery{ToolUseID: "absent"}); !errors.Is(err, ErrToolUseNotFound) {
					t.Fatalf("absent query = %v", err)
				}
			}
			if *scans != 1 {
				t.Fatalf("repeated absent query scans = %d, want 1", *scans)
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
			all := []*taskhistory.LoadedTask{
				{
					TaskID:            id.String(),
					Prompt:            "test task",
					Title:             "Test Title",
					Harness:           "claude",
					RuntimeName:       "docker",
					Repos:             []repo.Mount{{Name: "repo/a", Branch: "caic-1"}},
					State:             task.StateStopped,
					LastTrailer:       &task.Result{State: task.StateStopped},
					StartedAt:         now.Add(-1 * time.Hour),
					LastActivityAt:    now,
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
					RequestedModel:    "model-1",
					RequestedEffort:   "high",
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
			e, ok := m.GetEntry(id)
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
			if tk.RequestedModel != "model-1" || tk.RequestedEffort != "high" {
				t.Errorf("model/effort = %q/%q, want model-1/high", tk.RequestedModel, tk.RequestedEffort)
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
			all := []*taskhistory.LoadedTask{{
				TaskID:         id.String(),
				Prompt:         "old task",
				Harness:        "claude",
				State:          task.StateStopped,
				LastTrailer:    &task.Result{State: task.StateStopped},
				LastActivityAt: time.Now().UTC(),
			}}
			if err := m.LoadPurgedTasks(all); err != nil {
				t.Fatalf("LoadPurgedTasks: %v", err)
			}
			e, ok := m.GetEntry(id)
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
				marshal(logdata.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "pi task", Harness: string(harness.Pi), StartedAt: time.Now().UTC()}),
				`{"session_id":"ses-1","agent_version":"pi 1.2.3","t":"session"}`,
				`{"t":"text","text":"` + strings.Repeat("x", 70<<10) + `"}`,
				`{"state":"stopped","t":"result"}`,
			}
			path := filepath.Join(dir, id.String()+"--.jsonl")
			if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			logs, err := taskhistory.NewStore(testLogger(), taskslog.NewStore(dir), backends.Default("", nil)).LoadUnsettled()
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
			e, ok := m.GetEntry(id)
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
			all := []*taskhistory.LoadedTask{
				{
					TaskID:         id.String(),
					Prompt:         "was running",
					Repos:          []repo.Mount{{Name: "repo/a"}},
					State:          task.StateRunning,
					LastActivityAt: now,
				},
			}
			err := m.LoadPurgedTasks(all)
			if err != nil {
				t.Fatalf("LoadPurgedTasks: %v", err)
			}
			e, _ := m.GetEntry(id)
			if got := e.Task().GetState(); got != task.StateFailed {
				t.Errorf("state = %v, want StateFailed (running→failed)", got)
			}
		})
		t.Run("valid_fallback_title", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			now := time.Now().UTC()
			id := ksid.NewID()
			all := []*taskhistory.LoadedTask{
				{
					TaskID:         id.String(),
					Prompt:         "this is the prompt",
					LastActivityAt: now,
					State:          task.StateStopped,
				},
			}
			_ = m.LoadPurgedTasks(all)
			e, _ := m.GetEntry(id)
			if got := e.Task().Title(); got != "this is the prompt" {
				t.Errorf("Title = %q, want prompt fallback", got)
			}
		})
		t.Run("valid_no_result_fallback_to_failed", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			now := time.Now().UTC()
			id := ksid.NewID()
			all := []*taskhistory.LoadedTask{
				{
					TaskID:         id.String(),
					Prompt:         "test",
					LastActivityAt: now,
				},
			}
			_ = m.LoadPurgedTasks(all)
			e, _ := m.GetEntry(id)
			if got := e.Result().State; got != task.StateFailed {
				t.Errorf("Result.State = %v, want StateFailed (fallback)", got)
			}
		})
		t.Run("valid_invalid_ksid_fallback", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			now := time.Now().UTC()
			all := []*taskhistory.LoadedTask{
				{
					TaskID:         "invalid",
					Prompt:         "test",
					LastActivityAt: now,
					State:          task.StateStopped,
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
			active.Repos = []repo.Mount{{Name: "repo/a", Branch: "caic-live"}}
			active.SetTitle("active")
			m.Insert(activeID, m.NewEntry(active, nil))
			duplicateBranchID := ksid.NewID()
			keptID := ksid.NewID()
			all := []*taskhistory.LoadedTask{
				{
					TaskID:         activeID.String(),
					Prompt:         "replacement by id",
					Repos:          []repo.Mount{{Name: "repo/a", Branch: "caic-old"}},
					LastActivityAt: now,
					State:          task.StateStopped,
				},
				{
					TaskID:         duplicateBranchID.String(),
					Prompt:         "replacement by branch",
					Repos:          []repo.Mount{{Name: "repo/a", Branch: "caic-live"}},
					LastActivityAt: now,
					State:          task.StateStopped,
				},
				{
					TaskID:         keptID.String(),
					Prompt:         "kept",
					Repos:          []repo.Mount{{Name: "repo/a", Branch: "caic-kept"}},
					LastActivityAt: now,
					State:          task.StateStopped,
				},
			}
			if err := m.LoadPurgedTasks(all); err != nil {
				t.Fatalf("LoadPurgedTasks: %v", err)
			}
			if got := m.Len(); got != 2 {
				t.Fatalf("Len() = %d, want active + kept", got)
			}
			if _, ok := m.GetEntry(duplicateBranchID); ok {
				t.Fatal("duplicate branch task was loaded")
			}
			activeEntry, _ := m.GetEntry(activeID)
			if got := activeEntry.Task().Title(); got != "active" {
				t.Errorf("active title = %q, want active", got)
			}
			if _, ok := m.GetEntry(keptID); !ok {
				t.Fatal("kept task was not loaded")
			}
		})
	})
	t.Run("Sync", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_refreshes_live_repository_summary", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			initTaskRepo(t, dir, []string{"branch", "caic-0"})
			checkout, err := repo.NewCheckout(t.Context(), testLogger(), dir, t.TempDir(), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), "git", "rev-parse", "caic-0")
			cmd.Dir = dir
			out, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			backend := &runtimetest.FakeBackend{
				DiffOutput:            "3\t0\tcommitted.go\n1\t0\tpending.go\n",
				FetchedBranches:       []runtime.FetchedBranch{{RepositoryPath: "/home/user/src/repo", BranchName: "caic-0", CommitHash: strings.TrimSpace(string(out))}},
				RepositoryStatusValue: runtime.RepositoryStatus{Branch: "caic-0", DiffStat: []runtime.GitFileStat{{Path: "pending.go", LinesAdded: 1}}},
			}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, backend, nil)})
			registerCheckout(t, m.Checkouts, "repo", checkout)
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.Repos = []repo.Mount{{Name: "repo", GitRoot: dir, ContainerPath: "~/src/repo", Branch: "caic-0"}}
			tk.SetRuntimeConnectionInfo("test-runtime:ctr", runtime.ConnectionTarget{}, "", "", 0)
			tk.SetState(task.StateWaiting)
			tk.RestoreRepositorySummary(&repo.GitSnapshot{Read: repo.NewGitRead(tk.RuntimeInstanceID()), DiffStat: logdata.DiffStat{{Path: "committed.go", LinesAdded: 3}, {Path: "pending.go", LinesAdded: 1}}})
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID, e)
			recordTestTaskLog(t, m, e)
			changed := m.Changed()
			result, err := e.Lifecycle.Sync(t.Context(), SyncTargetOrigin, false)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != "synced" || len(result.DiffStat) != 2 {
				t.Fatalf("sync must preserve its pre-push report: %+v", result)
			}
			if got := tk.Snapshot().DiffStat; len(got) != 1 || got[0].Path != "pending.go" {
				t.Fatalf("post-push stats must retain only pending edits: %+v", got)
			}
			if got := tk.Snapshot().RepoStates; len(got) != 1 || got[0].Ahead != 0 || got[0].LinesAdded != 1 {
				t.Fatalf("post-push repository state: %+v", got)
			}
			select {
			case <-changed:
			default:
				t.Fatal("sync did not notify task-list subscribers")
			}
		})
		t.Run("error_pending_no_container", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetState(task.StatePending)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID, e)
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
			tk.SetState(task.StatePurging)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID, e)
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
			tk.SetState(task.StateProvisioning)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID, e)
			_, err := e.Lifecycle.Sync(t.Context(), SyncTargetOrigin, false)
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
		t.Run("error_force_not_supported", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetState(task.StateRunning)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID, e)
			_, err := e.Lifecycle.Sync(t.Context(), SyncTargetDefault, true)
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
	})
	t.Run("Purge", func(t *testing.T) {
		t.Parallel()
		t.Run("stopped_task_skips_recovery_window", func(t *testing.T) {
			t.Parallel()
			fake := &blockingPurgeBackend{
				FakeBackend: &runtimetest.FakeBackend{},
				started:     make(chan struct{}),
				release:     make(chan struct{}),
			}
			m := newTestManager(t, Config{
				ServerCtx: t.Context(),
				Runtimes:  newTestRuntime(t, fake, nil),
			})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, harness.Claude, "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(task.StateStopped)
			entry := m.NewEntry(tk, nil)
			m.Insert(tk.ID, entry)

			t.Cleanup(func() {
				close(fake.release)
				if err := entry.Lifecycle.Close(); err != nil {
					t.Errorf("Close: %v", err)
				}
			})
			recordTestTaskLog(t, m, entry)
			if err := entry.Lifecycle.Purge(t.Context(), time.Hour); err != nil {
				t.Fatalf("Purge: %v", err)
			}
			select {
			case <-fake.started:
			case <-time.After(time.Second):
				t.Fatal("backend Purge did not start immediately")
			}
		})
		t.Run("zero_delay_purges_immediately", func(t *testing.T) {
			t.Parallel()
			fake := &runtimetest.FakeBackend{}
			m := newTestManager(t, Config{
				ServerCtx: t.Context(),
				Runtimes:  newTestRuntime(t, fake, nil),
			})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, harness.Claude, "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(task.StateStopped)
			entry := m.NewEntry(tk, nil)
			m.Insert(tk.ID, entry)

			t.Cleanup(func() {
				if err := entry.Lifecycle.Close(); err != nil {
					t.Errorf("Close: %v", err)
				}
			})
			recordTestTaskLog(t, m, entry)
			if err := entry.Lifecycle.Purge(t.Context(), 0); err != nil {
				t.Fatalf("Purge: %v", err)
			}
			deadline := time.Now().Add(time.Second)
			for fake.Status("ctr-1") != runtimetest.StatusPurged {
				if time.Now().After(deadline) {
					t.Fatal("runtime was not purged immediately")
				}
				time.Sleep(time.Millisecond)
			}
		})
		t.Run("revive_cancels_crashed_task_recovery_window", func(t *testing.T) {
			t.Parallel()
			releaseRevive := make(chan struct{})
			releaseWire := make(chan struct{})
			wireStarted := make(chan struct{})
			var blockWire atomic.Bool
			backend := &agenttest.FakeBackend{WireFactory: func() agent.WireFormat {
				if blockWire.CompareAndSwap(true, false) {
					close(wireStarted)
					<-releaseWire
				}
				return claudecode.New().NewWire()
			}}
			fake := &blockingReviveBackend{FakeBackend: &runtimetest.FakeBackend{}, release: releaseRevive}
			m := newTestManager(t, Config{
				ServerCtx: t.Context(),
				Runtimes:  newTestRuntime(t, fake, nil),
				Backends:  agent.Backends{harness.Claude: backend},
			})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, harness.Claude, "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(task.StateCrashed)
			entry := m.NewEntry(tk, nil)
			m.Insert(tk.ID, entry)
			tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{&agent.UserInputMessage{Text: "x"}}))

			t.Cleanup(func() {
				close(releaseWire)
				close(releaseRevive)
				if err := entry.Lifecycle.Close(); err != nil {
					t.Errorf("Close: %v", err)
				}
			})
			recordTestTaskLog(t, m, entry)
			if err := entry.Lifecycle.Purge(t.Context(), 20*time.Millisecond); err != nil {
				t.Fatalf("Purge: %v", err)
			}
			blockWire.Store(true)
			if err := entry.Lifecycle.Revive(); err != nil {
				t.Fatalf("Revive: %v", err)
			}
			select {
			case <-wireStarted:
			case <-time.After(5 * time.Second):
				t.Fatal("revival did not reach its recording constructor")
			}
			// The reservation is held before the durable revival intent can
			// publish provisioning. Expiring the timer must not steal that task.
			if state := tk.GetState(); state != task.StateStopped {
				t.Fatalf("reserved revival state = %s, want stopped", state)
			}
			changed := m.Changed()
			select {
			case <-changed:
				t.Fatalf("purge published state %s while revival owned the reservation", tk.GetState())
			case <-time.After(100 * time.Millisecond):
			}
			if got := fake.Status("ctr-1"); got == runtimetest.StatusPurged {
				t.Fatal("runtime was purged after revive")
			}
		})
		t.Run("error_negative_delay", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetState(task.StateStopped)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID, e)
			err := e.Lifecycle.Purge(t.Context(), -time.Millisecond)
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
		t.Run("error_wrong_state", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetState(task.StatePurged)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID, e)
			err := e.Lifecycle.Purge(t.Context(), time.Millisecond)
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
		t.Run("valid_after_finished_crash", func(t *testing.T) {
			t.Parallel()
			fake := &runtimetest.FakeBackend{}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, fake, nil)})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, harness.Claude, "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(task.StateCrashed)
			entry := m.NewEntry(tk, nil)
			entry.Finish(&task.Result{State: task.StateCrashed, Err: errors.New("agent crashed")})
			m.Insert(tk.ID, entry)

			recordTestTaskLog(t, m, entry)
			if err := entry.Lifecycle.Purge(t.Context(), time.Millisecond); err != nil {
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, harness.Claude, "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(task.StateRunning)
			entry := m.NewEntry(tk, nil)
			m.Insert(tk.ID, entry)

			recordTestTaskLog(t, m, entry)
			// The purge must not reach the runtime before the stop it superseded
			// publishes its result, or the entry and the log describe the stop.
			var publishedDuringPurge *task.Result
			fake.onPurge = func() { publishedDuringPurge = entry.Result() }
			if err := entry.Lifecycle.Stop(t.Context()); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			select {
			case <-fake.started:
			case <-time.After(time.Second):
				t.Fatal("StopTask did not reach backend Stop")
			}

			if err := entry.Lifecycle.Purge(t.Context(), time.Millisecond); err != nil {
				t.Fatalf("Purge: %v", err)
			}
			intent, err := m.logStore.LoadForTaskIDs([]string{tk.ID.String()})
			if err != nil || len(intent) != 1 || intent[0].State != task.StatePurging {
				t.Fatalf("purge intent=%v err=%v", intent, err)
			}
			select {
			case <-entry.Done():
				t.Fatal("purge completed before graceful stop")
			default:
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
			for {
				if result := entry.Result(); result != nil && result.State == task.StatePurged {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("state = %v, result = %v, want StatePurged after StopTask finishes", tk.GetState(), entry.Result())
				}
				time.Sleep(time.Millisecond)
			}
			if publishedDuringPurge == nil || publishedDuringPurge.State != task.StateStopped {
				t.Fatalf("purge reached the runtime with published result %v, want the stopped result first", publishedDuringPurge)
			}
		})
	})
	t.Run("Stop", func(t *testing.T) {
		t.Parallel()
		t.Run("error_wrong_state", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetState(task.StateStopped)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID, e)
			err := e.Lifecycle.Stop(t.Context())
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
		t.Run("valid_stops_failed_container", func(t *testing.T) {
			t.Parallel()
			fake := &runtimetest.FakeBackend{}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, fake, nil)})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, harness.Claude, "")
			id := runtime.NewID("test-runtime", "ctr-1")
			tk.SetRuntimeConnectionInfo(id, runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(task.StateFailed)
			log, path, err := m.logStore.Open(tk.LogFilename(), tk.LogHeader())
			if err != nil {
				t.Fatal(err)
			}
			if err := log.AppendMessage(taskhistory.ResultMessage(tk.Title(), &task.Result{State: task.StateFailed})); err != nil {
				t.Fatal(err)
			}
			if err := log.Close(); err != nil {
				t.Fatal(err)
			}
			path, err = m.logStore.Compress(path, nil, task.StateFailed)
			if err != nil {
				t.Fatal(err)
			}
			entry := m.NewEntry(tk, nil)
			entry.LogPath.Set(path)
			entry.Finish(&task.Result{State: task.StateFailed, Err: errors.New("previous revive failed")})
			m.Insert(tk.ID, entry)
			if err := entry.Lifecycle.Stop(t.Context()); err != nil {
				t.Fatal(err)
			}
			entry.Lifecycle.wg.Wait()
			if got := tk.GetState(); got != task.StateStopped {
				t.Fatalf("state = %s, want stopped", got)
			}
			if got := fake.Status(id); got != runtimetest.StatusStopped {
				t.Fatalf("runtime status = %s, want stopped", got)
			}
			if res := entry.Result(); res == nil || res.State != task.StateStopped || res.Err != nil {
				t.Fatalf("result = %+v, want stopped without old failure", res)
			}
			loaded, err := m.logStore.LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}
			if len(loaded) != 1 || loaded[0].LastTrailer.State != task.StateStopped {
				t.Fatalf("loaded task = %+v, want stopped trailer", loaded)
			}
		})
		t.Run("valid_stops_container_backend", func(t *testing.T) {
			t.Parallel()
			// Backend is the interface seam, so a fake stands in for Docker.
			fake := &runtimetest.FakeBackend{}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, fake, nil)})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, harness.Claude, "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(task.StateRunning)
			entry := m.NewEntry(tk, nil)
			m.Insert(tk.ID, entry)

			recordTestTaskLog(t, m, entry)
			if err := entry.Lifecycle.Stop(t.Context()); err != nil {
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
		t.Run("error_stop_completion_in_progress", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetState(task.StateStopped)
			entry := m.NewEntry(tk, nil)
			entry.Finish(&task.Result{State: task.StateStopped})
			done := entry.Done()
			// Stop has published the task state but still owns trailer/result
			// publication. Revive must not reset this entry incarnation yet.
			entry.Lifecycle.operationMu.Lock()
			defer entry.Lifecycle.operationMu.Unlock()
			if err := entry.Lifecycle.Revive(); err == nil {
				t.Fatal("Revive accepted an unfinished stop")
			}
			if entry.Done() != done || tk.GetState() != task.StateStopped {
				t.Fatal("Revive reset the entry before stop completion")
			}
		})
		t.Run("error_wrong_state", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.SetState(task.StateRunning)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID, e)
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
			tk.SetState(task.StateCrashed)
			entry := m.NewEntry(tk, nil)
			m.Insert(tk.ID, entry)
			tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{&agent.UserInputMessage{Text: "x"}}))

			recordTestTaskLog(t, m, entry)
			if err := entry.Lifecycle.Revive(); err != nil {
				t.Fatalf("Revive: %v", err)
			}
			awaitQueueCondition(t, m, func() bool { return tk.GetState() == task.StateProvisioning })
			close(releaseRevive)
			select {
			case <-entry.Done():
			case <-time.After(time.Second):
				t.Fatal("failed revive did not close done")
			}
		})
		t.Run("error_failure_preserves_retryable_crash", func(t *testing.T) {
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
			tk.SetState(task.StateStopped)
			log, path, err := m.logStore.Open(tk.LogFilename(), tk.LogHeader())
			if err != nil {
				t.Fatal(err)
			}
			if err := log.AppendMessage(taskhistory.ResultMessage(tk.Title(), &task.Result{State: task.StateStopped})); err != nil {
				t.Fatal(err)
			}
			if err := log.Close(); err != nil {
				t.Fatal(err)
			}
			entry := m.NewEntry(tk, nil)
			entry.LogPath.Set(path)
			m.Insert(tk.ID, entry)

			tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{&agent.UserInputMessage{Text: "x"}}))
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
			if got := tk.GetState(); got != task.StateCrashed {
				t.Fatalf("state = %v, want StateCrashed", got)
			}
			result := entry.Result()
			if result == nil || result.State != task.StateCrashed || result.Err == nil {
				t.Fatalf("Result = %v, want crashed result with error", result)
			}
			if got := fake.Status(tk.RuntimeInstanceID()); got != runtimetest.StatusStopped {
				t.Fatalf("runtime status = %v, want stopped", got)
			}
		})
	})
	t.Run("SendInput_Images", func(t *testing.T) {
		t.Parallel()
		t.Run("error_images_unsupported", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: map[harness.Name]agent.Backend{"fake": &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}}}})
			registerCheckout(t, m.Checkouts, "repo/a", &repo.Checkout{Dir: "/tmp/repo"})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "fake", "")
			tk.Repos = []repo.Mount{{Name: "repo/a"}}
			tk.SetState(task.StateWaiting)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID, e)
			err := e.Lifecycle.SendInput(t.Context(), agent.Prompt{Text: "go", Images: []logdata.ImageData{{}}})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
	})
	t.Run("watchSession", func(t *testing.T) {
		t.Run("valid_session_error_crashes_and_stops_instance", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, harness.Claude, "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ssh-failed"), runtime.ConnectionTarget{SSHHost: "ssh-failed"}, "", "", 0)
			tk.SetState(task.StateRunning)
			runtimeBackend := &runtimetest.FakeBackend{}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, runtimeBackend, nil)})
			entry := m.NewEntry(tk, nil)
			m.Insert(tk.ID, entry)

			backend := &localProcessBackend{FakeBackend: &agenttest.FakeBackend{WireFactory: claudecode.New().NewWire}, command: []string{"sh", "-c", "exit 255"}}
			h := startManagedSession(t, entry, backend)
			entry.Lifecycle.watchSession(h)

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
		t.Run("server_shutdown_preserves_task_and_instance", func(t *testing.T) {
			t.Parallel()
			serverCtx, cancelServer := context.WithCancel(t.Context())
			t.Cleanup(cancelServer)
			runtimeBackend := &runtimetest.FakeBackend{}
			instanceID, err := runtimeBackend.Launch(t.Context(), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, harness.Claude, "")
			tk.SetRuntimeConnectionInfo(instanceID, runtime.ConnectionTarget{SSHHost: "ssh-restart"}, "", "", 0)
			tk.SetState(task.StateRunning)
			m := newTestManager(t, Config{ServerCtx: serverCtx, Runtimes: newTestRuntime(t, runtimeBackend, nil)})
			entry := m.NewEntry(tk, nil)
			m.Insert(tk.ID, entry)

			backend := &localProcessBackend{FakeBackend: &agenttest.FakeBackend{WireFactory: claudecode.New().NewWire}, command: []string{"sleep", "30"}}
			h := startManagedSession(t, entry, backend)
			entry.Lifecycle.watchSession(h)
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entry.Done():
				t.Fatal("server shutdown finished the task")
			case <-time.After(100 * time.Millisecond):
			}
			if got := tk.GetState(); got != task.StateRunning {
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
			m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: map[harness.Name]agent.Backend{"claude": &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}}}})
			if _, err := m.ResolveParser("claude"); err != nil {
				t.Fatalf("ResolveParser: %v", err)
			}
		})
		t.Run("missing_backend", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			if _, err := m.ResolveParser("unconfigured"); err == nil || !strings.Contains(err.Error(), "unknown harness") {
				t.Fatalf("ResolveParser error = %v, want unknown-harness error", err)
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
			if te.Code != CodeUnknownHarness {
				t.Errorf("Code = %q, want %q", te.Code, CodeUnknownHarness)
			}
		})
		t.Run("error_unsupported_model", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: map[harness.Name]agent.Backend{"fake": &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}}}})
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
			if te.Code != CodeUnsupportedModel {
				t.Errorf("Code = %q, want %q", te.Code, CodeUnsupportedModel)
			}
		})
		t.Run("error_unknown_extra_repo", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: map[harness.Name]agent.Backend{"fake": &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}}}})
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
			src.Repos = []repo.Mount{{Name: "repo/a", Branch: "caic-1"}}
			src.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "md-agent-src"), runtime.ConnectionTarget{SSHHost: "md-agent-src"}, "", "", 0)
			src.SetState(task.StateWaiting)
			e := m.NewEntry(src, nil)
			m.Insert(src.ID, e)
			return e
		}

		defaultBackends := map[harness.Name]agent.Backend{"fake": &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}}}

		t.Run("error_unknown_harness", func(t *testing.T) {
			t.Parallel()
			e := forkSetup(t, "fake", defaultBackends)
			_, err := e.Lifecycle.Fork(t.Context(), &ForkParams{Prompt: agent.Prompt{Text: "fork"}, Harness: "bogus"})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
			if te.Code != CodeUnknownHarness {
				t.Errorf("Code = %q, want %q", te.Code, CodeUnknownHarness)
			}
		})
		t.Run("error_unsupported_model", func(t *testing.T) {
			t.Parallel()
			e := forkSetup(t, "fake", defaultBackends)
			_, err := e.Lifecycle.Fork(t.Context(), &ForkParams{Prompt: agent.Prompt{Text: "fork"}, Model: "unsupported"})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
			if te.Code != CodeUnsupportedModel {
				t.Errorf("Code = %q, want %q", te.Code, CodeUnsupportedModel)
			}
		})
		t.Run("error_model_with_new_harness", func(t *testing.T) {
			t.Parallel()
			agents := map[harness.Name]agent.Backend{
				"fake":  &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}},
				"fake2": &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m2"}}}},
			}
			e := forkSetup(t, "fake", agents)
			_, err := e.Lifecycle.Fork(t.Context(), &ForkParams{Prompt: agent.Prompt{Text: "fork"}, Harness: "fake2", Model: "unsupported"})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
			if te.Code != CodeUnsupportedModel {
				t.Errorf("Code = %q, want %q", te.Code, CodeUnsupportedModel)
			}
		})
		t.Run("error_inherited_model_with_new_harness", func(t *testing.T) {
			t.Parallel()
			agents := map[harness.Name]agent.Backend{
				"fake":  &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}},
				"fake2": &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m2"}}}},
			}
			e := forkSetup(t, "fake", agents)
			e.Task().RequestedModel = "m1"
			_, err := e.Lifecycle.Fork(t.Context(), &ForkParams{Prompt: agent.Prompt{Text: "fork"}, Harness: "fake2"})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
			if te.Code != CodeUnsupportedModel {
				t.Errorf("Code = %q, want %q", te.Code, CodeUnsupportedModel)
			}
		})
		t.Run("new_harness_preserves_inherited_model", func(t *testing.T) {
			t.Parallel()
			agents := map[harness.Name]agent.Backend{
				"fake":  &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}},
				"fake2": &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}},
			}
			e := forkSetup(t, "fake", agents)
			e.Task().RequestedModel = "m1"
			id, err := e.Lifecycle.Fork(t.Context(), &ForkParams{Prompt: agent.Prompt{Text: "fork"}, Harness: "fake2"})
			if err != nil {
				t.Fatalf("Fork: %v", err)
			}
			awaitTaskCleanup(t, e.Lifecycle.manager, id)
			fork, ok := e.Lifecycle.manager.GetEntry(id)
			if !ok {
				t.Fatalf("forked task %q not found", id)
			}
			if got := fork.Task().RequestedModel; got != "m1" {
				t.Errorf("forked model = %q, want inherited m1", got)
			}
		})
		t.Run("error_no_container", func(t *testing.T) {
			t.Parallel()
			e := forkSetup(t, "fake", defaultBackends)
			// Overwrite the instance to empty.
			e.Task().SetRuntimeConnectionInfo("", runtime.ConnectionTarget{SSHHost: ""}, "", "", 0)
			_, err := e.Lifecycle.Fork(t.Context(), &ForkParams{Prompt: agent.Prompt{Text: "fork"}})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
		t.Run("error_wrong_state", func(t *testing.T) {
			t.Parallel()
			e := forkSetup(t, "fake", defaultBackends)
			e.Task().SetState(task.StateProvisioning)
			_, err := e.Lifecycle.Fork(t.Context(), &ForkParams{Prompt: agent.Prompt{Text: "fork"}})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
		t.Run("error_unknown_extra_repo", func(t *testing.T) {
			t.Parallel()
			e := forkSetup(t, "fake", defaultBackends)
			_, err := e.Lifecycle.Fork(t.Context(), &ForkParams{Prompt: agent.Prompt{Text: "fork"}, ExtraRepos: []ForkRepo{{Name: "ghost"}}})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
		t.Run("error_unavailable_source_harness_has_code", func(t *testing.T) {
			t.Parallel()
			e := forkSetup(t, "bogus", defaultBackends)
			_, err := e.Lifecycle.Fork(t.Context(), &ForkParams{Prompt: agent.Prompt{Text: "fork"}, Model: "m1"})
			te, ok := errors.AsType[*Error](err)
			if !ok || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
			if te.Code != CodeUnknownHarness {
				t.Errorf("Code = %q, want %q", te.Code, CodeUnknownHarness)
			}
		})
	})

	t.Run("AdoptInstances", func(t *testing.T) {
		t.Parallel()

		t.Run("reconnect_clears_historical_failure", func(t *testing.T) {
			t.Parallel()
			for _, mode := range []string{"attach_error", "early_exit", "healthy"} {
				t.Run(mode, func(t *testing.T) {
					t.Parallel()
					id := ksid.NewID()
					instanceID := runtime.NewID("test-runtime", "md-agent-retained-alert")
					backend := &reconnectInputBackend{FakeBackend: &agenttest.FakeBackend{HarnessName: harness.Codex}, exitImmediately: mode == "early_exit"}
					var selected agent.Backend = backend
					if mode == "attach_error" {
						selected = backend.FakeBackend
					}
					info := &runtimetest.FakeInfo{Meta: map[string]string{"md-agent-retained-alert\x00caic.id": id.String(), "md-agent-retained-alert\x00caic.harness": string(harness.Codex)}}
					m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, info), Backends: map[harness.Name]agent.Backend{harness.Codex: selected}})
					t.Cleanup(backend.stop)
					m.relay = fakeRelayReader{
						statusFn: func(context.Context, runtime.ConnectionTarget) (bool, string, error) { return true, "alive", nil },
						readTailFn: func(context.Context, runtime.ConnectionTarget, int64) (agent.RelaySnapshot, int64, error) {
							return agent.RelaySnapshot{}, 0, nil
						},
						readLogFn: func(context.Context, runtime.ConnectionTarget, int) string { return "" },
					}
					tk := mustNewTask(t, id, agent.Prompt{Text: "accepted prompt"}, harness.Codex, "")
					log, path, err := m.logStore.Open(tk.LogFilename(), tk.LogHeader())
					if err != nil {
						t.Fatal(err)
					}
					if err := log.AppendMessage(&agent.UserInputMessage{Text: "accepted prompt"}); err != nil {
						t.Fatal(err)
					}
					if err := log.AppendMessage(&agent.MetaSessionMessage{MessageType: "caic_session", SessionID: "original-session"}); err != nil {
						t.Fatal(err)
					}
					if err := log.AppendMessage(taskhistory.ResultMessage("", &task.Result{State: task.StateFailed, Err: errors.New("old failure")})); err != nil {
						t.Fatal(err)
					}
					if err := log.Close(); err != nil {
						t.Fatal(err)
					}
					logs, err := m.logStore.LoadForTaskIDs([]string{id.String()})
					if err != nil {
						t.Fatal(err)
					}
					entries, err := m.ImportInstances(t.Context(), []runtime.Instance{{ID: instanceID, State: "running", AgentTarget: runtime.ConnectionTarget{SSHHost: "md-agent-retained-alert"}}}, logs)
					if err != nil || len(entries) != 1 {
						t.Fatalf("entries=%v err=%v", entries, err)
					}
					e := entries[0]
					if mode != "healthy" {
						e.Lifecycle.wg.Wait()
						if e.Result() == nil || e.Result().Err == nil {
							t.Fatal("failed attachment cleared failure")
						}
						return
					}
					deadline := time.After(12 * time.Second)
					for e.Result() != nil {
						select {
						case <-deadline:
							t.Fatal("successful reconnect retained stale summary failure")
						case <-time.After(time.Millisecond):
						}
					}
					if !e.Task().HasAcceptedInputEvidence() || !e.Task().HasSession() {
						t.Fatal("reconnection lost session or history")
					}
					select {
					case <-e.Done():
						t.Fatal("reconnected task is still terminal")
					default:
					}
					history, err := taskhistory.NewStore(slog.New(slog.DiscardHandler), taskslog.NewStore(filepath.Dir(path)), backends.Default("", nil)).LoadSource(path)
					if err != nil {
						t.Fatal(err)
					}
					if err := history.LoadMessages(); err != nil {
						t.Fatal(err)
					}
					if history.LastTrailer == nil || history.LastTrailer.Err == nil {
						t.Fatal("historical failure was discarded")
					}
				})
			}
		})
		t.Run("counts_missing_logs_without_counting_non_caic_instances", func(t *testing.T) {
			t.Parallel()
			info := &runtimetest.FakeInfo{Meta: map[string]string{
				"md-agent-missing-one\x00caic.id": ksid.NewID().String(),
				"md-agent-missing-two\x00caic.id": ksid.NewID().String(),
			}}
			m := newTestManager(t, Config{
				ServerCtx: t.Context(),
				Runtimes:  newTestRuntime(t, &runtimetest.FakeBackend{}, info),
			})
			instances := []runtime.Instance{
				{ID: runtime.NewID("test-runtime", "md-agent-missing-one")},
				{ID: runtime.NewID("test-runtime", "md-agent-missing-two")},
				{ID: runtime.NewID("test-runtime", "md-agent-unowned")},
			}
			entries, err := m.ImportInstances(t.Context(), instances, nil)
			if len(entries) != 0 {
				t.Fatalf("imported missing logs: %+v", entries)
			}
			if failure, ok := errors.AsType[*ImportError](err); !ok || failure.Failed != 2 {
				t.Fatalf("missing log restoration count = %v, want two failed instances", err)
			}
		})
		t.Run("bounds_concurrent_imports", func(t *testing.T) {
			t.Parallel()
			const taskCount = maxConcurrentTaskImports + 3
			info := &runtimetest.FakeInfo{Meta: make(map[string]string, taskCount*2)}
			instances := make([]runtime.Instance, 0, taskCount)
			logDir := t.TempDir()
			store := taskhistory.NewStore(testLogger(), taskslog.NewStore(logDir), backends.Default("", nil))
			for i := range taskCount {
				taskID := ksid.NewID()
				name := fmt.Sprintf("md-agent-bounded-import-%d", i)
				info.Meta[name+"\x00caic.id"] = taskID.String()
				info.Meta[name+"\x00caic.harness"] = string(harness.Codex)
				instances = append(instances, runtime.Instance{
					ID:          runtime.NewID("test-runtime", runtime.InstanceID(name)),
					AgentTarget: runtime.ConnectionTarget{SSHHost: name},
					State:       "running",
				})
				tk := mustNewTask(t, taskID, agent.Prompt{Text: "accepted"}, harness.Codex, "")
				log, _, err := store.Open(tk.LogFilename(), tk.LogHeader())
				if err != nil {
					t.Fatal(err)
				}
				if err := errors.Join(log.AppendMessage(&agent.UserInputMessage{Text: "accepted"}), log.Close()); err != nil {
					t.Fatal(err)
				}
			}
			logs, err := store.LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}
			m := newTestManager(t, Config{
				ServerCtx: t.Context(),
				LogStore:  store,
				Runtimes:  newTestRuntime(t, &runtimetest.FakeBackend{}, info),
				Backends: map[harness.Name]agent.Backend{
					harness.Codex: &agenttest.FakeBackend{HarnessName: harness.Codex, WireFactory: codex.New("", nil).NewWire},
				},
			})
			started := make(chan struct{}, taskCount)
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseImports := func() {
				releaseOnce.Do(func() { close(release) })
			}
			t.Cleanup(releaseImports)
			var active atomic.Int32
			var peak atomic.Int32
			m.relay = fakeRelayReader{
				statusFn: func(context.Context, runtime.ConnectionTarget) (bool, string, error) { return true, "alive", nil },
				readTailFn: func(context.Context, runtime.ConnectionTarget, int64) (agent.RelaySnapshot, int64, error) {
					current := active.Add(1)
					for {
						previous := peak.Load()
						if current <= previous || peak.CompareAndSwap(previous, current) {
							break
						}
					}
					started <- struct{}{}
					<-release
					active.Add(-1)
					return agent.RelaySnapshot{}, 0, nil
				},
				readLogFn: func(context.Context, runtime.ConnectionTarget, int) string { return "" },
			}
			done := make(chan error, 1)
			var workers sync.WaitGroup
			workers.Go(func() {
				_, importErr := m.ImportInstances(t.Context(), instances, logs)
				done <- importErr
			})
			t.Cleanup(func() { releaseImports(); workers.Wait() })
			for range maxConcurrentTaskImports {
				select {
				case <-started:
				case <-time.After(time.Second):
					t.Fatal("timed out waiting for bounded imports to start")
				}
			}
			select {
			case <-started:
				t.Fatalf("more than %d imports started before a slot was released", maxConcurrentTaskImports)
			case <-time.After(50 * time.Millisecond):
			}
			releaseImports()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if got := peak.Load(); got != maxConcurrentTaskImports {
				t.Errorf("peak imports = %d, want %d", got, maxConcurrentTaskImports)
			}
		})
		t.Run("marks_interrupted_startup_failed_before_reconnect", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			instanceID := runtime.NewID("test-runtime", "md-agent-incomplete-startup")
			info := &runtimetest.FakeInfo{Meta: map[string]string{
				"md-agent-incomplete-startup\x00caic.id":      taskID.String(),
				"md-agent-incomplete-startup\x00caic.harness": string(harness.Codex),
			}}
			runtimeBackend := &runtimetest.FakeBackend{}
			logDir := t.TempDir()
			m := newTestManager(t, Config{
				ServerCtx: t.Context(),
				LogStore:  taskhistory.NewStore(testLogger(), taskslog.NewStore(logDir), backends.Default("", nil)),
				Runtimes:  newTestRuntime(t, runtimeBackend, info),
				Backends: map[harness.Name]agent.Backend{
					harness.Codex: &agenttest.FakeBackend{WireFactory: codex.New("", nil).NewWire},
				},
			})
			m.relay = fakeRelayReader{
				statusFn: func(context.Context, runtime.ConnectionTarget) (bool, string, error) {
					return true, "alive", nil
				},
				readTailFn: func(context.Context, runtime.ConnectionTarget, int64) (agent.RelaySnapshot, int64, error) {
					return agent.RelaySnapshot{}, 0, nil
				},
				readLogFn: func(context.Context, runtime.ConnectionTarget, int) string { return "" },
			}
			tk := mustNewTask(t, taskID, agent.Prompt{Text: "must not be silently retried"}, harness.Codex, "")
			log, _, err := m.logStore.Open(tk.LogFilename(), tk.LogHeader())
			if err != nil {
				t.Fatal(err)
			}
			if err := log.Close(); err != nil {
				t.Fatal(err)
			}
			logs, err := m.logStore.LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}

			adopted, err := m.ImportInstances(t.Context(), []runtime.Instance{{ID: instanceID, State: "running"}}, logs)
			if err != nil {
				t.Fatal(err)
			}
			if len(adopted) != 1 {
				t.Fatalf("len(adopted) = %d, want 1", len(adopted))
			}
			entry := adopted[0]
			if got := entry.Task().GetState(); got != task.StateFailed {
				t.Errorf("state = %s, want failed", got)
			}
			result := entry.Result()
			if result == nil || !errors.Is(result.Err, taskrun.ErrInitialPromptNotAccepted) {
				t.Errorf("result = %#v, want incomplete-startup failure", result)
			}
			if got := runtimeBackend.Status(instanceID); got != runtimetest.StatusStopped {
				t.Errorf("runtime status = %s, want stopped", got)
			}
			if entry.Task().HasSession() {
				t.Error("incomplete startup attached a live relay session")
			}
		})
		t.Run("offline_opencode_error_uses_recorded_input", func(t *testing.T) {
			t.Parallel()
			for _, compressed := range []bool{false, true} {
				t.Run(fmt.Sprintf("compressed_%v", compressed), func(t *testing.T) {
					t.Parallel()
					id := ksid.NewID()
					instance := runtime.Instance{ID: runtime.NewID("test-runtime", "md-agent-offline-error"), State: "running", AgentTarget: runtime.ConnectionTarget{SSHHost: "offline-error"}}
					store := taskhistory.NewStore(testLogger(), taskslog.NewStore(t.TempDir()), backends.Default("", nil))
					tk := mustNewTask(t, id, agent.Prompt{Text: "initial"}, harness.OpenCode, "")
					log, path, err := store.Open(tk.LogFilename(), tk.LogHeader())
					if err != nil {
						t.Fatal(err)
					}
					generation := `{"t":"relay_generation","generation":"offline"}`
					before := `{"t":"agent","ts":1.000,"msg":{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"before offline"}}}}}`
					request := []byte(`{"jsonrpc":"2.0","id":17,"method":"session/prompt","params":{"sessionId":"s","prompt":[{"type":"text","text":"initial"}]}}` + "\n")
					failure := `{"t":"agent","ts":2.000,"msg":{"jsonrpc":"2.0","id":17,"error":{"code":-32000,"message":"offline prompt rejected"}}}`
					if err := log.AppendRecord(agent.PhysicalRecord{Encoded: []byte(generation + "\n" + before + "\n")}); err != nil {
						t.Fatal(err)
					}
					if err := agenttest.AppendInputNativeRecord(log, request); err != nil {
						t.Fatal(err)
					}
					if err := log.AppendMessage(&agent.MetaSessionMessage{MessageType: "caic_session", SessionID: "s", AutoCompactTokenLimit: 244800}); err != nil {
						t.Fatal(err)
					}
					if compressed {
						if _, err := store.Compress(path, log, task.StateWaiting); err != nil {
							t.Fatal(err)
						}
					} else if err := log.Close(); err != nil {
						t.Fatal(err)
					}
					pending := inputqueue.Entry{ID: ksid.NewID().String(), TaskID: id.String(), Text: "queued follow-up"}
					if err := store.Journal.SaveInputQueue([]inputqueue.Entry{pending}); err != nil {
						t.Fatal(err)
					}
					// Repeat startup against the same relay snapshot. It includes no
					// stdin: request identity is available only in the local log.
					for startup := range 2 {
						t.Run(fmt.Sprintf("startup_%d", startup), func(t *testing.T) { //nolint:paralleltest // Successive startups own the same log and runtime exclusively.
							backend := &importedRecoveryBackend{FakeBackend: &agenttest.FakeBackend{HarnessName: harness.OpenCode}}
							m := newTestManager(t, Config{ServerCtx: t.Context(), LogStore: store,
								Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, &runtimetest.FakeInfo{Meta: map[string]string{
									"md-agent-offline-error\x00caic.id": id.String(), "md-agent-offline-error\x00caic.harness": string(harness.OpenCode),
								}}), Backends: map[harness.Name]agent.Backend{harness.OpenCode: backend}})
							t.Cleanup(func() {
								if err := m.Close(); err != nil {
									t.Error(err)
								}
								backend.stop()
							})
							m.relay = fakeRelayReader{
								statusFn: func(context.Context, runtime.ConnectionTarget) (bool, string, error) { return true, "alive", nil },
								readTailFn: func(_ context.Context, _ runtime.ConnectionTarget, _ int64) (agent.RelaySnapshot, int64, error) {
									return parsedRelayFixture(generation, before, failure)
								},
								readLogFn: func(context.Context, runtime.ConnectionTarget, int) string { return "" },
							}
							logs, err := store.LoadForTaskIDs([]string{id.String()})
							if err != nil {
								t.Fatal(err)
							}
							entries, err := m.ImportInstances(t.Context(), []runtime.Instance{instance}, logs)
							if err != nil || len(entries) != 1 {
								t.Fatalf("import = %v, %v", entries, err)
							}
							entry := entries[0]
							results := 0
							for _, message := range entry.Task().Messages() {
								if result, ok := message.(*agent.ResultMessage); ok {
									results++
									if !result.IsError || result.Result != "offline prompt rejected" {
										t.Fatalf("result = %+v", result)
									}
								}
							}
							if results != 1 || entry.Task().GetState() != task.StateWaiting {
								t.Fatalf("results = %d, state = %s; want one terminal error and waiting", results, entry.Task().GetState())
							}
							if got := entry.Task().Snapshot().AutoCompactTokenLimit; got != 244800 {
								t.Fatalf("auto-compact threshold = %d, want 244800", got)
							}
							if q := entry.PendingInput(); q == nil || q.ID.String() != pending.ID || q.Prompt.Text != pending.Text {
								t.Fatalf("pending input = %+v", q)
							}
							if got := entry.Task().RelayOffsetValue(); got != int64(len(generation)+len(before)+len(failure)+3) {
								t.Fatalf("relay offset = %d", got)
							}
							persisted, err := os.ReadFile(entry.LogPath.Get())
							if err != nil {
								t.Fatal(err)
							}
							if bytes.Count(persisted, []byte(failure+"\n")) != 1 || bytes.Count(persisted, []byte(before+"\n")) != 1 {
								t.Fatalf("relay records not persisted once: %s", persisted)
							}
						})
					}
				})
			}
		})
		t.Run("reconciled_semantic_error_aborts_before_registration", func(t *testing.T) {
			t.Parallel()
			id := ksid.NewID()
			parseErr := errors.New("invalid response in recorded request context")
			backend := &agenttest.FakeBackend{HarnessName: harness.Codex, WireFactory: func() agent.WireFormat {
				inputSeen := false
				return &agenttest.Wire{
					Input: func([]byte) ([]agent.Message, error) { inputSeen = true; return nil, nil },
					Output: func(line []byte) ([]agent.Message, error) {
						if inputSeen && bytes.Equal(line, []byte(`{"type":"after"}`)) {
							return nil, parseErr
						}
						return nil, nil
					},
				}
			}}
			m := newTestManager(t, Config{ServerCtx: t.Context(),
				Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, &runtimetest.FakeInfo{Meta: map[string]string{
					"md-agent-import-error\x00caic.id": id.String(), "md-agent-import-error\x00caic.harness": string(harness.Codex),
				}}), Backends: map[harness.Name]agent.Backend{harness.Codex: backend}})
			t.Cleanup(func() {
				if err := m.Close(); err != nil {
					t.Error(err)
				}
			})
			tk := mustNewTask(t, id, agent.Prompt{Text: "recorded prompt"}, harness.Codex, "")
			log, path, err := m.logStore.Open(tk.LogFilename(), tk.LogHeader())
			if err != nil {
				t.Fatal(err)
			}
			before := `{"t":"agent","ts":1.000,"msg":{"type":"before"}}`
			after := `{"t":"agent","ts":2.000,"msg":{"type":"after"}}`
			if err := log.AppendRecord(agent.PhysicalRecord{Encoded: []byte(before + "\n")}); err != nil {
				t.Fatal(err)
			}
			if err := agenttest.AppendInputNativeRecord(log, []byte("{\"type\":\"request\"}\n")); err != nil {
				t.Fatal(err)
			}
			if err := log.Close(); err != nil {
				t.Fatal(err)
			}
			m.relay = fakeRelayReader{
				statusFn: func(context.Context, runtime.ConnectionTarget) (bool, string, error) { return true, "alive", nil },
				readTailFn: func(_ context.Context, _ runtime.ConnectionTarget, _ int64) (agent.RelaySnapshot, int64, error) {
					return parsedRelayFixture(before, after)
				},
				readLogFn: func(context.Context, runtime.ConnectionTarget, int) string { return "" },
			}
			loaded, err := m.logStore.LoadSource(path)
			if err != nil {
				t.Fatal(err)
			}
			entries, err := m.ImportInstances(t.Context(), []runtime.Instance{{ID: runtime.NewID("test-runtime", "md-agent-import-error"), State: "running"}}, []*taskhistory.LoadedTask{loaded})
			if err == nil || !strings.Contains(err.Error(), parseErr.Error()) || len(entries) != 0 || m.Len() != 0 {
				t.Fatalf("import = %v, %v; registered = %d", entries, err, m.Len())
			}
			persisted, err := os.ReadFile(path) //nolint:gosec // Store returned this task's path in the test-owned directory.
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Count(persisted, []byte(after+"\n")) != 1 {
				t.Fatalf("verified suffix not persisted exactly once: %s", persisted)
			}
		})
		t.Run("restores_accepted_opencode_prompt_from_durable_input", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			instanceID := runtime.NewID("test-runtime", "md-agent-opencode-accepted-input")
			info := &runtimetest.FakeInfo{Meta: map[string]string{
				"md-agent-opencode-accepted-input\x00caic.id":      taskID.String(),
				"md-agent-opencode-accepted-input\x00caic.harness": string(harness.OpenCode),
			}}
			logDir := t.TempDir()
			m := newTestManager(t, Config{
				ServerCtx: t.Context(),
				LogStore:  taskhistory.NewStore(testLogger(), taskslog.NewStore(logDir), backends.Default("", nil)),
				Runtimes:  newTestRuntime(t, &runtimetest.FakeBackend{}, info),
				Backends: map[harness.Name]agent.Backend{
					harness.OpenCode: &agenttest.FakeBackend{HarnessName: harness.OpenCode, WireFactory: opencode.New("", nil).NewWire},
				},
			})
			m.relay = deadRelayFixture()
			tk := mustNewTask(t, taskID, agent.Prompt{Text: "accepted prompt"}, harness.OpenCode, "")
			log, _, err := m.logStore.Open(tk.LogFilename(), tk.LogHeader())
			if err != nil {
				t.Fatal(err)
			}
			request := []byte(`{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"ses_1","prompt":[{"type":"text","text":"accepted prompt"}]}}` + "\n")
			if err := agenttest.AppendInputNativeRecord(log, request); err != nil {
				t.Fatal(err)
			}
			if err := log.Close(); err != nil {
				t.Fatal(err)
			}
			logs, err := m.logStore.LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}

			adopted, err := m.ImportInstances(t.Context(), []runtime.Instance{{ID: instanceID, State: "running"}}, logs)
			if err != nil {
				t.Fatal(err)
			}
			if len(adopted) != 1 {
				t.Fatalf("len(adopted) = %d, want 1", len(adopted))
			}
			if !adopted[0].Task().HasAcceptedInputEvidence() {
				t.Fatal("durable OpenCode session/prompt did not restore accepted input")
			}
			if got := adopted[0].Task().GetState(); got == task.StateFailed {
				t.Fatalf("state = %s, accepted OpenCode prompt was misclassified", got)
			}
			if result := adopted[0].Result(); result != nil && errors.Is(result.Err, taskrun.ErrInitialPromptNotAccepted) {
				t.Fatalf("result = %#v, accepted OpenCode prompt was misclassified", result)
			}
		})
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
			if failure, ok := errors.AsType[*ImportError](err); !ok || failure.Failed != 2 {
				t.Fatalf("duplicate runtime restoration count = %v, want two failed instances", err)
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
			}}, []*taskhistory.LoadedTask{{
				TaskID:  taskID.String(),
				Harness: harness.Claude,
				Repos:   []repo.Mount{{Name: "repo/a", Branch: "caic-1"}},
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
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake), Backends: map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}}}})
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
				}, []*taskhistory.LoadedTask{{
					TaskID:  taskID.String(),
					Harness: harness.Claude,
					Repos:   []repo.Mount{{Name: "caic-xyz/caic", Branch: "caic-5"}},
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
			if got := quotas[0].RateLimits[0]; got.Utilization != 1 || !got.ResetsAt.Equal(resetAt) {
				t.Errorf("tracked rate limit = %#v, want 100%% used with reset %v", got, resetAt)
			}
		})
		t.Run("restores_diff_stat_and_repo_states", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			fake := &runtimetest.FakeInfo{Meta: map[string]string{
				"md-caic-caic-6\x00caic.id":      taskID.String(),
				"md-caic-caic-6\x00caic.harness": string(harness.Claude),
			}}
			runtimeBackend := &runtimetest.FakeBackend{
				DiffOutput: "5\t1\tmain.go\n",
				RepositoryStatusValue: runtime.RepositoryStatus{
					Branch: "caic-6",
					Ahead:  2,
					DiffStat: []runtime.GitFileStat{
						{Path: "main.go", LinesAdded: 5, LinesDeleted: 1},
					},
					Uncommitted: []runtime.GitFileStatus{
						{Path: "main.go", WorktreeStatus: "M"},
						{Path: "new.go", IndexStatus: "A"},
					},
				},
			}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, runtimeBackend, fake), Backends: map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{}}})
			m.relay = deadRelayFixture()
			registerCheckout(t, m.Checkouts, "caic-xyz/caic", &repo.Checkout{Dir: "/home/user/src/caic-xyz/caic"})

			adopted, err := m.ImportInstances(t.Context(),
				[]runtime.Instance{{
					ID:    runtime.NewID("test-runtime", "md-caic-caic-6"),
					State: "running",
					Repos: []runtime.Repo{{GitRoot: "/home/user/src/caic-xyz/caic", Branch: "caic-6", ContainerPath: "/home/user/src/caic-xyz/caic"}},
				}}, []*taskhistory.LoadedTask{{
					TaskID:  taskID.String(),
					Harness: harness.Claude,
					Repos:   []repo.Mount{{Name: "caic-xyz/caic", Branch: "caic-6"}},
				}})
			if err != nil {
				t.Fatalf("ImportInstances: %v", err)
			}
			if len(adopted) != 1 {
				t.Fatalf("adopted len = %d, want 1", len(adopted))
			}
			// The adopted card must immediately carry the probe summary: an
			// adopted session would otherwise publish nothing until its next
			// mutating tool call, leaving the card without stats.
			snap := adopted[0].Task().Snapshot()
			if len(snap.DiffStat) != 1 || snap.DiffStat[0].Path != "main.go" {
				t.Errorf("DiffStat = %+v, want [{main.go 5 1}]", snap.DiffStat)
			}
			want := []logdata.RepoState{{
				RepoIndex:        0,
				Branch:           "caic-6",
				Ahead:            2,
				ChangedFiles:     1,
				LinesAdded:       5,
				LinesDeleted:     1,
				UncommittedFiles: 2,
			}}
			if len(snap.RepoStates) != 1 || !reflect.DeepEqual(snap.RepoStates[0], want[0]) {
				t.Errorf("RepoStates = %+v, want %+v", snap.RepoStates, want)
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
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake), Backends: map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}}}})
			registerCheckout(t, m.Checkouts, "repo/a", &repo.Checkout{Dir: "/home/user/src/repo/a"})

			_, err := m.ImportInstances(t.Context(), []runtime.Instance{{
				ID: runtime.NewID("test-runtime", "repo-only-match"), State: "exited",
				Repos: []runtime.Repo{{GitRoot: "/home/user/src/repo/a", Branch: "caic-1", ContainerPath: "/home/user/src/repo/a"}},
			}}, []*taskhistory.LoadedTask{{
				TaskID:  otherTaskID.String(),
				Repos:   []repo.Mount{{Name: "repo/a", Branch: "caic-1"}},
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
			}}, []*taskhistory.LoadedTask{{
				TaskID:  taskID.String(),
				Repos:   []repo.Mount{{Name: "repo/a", Branch: "caic-1"}},
				Harness: "unknown",
			}})

			if err == nil || !strings.Contains(err.Error(), `unknown harness "unknown"`) {
				t.Fatalf("AdoptInstances error = %v, want unknown-harness error", err)
			}
		})
		t.Run("aborts_before_registration_or_reconnect_on_malformed_control", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			backend := &reconnectInputBackend{FakeBackend: &agenttest.FakeBackend{HarnessName: harness.Claude, Images: true}}
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
				readTailFn: func(context.Context, runtime.ConnectionTarget, int64) (agent.RelaySnapshot, int64, error) {
					return agent.RelaySnapshot{}, 0, nil
				},
				readLogFn: func(context.Context, runtime.ConnectionTarget, int) string { return "" },
			}

			meta, err := json.Marshal(logdata.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "semantic error", Harness: string(harness.Claude)})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(m.logStore.Journal.LogDir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(m.logStore.Journal.LogDir, taskID.String()+".jsonl")
			data := meta
			data = append(data, '\n')
			data = append(data, `{"state":123,"t":"result"}`...)
			data = append(data, '\n')
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			// Opening just the leading header must succeed. Adoption then owns
			// decoding the following malformed control before publishing the task.
			loaded, err := taskhistory.NewStore(slog.New(slog.DiscardHandler), taskslog.NewStore(filepath.Dir(path)), backends.Default("", nil)).LoadSource(path)
			if err != nil {
				t.Fatalf("valid leading header rejected: %v", err)
			}
			if loaded.TaskID != taskID.String() || loaded.Harness != harness.Claude {
				t.Fatalf("leading metadata = %+v", loaded)
			}
			if _, err := m.logStore.LoadForTaskIDs([]string{taskID.String()}); err == nil || !strings.Contains(err.Error(), "decode result") {
				t.Fatalf("inventory error = %v, want result control decoding error", err)
			}

			adopted, err := m.ImportInstances(t.Context(), []runtime.Instance{{
				ID:    runtime.NewID("test-runtime", "md-agent-semantic-error"),
				State: "running",
			}}, []*taskhistory.LoadedTask{loaded})

			if err == nil || !strings.Contains(err.Error(), "decode result") || !strings.Contains(err.Error(), "load reconciled task history "+taskID.String()) {
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
		t.Run("preserves_saved_title", func(t *testing.T) {
			t.Parallel()
			for _, saved := range []string{"", "Saved title"} {
				t.Run(saved, func(t *testing.T) {
					t.Parallel()
					id := ksid.NewID()
					fake := &runtimetest.FakeInfo{Meta: map[string]string{
						"md-agent-title\x00caic.id":      id.String(),
						"md-agent-title\x00caic.harness": string(harness.Claude),
					}}
					provider := &titleProvider{}
					m := newTestManager(t, Config{
						ServerCtx: t.Context(), Provider: provider,
						Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake),
						Backends: map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{}},
					})
					adopted, err := m.ImportInstances(t.Context(), []runtime.Instance{{ID: runtime.NewID("test-runtime", "md-agent-title"), State: "exited"}}, []*taskhistory.LoadedTask{{
						TaskID: id.String(), Harness: harness.Claude, Prompt: "test", Title: saved,
						Timeline: []agent.TimedMessage{{Message: &agent.ResultMessage{MessageType: "result", Result: "Completed work"}}},
					}})
					if err != nil || len(adopted) != 1 {
						t.Fatalf("ImportInstances = %v, %v", adopted, err)
					}
					adopted[0].Lifecycle.wg.Wait()
					want := saved
					calls := int32(0)
					if saved == "" {
						want = "Generated title"
						calls = 1
					}
					if got := adopted[0].Task().Title(); got != want || provider.calls.Load() != calls {
						t.Fatalf("title = %q, calls = %d; want %q, %d", got, provider.calls.Load(), want, calls)
					}
				})
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
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake), Backends: map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}}}})

			adopted, err := m.ImportInstances(t.Context(), []runtime.Instance{{ID: instanceID, State: "exited"}}, []*taskhistory.LoadedTask{{
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
		t.Run("log_only_history_is_fixed_prefix", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			instanceID := runtime.NewID("test-runtime", "md-agent-log-only-prefix")
			fake := &runtimetest.FakeInfo{Meta: map[string]string{
				"md-agent-log-only-prefix\x00caic.id":      taskID.String(),
				"md-agent-log-only-prefix\x00caic.harness": string(harness.Claude),
			}}
			m := newTestManager(t, Config{
				ServerCtx: t.Context(),
				Runtimes:  newTestRuntime(t, &runtimetest.FakeBackend{}, fake),
				Backends:  map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{}},
			})
			backing := make([]agent.TimedMessage, 2)
			backing[0].Message = &agent.TextMessage{Text: "durable"}
			loaded := &taskhistory.LoadedTask{
				TaskID: taskID.String(), Harness: harness.Claude, Prompt: "test", Timeline: backing[:1],
			}

			adopted, err := m.ImportInstances(t.Context(), []runtime.Instance{{
				ID: instanceID, State: "exited",
			}}, []*taskhistory.LoadedTask{loaded})
			if err != nil {
				t.Fatalf("ImportInstances: %v", err)
			}
			if len(adopted) != 1 {
				t.Fatalf("adopted len = %d, want 1", len(adopted))
			}

			recordTestTaskLog(t, m, adopted[0])
			if err := adopted[0].RecordHost(t.Context(), agent.ContextCleared()); err != nil {
				t.Fatal(err)
			}
			if got := backing[1].Message; got != nil {
				t.Fatalf("durable history spare slot = %T, want untouched", got)
			}
			if got := adopted[0].Task().MessageCount(); got != 2 {
				t.Fatalf("MessageCount = %d, want 2", got)
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
			runtimeBackend := &runtimetest.FakeBackend{
				RepositoryStatusValue: runtime.RepositoryStatus{
					Branch: "caic-9",
					Ahead:  3,
					DiffStat: []runtime.GitFileStat{
						{Path: "frontend/src/App.tsx", LinesAdded: 10, LinesDeleted: 2},
						{Path: "frontend/src/App.test.tsx", LinesAdded: 5, LinesDeleted: 1},
					},
					Uncommitted: []runtime.GitFileStatus{
						{Path: "frontend/src/App.tsx", WorktreeStatus: "M"},
					},
				},
			}
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
			}}, []*taskhistory.LoadedTask{{
				TaskID: taskID.String(), Harness: harness.Claude, Prompt: "restore diff",
				Repos: []repo.Mount{{Name: "repo/a", BaseBranch: "main", Branch: "caic-9"}},
			}})
			if err != nil {
				t.Fatalf("AdoptInstances: %v", err)
			}
			if len(adopted) != 1 {
				t.Fatalf("adopted len = %d, want 1", len(adopted))
			}
			want := logdata.DiffStat{
				{Path: "frontend/src/App.tsx", LinesAdded: 10, LinesDeleted: 2},
				{Path: "frontend/src/App.test.tsx", LinesAdded: 5, LinesDeleted: 1},
			}
			if got := adopted[0].Task().Snapshot().DiffStat; !slices.Equal(got, want) {
				t.Fatalf("DiffStat = %+v, want %+v", got, want)
			}
			wantStates := []logdata.RepoState{{
				RepoIndex:        0,
				Branch:           "caic-9",
				Ahead:            3,
				ChangedFiles:     2,
				LinesAdded:       15,
				LinesDeleted:     3,
				UncommittedFiles: 1,
			}}
			if got := adopted[0].Task().Snapshot().RepoStates; !reflect.DeepEqual(got, wantStates) {
				t.Fatalf("RepoStates = %+v, want %+v", got, wantStates)
			}
		})
		t.Run("valid_restores_launch_config_from_log", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			parentTaskID := ksid.NewID()
			fake := &runtimetest.FakeInfo{Meta: map[string]string{
				"restore-config\x00caic.id":      taskID.String(),
				"restore-config\x00caic.harness": string(harness.Claude),
			}}
			cacheDir := t.TempDir()
			m := newTestManager(t, Config{ServerCtx: t.Context(), LogStore: taskhistory.NewStore(testLogger(), taskslog.NewStore(filepath.Join(cacheDir, "tasks")), backends.Default("", nil)), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake), Backends: map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}})
			registerCheckout(t, m.Checkouts, "repo/a", &repo.Checkout{Dir: "/home/user/src/repo/a"})

			logDir := filepath.Join(cacheDir, "tasks")
			if err := os.MkdirAll(logDir, 0o750); err != nil {
				t.Fatal(err)
			}
			meta, err := json.Marshal(logdata.MetaMessage{
				MessageType:       "caic_meta",
				Version:           3,
				Prompt:            "restore config",
				Repos:             []logdata.MetaRepo{{Name: "repo/a", Branch: "caic-9"}},
				Harness:           string(harness.Claude),
				BaseImage:         "ghcr.io/caic/base:v1",
				ContainerPlatform: "linux/amd64",
				MaxCPUs:           5,
				CacheMounts:       []logdata.MetaCacheMount{{Name: "npm", HostPath: "~/.npm", ContainerPath: "/home/user/.npm", ReadOnly: true}},
				Mounts:            []logdata.MetaMount{{HostPath: "/host/work", ContainerPath: "/workspace/work", ReadOnly: true}},
				ParentTaskID:      parentTaskID.String(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(logDir, taskID.String()+"-repo-a-caic-9.jsonl"), append(meta, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			logs, err := taskhistory.NewStore(testLogger(), taskslog.NewStore(logDir), backends.Default("", nil)).LoadUnsettled()
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
			if got.ParentTaskID != parentTaskID {
				t.Errorf("ParentTaskID = %s, want %s", got.ParentTaskID, parentTaskID)
			}
		})

		t.Run("valid_persists_missing_v3_relay_records", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			fake := &runtimetest.FakeInfo{Meta: map[string]string{
				"persist-tail\x00caic.id":      taskID.String(),
				"persist-tail\x00caic.harness": string(harness.Claude),
			}}
			cacheDir := t.TempDir()
			logDir := filepath.Join(cacheDir, "tasks")
			store := taskhistory.NewStore(testLogger(), taskslog.NewStore(logDir), backends.Default("", nil))
			backend := &agenttest.FakeBackend{
				Inventory:   agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}},
				WireFactory: claudecode.New().NewWire,
			}
			m := newTestManager(t, Config{
				ServerCtx: t.Context(),
				LogStore:  store,
				Runtimes:  newTestRuntime(t, &runtimetest.FakeBackend{}, fake),
				Backends:  map[harness.Name]agent.Backend{harness.Claude: backend},
			})
			registerCheckout(t, m.Checkouts, "caic-xyz/caic", &repo.Checkout{Dir: "/home/user/src/caic-xyz/caic"})

			meta, err := taskslog.EncodeHeader(&logdata.MetaMessage{
				MessageType: "caic_meta",
				Version:     int(agent.LogVersionV3),
				Prompt:      "persist relay history",
				Repos:       []logdata.MetaRepo{{Name: "caic-xyz/caic", Branch: "caic-13"}},
				Harness:     string(harness.Claude),
			})
			if err != nil {
				t.Fatal(err)
			}
			beforeNative := `{"type":"assistant","message":{"model":"m","id":"msg_01","role":"assistant","content":[{"type":"text","text":"before restart"}],"usage":{}},"session_id":"s","uuid":"u1"}`
			duringNative := `{"type":"assistant","message":{"model":"m","id":"msg_02","role":"assistant","content":[{"type":"text","text":"during restart"}],"usage":{}},"session_id":"s","uuid":"u2"}`
			beforeRecord := fmt.Sprintf(`{"t":"agent","ts":1.000,"msg":%s}`, beforeNative)
			duringRecord := fmt.Sprintf(`{"t":"agent","ts":2.000,"msg":%s}`, duringNative)
			logPath := filepath.Join(logDir, taskID.String()+".jsonl")
			if err := os.MkdirAll(logDir, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(logPath, []byte(string(meta)+"\n"+beforeRecord+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			m.relay = fakeRelayReader{
				statusFn: func(context.Context, runtime.ConnectionTarget) (bool, string, error) {
					return true, "alive", nil
				},
				readTailFn: func(_ context.Context, _ runtime.ConnectionTarget, _ int64) (agent.RelaySnapshot, int64, error) {
					var timeline agent.RelaySnapshot
					var relayEnd int64
					for _, line := range []string{beforeRecord, duringRecord} {
						frame, err := taskslog.DecodeFrame([]byte(line))
						parsed := agent.ParsedRecord{Provenance: agent.FrameProvenance(&frame)}
						if err != nil {
							return agent.RelaySnapshot{}, 0, err
						}
						timeline.Encoded = append(timeline.Encoded, line...)
						timeline.Encoded = append(timeline.Encoded, '\n')
						relayEnd += int64(len(line) + 1)
						if parsed.Provenance.Source != agent.RecordSourceRelay {
							return agent.RelaySnapshot{}, 0, errors.New("v3 agent record is not relay-owned")
						}
						timeline.RelayRecords = append(timeline.RelayRecords, agent.RelayRecordBoundary{
							RelayEnd: relayEnd,

							ByteEnd:     len(timeline.Encoded),
							Fingerprint: sha256.Sum256([]byte(line)),
						})
					}
					return timeline, int64(len(timeline.Encoded)), nil
				},
				readLogFn: func(context.Context, runtime.ConnectionTarget, int) string { return "" },
			}
			logs, err := store.LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}
			adopted, err := m.ImportInstances(t.Context(), []runtime.Instance{{
				ID:    runtime.NewID("test-runtime", "persist-tail"),
				State: "running",
				Repos: []runtime.Repo{{
					GitRoot:       "/home/user/src/caic-xyz/caic",
					Branch:        "caic-13",
					ContainerPath: "/home/user/src/caic-xyz/caic",
				}},
			}}, logs)
			if err != nil {
				t.Fatalf("AdoptInstances: %v", err)
			}
			if len(adopted) != 1 {
				t.Fatalf("adopted len = %d, want 1", len(adopted))
			}
			persisted, err := os.ReadFile(logPath) //nolint:gosec // test-controlled path.
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Count(persisted, []byte(beforeRecord)) != 1 || bytes.Count(persisted, []byte(duringRecord)) != 1 {
				t.Fatalf("persisted relay records are not exactly once:\n%s", persisted)
			}
			texts := textMessages(adopted[0].Task().Messages())
			if !slices.Equal(texts, []string{"before restart", "during restart"}) {
				t.Fatalf("restored output = %#v", texts)
			}
			if got, want := adopted[0].Task().RelayOffsetValue(), int64(len(beforeRecord)+len(duringRecord)+2); got != want {
				t.Fatalf("relay offset = %d, want %d", got, want)
			}
		})

		t.Run("rejects_unverified_relay_tail_without_live_recovery", func(t *testing.T) {
			t.Parallel()
			cases := []struct {
				name string

				relayAlive bool
				readErr    error
			}{
				{name: "v3_mismatch", relayAlive: true},
				{name: "dead_relay"},
				{name: "unreadable_snapshot", relayAlive: true, readErr: errors.New("malformed relay snapshot")},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					taskID := ksid.NewID()
					cacheDir := t.TempDir()
					logDir := filepath.Join(cacheDir, "tasks")
					store := taskhistory.NewStore(testLogger(), taskslog.NewStore(logDir), agent.Backends{"reconnect": &agenttest.FakeBackend{HarnessName: "reconnect", WireFactory: codex.New("", nil).NewWire}})
					fake := &runtimetest.FakeInfo{Meta: map[string]string{
						"reject-tail\x00caic.id":      taskID.String(),
						"reject-tail\x00caic.harness": "reconnect",
					}}
					m := newTestManager(t, Config{
						ServerCtx: t.Context(),
						LogStore:  store,
						Runtimes:  newTestRuntime(t, &runtimetest.FakeBackend{}, fake),
						Backends: map[harness.Name]agent.Backend{"reconnect": &agenttest.FakeBackend{
							HarnessName: "reconnect",
							WireFactory: codex.New("", nil).NewWire,
						}},
					})
					registerCheckout(t, m.Checkouts, "repo/a", &repo.Checkout{Dir: "/home/user/src/repo/a"})

					meta, err := taskslog.EncodeHeader(&logdata.MetaMessage{
						MessageType: "caic_meta",
						Version:     agent.LogVersionV3,
						Prompt:      "reject unverified relay",
						Repos:       []logdata.MetaRepo{{Name: "repo/a", Branch: "caic-reject"}},
						Harness:     "reconnect",
					})
					if err != nil {
						t.Fatal(err)
					}
					diskNative := `{"method":"thread/started","params":{"thread":{"id":"disk-thread","cliVersion":"1.0","createdAt":1,"cwd":"/repo","modelProvider":"openai","path":"/repo","preview":"","source":"user","status":{"type":"idle"},"updatedAt":2}}}`
					diskRecord := fmt.Sprintf(`{"t":"agent","ts":1.000,"msg":%s}`, diskNative)
					original := string(meta) + "\n" + diskRecord + "\n"
					logPath := filepath.Join(logDir, taskID.String()+".jsonl")
					if err := os.MkdirAll(logDir, 0o750); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(logPath, []byte(original), 0o600); err != nil {
						t.Fatal(err)
					}

					const relayRecord = `{"t":"agent","ts":2.000,"msg":{"method":"offline"}}`
					m.relay = fakeRelayReader{
						statusFn: func(context.Context, runtime.ConnectionTarget) (bool, string, error) {
							return tc.relayAlive, "relay state", nil
						},
						readTailFn: func(context.Context, runtime.ConnectionTarget, int64) (agent.RelaySnapshot, int64, error) {
							return agent.RelaySnapshot{
								RelayRecords: []agent.RelayRecordBoundary{{
									RelayEnd: 123,

									ByteEnd:     len(relayRecord) + 1,
									Fingerprint: sha256.Sum256([]byte(relayRecord)),
								}},
								Encoded: []byte(relayRecord + "\n"),
							}, 123, tc.readErr
						},
						readLogFn: func(context.Context, runtime.ConnectionTarget, int) string { return "" },
					}
					logs, err := store.LoadUnsettled()
					if err != nil {
						t.Fatal(err)
					}
					adopted, err := m.ImportInstances(t.Context(), []runtime.Instance{{
						ID:    runtime.NewID("test-runtime", "reject-tail"),
						State: "running",
						Repos: []runtime.Repo{{
							GitRoot: "/home/user/src/repo/a", Branch: "caic-reject", ContainerPath: "/home/user/src/repo/a",
						}},
					}}, logs)
					wantError := "reconcile imported relay snapshot"
					if tc.readErr != nil {
						wantError = "read relay snapshot"
					}
					if err == nil || !strings.Contains(err.Error(), wantError) {
						t.Fatalf("ImportInstances error = %v, want relay reconciliation failure", err)
					}
					if len(adopted) != 0 {
						t.Fatalf("adopted = %#v, want no task", adopted)
					}
					persisted, err := os.ReadFile(logPath) //nolint:gosec // test-controlled path.
					if err != nil {
						t.Fatal(err)
					}
					if got := string(persisted); got != original {
						t.Fatalf("log changed after rejected relay snapshot:\n%s", got)
					}
				})
			}
		})
		t.Run("missing_local_log_refuses_live_reconnect", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			backend := &reconnectInputBackend{FakeBackend: &agenttest.FakeBackend{HarnessName: "reconnect", Images: true}}
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
				readTailFn: func(context.Context, runtime.ConnectionTarget, int64) (agent.RelaySnapshot, int64, error) {
					return agent.RelaySnapshot{}, 599440, nil
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
				}, []*taskhistory.LoadedTask{{
					TaskID:  taskID.String(),
					Harness: "reconnect",

					Repos: []repo.Mount{{Name: "repo/a", Branch: "caic-10"}},
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
			m := newTestManager(t, Config{ServerCtx: t.Context(), LogStore: taskhistory.NewStore(testLogger(), taskslog.NewStore(filepath.Join(cacheDir, "tasks")), backends.Default("", nil)), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake), Backends: map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}})
			m.relay = deadRelayFixture()
			registerCheckout(t, m.Checkouts, "caic-xyz/caic", &repo.Checkout{Dir: "/home/user/src/caic-xyz/caic"})

			logDir := filepath.Join(cacheDir, "tasks")
			if err := os.MkdirAll(logDir, 0o750); err != nil {
				t.Fatal(err)
			}
			meta, err := json.Marshal(logdata.MetaMessage{
				MessageType: "caic_meta",
				Version:     3,
				Prompt:      "dead relay task",
				Repos:       []logdata.MetaRepo{{Name: "caic-xyz/caic", Branch: "caic-7"}},
				Harness:     string(harness.Claude),
			})
			if err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(logDir, taskID.String()+".jsonl")
			body := string(meta) + "\n" + `{"t":"exit","exit_code":2,"error":"Unknown option: --approve"}` + "\n"
			if err := os.WriteFile(logPath, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			logs, err := taskhistory.NewStore(testLogger(), taskslog.NewStore(logDir), backends.Default("", nil)).LoadUnsettled()
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
			if got := adopted[0].Task().GetState(); got != task.StateCrashed {
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
			if !strings.Contains(string(persisted), `"t":"result"`) || !strings.Contains(string(persisted), `"state":"crashed"`) {
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
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, runtimeBackend, fake), Backends: map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}}}})
			dir := t.TempDir()
			m.logStore = taskhistory.NewStore(testLogger(), taskslog.NewStore(dir), backends.Default("", nil))
			name := taskID.String() + ".jsonl"
			const header = `{"t":"caic_meta","version":3,"prompt":"dead tail","repos":[{"name":"caic-xyz/caic","branch":"caic-8"}],"harness":"claude"}`
			if err := os.WriteFile(filepath.Join(dir, name), []byte(header+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			logs, err := m.logStore.LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}
			m.relay = fakeRelayReader{
				statusFn: func(context.Context, runtime.ConnectionTarget) (bool, string, error) {
					return false, "dead", nil
				},
				readTailFn: func(_ context.Context, _ runtime.ConnectionTarget, _ int64) (agent.RelaySnapshot, int64, error) {
					const record = `{"t":"exit","exit_code":2,"error":"Unknown option: --approve"}`

					_, err := taskslog.DecodeFrame([]byte(record))
					if err != nil {
						return agent.RelaySnapshot{}, 0, err
					}
					encoded := []byte(record + "\n")
					return agent.RelaySnapshot{Encoded: encoded, RelayRecords: []agent.RelayRecordBoundary{{RelayEnd: int64(len(encoded)), ByteEnd: len(encoded), Fingerprint: sha256.Sum256([]byte(record))}}}, int64(len(encoded)), nil
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
				}, logs)
			if err != nil {
				t.Fatalf("AdoptInstances: %v", err)
			}
			if len(adopted) != 1 {
				t.Fatalf("adopted len = %d, want 1", len(adopted))
			}
			if got := adopted[0].Task().GetState(); got != task.StateCrashed {
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
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, runtimeBackend, fake), Backends: map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}}}})
			m.relay = fakeRelayReader{
				statusFn: func(context.Context, runtime.ConnectionTarget) (bool, string, error) {
					return false, "dead", nil
				},
				readTailFn: func(context.Context, runtime.ConnectionTarget, int64) (agent.RelaySnapshot, int64, error) {
					return agent.RelaySnapshot{}, 256, nil
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
				}, []*taskhistory.LoadedTask{{
					TaskID:  taskID.String(),
					Harness: harness.Claude,

					Repos: []repo.Mount{{Name: "caic-xyz/caic", Branch: "caic-9"}},
				}})
			if err != nil {
				t.Fatalf("AdoptInstances: %v", err)
			}
			if len(adopted) != 1 {
				t.Fatalf("adopted len = %d, want 1", len(adopted))
			}
			if got := adopted[0].Task().GetState(); got != task.StateStopped {
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
			m := newTestManager(t, Config{ServerCtx: t.Context(), LogStore: taskhistory.NewStore(testLogger(), taskslog.NewStore(filepath.Join(cacheDir, "tasks")), backends.Default("", nil)), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake), Backends: map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}}})
			registerCheckout(t, m.Checkouts, "caic-xyz/caic", &repo.Checkout{Dir: "/home/user/src/caic-xyz/caic"})

			logDir := filepath.Join(cacheDir, "tasks")
			if err := os.MkdirAll(logDir, 0o750); err != nil {
				t.Fatal(err)
			}
			meta, err := json.Marshal(logdata.MetaMessage{
				MessageType: "caic_meta",
				Version:     3,
				Prompt:      "clean task with stale crash trailer",
				Repos:       []logdata.MetaRepo{{Name: "caic-xyz/caic", Branch: "caic-10"}},
				Harness:     string(harness.Claude),
			})
			if err != nil {
				t.Fatal(err)
			}
			result := `{"t":"agent","ts":1.000,"msg":{"type":"result","subtype":"success","is_error":false,"duration_ms":1,"num_turns":1,"result":"done"}}`
			staleExit := `{"t":"exit","exit_code":2,"error":"stale crash"}`
			staleTrailer := `{"t":"result","state":"crashed","error":"agent session crashed"}`
			logs := string(meta) + "\n" + result + "\n" + staleExit + "\n" + staleTrailer + "\n"
			if err := os.WriteFile(filepath.Join(logDir, taskID.String()+".jsonl"), []byte(logs), 0o600); err != nil {
				t.Fatal(err)
			}
			loaded, err := taskhistory.NewStore(testLogger(), taskslog.NewStore(logDir), backends.Default("", nil)).LoadUnsettled()
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
			if got := adopted[0].Task().GetState(); got != task.StateStopped {
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
			m := newTestManager(t, Config{ServerCtx: t.Context(), LogStore: taskhistory.NewStore(testLogger(), taskslog.NewStore(filepath.Join(cacheDir, "tasks")), backends.Default("", nil)), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake), Backends: map[harness.Name]agent.Backend{harness.Codex: codex.New("", nil)}})
			registerCheckout(t, m.Checkouts, "caic-xyz/caic", &repo.Checkout{Dir: "/home/user/src/caic-xyz/caic"})

			logDir := filepath.Join(cacheDir, "tasks")
			if err := os.MkdirAll(logDir, 0o750); err != nil {
				t.Fatal(err)
			}
			meta, err := json.Marshal(logdata.MetaMessage{
				MessageType: "caic_meta",
				Version:     3,
				Prompt:      "legacy codex task",
				Repos:       []logdata.MetaRepo{{Name: "caic-xyz/caic", Branch: "caic-6"}},
				Harness:     string(harness.Codex),
			})
			if err != nil {
				t.Fatal(err)
			}
			init := `{"t":"agent","ts":1.000,"msg":{"method":"thread/started","params":{"thread":{"id":"thread-from-started","cliVersion":"1.0","createdAt":1,"cwd":"/repo","modelProvider":"openai","path":"/repo","preview":"","source":"user","status":{"type":"idle"},"updatedAt":2}}}}`
			if err := os.WriteFile(filepath.Join(logDir, taskID.String()+".jsonl"), []byte(string(meta)+"\n"+init+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			logs, err := taskhistory.NewStore(testLogger(), taskslog.NewStore(logDir), backends.Default("", nil)).LoadUnsettled()
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
		started := make(chan []runtime.ID, 1)
		fake := &runtimetest.FakeInfo{
			WatchStarted: started,
			Stats:        []runtime.StatsSample{{InstanceID: "ctr-1", Stats: runtime.Stats{CPUPerc: 2.5, MemUsed: 200, DiskUsed: -1}}},
		}
		m := newTestManager(t, Config{ServerCtx: ctx, Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake)})
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
		tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
		tk.SetState(task.StateRunning)
		m.Insert(tk.ID, m.NewEntry(tk, nil))

		var workers sync.WaitGroup
		workers.Go(func() { m.watchStats(ctx) })
		t.Cleanup(func() { cancel(); workers.Wait() })
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

	t.Run("watchStatsChanges", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		started := make(chan []runtime.ID, 8)
		fake := &runtimetest.FakeInfo{WatchStarted: started}
		m := newTestManager(t, Config{ServerCtx: ctx, Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake)})
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
		id := runtime.NewID("test-runtime", "ctr-1")
		tk.SetRuntimeConnectionInfo(id, runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
		tk.SetState(task.StateRunning)
		m.Insert(tk.ID, m.NewEntry(tk, nil))
		var workers sync.WaitGroup
		workers.Go(func() { m.watchStats(ctx) })
		t.Cleanup(func() { cancel(); workers.Wait() })
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for initial stats stream")
		}

		// Running and waiting tasks both retain their running container. Updates
		// to either task's data must not reset the runtime's sampling interval.
		tk.SetState(task.StateWaiting)
		m.NotifyTaskChange()
		select {
		case <-started:
			t.Fatal("restarted stats stream without a container-set change")
		case <-time.After(100 * time.Millisecond):
		}

		other := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "y"}, "", "")
		otherID := runtime.NewID("test-runtime", "ctr-2")
		other.SetRuntimeConnectionInfo(otherID, runtime.ConnectionTarget{SSHHost: "ctr-2"}, "", "", 0)
		other.SetState(task.StateRunning)
		m.Insert(other.ID, m.NewEntry(other, nil))
		select {
		case ids := <-started:
			if !slices.Equal(ids, []runtime.ID{id, otherID}) {
				t.Fatalf("watch ids = %v, want both containers in sorted order", ids)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("did not restart stats stream after adding a container")
		}
		other.SetState(task.StateStopped)
		m.NotifyTaskChange()
		select {
		case ids := <-started:
			if !slices.Equal(ids, []runtime.ID{id}) {
				t.Fatalf("watch ids = %v, want only the running container", ids)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("did not restart stats stream after stopping a container")
		}
	})

	t.Run("watchStatsRetry", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		started := make(chan []runtime.ID)
		fake := &endedStatsInfo{WatchStarted: started}
		m := newTestManager(t, Config{ServerCtx: ctx, Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake)})
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
		tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
		tk.SetState(task.StateRunning)
		m.Insert(tk.ID, m.NewEntry(tk, nil))
		var workers sync.WaitGroup
		workers.Go(func() { m.watchStats(ctx) })
		t.Cleanup(func() { cancel(); workers.Wait() })
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for initial stats stream")
		}
		m.NotifyTaskChange()
		select {
		case <-started:
			t.Fatal("retried ended stats stream without the retry delay")
		case <-time.After(100 * time.Millisecond):
		}
		tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-2"), runtime.ConnectionTarget{SSHHost: "ctr-2"}, "", "", 0)
		m.NotifyTaskChange()
		select {
		case ids := <-started:
			if !slices.Equal(ids, []runtime.ID{"test-runtime:ctr-2"}) {
				t.Fatalf("watch ids = %v, want the replacement container", ids)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("container-set change did not bypass the retry delay")
		}
	})

	t.Run("watchDiskUsage", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		started := make(chan []runtime.ID, 4)
		id := runtime.NewID("test-runtime", "ctr-1")
		fake := &runtimetest.FakeInfo{
			DiskSizes:        map[runtime.ID]int64{id: 700},
			DiskUsageStarted: started,
		}
		m := newTestManager(t, Config{ServerCtx: ctx, Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake)})
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
		tk.SetRuntimeConnectionInfo(id, runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
		tk.SetState(task.StateRunning)
		tk.PushStats(&runtime.Stats{CPUPerc: 2.5, DiskUsed: -1})
		m.Insert(tk.ID, m.NewEntry(tk, nil))

		var workers sync.WaitGroup
		workers.Go(func() { m.watchDiskUsage(ctx) })
		t.Cleanup(func() { cancel(); workers.Wait() })
		select {
		case ids := <-started:
			if !slices.Equal(ids, []runtime.ID{id}) {
				t.Fatalf("disk usage ids = %v, want [%s]", ids, id)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for initial disk usage")
		}

		tk.SetState(task.StateStopping)
		m.NotifyTaskChange()
		tk.SetState(task.StateStopped)
		m.NotifyTaskChange()
		select {
		case ids := <-started:
			if !slices.Equal(ids, []runtime.ID{id}) {
				t.Fatalf("final disk usage ids = %v, want [%s]", ids, id)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for final disk usage")
		}

		history, _, unsub := tk.SubscribeStats(t.Context())
		unsub()
		if len(history) != 1 || history[0].DiskUsed != 700 {
			t.Fatalf("stopped task stats = %+v, want disk usage 700", history)
		}
		select {
		case ids := <-started:
			t.Fatalf("unexpected disk usage poll with no active task: %v", ids)
		case <-time.After(100 * time.Millisecond):
		}
	})

	t.Run("watchRuntimeEvents", func(t *testing.T) {
		t.Parallel()
		t.Run("buffers_before_import", func(t *testing.T) {
			t.Parallel()
			events := make(chan runtime.Event, 1)
			fake := &runtimetest.FakeInfo{Events: events}
			rb := &runtimetest.FakeBackend{}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, rb, &observedRuntimeInfo{FakeInfo: fake, backend: rb})})
			t.Cleanup(func() {
				if err := m.Close(); err != nil {
					t.Error(err)
				}
			})
			if err := m.Start(&fakeTaskMCPScoper{}); err != nil {
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
			tk.SetState(task.StateRunning)
			m.Insert(tk.ID, m.NewEntry(tk, nil))
			if _, err := m.ImportInstances(t.Context(), []runtime.Instance{}, nil); err != nil {
				t.Fatal(err)
			}
			if got := tk.GetState(); got != task.StateStopped {
				t.Fatalf("state = %v, want StateStopped", got)
			}
		})
		t.Run("unavailable_at_startup", func(t *testing.T) {
			t.Parallel()
			fake := &runtimetest.FakeInfo{WatchErr: errors.New("unavailable")}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, &runtimetest.FakeBackend{}, fake)})
			if err := m.Start(&fakeTaskMCPScoper{}); err == nil {
				t.Fatal("Start() succeeded, want runtime event watch error")
			}
			m.eventMu.Lock()
			importing := m.importing
			m.eventMu.Unlock()
			if !importing {
				t.Fatal("startup import lost its fence after event subscription failed")
			}
		})
		t.Run("valid_dispatches_death", func(t *testing.T) {
			t.Parallel()
			events := make(chan runtime.Event, 1)
			fake := &runtimetest.FakeInfo{Events: events}
			rb := &runtimetest.FakeBackend{}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, rb, &observedRuntimeInfo{FakeInfo: fake, backend: rb})})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			tk.Repos = []repo.Mount{{Name: "repo/x", Branch: "caic-1"}}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-dead"), runtime.ConnectionTarget{SSHHost: "ctr-dead"}, "", "", 0)
			tk.SetState(task.StateRunning)
			m.Insert(tk.ID, m.NewEntry(tk, nil))

			ctx, cancel := context.WithCancel(t.Context())
			var workers sync.WaitGroup
			workers.Go(func() { m.watchRuntimeEvents(ctx, nil) })
			t.Cleanup(func() { cancel(); workers.Wait() })

			events <- runtime.Event{InstanceID: "ctr-dead", Kind: runtime.EventDie}

			// Wait for the state transition rather than sleeping a fixed duration.
			deadline := time.Now().Add(2 * time.Second)
			for tk.GetState() != task.StateStopped {
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
				initial task.State
				kind    runtime.EventKind
				want    task.State
			}{
				{"start_restores_stopped_task", "fake", task.StateStopped, runtime.EventStart, task.StateWaiting},
				{"destroy_keeps_purged_task", "", task.StatePurged, runtime.EventDestroy, task.StatePurged},
			} {
				t.Run(tt.name, func(t *testing.T) {
					t.Parallel()
					m := newTestManager(t, Config{ServerCtx: t.Context()})
					tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, tt.harness, "")
					instanceID := runtime.NewID("test-runtime", runtime.InstanceID("ctr-"+tt.name))
					tk.SetRuntimeConnectionInfo(instanceID, runtime.ConnectionTarget{SSHHost: string(instanceID)}, "", "", 0)
					tk.SetState(tt.initial)
					m.Insert(tk.ID, m.NewEntry(tk, nil))
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
				tk.SetState(task.StateRunning)
				m.Insert(tk.ID, m.NewEntry(tk, nil))
				m.handleRuntimeEvent(t.Context(), runtime.Event{InstanceID: runtime.NewID("test-runtime", "ctr-oom"), Kind: runtime.EventOOM})
				m.handleRuntimeEvent(t.Context(), runtime.Event{InstanceID: runtime.NewID("test-runtime", "ctr-oom"), Kind: runtime.EventDie})
				if got := tk.GetState(); got != task.StateCrashed {
					t.Fatalf("state = %v, want StateCrashed", got)
				}
			})
			t.Run("unknown_is_ignored", func(t *testing.T) {
				t.Parallel()
				m := newTestManager(t, Config{ServerCtx: t.Context()})
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
				tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-unknown"), runtime.ConnectionTarget{SSHHost: "ctr-unknown"}, "", "", 0)
				tk.SetState(task.StateRunning)
				m.Insert(tk.ID, m.NewEntry(tk, nil))
				m.handleRuntimeEvent(t.Context(), runtime.Event{InstanceID: runtime.NewID("test-runtime", "ctr-unknown"), Kind: "unknown"})
				if got := tk.GetState(); got != task.StateRunning {
					t.Fatalf("state = %v, want StateRunning", got)
				}
			})
		})
	})

	t.Run("LoadUnsettledTasks", runLoadUnsettledTasksTests)
	t.Run("Preparation", runPreparationTests)
	t.Run("allocateBranches", runAllocateBranchesTests)
	t.Run("RegisteredLogPaths", runRegisteredLogPathsTests)
}

// TestLoadUnsettledTasks covers LoadUnsettledTasks: the plain (uncompressed)
// scan is loaded uncapped, so live and just-terminal tasks are not lost, and
// running tasks are coerced to failed.
func runLoadUnsettledTasksTests(t *testing.T) {
	t.Parallel()
	t.Run("OwnsPendingInputAtRegistration", func(t *testing.T) {
		t.Parallel()
		store := taskhistory.NewStore(testLogger(), taskslog.NewStore(t.TempDir()), backends.Default("", nil))
		id := ksid.NewID()
		queued := inputqueue.Entry{
			ID: ksid.NewID().String(), TaskID: id.String(),
			Text: "next", Images: []inputqueue.Image{{MediaType: "image/png", Data: "aGVsbG8="}},
		}
		if err := store.Journal.SaveInputQueue([]inputqueue.Entry{queued}); err != nil {
			t.Fatal(err)
		}
		m := newTestManager(t, Config{ServerCtx: t.Context(), LogStore: store})
		if err := m.LoadUnsettledTasks([]*taskhistory.LoadedTask{{
			TaskID: id.String(), Prompt: "initial", State: task.StateStopped,
		}}); err != nil {
			t.Fatal(err)
		}
		e, ok := m.GetEntry(id)
		if !ok {
			t.Fatal("restored task missing")
		}
		// Inspect registration directly: pruning must never see a staged prompt
		// for a registered task.
		m.inputMu.Lock()
		e.mu.Lock()
		owned := e.pendingInput != nil && e.pendingInput.ID.String() == queued.ID
		e.mu.Unlock()
		_, staged := m.unrestoredInputs[id]
		m.inputMu.Unlock()
		if !owned || staged {
			t.Fatalf("registered prompt: task-owned=%v, still staged=%v", owned, staged)
		}
		m.CompleteRestoration(nil)
		m.drainInputQueue(t.Context())
		persisted, err := store.Journal.LoadInputQueue()
		if err != nil || !reflect.DeepEqual(persisted, []inputqueue.Entry{queued}) {
			t.Fatalf("persisted pending prompt = %+v, err=%v", persisted, err)
		}
	})
	t.Run("UncappedPerRepo", func(t *testing.T) {
		t.Parallel()
		m := newTestManager(t, Config{ServerCtx: t.Context()})
		now := time.Now().UTC()
		all := make([]*taskhistory.LoadedTask, 0, 7)
		for i := range 7 {
			all = append(all, &taskhistory.LoadedTask{
				TaskID:         ksid.NewID().String(),
				Prompt:         "unsettled",
				Repos:          []repo.Mount{{Name: "repo/a"}},
				State:          task.StateStopped,
				LastActivityAt: now.Add(time.Duration(i) * time.Second),
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
		if err := m.LoadUnsettledTasks([]*taskhistory.LoadedTask{{
			TaskID:         id.String(),
			Prompt:         "was running",
			Repos:          []repo.Mount{{Name: "repo/a"}},
			State:          task.StateRunning,
			LastActivityAt: time.Now().UTC(),
		}}); err != nil {
			t.Fatalf("LoadUnsettledTasks: %v", err)
		}
		e, ok := m.GetEntry(id)
		if !ok {
			t.Fatal("entry not found")
		}
		if got := e.Task().GetState(); got != task.StateFailed {
			t.Errorf("state = %v, want StateFailed (running coerced)", got)
		}
	})
	t.Run("RestoresParentTaskID", func(t *testing.T) {
		t.Parallel()
		m := newTestManager(t, Config{ServerCtx: t.Context()})
		id := ksid.NewID()
		parentID := ksid.NewID()
		if err := m.LoadUnsettledTasks([]*taskhistory.LoadedTask{{
			TaskID:         id.String(),
			ParentTaskID:   parentID.String(),
			Prompt:         "delegated work",
			Repos:          []repo.Mount{{Name: "repo/a"}},
			State:          task.StateStopped,
			LastActivityAt: time.Now().UTC(),
		}}); err != nil {
			t.Fatalf("LoadUnsettledTasks: %v", err)
		}
		e, ok := m.GetEntry(id)
		if !ok {
			t.Fatal("entry not found")
		}
		if got := e.Task().ParentTaskID; got != parentID {
			t.Errorf("ParentTaskID = %s, want %s", got, parentID)
		}
	})
}

func runAllocateBranchesTests(t *testing.T) {
	t.Parallel()
	t.Run("basic", runAllocateBranchesBasic)
	t.Run("adoptsAvailableLocalBranch", runAllocateBranchesAdoptsAvailableLocalBranch)
	t.Run("sharesBranchNameAcrossRepos", runAllocateBranchesSharesBranchNameAcrossRepos)
	t.Run("fallsBackWhenSharedNameTaken", runAllocateBranchesFallsBackWhenSharedNameTaken)
	t.Run("adoptionDisablesSharing", runAllocateBranchesAdoptionDisablesSharing)
}

func allocateTestBranches(ctx context.Context, m *Manager, tk *taskrun.Task, mounts []repo.Mount, reserveOnly int, adoptLocal bool) error {
	unlock, err := m.lockPreparation(mounts)
	if err != nil {
		return err
	}
	defer unlock()
	created, err := m.allocateBranchesLocked(ctx, tk, mounts, reserveOnly, adoptLocal)
	if err != nil {
		for _, branch := range created {
			err = errors.Join(err, branch.Rollback(ctx, m.log))
		}
	}
	return err
}

func runAllocateBranchesBasic(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Config{ServerCtx: t.Context()})
	// Fake dirs: Init's branch scan fails and is ignored, leaving nextID at 0, so
	// the first ReserveBranchName on each repo yields caic-0.
	registerCheckout(t, m.Checkouts, "acme/app", &repo.Checkout{Dir: "/tmp/app"})
	registerCheckout(t, m.Checkouts, "caic-xyz/caic", &repo.Checkout{Dir: "/tmp/caic"})

	// Forking a 2-repo task carries both repos on their source branches. Every
	// repo — primary and non-primary alike — is reallocated to a fresh branch
	// from its own checkout, so the fork never shares a branch with the
	// still-checked-out source instance. Index 0 is not special. Both repos
	// share one name: the highest next sequence number across their checkouts.
	mounts := []repo.Mount{
		{Name: "acme/app", Branch: "caic-2"},
		{Name: "caic-xyz/caic", Branch: "caic-18"},
	}
	tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
	tk.Repos = slices.Clone(mounts)
	if err := allocateTestBranches(t.Context(), m, tk, mounts, len(mounts), false); err != nil {
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
	bad.Repos = []repo.Mount{{Name: "ghost/repo", Branch: "caic-1"}}
	if err := allocateTestBranches(t.Context(), m, bad, bad.ReposSnapshot(), 1, false); err == nil {
		t.Fatal("allocateForkBranches error = nil, want error for unregistered repo")
	}
}

func runAllocateBranchesAdoptsAvailableLocalBranch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // Test helper receives only controlled arguments.
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "initial")
	run("branch", "local-work")
	run("remote", "add", "origin", dir)
	run("update-ref", "refs/remotes/origin/main", "main")
	run("branch", "--set-upstream-to", "origin/main", "local-work")
	run("-c", "branch.autoSetupMerge=false", "branch", "untracked", "main")
	run("update-ref", "refs/remotes/origin/tracked", "main")
	run("branch", "tracked", "main")
	run("branch", "--set-upstream-to", "origin/tracked", "tracked")
	run("rev-parse", "tracked@{upstream}")
	run("update-ref", "refs/remotes/origin/remote-only", "main")

	m := newTestManager(t, Config{ServerCtx: t.Context()})
	checkout := &repo.Checkout{Dir: dir, BaseBranch: "main", GitTimeout: time.Minute}
	registerCheckout(t, m.Checkouts, "repo", checkout)

	allocate := func(base string) *taskrun.Task {
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
		tk.Repos = []repo.Mount{{Name: "repo", BaseBranch: base, GitRoot: dir}}
		m.insertEntry(tk.ID, m.NewEntry(tk, nil))
		if err := allocateTestBranches(t.Context(), m, tk, tk.ReposSnapshot(), 1, true); err != nil {
			t.Fatal(err)
		}
		return tk
	}

	if got := allocate("local-work").Primary().Branch; got != "local-work" {
		t.Errorf("local branch = %q, want local-work", got)
	}
	if !m.BranchAssociated("repo", "local-work") {
		t.Error("BranchAssociated = false, want true for claimed local-work")
	}
	if got := allocate("tracked").Primary().Branch; got != "tracked" {
		t.Errorf("tracking branch = %q, want tracked", got)
	}
	if got := allocate("untracked").Primary().Branch; !strings.HasPrefix(got, "caic-") {
		t.Errorf("untracked local branch = %q, want caic-N", got)
	}
	if got := allocate("remote-only").Primary().Branch; !strings.HasPrefix(got, "caic-") {
		t.Errorf("remote branch = %q, want caic-N", got)
	}
	if got := allocate("local-work").Primary().Branch; !strings.HasPrefix(got, "caic-") {
		t.Errorf("associated local branch = %q, want caic-N", got)
	}
}

// initTaskRepo creates a minimal git repo with a main commit and a self-referencing
// origin remote with origin/main populated, ready for AllocateBranch to fetch from.
func initTaskRepo(t *testing.T, dir string, args ...[]string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // Test helper receives only controlled arguments.
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "initial")
	run("remote", "add", "origin", dir)
	run("update-ref", "refs/remotes/origin/main", "main")
	for _, extra := range args {
		run(extra...)
	}
}

// Multi-repo tasks share one branch name across their repos: the highest next
// sequence number across the involved checkouts, so it is free in each. The
// reservation also advances the low counter, so a later single-repo task on
// that repo cannot reissue the shared name.
func runAllocateBranchesSharesBranchNameAcrossRepos(t *testing.T) {
	t.Parallel()
	dirApp, dirLib := t.TempDir(), t.TempDir()
	initTaskRepo(t, dirApp)
	initTaskRepo(t, dirLib)

	m := newTestManager(t, Config{ServerCtx: t.Context()})
	app := &repo.Checkout{Dir: dirApp, BaseBranch: "main", GitTimeout: time.Minute}
	lib := &repo.Checkout{Dir: dirLib, BaseBranch: "main", GitTimeout: time.Minute}
	registerCheckout(t, m.Checkouts, "acme/app", app)
	registerCheckout(t, m.Checkouts, "acme/lib", lib)

	// Drift the counters: acme/app has allocated two branches, acme/lib five.
	for range 2 {
		app.ReserveBranchName()
	}
	for range 5 {
		lib.ReserveBranchName()
	}

	mounts := []repo.Mount{
		{Name: "acme/app", GitRoot: dirApp},
		{Name: "acme/lib", GitRoot: dirLib},
	}
	tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
	tk.Repos = slices.Clone(mounts)
	if err := allocateTestBranches(t.Context(), m, tk, tk.ReposSnapshot(), 1, false); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"caic-5", "caic-5"} {
		if got := tk.ReposSnapshot()[i].Branch; got != want {
			t.Errorf("repo %d branch = %q, want %q", i, got, want)
		}
	}

	// The extra repo's branch was actually created from its base.
	cmd := exec.CommandContext(t.Context(), "git", "rev-parse", "--verify", "refs/heads/caic-5")
	cmd.Dir = dirLib
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("caic-5 not created in acme/lib: %v\n%s", err, out)
	}

	// Both counters moved past the shared number, so neither repo reissues it.
	for name, ws := range map[string]*repo.Checkout{"acme/app": app, "acme/lib": lib} {
		if got := ws.ReserveBranchName(); got != "caic-6" {
			t.Errorf("%s next reserve = %q, want caic-6", name, got)
		}
	}
}

// A repo whose shared name already exists in its git — a stale branch from an
// older task the in-memory counter does not know about — falls back to its own
// next sequential name.
func runAllocateBranchesFallsBackWhenSharedNameTaken(t *testing.T) {
	t.Parallel()
	dirApp, dirLib := t.TempDir(), t.TempDir()
	initTaskRepo(t, dirApp)
	initTaskRepo(t, dirLib, []string{"branch", "caic-0", "main"})

	m := newTestManager(t, Config{ServerCtx: t.Context()})
	registerCheckout(t, m.Checkouts, "acme/app", &repo.Checkout{Dir: dirApp, BaseBranch: "main", GitTimeout: time.Minute})
	registerCheckout(t, m.Checkouts, "acme/lib", &repo.Checkout{Dir: dirLib, BaseBranch: "main", GitTimeout: time.Minute})

	mounts := []repo.Mount{
		{Name: "acme/app", GitRoot: dirApp},
		{Name: "acme/lib", GitRoot: dirLib},
	}
	tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
	tk.Repos = slices.Clone(mounts)
	if err := allocateTestBranches(t.Context(), m, tk, tk.ReposSnapshot(), 1, false); err != nil {
		t.Fatal(err)
	}
	want := []string{"caic-0", "caic-1"}
	for i, w := range want {
		if got := tk.ReposSnapshot()[i].Branch; got != w {
			t.Errorf("repo %d branch = %q, want %q", i, got, w)
		}
	}
}

// An adopted local branch keeps its own name and disables sharing for that
// repo; the remaining repos still share a name among themselves.
func runAllocateBranchesAdoptionDisablesSharing(t *testing.T) {
	t.Parallel()
	dirApp, dirLib := t.TempDir(), t.TempDir()
	initTaskRepo(t, dirApp, []string{"branch", "local-work"}, []string{"branch", "--set-upstream-to", "origin/main", "local-work"})
	initTaskRepo(t, dirLib)

	m := newTestManager(t, Config{ServerCtx: t.Context()})
	registerCheckout(t, m.Checkouts, "acme/app", &repo.Checkout{Dir: dirApp, BaseBranch: "main", GitTimeout: time.Minute})
	registerCheckout(t, m.Checkouts, "acme/lib", &repo.Checkout{Dir: dirLib, BaseBranch: "main", GitTimeout: time.Minute})

	mounts := []repo.Mount{
		{Name: "acme/app", BaseBranch: "local-work", GitRoot: dirApp},
		{Name: "acme/lib", GitRoot: dirLib},
	}
	tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
	tk.Repos = slices.Clone(mounts)
	if err := allocateTestBranches(t.Context(), m, tk, tk.ReposSnapshot(), 1, true); err != nil {
		t.Fatal(err)
	}
	want := []string{"local-work", "caic-0"}
	for i, w := range want {
		if got := tk.ReposSnapshot()[i].Branch; got != w {
			t.Errorf("repo %d branch = %q, want %q", i, got, w)
		}
	}
}

// TestManagerRestoresTaskTimes verifies that an adopted task keeps the waiting
// duration and total duration its card showed before the restart.
func TestManagerRestoresTaskTimes(t *testing.T) {
	t.Parallel()
	taskID := ksid.NewID()
	instanceID := runtime.NewID("test-runtime", "restore-times")
	fake := &runtimetest.FakeInfo{Meta: map[string]string{
		"restore-times\x00caic.id":      taskID.String(),
		"restore-times\x00caic.harness": string(harness.Claude),
	}}
	cacheDir := t.TempDir()
	logDir := filepath.Join(cacheDir, "tasks")
	m := newTestManager(t, Config{
		ServerCtx: t.Context(),
		LogStore:  taskhistory.NewStore(testLogger(), taskslog.NewStore(logDir), backends.Default("", nil)),
		Runtimes:  newTestRuntime(t, &runtimetest.FakeBackend{}, fake),
		Backends:  map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{WireFactory: claudecode.New().NewWire}},
	})
	t.Cleanup(func() { _ = m.Close() })
	registerCheckout(t, m.Checkouts, "caic-xyz/caic", &repo.Checkout{Dir: "/home/user/src/caic-xyz/caic"})
	m.relay = fakeRelayReader{
		statusFn: func(context.Context, runtime.ConnectionTarget) (bool, string, error) { return true, "alive", nil },
		readTailFn: func(context.Context, runtime.ConnectionTarget, int64) (agent.RelaySnapshot, int64, error) {
			return agent.RelaySnapshot{}, 0, nil
		},
		readLogFn: func(context.Context, runtime.ConnectionTarget, int) string { return "" },
	}

	now := time.Now().UTC().Truncate(time.Second)
	resultAt := now.Add(-2 * time.Minute)
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		t.Fatal(err)
	}
	meta, err := json.Marshal(logdata.MetaMessage{
		MessageType: "caic_meta",
		Version:     3,
		Prompt:      "do it",
		Repos:       []logdata.MetaRepo{{Name: "caic-xyz/caic", Branch: "caic-11"}},
		Harness:     string(harness.Claude),
	})
	if err != nil {
		t.Fatal(err)
	}
	text := fmt.Sprintf(`{"t":"agent","ts":%.3f,"msg":{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}}`, float64(resultAt.UnixMilli()-60000)/1000)
	result := fmt.Sprintf(`{"t":"agent","ts":%.3f,"msg":{"type":"result","subtype":"success","is_error":false,"duration_ms":120000,"num_turns":1,"result":"done","session_id":"s","total_cost_usd":0.01,"usage":{}}}`, float64(resultAt.UnixMilli())/1000)
	logs := string(meta) + "\n" + text + "\n" + result + "\n"
	if err := os.WriteFile(filepath.Join(logDir, taskID.String()+".jsonl"), []byte(logs), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := m.logStore.LoadUnsettled()
	if err != nil {
		t.Fatal(err)
	}

	adopted, err := m.ImportInstances(t.Context(), []runtime.Instance{{
		ID:    instanceID,
		State: "running",
		Repos: []runtime.Repo{{GitRoot: "/home/user/src/caic-xyz/caic", Branch: "caic-11", ContainerPath: "/home/user/src/caic-xyz/caic"}},
	}}, loaded)
	if err != nil {
		t.Fatalf("ImportInstances: %v", err)
	}
	if len(adopted) != 1 {
		t.Fatalf("adopted len = %d, want 1", len(adopted))
	}
	snap := adopted[0].Task().Snapshot()
	if snap.State != task.StateWaiting {
		t.Errorf("state = %v, want waiting", snap.State)
	}
	if !snap.StateUpdatedAt.Equal(resultAt) {
		t.Errorf("StateUpdatedAt = %v, want recorded result time %v", snap.StateUpdatedAt, resultAt)
	}
	if !snap.TurnStartedAt.IsZero() {
		t.Errorf("TurnStartedAt = %v, want zero for a waiting task", snap.TurnStartedAt)
	}
	if snap.Duration != 2*time.Minute {
		t.Errorf("Duration = %v, want 2m", snap.Duration)
	}
}

func TestLastResultText(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "")
		tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
			&agent.ResultMessage{MessageType: "result", Result: "first"},
			&agent.ResultMessage{MessageType: "result", Result: "last"},
		}))
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

// TestRestorationState verifies the background runtime/history restoration state
// machine exposed to the task-list stream.
func TestRestorationState(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Config{ServerCtx: t.Context()})
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})

	if m.TaskListComplete() {
		t.Fatal("fresh manager certified task-list absence")
	}

	// A fresh Manager has not completed a pass, so it reports loading.
	if loading, err := m.RestorationStatus(); !loading || err != "" {
		t.Fatalf("initial RestorationStatus = (%v, %q), want (true, \"\")", loading, err)
	}

	m.CompleteRestoration(errors.New("prior failure"))
	if m.TaskListComplete() {
		t.Fatal("failed restoration certified task-list absence")
	}
	if loading, _ := m.RestorationStatus(); loading {
		t.Fatal("after CompleteRestoration: SettledLoading = true, want false")
	}
	if _, got := m.RestorationStatus(); got != "prior failure" {
		t.Fatalf("after CompleteRestoration(err): SettledError = %q, want %q", got, "prior failure")
	}

	m.CompleteRestoration(nil)
	if !m.TaskListComplete() {
		t.Fatal("successful restoration did not certify membership")
	}
	if loading, _ := m.RestorationStatus(); loading {
		t.Fatal("after clean CompleteRestoration: SettledLoading = true, want false")
	}
	if _, got := m.RestorationStatus(); got != "" {
		t.Fatalf("after clean CompleteRestoration: SettledError = %q, want empty", got)
	}

	// A clean completion clears a prior error, so the state cannot report a
	// stale failure across a retry or second pass.
	m.CompleteRestoration(errors.New("stale failure"))
	if _, got := m.RestorationStatus(); got != "stale failure" {
		t.Fatalf("CompleteRestoration(err): SettledError = %q, want %q", got, "stale failure")
	}
	m.CompleteRestoration(nil)
	if _, got := m.RestorationStatus(); got != "" {
		t.Fatalf("CompleteRestoration(nil) after failure: SettledError = %q, want empty", got)
	}
}

func runRegisteredLogPathsTests(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logStore := taskhistory.NewStore(testLogger(), taskslog.NewStore(dir), backends.Default("", nil))
	m := newTestManager(t, Config{ServerCtx: t.Context(), LogStore: logStore})

	meta, err := json.Marshal(logdata.MetaMessage{
		MessageType: "caic_meta",
		Version:     int(agent.LogVersionV3),
		Harness:     string(harness.Claude),
		Prompt:      "history",
	})
	if err != nil {
		t.Fatal(err)
	}
	trailer := []byte(`{"state":"purged","t":"result"}`)
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
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			lt := &taskhistory.LoadedTask{
				TaskID: fmt.Sprintf("load-%d", i),
				State:  task.StatePurged,
			}
			if err := m.LoadPurgedTasks([]*taskhistory.LoadedTask{lt}); err != nil {
				t.Error(err)
				return
			}
			if err := m.LoadUnsettledTasks([]*taskhistory.LoadedTask{
				{TaskID: fmt.Sprintf("unsettled-%d", i), State: task.StateRunning},
			}); err != nil {
				t.Error(err)
				return
			}
		}
	})
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			m.NotifyTaskChange()
			if err := m.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// importedRecoveryBackend resumes an idle session or rejects its launch.
type importedRecoveryBackend struct {
	reconnectInputBackend

	started  chan *agent.Options
	startErr error
}

func (b *importedRecoveryBackend) Start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	b.started <- opts
	if b.startErr != nil {
		return nil, b.startErr
	}
	return b.AttachRelay(ctx, opts)
}

func (b *importedRecoveryBackend) NewWire() agent.WireFormat {
	switch b.HarnessName {
	case harness.Claude:
		return claudecode.New().NewWire()
	case harness.Codex:
		return codex.New("", nil).NewWire()
	case harness.Dsh:
		return dsh.New("", nil).NewWire()
	case harness.OpenCode:
		return opencode.New("", nil).NewWire()
	default:
		return pi.New("", nil).NewWire()
	}
}

type importedRecoverySetup struct {
	harness      harness.Name
	runtimeState string
	statusErr    error
	alive        bool
	state        task.State
	reason       string
	startErr     error
}

type importedRecoveryTestEnv struct {
	m          *Manager
	backend    *importedRecoveryBackend
	rb         *runtimetest.FakeBackend
	id         ksid.ID
	instanceID runtime.ID
	entries    []*Entry
	err        error
}

func setupImportedRecovery(t *testing.T, s *importedRecoverySetup) *importedRecoveryTestEnv {
	h := s.harness
	if h == "" {
		h = harness.Pi
	}
	runtimeState := s.runtimeState
	if runtimeState == "" {
		runtimeState = "running"
	}
	id := ksid.NewID()
	instanceID := runtime.NewID("test-runtime", "md-agent-recovery")
	backend := &importedRecoveryBackend{started: make(chan *agent.Options, 1), startErr: s.startErr}
	backend.FakeBackend = &agenttest.FakeBackend{HarnessName: h}
	rb := &runtimetest.FakeBackend{}
	info := &runtimetest.FakeInfo{Meta: map[string]string{
		"md-agent-recovery\x00caic.id":      id.String(),
		"md-agent-recovery\x00caic.harness": string(h),
	}}
	store := taskhistory.NewStore(testLogger(), taskslog.NewStore(t.TempDir()), backends.Default("", nil))
	m := newTestManager(t, Config{ServerCtx: t.Context(), LogStore: store, Runtimes: newTestRuntime(t, rb, &observedRuntimeInfo{FakeInfo: info, backend: rb}), Backends: map[harness.Name]agent.Backend{h: backend}})
	// Exercise the startup fence used by Manager.Start without launching
	// unrelated stats and runtime watcher goroutines.
	m.importing = true
	m.importDone = make(chan struct{})
	m.pendingRuntimeEvents = []runtime.Event{{Kind: runtime.EventDie, InstanceID: instanceID}}
	t.Cleanup(func() { _ = m.Close(); backend.stop() })
	m.relay = fakeRelayReader{
		statusFn: func(context.Context, runtime.ConnectionTarget) (bool, string, error) {
			return s.alive, "probe", s.statusErr
		},
		readTailFn: func(context.Context, runtime.ConnectionTarget, int64) (agent.RelaySnapshot, int64, error) {
			return agent.RelaySnapshot{}, 0, errors.New("relay output lost")
		},
		readLogFn: func(context.Context, runtime.ConnectionTarget, int) string { return "" },
	}
	tk, err := taskrun.NewTask(id, agent.Prompt{Text: "accepted prompt"}, h, "", "", "", "", "existing title")
	if err != nil {
		t.Fatal(err)
	}
	log, path, err := store.Open(tk.LogFilename(), tk.LogHeader())
	if err != nil {
		t.Fatal(err)
	}
	if err := log.AppendMessage(&agent.UserInputMessage{Text: "accepted prompt"}); err != nil {
		t.Fatal(err)
	}
	if err := log.AppendMessage(&agent.MetaSessionMessage{MessageType: "caic_session", SessionID: "original-session"}); err != nil {
		t.Fatal(err)
	}
	if err := log.AppendMessage(taskhistory.ResultMessage("existing title", &task.Result{State: s.state, Err: errors.New(s.reason)})); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Compress(path, log, s.state); err != nil {
		t.Fatal(err)
	}
	logs, err := store.LoadForTaskIDs([]string{id.String()})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := m.ImportInstances(t.Context(), []runtime.Instance{{ID: instanceID, State: runtimeState, AgentTarget: runtime.ConnectionTarget{SSHHost: "recovery"}}}, logs)
	return &importedRecoveryTestEnv{
		m:          m,
		backend:    backend,
		rb:         rb,
		id:         id,
		instanceID: instanceID,
		entries:    entries,
		err:        err,
	}
}

func assertImportedRecoveryHistory(t *testing.T, logPath string) {
	if strings.HasSuffix(logPath, ".zst") {
		t.Fatal("recovery did not restore the archived log")
	}
	loaded, err := taskhistory.NewStore(slog.New(slog.DiscardHandler), taskslog.NewStore(filepath.Dir(logPath)), backends.Default("", nil)).LoadSource(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := loaded.LoadMessages(); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, msg := range loaded.Timeline {
		if input, ok := msg.Message.(*agent.UserInputMessage); ok && input.Text == "accepted prompt" {
			found = true
		}
	}
	if !found {
		t.Fatal("recovery lost original history")
	}
}

func TestManagerImportedRecovery(t *testing.T) {
	t.Parallel()

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name         string
			harness      harness.Name
			runtimeState string
			sendInput    bool
			statusErr    error
			alive        bool
			state        task.State
			reason       string
			recover      bool
		}{
			{name: "input_during_readiness", sendInput: true, state: task.StateFailed, reason: "revive instance: missing host branch", recover: true},
			{name: "success", state: task.StateFailed, reason: "revive instance: missing host branch", recover: true},
			{name: "deliberately_stopped", state: task.StateStopped},
			{name: "relay_crash", state: task.StateRunning, recover: true},
			{name: "waiting_relay_crash", state: task.StateWaiting, recover: true},
			{name: "claude_oom_runtime_exit", harness: harness.Claude, runtimeState: "exited", state: task.StateCrashed, reason: "runtime instance ran out of memory", recover: true},
			{name: "codex_relay_crash", harness: harness.Codex, state: task.StateRunning, recover: true},
			{name: "dsh_runtime_exit", harness: harness.Dsh, runtimeState: "exited", state: task.StateRunning, recover: true},
			{name: "opencode_runtime_exit", harness: harness.OpenCode, runtimeState: "exited", state: task.StateRunning, recover: true},
			{name: "persisted_stop_intent", state: task.StateStopping},
			{name: "live_persisted_stop_intent", alive: true, state: task.StateStopping},
			{name: "uncertain_persisted_stop_intent", statusErr: errors.New("SSH unavailable"), state: task.StateStopping},
			{name: "live_persisted_purge_intent", alive: true, state: task.StatePurging},
			{name: "uncertain_persisted_purge_intent", statusErr: errors.New("SSH unavailable"), state: task.StatePurging},
			{name: "persisted_purge_intent", state: task.StatePurging},
			{name: "ordinary_failure", state: task.StateFailed, reason: "agent startup failed"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				env := setupImportedRecovery(t, &importedRecoverySetup{
					harness:      tc.harness,
					runtimeState: tc.runtimeState,
					statusErr:    tc.statusErr,
					alive:        tc.alive,
					state:        tc.state,
					reason:       tc.reason,
				})
				if env.err != nil || len(env.entries) != 1 {
					t.Fatalf("import entries=%v err=%v", env.entries, env.err)
				}
				shutdownIntent := tc.state == task.StateStopping || tc.state == task.StatePurging || tc.state == task.StateStopped || tc.state == task.StatePurged
				if !tc.recover {
					if shutdownIntent && (env.entries[0].Task().GetState() != task.StateStopped || env.rb.Status(env.instanceID) != runtimetest.StatusStopped) {
						t.Fatal("persisted shutdown intent was not completed")
					}
					select {
					case <-env.backend.started:
						t.Fatal("ineligible task was automatically resumed")
					default:
					}
					return
				}
				select {
				case opts := <-env.backend.started:
					if opts.ResumeSessionID != "original-session" || opts.InitialPrompt.Text != "" {
						t.Fatalf("resume options = %#v", opts)
					}
				case <-time.After(time.Second):
					t.Fatal("saved session was not resumed")
				}
				env.m.handleRuntimeInstanceExit(t.Context(), env.instanceID)
				entry := env.entries[0]
				if tc.sendInput {
					deadline := time.After(time.Second)
					for !entry.Task().HasSession() {
						select {
						case <-deadline:
							t.Fatal("session was not attached")
						case <-time.After(time.Millisecond):
						}
					}
					if err := entry.Lifecycle.SendInput(t.Context(), agent.Prompt{Text: "new prompt during readiness"}); err != nil {
						t.Fatal(err)
					}
				}
				want := task.StateWaiting
				if tc.sendInput {
					want = task.StateRunning
				}
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
				defer cancel()
				for entry.Task().GetState() != want {
					select {
					case <-ctx.Done():
						t.Fatalf("state=%s want=%s", entry.Task().GetState(), want)
					case <-time.After(time.Millisecond):
					}
				}
				func() {
					entry.Lifecycle.operationMu.Lock()
					defer entry.Lifecycle.operationMu.Unlock()
				}()
				env.m.handleRuntimeInstanceExit(t.Context(), env.instanceID)
				if entry.Task().GetState() != want || !entry.Task().HasSession() {
					t.Fatal("delayed death detached revived session")
				}
				if tc.sendInput {
					func() {
						entry.Lifecycle.operationMu.Lock()
						defer entry.Lifecycle.operationMu.Unlock()
						if entry.Task().GetState() != task.StateRunning {
							t.Fatal("readiness overwrote the new prompt's running state")
						}
					}()
				}
				if entry.Task().ID != env.id || entry.Task().Title() != "existing title" {
					t.Fatal("recovery replaced task identity or title")
				}
				assertImportedRecoveryHistory(t, entry.LogPath.Get())
			})
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		t.Run("unverified_snapshot", func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name      string
				state     task.State
				statusErr error
				alive     bool
			}{
				{name: "running_uncertain_liveness", statusErr: errors.New("SSH unavailable"), state: task.StateRunning},
				{name: "live_relay_missing_output", alive: true, state: task.StateRunning},
				{name: "uncertain_liveness", statusErr: errors.New("SSH unavailable"), state: task.StateFailed},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					env := setupImportedRecovery(t, &importedRecoverySetup{
						state:     tc.state,
						statusErr: tc.statusErr,
						alive:     tc.alive,
						reason:    "revive instance: missing host branch",
					})
					if env.err == nil || len(env.entries) != 0 {
						t.Fatalf("unverified snapshot import entries=%v err=%v", env.entries, env.err)
					}
					if env.rb.Status(env.instanceID) == runtimetest.StatusStopped {
						t.Fatal("unverified live runtime was stopped")
					}
					select {
					case <-env.backend.started:
						t.Fatal("unverified relay was resumed")
					default:
					}
				})
			}
		})

		t.Run("resume_failure", func(t *testing.T) {
			t.Parallel()
			env := setupImportedRecovery(t, &importedRecoverySetup{
				state:    task.StateFailed,
				reason:   "revive instance: missing host branch",
				startErr: errors.New("resume rejected"),
			})
			if env.err != nil || len(env.entries) != 1 {
				t.Fatalf("import entries=%v err=%v", env.entries, env.err)
			}
			select {
			case opts := <-env.backend.started:
				if opts.ResumeSessionID != "original-session" || opts.InitialPrompt.Text != "" {
					t.Fatalf("resume options = %#v", opts)
				}
			case <-time.After(time.Second):
				t.Fatal("saved session was not resumed")
			}
			entry := env.entries[0]
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			for entry.Task().GetState() != task.StateCrashed {
				select {
				case <-ctx.Done():
					t.Fatalf("state=%s want=%s", entry.Task().GetState(), task.StateCrashed)
				case <-time.After(time.Millisecond):
				}
			}
			select {
			case <-entry.Done():
			case <-ctx.Done():
				t.Fatal("failed recovery did not finish")
			}
			if entry.Task().ID != env.id || entry.Task().Title() != "existing title" {
				t.Fatal("recovery replaced task identity or title")
			}
			assertImportedRecoveryHistory(t, entry.LogPath.Get())
			if env.rb.Status(env.instanceID) != runtimetest.StatusStopped {
				t.Fatal("failed recovery left runtime running")
			}
		})
	})
}

func TestCanRecoverImportedTask(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		state    task.State
		err      string
		session  string
		accepted bool
		exit     bool
		harness  harness.Name
		want     bool
	}{
		{name: "legacy_failed_revive", state: task.StateFailed, err: "revive instance: missing branch", session: "saved", accepted: true, harness: harness.Pi, want: true},
		{name: "active", state: task.StateRunning, session: "saved", accepted: true, harness: harness.Pi, want: true},
		{name: "stopped", state: task.StateStopped, session: "saved", accepted: true, harness: harness.Pi},
		{name: "ordinary_failure", state: task.StateFailed, err: "agent failed", session: "saved", accepted: true, harness: harness.Pi},
		{name: "crashed", state: task.StateCrashed, session: "saved", accepted: true, harness: harness.Pi, want: true},
		{name: "missing_session", state: task.StateRunning, accepted: true, harness: harness.Pi},
		{name: "unaccepted_input", state: task.StateRunning, session: "saved", harness: harness.Pi},
		{name: "agent_exit", state: task.StateRunning, session: "saved", accepted: true, exit: true, harness: harness.Pi, want: true},
		{name: "dsh_active", state: task.StateRunning, session: "saved", accepted: true, harness: harness.Dsh, want: true},
		{name: "other_harness", state: task.StateFailed, err: "revive instance: missing branch", session: "saved", accepted: true, harness: harness.Codex, want: true},
		{name: "unknown_harness", state: task.StateRunning, session: "saved", accepted: true, harness: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tk, err := taskrun.NewTask(ksid.NewID(), agent.Prompt{Text: "prompt"}, tc.harness, "", "", "", "", "")
			if err != nil {
				t.Fatal(err)
			}
			tk.SetSessionMetadata(agent.SessionMetadata{SessionID: tc.session})
			var messages []agent.Message
			if tc.accepted {
				messages = append(messages, &agent.UserInputMessage{Text: "prompt"})
			}
			if tc.exit {
				messages = append(messages, &agent.ExitMessage{ExitCode: 1, Error: "agent failed"})
			}
			tk.SeedTimelineParts(nil, agenttest.TimedMessages(messages))
			lt := &taskhistory.LoadedTask{State: tc.state}
			if tc.err != "" {
				lt.LastTrailer = &task.Result{Err: errors.New(tc.err)}
			}
			if got := canRecoverImportedTask(tk, lt); got != tc.want {
				t.Fatalf("eligible=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestLifecycleImportedRecoveryReservation(t *testing.T) {
	t.Parallel()
	rb := &blockingStopBackend{FakeBackend: &runtimetest.FakeBackend{}, started: make(chan struct{}), returned: make(chan struct{}), release: make(chan struct{})}
	backend := &importedRecoveryBackend{started: make(chan *agent.Options, 1)}
	backend.FakeBackend = &agenttest.FakeBackend{}
	store := taskhistory.NewStore(testLogger(), taskslog.NewStore(t.TempDir()), backends.Default("", nil))
	m := newTestManager(t, Config{ServerCtx: t.Context(), LogStore: store, Runtimes: newTestRuntime(t, rb, nil), Backends: map[harness.Name]agent.Backend{harness.Pi: backend}})
	t.Cleanup(func() {
		select {
		case <-rb.release:
		default:
			close(rb.release)
		}
		_ = m.Close()
		backend.stop()
	})
	tk, err := taskrun.NewTask(ksid.NewID(), agent.Prompt{Text: "accepted"}, harness.Pi, "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	tk.SetState(task.StateFailed)
	tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "recovery"), runtime.ConnectionTarget{SSHHost: "recovery"}, "", "", 0)
	tk.SetSessionMetadata(agent.SessionMetadata{SessionID: "saved-session"})
	tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{&agent.UserInputMessage{Text: "accepted"}}))
	tk.SetState(task.StateFailed)
	log, path, err := store.Open(tk.LogFilename(), tk.LogHeader())
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	entry := m.NewEntry(tk, nil)
	entry.LogPath.Set(path)
	m.Insert(tk.ID, entry)
	result := make(chan error, 1)
	var workers sync.WaitGroup
	workers.Go(func() { result <- entry.Lifecycle.recoverImportedSession(task.StateFailed) })
	t.Cleanup(func() {
		select {
		case <-rb.release:
		default:
			close(rb.release)
		}
		workers.Wait()
	})
	select {
	case <-rb.started:
	case <-time.After(time.Second):
		t.Fatal("recovery did not stop retained runtime")
	}
	if got := tk.GetState(); got != task.StateProvisioning {
		t.Fatalf("reservation state=%s", got)
	}
	if err := entry.Lifecycle.Purge(t.Context(), 0); err == nil {
		t.Fatal("purge accepted during recovery")
	}
	if err := entry.Lifecycle.SendInput(t.Context(), agent.Prompt{Text: "too early"}); err == nil {
		t.Fatal("input accepted during recovery stop")
	}
	if err := entry.Lifecycle.Revive(); err == nil {
		t.Fatal("manual revival accepted during recovery")
	}
	if got := tk.GetState(); got != task.StateProvisioning {
		t.Fatalf("concurrent operation overwrote reservation: %s", got)
	}
	close(rb.release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("recovery did not launch revival")
	}
	select {
	case opts := <-backend.started:
		if opts.ResumeSessionID != "saved-session" {
			t.Fatalf("resumed session=%s", opts.ResumeSessionID)
		}
	case <-time.After(time.Second):
		t.Fatal("reserved recovery did not resume")
	}
}

func TestLifecycleIntentFailure(t *testing.T) {
	t.Parallel()
	for _, purge := range []bool{false, true} {
		t.Run(fmt.Sprintf("purge_%v", purge), func(t *testing.T) {
			t.Parallel()
			rb := &runtimetest.FakeBackend{}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, rb, nil)})
			t.Cleanup(func() { _ = m.Close() })
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "accepted"}, harness.Pi, "")
			id := runtime.NewID("test-runtime", "missing-log")
			tk.SetRuntimeConnectionInfo(id, runtime.ConnectionTarget{}, "", "", 0)
			tk.SetState(task.StateWaiting)
			entry := m.NewEntry(tk, nil)
			m.Insert(tk.ID, entry)
			var err error
			if purge {
				err = entry.Lifecycle.Purge(t.Context(), 0)
			} else {
				err = entry.Lifecycle.Stop(t.Context())
			}
			if err == nil || !strings.Contains(err.Error(), "persist") {
				t.Fatalf("intent error=%v", err)
			}
			if tk.GetState() != task.StateWaiting || rb.Status(id) == runtimetest.StatusStopped || rb.Status(id) == runtimetest.StatusPurged {
				t.Fatal("failed intent persistence changed task or runtime")
			}
		})
	}
}

// observedRuntimeInfo reports the fake lifecycle's current incarnation state.
type observedRuntimeInfo struct {
	*runtimetest.FakeInfo

	backend *runtimetest.FakeBackend
}

func (i *observedRuntimeInfo) Inspect(_ context.Context, id runtime.ID) (*runtime.InstanceInspect, error) {
	state := "exited"
	if i.backend.Status(id) == runtimetest.StatusRunning {
		state = "running"
	}
	return &runtime.InstanceInspect{ID: id, State: state}, nil
}

func TestApplyLoadedSessionMetadata(t *testing.T) {
	t.Parallel()
	tk, err := taskrun.NewTask(ksid.NewID(), agent.Prompt{Text: "test"}, harness.Codex, "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	applyLoadedSessionMetadata(tk, &taskhistory.LoadedTask{SessionID: "session", AutoCompactTokenLimit: 244800})
	if got := tk.Snapshot().AutoCompactTokenLimit; got != 244800 {
		t.Fatalf("restored auto-compact threshold = %d, want 244800", got)
	}
}

func awaitQueueCondition(t *testing.T, m *Manager, ready func() bool) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	for {
		changed := m.Changed()
		if ready() {
			return
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("queue condition timed out")
		}
	}
}

// gatedQueueConn pauses a real prompt write to exercise in-flight queue behavior.
type gatedQueueConn struct {
	agent.Conn

	started chan struct{}
	release chan struct{}
}

func newGatedQueueSession(t *testing.T, e *Entry) *gatedQueueConn {
	b := &gatedQueueBackend{FakeBackend: &agenttest.FakeBackend{HarnessName: harness.Claude, WireFactory: claudecode.New().NewWire}}
	startQueueSession(t, e, b)
	return b.conn
}

func (c *gatedQueueConn) SendPrompt(p agent.Prompt, raw []byte) error {
	close(c.started)
	<-c.release
	return c.Conn.SendPrompt(p, raw)
}

type gatedQueueBackend struct {
	*agenttest.FakeBackend

	conn *gatedQueueConn
}

func (b *gatedQueueBackend) Start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	cmd := exec.CommandContext(ctx, "python3", "-c", "import sys; sys.stdin.buffer.read()")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, errors.Join(err, stdin.Close())
	}
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(err, stdin.Close())
	}
	b.conn = &gatedQueueConn{Conn: agent.NewConn(ctx, opts.Logger, stdin, opts.Records, nil), started: make(chan struct{}), release: make(chan struct{})}
	return agent.NewSession(ctx, cmd, b.conn, stdout, opts.Logger), nil
}
func startQueueSession(t testing.TB, e *Entry, b agent.Backend) {
	tk := e.Task()
	tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", runtime.InstanceID(tk.ID.String())), runtime.ConnectionTarget{SSHHost: "queue-fixture"}, "", "", 0)
	startManagedSession(t, e, b)
}
func startManagedSession(t testing.TB, e *Entry, b agent.Backend) *taskrun.SessionHandle {
	tk := e.Task()
	r := e.Lifecycle.agentRuntime
	r.Backends = agent.Backends{tk.Harness: b}
	h, err := r.StartSession(e.Lifecycle.ctx, tk, agent.Prompt{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Shutdown(context.WithoutCancel(t.Context()), tk); err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	})
	return h
}

// localProcessBackend supplies a real joined transport for lifecycle exit tests.
type localProcessBackend struct {
	*agenttest.FakeBackend

	command []string
}

func (b *localProcessBackend) Start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	cmd := exec.CommandContext(ctx, b.command[0], b.command[1:]...) //nolint:gosec // Fixed command supplied by this test fixture.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, errors.Join(err, stdin.Close())
	}
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(err, stdin.Close())
	}
	return agent.NewSession(ctx, cmd, agent.NewConn(ctx, opts.Logger, stdin, opts.Records, nil), stdout, opts.Logger), nil
}

// blockedQueueInput exercises cancellation of an actual Conn stdin write.
type blockedQueueInput struct {
	*io.PipeWriter

	started chan struct{}
}

func (w *blockedQueueInput) Write(p []byte) (int, error) {
	close(w.started)
	return w.PipeWriter.Write(p)
}

// blockedInputBackend keeps stdout alive while its real input writer is stalled.
type blockedInputBackend struct {
	*agenttest.FakeBackend

	stdin *blockedQueueInput
}

func (b *blockedInputBackend) Start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	cmd := exec.CommandContext(ctx, "sleep", "300")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return agent.NewSession(ctx, cmd, agent.NewConn(ctx, opts.Logger, b.stdin, opts.Records, nil), stdout, opts.Logger), nil
}

// TestMain isolates the package from the Git configuration of the host.
func TestMain(m *testing.M) {
	gittest.Isolate()
	os.Exit(m.Run())
}

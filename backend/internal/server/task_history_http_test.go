// Tests cache-isolated immutable history resources and exact raw-record live handoff.

package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"

	"github.com/klauspost/compress/zstd"
	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
)

func historyHTTPFixture(t testing.TB) (*testRouter, *task.Task, string) {
	s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: claudecode.New()})
	tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "history"}, harness.Claude)
	tk.SetState(taskslog.StateRunning)
	insertTestTask(s, tk.ID, tk)
	e, _ := s.taskMgr.GetEntry(tk.ID)
	p := filepath.Join(t.TempDir(), "history.jsonl")
	data := `{"t":"caic_meta","version":3,"harness":"claude","prompt":"history"}` + "\n" +
		`{"t":"agent","ts":1.000,"msg":{"type":"assistant","message":{"content":[{"type":"text","text":"answer"}]}}}` + "\n"
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	e.LogPath.Set(p)
	t.Cleanup(func() { s.taskHandlers.taskSvc.history.prune(true) })
	return s, tk, p
}

func TestTaskHistoryHTTP(t *testing.T) {
	t.Parallel()
	t.Run("aligned chunks", func(t *testing.T) {
		t.Parallel()
		s, tk, _ := historyHTTPFixture(t)
		b := historyBootstrap(t, s, tk, "")
		entry, _ := s.taskMgr.GetEntry(tk.ID)
		owner := s.taskHandlers.taskSvc.history
		source, release, err := owner.acquire(t.Context(), entry)
		if err != nil {
			t.Fatal(err)
		}
		release()
		if got := historyRequest(t, s, b.ResourceBase+"/chunks/2/2", ""); got.Code != http.StatusBadRequest {
			t.Fatalf("unaligned chunk: %d %s", got.Code, got.Body)
		}
		owner.invalidate(entry, source)
		if _, err := source.Snapshot(t.Context()); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("malformed request retained its source lease: %v", err)
		}
	})
	t.Run("bootstrap", func(t *testing.T) {
		t.Parallel()
		s, tk, _ := historyHTTPFixture(t)
		w := httptest.NewRecorder()
		s.taskHandlers.routes().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/tasks/"+tk.ID.String()+"/history/bootstrap", http.NoBody))
		if w.Code != http.StatusOK {
			t.Fatalf("bootstrap status %d: %s", w.Code, w.Body)
		}
		var b map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
			t.Fatal(err)
		}
		if b["through"] != float64(2) || b["recordsPerChunk"] != float64(256) {
			t.Fatalf("invalid discovery: %s", w.Body)
		}
		if _, ok := b["messages"]; ok {
			t.Fatal("bootstrap retransmits history")
		}
	})
}

func historyRequest(t testing.TB, s *testRouter, target, user string) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(context.WithValue(t.Context(), httpLoggerKey{}, slog.New(slog.DiscardHandler)), http.MethodGet, strings.TrimPrefix(target, "/api/caic/v1"), nil)
	if user != "" {
		r = r.WithContext(auth.NewContext(r.Context(), &auth.User{ID: user}))
	}
	w := httptest.NewRecorder()
	s.taskHandlers.routes().ServeHTTP(w, r)
	return w
}
func historyBootstrap(t testing.TB, s *testRouter, tk *task.Task, user string) v1.HistoryBootstrap {
	w := historyRequest(t, s, "/tasks/"+tk.ID.String()+"/history/bootstrap", user)
	if w.Code != 200 {
		t.Fatalf("bootstrap %d: %s", w.Code, w.Body)
	}
	var b v1.HistoryBootstrap
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	return b
}
func appendHistory(t testing.TB, path, line string) {
	f, err := os.OpenFile(filepath.Clean(path), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(line + "\n")
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("append: %v/%v", err, closeErr)
	}
}

const historyTextRecord = `{"t":"agent","ts":2.000,"msg":{"type":"assistant","message":{"content":[{"type":"text","text":"next"},{"type":"text","text":"last"}]}}}`

func TestHistoryResources(t *testing.T) {
	t.Parallel()
	t.Run("immutable partial ranges and direct lookup", func(t *testing.T) {
		t.Parallel()
		s, tk, p := historyHTTPFixture(t)
		b := historyBootstrap(t, s, tk, "")
		if b.Through == 0 {
			t.Fatal("missing recent range")
		}
		first := historyRequest(t, s, recentHistoryURL(&b), "")
		if first.Code != 200 || !strings.Contains(first.Header().Get("Cache-Control"), "immutable") {
			t.Fatalf("chunk: %d", first.Code)
		}
		appendHistory(t, p, historyTextRecord)
		next := historyBootstrap(t, s, tk, "")
		if next.Through != 3 || recentHistoryURL(&next) == recentHistoryURL(&b) {
			t.Fatal("partial URL failed to change")
		}
		old := historyRequest(t, s, recentHistoryURL(&b), "")
		if old.Body.String() != first.Body.String() {
			t.Fatal("published body changed after append")
		}
		// An unread invalid suffix proves old resources do not Refresh unrelated tails.
		appendHistory(t, p, `{"invalid":`)
		old = historyRequest(t, s, recentHistoryURL(&b), "")
		if old.Code != 200 || old.Body.String() != first.Body.String() {
			t.Fatalf("old range parsed unrelated suffix: %d %s", old.Code, old.Body)
		}
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, strings.TrimPrefix(recentHistoryURL(&b), "/api/caic/v1"), http.NoBody)
		r.Header.Set("If-None-Match", first.Header().Get("ETag"))
		w := httptest.NewRecorder()
		s.taskHandlers.routes().ServeHTTP(w, r)
		if w.Code != 304 || w.Body.Len() != 0 {
			t.Fatalf("revalidation %d: %s", w.Code, w.Body)
		}
		failed := historyRequest(t, s, "/tasks/"+tk.ID.String()+"/history/bootstrap", "")
		if failed.Code != 500 || failed.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("projection failure cached: %d %v", failed.Code, failed.Header())
		}
	})
	t.Run("account isolation including legacy shared task", func(t *testing.T) {
		t.Parallel()
		s, tk, _ := historyHTTPFixture(t)
		a := historyBootstrap(t, s, tk, "alice")
		b := historyBootstrap(t, s, tk, "bob")
		if a.Scope == b.Scope || a.ResourceBase == b.ResourceBase {
			t.Fatal("shared task cache scope is not account isolated")
		}
		// Warm Alice's immutable resource before changing the request identity.
		// Authorization still runs for the same URL under Bob's credentials.
		warmed := historyRequest(t, s, recentHistoryURL(&a), "alice")
		if warmed.Code != 200 || !strings.Contains(warmed.Header().Get("Cache-Control"), "private") || strings.Join(warmed.Header().Values("Vary"), ",") != "Cookie,Authorization" {
			t.Fatalf("authenticated chunk cache contract: %d %v", warmed.Code, warmed.Header())
		}
		w := historyRequest(t, s, recentHistoryURL(&a), "bob")
		if w.Code != 403 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("cross-account resource %d %s", w.Code, w.Body)
		}
		tk.OwnerID = "alice"
		for _, target := range []string{"/tasks/" + tk.ID.String() + "/history/bootstrap", recentHistoryURL(&b), "/tasks/" + tk.ID.String() + "/history/events?cursor=" + b.Cursor, "/tasks/" + tk.ID.String() + "/history/costs"} {
			w := historyRequest(t, s, target, "bob")
			if w.Code != 403 || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("unauthorized %s: %d", target, w.Code)
			}
		}
	})
	t.Run("range boundaries and stale references", func(t *testing.T) {
		t.Parallel()
		s, tk, _ := historyHTTPFixture(t)
		b := historyBootstrap(t, s, tk, "")
		for _, suffix := range []string{"/chunks/0/1", "/chunks/2/2", "/chunks/1/257", "/chunks/01/2"} {
			w := historyRequest(t, s, b.ResourceBase+suffix, "")
			if w.Code != 400 || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("bad range %s: %d", suffix, w.Code)
			}
		}
		for _, target := range []string{strings.Replace(recentHistoryURL(&b), "/"+b.Generation+"/", "/expired/", 1), strings.Replace(recentHistoryURL(&b), "/"+historyProjectionVersion+"/"+b.Generation+"/", "/99/"+b.Generation+"/", 1), b.ResourceBase + "/chunks/1/3", "/tasks/" + tk.ID.String() + "/history/costs?generation=expired&through=2"} {
			w := historyRequest(t, s, target, "")
			if w.Code != 409 || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("stale %s: %d %s", target, w.Code, w.Body)
			}
		}
		replacement := mustNewTask(t, tk.ID, agent.Prompt{Text: "replacement"}, harness.Claude)
		replacement.SetState(taskslog.StateRunning)
		insertTestTask(s, tk.ID, replacement)
		if w := historyRequest(t, s, recentHistoryURL(&b), ""); w.Code != 409 {
			t.Fatalf("replacement accepted old reference: %d", w.Code)
		}
	})
	t.Run("bounded resource ring and current disk revision", func(t *testing.T) {
		t.Parallel()
		s, tk, _ := historyHTTPFixture(t)
		for i := range 75 {
			tk.PushStats(&runtime.Stats{Ts: time.Unix(int64(i+1), 0), DiskUsed: -1})
		}
		tk.UpdateDiskUsage(123)
		b := historyBootstrap(t, s, tk, "")
		r, cancel := historyStream(t, s, tk, b.Cursor, "")
		t.Cleanup(cancel)
		f := readHistoryFrame(t, r)
		if f.name != "statsSnapshot" || f.id != "" {
			t.Fatalf("resource snapshot must precede history without a cursor: %+v", f)
		}
		var stats v1.HistoryStats
		if err := json.Unmarshal([]byte(f.data), &stats); err != nil {
			t.Fatal(err)
		}
		if len(stats.Samples) != 60 || stats.Samples[0].Ts != 16000 || stats.Samples[59].Ts != 75000 || stats.Samples[59].DiskUsed != 123 {
			t.Fatalf("invalid ring: %+v", stats)
		}
	})
	t.Run("source reconstruction and cancellation", func(t *testing.T) {
		t.Parallel()
		s, tk, _ := historyHTTPFixture(t)
		b := historyBootstrap(t, s, tk, "")
		first := historyRequest(t, s, recentHistoryURL(&b), "")
		e, _ := s.taskMgr.GetEntry(tk.ID)
		source, release, err := s.taskHandlers.taskSvc.history.acquire(t.Context(), e)
		if err != nil {
			t.Fatal(err)
		}
		release()
		s.taskHandlers.taskSvc.history.invalidate(e, source)
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, strings.TrimPrefix(recentHistoryURL(&b), "/api/caic/v1"), http.NoBody)
		ctx, cancel := context.WithCancel(r.Context())
		cancel()
		r = r.WithContext(context.WithValue(ctx, httpLoggerKey{}, slog.New(slog.DiscardHandler)))
		w := httptest.NewRecorder()
		s.taskHandlers.routes().ServeHTTP(w, r)
		if w.Code == 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("canceled source request succeeded")
		}
		recovered := historyRequest(t, s, recentHistoryURL(&b), "")
		if recovered.Code != 200 || recovered.Body.String() != first.Body.String() {
			t.Fatalf("same-generation recovery changed bytes: %d", recovered.Code)
		}
	})
}

type historyTestFrame struct{ name, id, data string }

func readHistoryFrame(t testing.TB, r *bufio.Reader) historyTestFrame {
	var f historyTestFrame
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE: %v", err)
		}
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			if f.data != "" {
				return f
			}
			continue
		}
		if value, ok := strings.CutPrefix(line, "event: "); ok {
			f.name = value
		}
		if value, ok := strings.CutPrefix(line, "id: "); ok {
			f.id = value
		}
		if value, ok := strings.CutPrefix(line, "data: "); ok {
			f.data = value
		}
	}
}
func historyStream(t testing.TB, s *testRouter, tk *task.Task, cursor, header string) (*bufio.Reader, context.CancelFunc) {
	server := httptest.NewServer(s.taskHandlers.routes())
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/tasks/"+tk.ID.String()+"/history/events?cursor="+cursor, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if header != "" {
		r.Header.Set("Last-Event-ID", header)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE status %d", resp.StatusCode)
	}
	return bufio.NewReader(resp.Body), cancel
}

func assertStatsSnapshot(t testing.TB, r *bufio.Reader) {
	t.Helper()
	f := readHistoryFrame(t, r)
	if f.name != "statsSnapshot" || f.id != "" {
		t.Fatalf("resource snapshot: %+v", f)
	}
	var stats v1.HistoryStats
	if err := json.Unmarshal([]byte(f.data), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Samples == nil {
		t.Fatal("resource snapshot must contain an array")
	}
}

func TestTaskResourceSnapshot(t *testing.T) {
	t.Parallel()
	for _, state := range []taskslog.State{taskslog.StatePurged, taskslog.StateStopped} {
		for _, retained := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/retained=%v", state, retained), func(t *testing.T) {
				t.Parallel()
				s, tk, _ := historyHTTPFixture(t)
				tk.SetState(state)
				if retained {
					tk.PushStats(&runtime.Stats{Ts: time.Unix(1, 0), CPUQuota: new(0.0), DiskUsed: -1})
					tk.UpdateDiskUsage(42)
				}
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				server := httptest.NewServer(s.taskHandlers.routes())
				t.Cleanup(server.Close)
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/tasks/"+tk.ID.String()+"/history/events?cursor="+historyCursor{generation: tk.TimelineID(), end: true}.String(), http.NoBody)
				if err != nil {
					t.Fatal(err)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := resp.Body.Close(); err != nil {
						t.Error(err)
					}
				})
				reader := bufio.NewReader(resp.Body)
				frame := readHistoryFrame(t, reader)
				if frame.name != "statsSnapshot" || frame.id != "" {
					t.Fatalf("snapshot must precede disk replay: %+v", frame)
				}
				var snapshot v1.HistoryStats
				if err := json.Unmarshal([]byte(frame.data), &snapshot); err != nil {
					t.Fatal(err)
				}
				if snapshot.Samples == nil {
					t.Fatal("empty ring must be an array")
				}
				if retained && (len(snapshot.Samples) != 1 || snapshot.Samples[0].DiskUsed != 42 || snapshot.Samples[0].CPUQuota == nil || *snapshot.Samples[0].CPUQuota != 0 || snapshot.Samples[0].CPUCeiling != nil) {
					t.Fatalf("retained resources: %+v", snapshot)
				}
				for frame.name != "checkpoint" {
					frame = readHistoryFrame(t, reader)
				}
				tk.PushStats(&runtime.Stats{Ts: time.Unix(2, 0), DiskUsed: -1})
				frame = readHistoryFrame(t, reader)
				if frame.name != "stats" || frame.id != "" {
					t.Fatalf("live resource update: %+v", frame)
				}

				cancel()
			})
		}
	}
}

func TestHistoryStream(t *testing.T) {
	t.Parallel()
	t.Run("appends after bootstrap, output resume, zero records and stats", func(t *testing.T) {
		t.Parallel()
		s, tk, p := historyHTTPFixture(t)
		b := historyBootstrap(t, s, tk, "")
		appendHistory(t, p, historyTextRecord)
		r, body := historyStream(t, s, tk, b.Cursor, "")
		assertStatsSnapshot(t, r)
		first := readHistoryFrame(t, r)
		second := readHistoryFrame(t, r)
		checkpoint := readHistoryFrame(t, r)
		if first.name != "" || second.name != "" || first.id == second.id || checkpoint.name != "checkpoint" || !strings.HasSuffix(checkpoint.id, ".3.end") {
			t.Fatalf("bad raw handoff: %+v %+v %+v", first, second, checkpoint)
		}
		// A header cursor must override a stale initial URL cursor and resume inside
		// a physical record that emitted several normalized outputs.
		rr, other := historyStream(t, s, tk, b.Cursor, first.id)
		assertStatsSnapshot(t, rr)
		resumed := readHistoryFrame(t, rr)
		if resumed.id != second.id || resumed.data != second.data {
			t.Fatalf("output resume: %+v", resumed)
		}
		other()
		appendHistory(t, p, `{"t":"caic_meta","version":3,"harness":"claude","prompt":"history"}`)
		for {
			f := readHistoryFrame(t, r)
			if f.name == "checkpoint" && strings.HasSuffix(f.id, ".4.end") {
				break
			}
			if f.name == "" {
				t.Fatalf("duplicate output: %+v", f)
			}
		}
		now := time.Unix(20, 0)
		tk.PushStats(&runtime.Stats{Ts: now, DiskUsed: 1})
		f := readHistoryFrame(t, r)
		if f.name != "stats" || f.id != "" {
			t.Fatalf("stats replaced raw cursor: %+v", f)
		}
		tk.UpdateDiskUsage(88)
		f = readHistoryFrame(t, r)
		var stats v1.EventStats
		if err := json.Unmarshal([]byte(f.data), &stats); err != nil {
			t.Fatal(err)
		}
		if f.name != "stats" || f.id != "" || stats.Ts != 20000 || stats.DiskUsed != 88 {
			t.Fatalf("disk revision lost: %+v", f)
		}
		body()
	})
	t.Run("stale and malformed cursors reset", func(t *testing.T) {
		t.Parallel()
		for _, cursor := range []string{"invalid", "1.expired.1.end", "1.expired.99999999999999999999.end"} {
			s, tk, _ := historyHTTPFixture(t)
			r, _ := historyStream(t, s, tk, cursor, "")
			f := readHistoryFrame(t, r)
			if f.name != "reset" || f.id != "" || !strings.Contains(f.data, "history/bootstrap") {
				t.Fatalf("stale cursor: %+v", f)
			}
		}
	})
	t.Run("live projection failure is explicit and recoverable", func(t *testing.T) {
		t.Parallel()
		s, tk, p := historyHTTPFixture(t)
		b := historyBootstrap(t, s, tk, "")
		r, _ := historyStream(t, s, tk, b.Cursor, "")
		assertStatsSnapshot(t, r)
		readHistoryFrame(t, r)
		file, err := os.OpenFile(filepath.Clean(p), os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := file.Close(); err != nil {
				t.Error(err)
			}
		})
		original, err := io.ReadAll(file)
		if err != nil {
			t.Fatal(err)
		}
		appendHistory(t, p, `{"broken":`)
		f := readHistoryFrame(t, r)
		if f.name != "error" {
			t.Fatalf("failed live projection silently ended: %+v", f)
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(original); err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(int64(len(original))); err != nil {
			t.Fatal(err)
		}
		recovered := historyBootstrap(t, s, tk, "")
		if recovered.Generation != b.Generation || recovered.Through != 2 {
			t.Fatal("recovery changed logical identity")
		}
	})
}

func TestHistorySharedRecovery(t *testing.T) {
	t.Parallel()
	s, tk, p := historyHTTPFixture(t)
	var calls atomic.Int64
	backend := &agenttest.FakeBackend{HarnessName: harness.Claude, WireFactory: func() agent.WireFormat {
		return &agenttest.Wire{Parse: func([]byte) ([]agent.Message, error) {
			calls.Add(1)
			return []agent.Message{&agent.TextMessage{Text: "configured fake wire"}}, nil
		}}
	}}
	s.taskMgr.Backends[harness.Claude] = backend
	var fixture strings.Builder
	fixture.WriteString(`{"t":"caic_meta","version":3,"harness":"claude","prompt":"history"}` + "\n")
	for range 1024 {
		fixture.WriteString(historyTextRecord + "\n")
	}
	if err := os.WriteFile(p, []byte(fixture.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	// Obtain the resource identity without projecting; concurrent missing-range
	// requests must create one shared source and parse each native record once.
	base := "/tasks/" + tk.ID.String() + "/history/" + historyScope(t.Context()) + "/" + historyProjectionVersion + "/" + tk.TimelineID()
	var wg sync.WaitGroup
	responses := make(chan *httptest.ResponseRecorder, 8)
	for range 8 {
		wg.Go(func() { responses <- historyRequest(t, s, base+"/chunks/769/1024", "") })
	}
	wg.Wait()
	close(responses)
	for w := range responses {
		if w.Code != 200 || !strings.Contains(w.Body.String(), "configured fake wire") {
			t.Fatalf("configured resolver/direct lookup: %d %s", w.Code, w.Body)
		}
		var events []v1.HistoryEvent
		if err := json.Unmarshal(w.Body.Bytes(), &events); err != nil {
			t.Fatal(err)
		}
		if len(events) != 256 || events[0].RecordID != 769 || events[255].RecordID != 1024 {
			t.Fatalf("physical chunk boundary lost: %d", len(events))
		}
	}
	if calls.Load() != 1024 {
		t.Fatalf("concurrent requests repeated prefix parsing: %d calls", calls.Load())
	}
	appendHistory(t, p, historyTextRecord)
	if w := historyRequest(t, s, base+"/chunks/1/256", ""); w.Code != 200 {
		t.Fatalf("old direct lookup: %d", w.Code)
	}
	if calls.Load() != 1024 {
		t.Fatal("old range refreshed unrelated tail")
	}
	b := historyBootstrap(t, s, tk, "")
	if b.Through != 1026 || calls.Load() != 1025 {
		t.Fatalf("incremental recovery: through=%d calls=%d", b.Through, calls.Load())
	}
}

func BenchmarkHistoryHTTP(b *testing.B) {
	for _, records := range []int{1024, 20000} {
		b.Run(strconv.Itoa(records), func(b *testing.B) {
			s, tk, p := historyHTTPFixture(b)
			var fixture strings.Builder
			fixture.WriteString(`{"t":"caic_meta","version":3,"harness":"claude","prompt":"history"}` + "\n")
			for range records {
				fixture.WriteString(historyTextRecord + "\n")
			}
			if err := os.WriteFile(p, []byte(fixture.String()), 0o600); err != nil {
				b.Fatal(err)
			}
			bootstrap := historyBootstrap(b, s, tk, "")
			target := bootstrap.ResourceBase + "/chunks/1/256"
			handler := s.taskHandlers.routes()
			b.Run("warm-old-chunk", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					r := httptest.NewRequestWithContext(b.Context(), http.MethodGet, strings.TrimPrefix(target, "/api/caic/v1"), http.NoBody)
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, r)
					if w.Code != 200 {
						b.Fatal(w.Code)
					}
				}
			})
		})
	}
}

func BenchmarkHistorySwitchViews(b *testing.B) {
	for _, records := range []int{1024, 20000} {
		b.Run(strconv.Itoa(records), func(b *testing.B) {
			s, tk, p := historyHTTPFixture(b)
			price := &historyPricerSpy{}
			price.rate.Store(1)
			tk.Pricer = price
			var fixture strings.Builder
			fixture.WriteString(`{"t":"caic_meta","version":3,"harness":"claude","model":"priced-model","prompt":"history"}` + "\n")
			fixture.WriteString(`{"t":"agent","ts":1.000,"msg":{"type":"assistant","message":{"model":"priced-model","content":[{"type":"text","text":"priced session"}],"usage":{"input_tokens":1000000}}}}` + "\n")
			for range records {
				fixture.WriteString(`{"t":"agent","ts":2.000,"msg":{"type":"result","subtype":"success","total_cost_usd":1.25,"usage":{"input_tokens":1000000}}}` + "\n")
			}
			if err := os.WriteFile(p, []byte(fixture.String()), 0o600); err != nil {
				b.Fatal(err)
			}
			_ = historyBootstrap(b, s, tk, "")
			for _, target := range []struct{ name, url string }{
				{"bootstrap", "/tasks/" + tk.ID.String() + "/history/bootstrap"},
			} {
				b.Run(target.name, func(b *testing.B) {
					before := price.calls.Load()
					b.ReportAllocs()
					for b.Loop() {
						response := historyRequest(b, s, target.url, "")
						if response.Code != http.StatusOK {
							b.Fatal(response.Code)
						}
					}
					b.ReportMetric(float64(price.calls.Load()-before)/float64(b.N), "pricing-calls/op")
				})
			}
		})
	}
}

func TestHistoryMutablePrices(t *testing.T) {
	t.Parallel()
	s, tk, p := historyHTTPFixture(t)
	price := &historyPricerSpy{}
	price.rate.Store(10)
	tk.Pricer = price
	data := `{"t":"caic_meta","version":3,"harness":"claude","model":"priced-model","prompt":"history"}` + "\n" +
		`{"t":"agent","ts":1.000,"msg":{"type":"assistant","message":{"model":"priced-model","content":[{"type":"tool_use","id":"exit","name":"ExitPlanMode","input":{}}],"usage":{"input_tokens":1000000}}}}` + "\n" +
		`{"t":"agent","ts":2.000,"msg":{"type":"result","subtype":"success","total_cost_usd":1.25,"usage":{"input_tokens":1000000}}}` + "\n"
	if err := os.WriteFile(filepath.Clean(p), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	b := historyBootstrap(t, s, tk, "")
	canonical := historyRequest(t, s, recentHistoryURL(&b), "")
	prefix := "/tasks/" + tk.ID.String() + "/history/"
	costURL := prefix + "costs?generation=" + b.Generation + "&through=3"
	costs := historyRequest(t, s, costURL, "")
	var estimates []v1.HistoryTurnEstimate
	if err := json.Unmarshal(costs.Body.Bytes(), &estimates); err != nil {
		t.Fatal(err)
	}
	if len(estimates) != 1 || estimates[0].RecordID != 3 || estimates[0].Cost.USD != 10 || estimates[0].TotalUSD != 10 || estimates[0].Model != "priced-model" {
		t.Fatalf("prefix pricing: %s", costs.Body)
	}
	price.rate.Store(20)
	repriced := historyRequest(t, s, costURL, "")
	var current []v1.HistoryTurnEstimate
	if err := json.Unmarshal(repriced.Body.Bytes(), &current); err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 || current[0].Cost.USD != 20 || repriced.Body.String() == costs.Body.String() || repriced.Header().Get("Cache-Control") != "private, no-cache" {
		t.Fatal("full-prefix pricing did not revalidate")
	}
	if historyRequest(t, s, recentHistoryURL(&b), "").Body.String() != canonical.Body.String() {
		t.Fatal("repricing mutated canonical chunk")
	}
	unauthorized := httptest.NewRecorder()
	auth.RequireUser(s.taskHandlers.routes()).ServeHTTP(unauthorized, httptest.NewRequestWithContext(t.Context(), http.MethodGet, prefix+"bootstrap", nil))
	if unauthorized.Code != 401 || unauthorized.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("unauthenticated failure was cacheable")
	}
}

type historyAppendWriter struct {
	*httptest.ResponseRecorder

	append func()
}

func (w *historyAppendWriter) Write(data []byte) (int, error) {
	if w.append != nil {
		f := w.append
		w.append = nil
		f()
	}
	return w.ResponseRecorder.Write(data)
}

func TestHistoryBootstrapAppend(t *testing.T) {
	t.Parallel()
	s, tk, p := historyHTTPFixture(t)
	w := &historyAppendWriter{ResponseRecorder: httptest.NewRecorder(), append: func() { appendHistory(t, p, historyTextRecord) }}
	s.taskHandlers.routes().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/tasks/"+tk.ID.String()+"/history/bootstrap", nil))
	var b v1.HistoryBootstrap
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if b.Through != 2 {
		t.Fatal("bootstrap snapshot changed while writing response")
	}
	r, _ := historyStream(t, s, tk, b.Cursor, "")
	assertStatsSnapshot(t, r)
	f := readHistoryFrame(t, r)
	var event v1.HistoryEvent
	if err := json.Unmarshal([]byte(f.data), &event); err != nil {
		t.Fatal(err)
	}
	if event.RecordID != 3 || event.Index != 0 {
		t.Fatalf("append during bootstrap lost: %+v", f)
	}
}

func TestHistoryAttachAppend(t *testing.T) {
	t.Parallel()
	s, tk, p := historyHTTPFixture(t)
	var calls atomic.Int64
	s.taskMgr.Backends[harness.Claude] = &agenttest.FakeBackend{HarnessName: harness.Claude, WireFactory: func() agent.WireFormat {
		return &agenttest.Wire{Parse: func([]byte) ([]agent.Message, error) {
			if calls.Add(1) == 2 {
				appendHistory(t, p, historyTextRecord)
			}
			return []agent.Message{&agent.TextMessage{Text: "during attach"}}, nil
		}}
	}}
	b := historyBootstrap(t, s, tk, "")
	appendHistory(t, p, historyTextRecord)
	r, _ := historyStream(t, s, tk, b.Cursor, "")
	assertStatsSnapshot(t, r)
	var records []uint64
	for {
		frame := readHistoryFrame(t, r)
		if frame.name == "checkpoint" && strings.HasSuffix(frame.id, ".4.end") {
			break
		}
		if frame.name == "" {
			var event v1.HistoryEvent
			if err := json.Unmarshal([]byte(frame.data), &event); err != nil {
				t.Fatal(err)
			}
			records = append(records, event.RecordID)
		}
	}
	if !slices.Equal(records, []uint64{3, 4}) || calls.Load() != 3 {
		t.Fatalf("attach race duplicated/lost records: %v, parser calls %d", records, calls.Load())
	}
}

func TestHistoryRecoveryCancellation(t *testing.T) {
	t.Parallel()
	s, tk, p := historyHTTPFixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	s.taskMgr.Backends[harness.Claude] = &agenttest.FakeBackend{HarnessName: harness.Claude, WireFactory: func() agent.WireFormat {
		return &agenttest.Wire{Parse: func([]byte) ([]agent.Message, error) {
			if calls.Add(1) == 1 {
				close(started)
				<-release
			}
			return []agent.Message{&agent.TextMessage{Text: "shared recovery"}}, nil
		}}
	}}
	// Block the sole native parse while another request cancels against its gate.
	// The canceled follower must not invalidate shared completed/in-flight work.
	bURL := "/tasks/" + tk.ID.String() + "/history/bootstrap"
	result := make(chan *httptest.ResponseRecorder, 1)
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	wg.Go(func() { result <- historyRequest(t, s, bURL, "") })
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery did not start")
	}
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), httpLoggerKey{}, s.log))
	cancel()
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, bURL, nil)
	w := httptest.NewRecorder()
	s.taskHandlers.routes().ServeHTTP(w, r)
	if w.Code == 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("canceled follower succeeded")
	}
	unblock()
	select {
	case first := <-result:
		if first.Code != 200 {
			t.Fatalf("leader failed: %d %s", first.Code, first.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("leader failed to finish")
	}
	// A subsequent bootstrap refresh sees only the appended suffix.
	appendHistory(t, p, historyTextRecord)
	b := historyBootstrap(t, s, tk, "")
	if b.Through != 3 || calls.Load() != 2 {
		t.Fatalf("follower cancellation poisoned shared recovery: %d/%d", b.Through, calls.Load())
	}
}

func recentHistoryURL(b *v1.HistoryBootstrap) string {
	first := ((b.Through-1)/b.RecordsPerChunk)*b.RecordsPerChunk + 1
	return fmt.Sprintf("%s/chunks/%d/%d", b.ResourceBase, first, b.Through)
}

func TestHistoryCompressionAndRevival(t *testing.T) {
	t.Parallel()
	s, tk, path := historyHTTPFixture(t)
	initial := historyBootstrap(t, s, tk, "")
	original := historyRequest(t, s, recentHistoryURL(&initial), "")
	entry, _ := s.taskMgr.GetEntry(tk.ID)
	owner := s.taskHandlers.taskSvc.history
	leased, release, err := owner.acquire(t.Context(), entry)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := path + ".zst"
	if err := os.WriteFile(filepath.Clean(compressed), encoder.EncodeAll(data, nil), 0o600); err != nil { //nolint:gosec // Fixture path belongs to this test's private temporary directory.
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	tk.SetState(taskslog.StatePurged)
	entry.LogPath.Set(compressed)
	owner.prune(false)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := leased.Snapshot(t.Context()); err != nil {
		t.Fatalf("compression retired an active request: %v", err)
	}
	closed := historyBootstrap(t, s, tk, "")
	if closed.ResourceBase != initial.ResourceBase || closed.Through != initial.Through {
		t.Fatal("compression changed immutable history identity")
	}
	projected := historyRequest(t, s, recentHistoryURL(&initial), "")
	if projected.Code != 200 || projected.Body.String() != original.Body.String() {
		t.Fatalf("compressed history changed: %d %s", projected.Code, projected.Body)
	}
	if err := os.WriteFile(path, append(data, []byte(historyTextRecord+"\n")...), 0o600); err != nil { //nolint:gosec // Fixture path belongs to this test's private temporary directory.
		t.Fatal(err)
	}
	entry.LogPath.Set(path)
	tk.SetState(taskslog.StateRunning)
	revived := historyBootstrap(t, s, tk, "")
	if revived.Through != initial.Through+1 {
		t.Fatalf("revived prefix: %+v", revived)
	}
	retained := historyRequest(t, s, recentHistoryURL(&initial), "")
	if retained.Code != 200 || retained.Body.String() != original.Body.String() {
		t.Fatal("revival changed an earlier immutable chunk")
	}
}

func TestHistoryStreamAcquisitionReplacement(t *testing.T) {
	t.Parallel()
	s, tk, path := historyHTTPFixture(t)
	bootstrap := historyBootstrap(t, s, tk, "")
	entry, _ := s.taskMgr.GetEntry(tk.ID)
	owner := s.taskHandlers.taskSvc.history
	source, release, err := owner.acquire(t.Context(), entry)
	if err != nil {
		t.Fatal(err)
	}
	release()
	owner.invalidate(entry, source)
	replacement := filepath.Join(t.TempDir(), "replacement.jsonl")
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacement, append(raw, []byte(historyTextRecord+"\n")...), 0o600); err != nil { //nolint:gosec // Private generated test fixture.
		t.Fatal(err)
	}
	s.taskMgr.Backends[harness.Claude] = &agenttest.FakeBackend{WireFactory: func() agent.WireFormat {
		entry.LogPath.Set(replacement)
		return claudecode.New().NewWire()
	}}
	reader, cancel := historyStream(t, s, tk, bootstrap.Cursor, "")
	t.Cleanup(cancel)
	frame := readHistoryFrame(t, reader)
	if frame.name != "reset" {
		t.Fatalf("source replaced during acquisition was accepted: %+v", frame)
	}
}

// BenchmarkHistoryTaskOpen measures server-side task-open transport only. Cold
// opens project and read every immutable chunk; cached opens omit chunk requests,
// as a browser with the same immutable URLs would. Both include discovery and
// the resource snapshot/checkpoint stream. Browser JSON decoding and rendering
// are excluded.
func BenchmarkHistoryTaskOpen(b *testing.B) {
	for _, compressed := range []bool{false, true} {
		format := "plain"
		if compressed {
			format = "compressed"
		}
		b.Run(format, func(b *testing.B) {
			s, tk, path := historyHTTPFixture(b)
			tk.SetState(taskslog.StatePurged)
			var raw strings.Builder
			raw.WriteString(`{"t":"caic_meta","version":3,"harness":"claude","prompt":"history"}` + "\n")
			for i := range 1000 {
				fmt.Fprintf(&raw, `{"t":"user_input","text":"turn %d"}`+"\n", i)
				fmt.Fprintf(&raw, `{"t":"agent","ts":1.000,"msg":{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tool-%d","name":"Bash","input":{"command":"git status"}}]}}}`+"\n", i)
				fmt.Fprintf(&raw, `{"t":"agent","ts":2.000,"msg":{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tool-%d","content":%q}]}}}`+"\n", i, strings.Repeat("tool output ", 16))
				fmt.Fprintf(&raw, `{"t":"agent","ts":3.000,"msg":{"type":"assistant","message":{"content":[{"type":"text","text":%q}],"usage":{"input_tokens":10000,"output_tokens":1000}}}}`+"\n", strings.Repeat("Completed validation. ", 8))
				fmt.Fprintf(&raw, `{"t":"agent","ts":4.000,"msg":{"type":"result","subtype":"success","total_cost_usd":%.3f,"usage":{"input_tokens":10000,"output_tokens":1000}}}`+"\n", float64(i+1)*0.01)
			}
			data := []byte(raw.String())
			if compressed {
				encoder, err := zstd.NewWriter(nil)
				if err != nil {
					b.Fatal(err)
				}
				data = encoder.EncodeAll(data, nil)
				if err := encoder.Close(); err != nil {
					b.Fatal(err)
				}
				path += ".zst"
			}
			if err := os.WriteFile(filepath.Clean(path), data, 0o600); err != nil {
				b.Fatal(err)
			}
			entry, _ := s.taskMgr.GetEntry(tk.ID)
			entry.LogPath.Set(path)
			handler := s.taskHandlers.routes()
			open := func(cold bool) int {
				response := historyRequest(b, s, "/tasks/"+tk.ID.String()+"/history/bootstrap", "")
				if response.Code != http.StatusOK {
					b.Fatal(response.Code, response.Body.String())
				}
				var bootstrap v1.HistoryBootstrap
				if err := json.Unmarshal(response.Body.Bytes(), &bootstrap); err != nil {
					b.Fatal(err)
				}
				total := response.Body.Len()
				if cold {
					for first := uint64(1); first <= bootstrap.Through; first += bootstrap.RecordsPerChunk {
						last := min(first+bootstrap.RecordsPerChunk-1, bootstrap.Through)
						writer := &historyOpenWriter{header: make(http.Header)}
						request := httptest.NewRequestWithContext(b.Context(), http.MethodGet, strings.TrimPrefix(fmt.Sprintf("%s/chunks/%d/%d", bootstrap.ResourceBase, first, last), "/api/caic/v1"), http.NoBody)
						handler.ServeHTTP(writer, request)
						if writer.code != http.StatusOK {
							b.Fatal(writer.code)
						}
						total += writer.bytes
					}
				}
				ctx, cancel := context.WithCancel(b.Context())
				writer := &historyOpenWriter{header: make(http.Header), cancel: cancel}
				request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/tasks/"+tk.ID.String()+"/history/events?cursor="+bootstrap.Cursor, http.NoBody)
				handler.ServeHTTP(writer, request)
				cancel()
				if writer.code != http.StatusOK {
					b.Fatal(writer.code)
				}
				return total + writer.bytes
			}
			for _, cold := range []bool{true, false} {
				name := "cached"
				if cold {
					name = "cold"
				}
				b.Run(name, func(b *testing.B) {
					_ = open(true)
					b.ReportAllocs()
					total := 0
					for b.Loop() {
						if cold {
							s.taskHandlers.taskSvc.history.prune(true)
						}
						total = open(cold)
					}
					b.ReportMetric(float64(total), "response-B/op")
				})
			}
		})
	}
}

type historyOpenWriter struct {
	header http.Header
	code   int
	bytes  int
	cancel context.CancelFunc
}

func (w *historyOpenWriter) Header() http.Header  { return w.header }
func (w *historyOpenWriter) WriteHeader(code int) { w.code = code }
func (w *historyOpenWriter) Flush()               {}
func (w *historyOpenWriter) Write(data []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	w.bytes += len(data)
	if w.cancel != nil && bytes.Contains(data, []byte("event: checkpoint")) {
		w.cancel()
	}
	return len(data), nil
}

// Tests CI service shutdown and journal-first publication of created pull requests.

package ci

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/backends"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/forge/github"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/runtimetest"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/task/history"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
	"github.com/caic-xyz/caic/backend/internal/task/taskrun"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	"github.com/caic-xyz/caic/metrics"
	"github.com/maruel/ksid"
)

func TestService(t *testing.T) {
	t.Parallel()
	t.Run("StartPRFlow", func(t *testing.T) {
		t.Parallel()
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			t.Run("missing journal preserves task metadata", func(t *testing.T) {
				t.Parallel()
				log := slog.New(slog.DiscardHandler)
				router, err := runtime.NewRouter([]runtime.System{&runtimetest.FakeSystem{}}, metrics.Nop{})
				if err != nil {
					t.Fatal(err)
				}
				parsers := backends.Default("", nil)
				manager, err := taskmgr.New(taskmgr.Config{ServerCtx: t.Context(), Log: log,
					LogStore: history.NewStore(log, taskslog.NewStore(t.TempDir()), parsers),
					Runtimes: router, Checkouts: repo.NewRegistry(), Backends: parsers,
					Rollup: task.DiscardRollup{}, RuntimeStartTimeout: time.Hour})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := manager.Close(); err != nil {
						t.Error(err)
					}
				})
				tk, err := taskrun.NewTask(ksid.NewID(), agent.Prompt{Text: "accepted"}, harness.Claude, "", "", "", "", "")
				if err != nil {
					t.Fatal(err)
				}
				tk.SetState(task.StateStopped)
				entry := manager.NewEntry(tk, nil)
				svc := NewService(log, nil, nil, nil)
				t.Cleanup(svc.Close)
				client := github.NewClient("", prCreatedTransport{})
				number, err := svc.StartPRFlow(t.Context(), entry, client, &RepoInfo{ForgeOwner: "owner", ForgeRepo: "repo"}, "branch", "main")
				if err == nil || number != 42 {
					t.Fatalf("created PR = %d, err = %v; want creation retained with journal failure", number, err)
				}
				got := tk.Snapshot()
				if got.ForgeOwner != "" || got.ForgeRepo != "" || got.ForgePR != 0 || got.ForgePRState != "" || entry.MonitorBranch() != "" {
					t.Fatalf("failed record published PR %s/%s#%d state=%s monitor=%q", got.ForgeOwner, got.ForgeRepo, got.ForgePR, got.ForgePRState, entry.MonitorBranch())
				}
			})
		})
	})
	t.Run("Close", func(t *testing.T) {
		t.Parallel()
		svc := NewService(slog.New(slog.DiscardHandler), nil, nil, nil)
		requestCtx, cancel := context.WithCancel(t.Context())
		started := make(chan context.Context, 1)
		done := make(chan struct{})
		svc.startWorker(requestCtx, func(ctx context.Context) {
			started <- ctx
			<-ctx.Done()
			close(done)
		})
		workerCtx := <-started
		cancel()
		if workerCtx.Err() != nil {
			t.Fatal("request cancellation stopped background CI work")
		}
		svc.Close()
		select {
		case <-done:
		default:
			t.Fatal("Close returned before worker stopped")
		}
	})
}

// prCreatedTransport returns a completed external PR creation independently of task recording.
type prCreatedTransport struct{}

func (prCreatedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusCreated, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"number":42,"head":{"sha":"abc"}}`))}, nil
}

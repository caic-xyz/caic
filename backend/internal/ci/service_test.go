// Tests CI service shutdown, owner admission, and journal-first pull request publication.

package ci

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/backends"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/forge/github"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/runtimetest"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/task/history"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
	"github.com/caic-xyz/caic/backend/internal/task/taskrun"
	"github.com/caic-xyz/caic/backend/internal/task/taskrun/taskruntest"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	"github.com/caic-xyz/caic/metrics"
	"github.com/maruel/ksid"
)

func TestService(t *testing.T) {
	t.Run("automation admission across suspension and reenable", func(t *testing.T) {
		t.Parallel()
		for _, operation := range []string{"apply result", "create PR", "detached worker", "monitor"} {
			t.Run(operation, func(t *testing.T) {
				t.Parallel()
				gate := &beforeOwnerBinding{entered: make(chan struct{}), resume: make(chan struct{})}
				manager, entry, store, user := ownedCIEntryWithPolicy(t, func(store *auth.Store) task.OwnerPolicy { gate.Store = store; return gate })
				cache, err := forgecache.Open("")
				if err != nil {
					t.Fatal(err)
				}
				if err := cache.Put("owner", "repo", "abc", forgecache.Result{Status: forge.CIStatusSuccess}); err != nil {
					t.Fatal(err)
				}
				client := &admissionForge{}
				backend := &admissionBackend{}
				svc := NewService(slog.New(slog.DiscardHandler), cache, nil, backend)
				t.Cleanup(svc.Close)
				entry.Task().SetPR("owner", "repo", 42)
				var worked atomic.Bool
				done := make(chan struct{})
				go func() {
					defer close(done)
					// Webhooks enter without an authenticated request or captured account epoch.
					switch operation {
					case "apply result":
						svc.ApplyMonitorCIResult(t.Context(), entry, client, "owner", "repo", "abc", forgecache.Result{Status: forge.CIStatusSuccess})
					case "create PR":
						_, _ = svc.StartPRFlow(t.Context(), entry, client, &RepoInfo{ForgeOwner: "owner", ForgeRepo: "repo"}, "branch", "main")
					case "detached worker":
						svc.startWorker(t.Context(), entry, func(context.Context) { worked.Store(true) })
					case "monitor":
						svc.MonitorCI(t.Context(), entry, client, "owner", "repo", "abc")
					}
				}()
				<-gate.entered
				// Pause before owner binding: any earlier state snapshot is now stale.
				if err := store.SetDisabled(user.ID, true); err != nil {
					t.Fatal(err)
				}
				if err := manager.SuspendOwner(t.Context(), user.ID); err != nil {
					t.Fatal(err)
				}
				if entry.Task().GetState() != task.StateStopped {
					t.Fatal("suspension did not stop task")
				}
				if err := store.SetDisabled(user.ID, false); err != nil {
					t.Fatal(err)
				}
				close(gate.resume)
				<-done
				if err := svc.workers.Wait(); err != nil {
					t.Fatal(err)
				}
				if client.called.Load() || backend.called.Load() || worked.Load() {
					t.Error("automation admitted a task stopped during owner binding")
				}
				// Rejected admission must release its new-generation binding, or suspension hangs.
				if err := store.SetDisabled(user.ID, true); err != nil {
					t.Fatal(err)
				}
				joined, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				if err := manager.SuspendOwner(joined, user.ID); err != nil {
					t.Fatalf("rejected admission retained owner work: %v", err)
				}
			})
		}
	})

	t.Run("owner cancellation joins detached worker", func(t *testing.T) {
		t.Parallel()
		manager, entry, store, user := ownedCIEntry(t)
		svc := NewService(slog.New(slog.DiscardHandler), nil, nil, nil)
		t.Cleanup(svc.Close)
		request, cancel := context.WithCancel(auth.NewContext(t.Context(), &user))
		t.Cleanup(cancel)
		started := make(chan context.Context, 1)
		done := make(chan struct{})
		svc.startWorker(request, entry, func(ctx context.Context) { started <- ctx; <-ctx.Done(); close(done) })
		work := <-started
		cancel()
		if work.Err() != nil {
			t.Fatal("disconnect cancelled admitted owner automation")
		}
		if err := store.SetDisabled(user.ID, true); err != nil {
			t.Fatal(err)
		}
		if err := manager.SuspendOwner(t.Context(), user.ID); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
		default:
			t.Fatal("suspension returned before detached CI worker joined")
		}
	})

	t.Run("suspension joins cancelled CI publication", func(t *testing.T) {
		t.Parallel()
		manager, entry, store, user := ownedCIEntry(t)
		cache, err := forgecache.Open("")
		if err != nil {
			t.Fatal(err)
		}
		synctest.Test(t, func(t *testing.T) {
			client := &blockedOwnerMerge{started: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
			svc := NewService(slog.New(slog.DiscardHandler), cache, nil, &notificationBackend{})
			t.Cleanup(svc.Close)
			entry.Task().SetPR("owner", "repo", 42)
			var workers sync.WaitGroup
			t.Cleanup(workers.Wait)
			workers.Go(func() {
				svc.ApplyMonitorCIResult(t.Context(), entry, client, "owner", "repo", "abc", forgecache.Result{Status: forge.CIStatusSuccess})
			})
			<-client.started
			if err := store.SetDisabled(user.ID, true); err != nil {
				t.Fatal(err)
			}
			<-client.cancelled
			suspended := make(chan error, 1)
			workers.Go(func() { suspended <- manager.SuspendOwner(t.Context(), user.ID) })
			synctest.Wait()
			select {
			case err := <-suspended:
				t.Fatalf("suspension certified unfinished CI work: %v", err)
			default:
			}
			close(client.release)
			if err := <-suspended; err != nil {
				t.Fatal(err)
			}
		})
	})

	t.Run("disabled owner cannot merge", func(t *testing.T) {
		t.Parallel()
		_, entry, store, user := ownedCIEntry(t)
		client := &ownerMergeForge{}
		cache, err := forgecache.Open("")
		if err != nil {
			t.Fatal(err)
		}
		svc := NewService(slog.New(slog.DiscardHandler), cache, nil, &notificationBackend{})
		t.Cleanup(svc.Close)
		entry.Task().SetPR("owner", "repo", 42)
		if err := store.SetDisabled(user.ID, true); err != nil {
			t.Fatal(err)
		}
		svc.ApplyMonitorCIResult(t.Context(), entry, client, "owner", "repo", "abc", forgecache.Result{Status: forge.CIStatusSuccess})
		if client.merged {
			t.Fatal("disabled owner automatic merge reached forge")
		}
		if err := store.SetDisabled(user.ID, false); err != nil {
			t.Fatal(err)
		}
		taskruntest.SetState(entry.Task(), task.StateStopped)
		svc.ApplyMonitorCIResult(t.Context(), entry, client, "owner", "repo", "def", forgecache.Result{Status: forge.CIStatusSuccess})
		if client.merged {
			t.Fatal("webhook-style result resumed stopped-owner automation")
		}
	})

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
				taskruntest.SetState(tk, task.StateWaiting)
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
		_, entry, _, _ := ownedCIEntry(t)
		svc.startWorker(requestCtx, entry, func(ctx context.Context) {
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

type notificationBackend struct{ Backend }

func (*notificationBackend) NotifyTaskChange(ksid.ID) {}

type ownerMergeForge struct {
	forge.Forge

	merged bool
}

func (*ownerMergeForge) Name() string       { return "fixture" }
func (*ownerMergeForge) PRLabel(int) string { return "PR" }
func (f *ownerMergeForge) MergePR(context.Context, string, string, int, string, string) error {
	f.merged = true
	return nil
}

func ownedCIEntry(t *testing.T) (*taskmgr.Manager, *taskmgr.Entry, *auth.Store, auth.User) {
	return ownedCIEntryWithPolicy(t, nil)
}

func ownedCIEntryWithPolicy(t *testing.T, policy func(*auth.Store) task.OwnerPolicy) (*taskmgr.Manager, *taskmgr.Entry, *auth.Store, auth.User) {
	store, err := auth.Open(filepath.Join(t.TempDir(), "users.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	user, err := store.UpsertUser(&auth.User{Provider: auth.ProviderGitHub, ProviderID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	system := &runtimetest.FakeSystem{}
	router, err := runtime.NewRouter([]runtime.System{system}, metrics.Nop{})
	if err != nil {
		t.Fatal(err)
	}
	parsers := backends.Default("", nil)
	logs := history.NewStore(log, taskslog.NewStore(t.TempDir()), parsers)
	var ownerPolicy task.OwnerPolicy = store
	if policy != nil {
		ownerPolicy = policy(store)
	}
	manager, err := taskmgr.New(taskmgr.Config{ServerCtx: t.Context(), Log: log, LogStore: logs, Runtimes: router, Checkouts: repo.NewRegistry(), Backends: parsers, OwnerPolicy: ownerPolicy, Rollup: task.DiscardRollup{}, RuntimeStartTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	manager.CompleteRestoration(nil)
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
	})
	tk, err := taskrun.NewTask(ksid.NewID(), agent.Prompt{Text: "owned"}, harness.Claude, "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	tk.OwnerID = user.ID
	id := runtime.NewID(system.Name(), runtime.InstanceID(tk.ID.String()))
	if err := system.Revive(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	tk.SetRuntimeConnectionInfo(id, runtime.ConnectionTarget{SSHHost: tk.ID.String()}, "", "", 0)
	taskruntest.SetState(tk, task.StateWaiting)
	entry := manager.NewEntry(tk, nil)
	journal, path, err := logs.Open(tk.LogFilename(), tk.LogHeader())
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	entry.LogPath.Set(path)
	manager.Insert(tk.ID, entry)
	return manager, entry, store, user
}

type blockedOwnerMerge struct {
	ownerMergeForge

	started, cancelled, release chan struct{}
}

func (f *blockedOwnerMerge) MergePR(ctx context.Context, _, _ string, _ int, _, _ string) error {
	close(f.started)
	<-ctx.Done()
	close(f.cancelled)
	<-f.release
	return ctx.Err()
}

// beforeOwnerBinding pauses server-originated admission before capturing account authority.
type beforeOwnerBinding struct {
	*auth.Store

	entered, resume chan struct{}
	once            sync.Once
}

func (p *beforeOwnerBinding) BindOwner(ctx context.Context, owner string) (context.Context, context.CancelFunc, error) {
	p.once.Do(func() { close(p.entered); <-p.resume })
	return p.Store.BindOwner(ctx, owner)
}

type admissionForge struct {
	forge.Forge

	called atomic.Bool
}

func (*admissionForge) Name() string       { return "fixture" }
func (*admissionForge) PRLabel(int) string { return "PR" }
func (f *admissionForge) MergePR(context.Context, string, string, int, string, string) error {
	f.called.Store(true)
	return nil
}
func (f *admissionForge) CreatePR(context.Context, string, string, string, string, string, string) (forge.PR, error) {
	f.called.Store(true)
	return forge.PR{}, errors.New("unexpected PR creation")
}

type admissionBackend struct {
	notificationBackend

	called atomic.Bool
}

func (b *admissionBackend) GitHubApp() GitHubAppClient { b.called.Store(true); return nil }

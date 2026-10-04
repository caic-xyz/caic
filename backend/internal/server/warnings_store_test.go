// Tests categorized warning episodes, account isolation, and CI polling aggregation.

package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
)

func TestWarningStore(t *testing.T) {
	t.Parallel()
	t.Run("UpdateRuntimeRestore", func(t *testing.T) {
		t.Parallel()
		router := newTestRouter(t, nil)
		w := NewWarningStore(router.taskMgr)
		if err := w.UpdateCI("a", ci.WarningCategoryCIPollFailed, "private CI failure", []ci.WarningDetail{{Repo: "private", Error: "timeout"}}); err != nil {
			t.Fatal(err)
		}
		w.UpdateRuntimeRestore(&taskmgr.ImportError{Failed: 2, Err: errors.New("private runtime diagnostics")})
		for _, owner := range []string{"", "b"} {
			got := w.Since(owner, 0)
			if len(got) != 1 || got[0].Category != "runtime_restore_failed" || len(got[0].Details) != 0 {
				t.Fatalf("public warnings for %q = %+v", owner, got)
			}
			if got[0].Message != "2 tasks could not be restored." {
				t.Fatalf("runtime failure was not translated with the exact count: %+v", got)
			}
			data, err := json.Marshal(&got[0].Warning)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), `"details":[]`) || strings.Contains(string(data), "private runtime diagnostics") {
				t.Fatalf("public warning JSON = %s", data)
			}
			w.UpdateRuntimeRestore(&taskmgr.ImportError{Failed: 2, Err: errors.New("private runtime diagnostics")})
			if next := w.Since(owner, got[0].seq); len(next) != 0 {
				t.Fatalf("unchanged public warning was republished: %+v", next)
			}
		}
		if got := w.Since("a", 0); len(got) != 2 {
			t.Fatalf("own account and public warnings = %+v", got)
		}
	})
	t.Run("UpdateCI", func(t *testing.T) {
		t.Parallel()
		router := newTestRouter(t, nil)
		w := NewWarningStore(router.taskMgr)
		details := []ci.WarningDetail{{Repo: "a", Error: "rate limit"}}
		if err := w.UpdateCI("a", ci.WarningCategoryCIPollFailed, "original", details); err != nil {
			t.Fatal(err)
		}
		first := w.Since("a", 0)
		if len(first) != 1 || first[0].ID == "" {
			t.Fatalf("first episode = %+v", first)
		}
		details[0].Error = "caller mutation"
		first[0].Details[0].Error = "reader mutation"
		if got := w.Since("a", 0); got[0].Details[0].Error != "rate limit" {
			t.Fatalf("details were not isolated: %+v", got)
		}
		if err := w.UpdateCI("a", ci.WarningCategoryCIPollFailed, "original", []ci.WarningDetail{{Repo: "a", Error: "rate limit"}}); err != nil {
			t.Fatal(err)
		}
		if got := w.Since("a", first[0].seq); len(got) != 0 {
			t.Fatalf("unchanged warning was republished: %+v", got)
		}
		if err := w.UpdateCI("a", ci.WarningCategoryCIPollFailed, "translated", []ci.WarningDetail{{Repo: "b", Error: "timeout"}}); err != nil {
			t.Fatal(err)
		}
		updated := w.Since("a", first[0].seq)
		if len(updated) != 1 || updated[0].ID != first[0].ID || updated[0].Message != "translated" || updated[0].Details[0].Repo != "b" {
			t.Fatalf("update did not retain episode identity: %+v", updated)
		}
		if err := w.UpdateCI("b", ci.WarningCategoryCIPollFailed, "other account", []ci.WarningDetail{{Repo: "private", Error: "denied"}}); err != nil {
			t.Fatal(err)
		}
		if got := w.Since("a", 0); len(got) != 1 || got[0].Message != "translated" {
			t.Fatalf("account warnings mixed: %+v", got)
		}
		if err := w.ResolveCI("a", ci.WarningCategoryCIPollFailed); err != nil {
			t.Fatal(err)
		}
		if got := w.Since("a", 0); len(got) != 0 {
			t.Fatalf("recovered warning remains replayable: %+v", got)
		}
		if err := w.UpdateCI("a", ci.WarningCategoryCIPollFailed, "new outage", details); err != nil {
			t.Fatal(err)
		}
		if got := w.Since("a", 0); len(got) != 1 || got[0].ID == first[0].ID {
			t.Fatalf("new outage did not get a new ID: %+v", got)
		}
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			before := w.Since("a", 0)
			for _, category := range []ci.WarningCategory{"", "runtime_restore_failed", "unknown"} {
				if err := w.UpdateCI("a", category, "invalid", nil); err == nil {
					t.Fatalf("accepted unsupported CI category %q", category)
				}
				if err := w.ResolveCI("a", category); err == nil {
					t.Fatalf("accepted recovery for unsupported CI category %q", category)
				}
			}
			if got := w.Since("a", 0); len(got) != 1 || got[0].ID != before[0].ID || got[0].seq != before[0].seq {
				t.Fatalf("invalid category mutated a published warning: %+v", got)
			}
		})
	})
	t.Run("concurrent updates and reads", func(t *testing.T) {
		t.Parallel()
		router := newTestRouter(t, nil)
		w := NewWarningStore(router.taskMgr)
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				if err := w.UpdateCI("a", ci.WarningCategoryCIPollFailed, "failed", []ci.WarningDetail{{Repo: "a", Error: "timeout"}}); err != nil {
					t.Error(err)
				}
				w.Since("a", 0)
			})
		}
		wg.Wait()
		if got := w.Since("a", 0); len(got) != 1 {
			t.Fatalf("concurrent updates produced duplicate alerts: %+v", got)
		}
	})
	t.Run("SSE replays only active warnings with stable IDs", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		w := s.taskHandlers.warnings
		if err := w.UpdateCI("", ci.WarningCategoryCIPollFailed, "CI unavailable", []ci.WarningDetail{{Repo: "a", Error: "timeout"}}); err != nil {
			t.Fatal(err)
		}
		if err := w.UpdateCI("other-account", ci.WarningCategoryCIPollFailed, "private alert", []ci.WarningDetail{{Repo: "private", Error: "denied"}}); err != nil {
			t.Fatal(err)
		}
		var id string
		for range 2 {
			r := connectTaskListStream(t, s)
			for _, kind := range []string{"status", "snapshot", "repos"} {
				if got := readNextTaskListEvent(t, r); got.Kind != kind {
					t.Fatalf("event = %+v, want %s", got, kind)
				}
			}
			ev := readNextTaskListEvent(t, r)
			if ev.Kind != "warning" || ev.Warning == nil || ev.Warning.Message != "CI unavailable" || ev.Warning.Category != "ci_poll_failed" || len(ev.Warning.Details) != 1 || ev.Warning.Details[0].Repo != "a" {
				t.Fatalf("warning event = %+v", ev)
			}
			if id != "" && ev.Warning.ID != id {
				t.Fatalf("replay changed ID: %q != %q", ev.Warning.ID, id)
			}
			id = ev.Warning.ID
		}
	})
}

type pollingWarningBackend struct {
	*testCIBackend

	warnings *WarningStore
	repos    []ci.RepoInfo
	client   forge.Forge
}

func (b *pollingWarningBackend) ListActiveRepos() []ci.RepoInfo { return b.repos }
func (b *pollingWarningBackend) ForgeForInfo(context.Context, *ci.RepoInfo) forge.Forge {
	return b.client
}
func (b *pollingWarningBackend) UpdateWarning(ctx context.Context, category ci.WarningCategory, message string, details []ci.WarningDetail) error {
	ownerID := ""
	if u, ok := auth.UserFromContext(ctx); ok {
		ownerID = u.ID
	}
	return b.warnings.UpdateCI(ownerID, category, message, details)
}

func (b *pollingWarningBackend) ResolveWarning(ctx context.Context, category ci.WarningCategory) error {
	ownerID := ""
	if u, ok := auth.UserFromContext(ctx); ok {
		ownerID = u.ID
	}
	return b.warnings.ResolveCI(ownerID, category)
}

type pollingWarningForge struct {
	forge.Forge

	shaErrors   map[string]error
	checkErrors map[string]error
	entered     chan struct{}
	release     chan struct{}
}

func (f *pollingWarningForge) GetDefaultBranchSHA(ctx context.Context, _, repo, _ string) (string, error) {
	if f.entered != nil {
		f.entered <- struct{}{}
		select {
		case <-f.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "sha", f.shaErrors[repo]
}
func (f *pollingWarningForge) GetCheckRuns(_ context.Context, _, repo, _ string) ([]forge.CheckRun, error) {
	return nil, f.checkErrors[repo]
}

func TestCIPollWarnings(t *testing.T) {
	t.Parallel()
	t.Run("aggregate rounds and recover", func(t *testing.T) {
		t.Parallel()
		router := newTestRouter(t, nil)
		w := NewWarningStore(router.taskMgr)
		f := &pollingWarningForge{
			shaErrors:   map[string]error{"a": errors.New("SHA request failed")},
			checkErrors: map[string]error{"b": errors.New("checks request failed")},
		}
		b := &pollingWarningBackend{warnings: w, client: f, repos: []ci.RepoInfo{
			{RelPath: "b", ForgeRepo: "b"}, {RelPath: "a", ForgeRepo: "a"},
		}}
		cache, err := forgecache.Open("")
		if err != nil {
			t.Fatal(err)
		}
		svc := ci.NewService(testLogger(), cache, nil, b)
		svc.PollCIForActiveRepos(t.Context())
		first := w.Since("", 0)
		if len(first) != 1 || len(first[0].Details) != 2 || first[0].Details[0].Repo != "a" || first[0].Details[1].Error != "checks request failed" {
			t.Fatalf("round not aggregated: %+v", first)
		}
		f.shaErrors["a"] = errors.New("different failure")
		svc.PollCIForActiveRepos(t.Context())
		if got := w.Since("", 0); len(got) != 1 || got[0].ID != first[0].ID || got[0].Details[0].Error != "different failure" {
			t.Fatalf("ongoing outage changed identity: %+v", got)
		}
		b.client = nil
		svc.PollCIForActiveRepos(t.Context())
		b.client = f
		f.shaErrors["a"] = forge.ErrNotFound
		f.checkErrors["b"] = forge.ErrNotFound
		svc.PollCIForActiveRepos(t.Context())
		if got := w.Since("", 0); len(got) != 1 || got[0].ID != first[0].ID {
			t.Fatalf("no-access round falsely recovered: %+v", got)
		}
		clear(f.shaErrors)
		clear(f.checkErrors)
		f.shaErrors["a"] = forge.ErrNotFound
		svc.PollCIForActiveRepos(t.Context())
		if got := w.Since("", 0); len(got) != 1 || got[0].ID != first[0].ID {
			t.Fatalf("partial access falsely recovered: %+v", got)
		}
		clear(f.shaErrors)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		svc.PollCIForActiveRepos(ctx)
		if got := w.Since("", 0); len(got) != 1 {
			t.Fatalf("canceled round falsely recovered: %+v", got)
		}
		svc.PollCIForActiveRepos(t.Context())
		if got := w.Since("", 0); len(got) != 0 {
			t.Fatalf("healthy round did not recover: %+v", got)
		}
		f.shaErrors["a"] = errors.New("new outage")
		svc.PollCIForActiveRepos(t.Context())
		if got := w.Since("", 0); len(got) != 1 || got[0].ID == first[0].ID {
			t.Fatalf("later outage reused ID: %+v", got)
		}
	})
	t.Run("overlapping polls share a round", func(t *testing.T) {
		t.Parallel()
		router := newTestRouter(t, nil)
		f := &pollingWarningForge{entered: make(chan struct{}, 2), release: make(chan struct{})}
		b := &pollingWarningBackend{warnings: NewWarningStore(router.taskMgr), client: f, repos: []ci.RepoInfo{{RelPath: "a"}}}
		cache, err := forgecache.Open("")
		if err != nil {
			t.Fatal(err)
		}
		svc := ci.NewService(testLogger(), cache, nil, b)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		t.Cleanup(cancel)
		done := make(chan struct{})
		go func() { svc.PollCIForActiveRepos(ctx); close(done) }()
		select {
		case <-f.entered:
		case <-ctx.Done():
			t.Fatal("poll did not start")
		}
		svc.PollCIForActiveRepos(t.Context())
		select {
		case <-f.entered:
			t.Fatal("overlapping poll reached forge")
		default:
		}
		close(f.release)
		<-done
	})
	t.Run("different accounts can poll concurrently", func(t *testing.T) {
		t.Parallel()
		router := newTestRouter(t, nil)
		f := &pollingWarningForge{entered: make(chan struct{}, 2), release: make(chan struct{})}
		b := &pollingWarningBackend{warnings: NewWarningStore(router.taskMgr), client: f, repos: []ci.RepoInfo{{RelPath: "a"}}}
		cache, err := forgecache.Open("")
		if err != nil {
			t.Fatal(err)
		}
		svc := ci.NewService(testLogger(), cache, nil, b)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		t.Cleanup(cancel)
		var wg sync.WaitGroup
		for _, id := range []string{"a", "b"} {
			wg.Go(func() { svc.PollCIForActiveRepos(auth.NewContext(ctx, &auth.User{ID: id})) })
		}
		for range 2 {
			select {
			case <-f.entered:
			case <-ctx.Done():
				t.Fatal("account polls were serialized together")
			}
		}
		close(f.release)
		wg.Wait()
	})
}

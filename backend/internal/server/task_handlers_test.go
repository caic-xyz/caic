// Tests task HTTP handler state-precondition errors.

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/runtimetest"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
)

type fileDiffCall struct {
	repository   int
	commit       string
	path         string
	originalPath string
}

type diffRuntimeBackend struct {
	*runtimetest.FakeBackend

	fileDiffCalls []fileDiffCall
}

func (b *diffRuntimeBackend) FileDiff(_ context.Context, _ runtime.ID, repository int, commit, path, originalPath string) (string, error) {
	b.fileDiffCalls = append(b.fileDiffCalls, fileDiffCall{repository: repository, commit: commit, path: path, originalPath: originalPath})
	if commit == "" {
		return "uncommitted patch", nil
	}
	return "committed patch", nil
}

const diffTestCommit = "0123456789abcdef0123456789abcdef01234567"

func newTaskDiffTestRouter(t *testing.T) (*testRouter, *diffRuntimeBackend) {
	backend := &diffRuntimeBackend{FakeBackend: &runtimetest.FakeBackend{
		RepositoryStatusValue: runtime.RepositoryStatus{
			Branch:   "caic-1",
			Upstream: "origin/main",
			Ahead:    1,
			Commits: []runtime.GitCommit{{
				SHA:          diffTestCommit,
				Subject:      "Add API",
				AuthoredDate: "2026-09-15",
				Stat:         []runtime.GitFileStat{{Path: "committed.go", Added: 4, Deleted: 1}},
			}},
			Uncommitted: []runtime.GitFileStatus{{Path: "renamed.go", OriginalPath: "old.go", WorktreeStatus: "R", Added: 2}},
		},
	}}
	s := newTestRouter(t, nil)
	testTaskHandlers(s).taskSvc.runtimes = newTestRuntime(t, backend)
	registerRouterCheckout(t, s.checkouts, "repo", newRouterTestCheckout(t.TempDir()))
	tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "")
	tk.Repos = []taskslog.RepoMount{{Name: "repo", Branch: "caic-1", ContainerPath: "/workspace/repo"}}
	tk.SetRuntimeConnectionInfo("test-runtime:ctr", runtime.ConnectionTarget{SSHHost: "ctr"}, "", "", 0)
	insertTestTask(s, "t1", tk)
	return s, backend
}

func TestTaskDiffHandlers(t *testing.T) {
	t.Parallel()

	t.Run("index omits patch reads", func(t *testing.T) {
		t.Parallel()

		s, backend := newTaskDiffTestRouter(t)
		w := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/tasks/t1/diff/index", nil)
		testTaskHandlers(s).routes().ServeHTTP(w, r)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		if len(backend.fileDiffCalls) != 0 {
			t.Fatalf("FileDiff calls = %d, want none", len(backend.fileDiffCalls))
		}
		var resp v1.TaskDiffIndexResp
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Repositories) != 1 || len(resp.Repositories[0].Commits) != 1 || len(resp.Repositories[0].Uncommitted) != 1 {
			t.Fatalf("index response = %+v, want one repository with committed and uncommitted files", resp)
		}
	})

	t.Run("committed patch forwards selector", func(t *testing.T) {
		t.Parallel()

		s, backend := newTaskDiffTestRouter(t)
		w := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/tasks/t1/diff/file?repository=0&commit="+diffTestCommit+"&path=committed.go&originalPath=", nil)
		testTaskHandlers(s).routes().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var resp v1.FileDiffResp
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp.Diff != "committed patch" {
			t.Fatalf("diff = %q, want committed patch", resp.Diff)
		}
		want := fileDiffCall{repository: 0, commit: diffTestCommit, path: "committed.go", originalPath: ""}
		if len(backend.fileDiffCalls) != 1 || backend.fileDiffCalls[0] != want {
			t.Fatalf("FileDiff calls = %+v, want [%+v]", backend.fileDiffCalls, want)
		}
	})

	t.Run("uncommitted patch forwards selector", func(t *testing.T) {
		t.Parallel()

		s, backend := newTaskDiffTestRouter(t)
		w := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/tasks/t1/diff/file?repository=0&commit=&path=renamed.go&originalPath=old.go", nil)
		testTaskHandlers(s).routes().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var resp v1.FileDiffResp
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp.Diff != "uncommitted patch" {
			t.Fatalf("diff = %q, want uncommitted patch", resp.Diff)
		}
		want := fileDiffCall{repository: 0, commit: "", path: "renamed.go", originalPath: "old.go"}
		if len(backend.fileDiffCalls) != 1 || backend.fileDiffCalls[0] != want {
			t.Fatalf("FileDiff calls = %+v, want [%+v]", backend.fileDiffCalls, want)
		}
	})

	t.Run("combined endpoint remains compatible", func(t *testing.T) {
		t.Parallel()

		s, backend := newTaskDiffTestRouter(t)
		w := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/tasks/t1/diff?path=", nil)
		testTaskHandlers(s).routes().ServeHTTP(w, r)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var resp v1.DiffResp
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Repositories) != 1 || resp.Repositories[0].Commits[0].Stat[0].Diff != "committed patch" || resp.Repositories[0].Uncommitted[0].Diff != "uncommitted patch" {
			t.Fatalf("combined diff response = %+v", resp)
		}
		if len(backend.fileDiffCalls) != 2 {
			t.Fatalf("FileDiff calls = %+v, want two", backend.fileDiffCalls)
		}
	})

	t.Run("rejects invalid selectors", func(t *testing.T) {
		t.Parallel()

		s, backend := newTaskDiffTestRouter(t)
		h := testTaskHandlers(s).routes()
		urls := []string{
			"/tasks/t1/diff/file?repository=bad&commit=&path=file.go&originalPath=",
			"/tasks/t1/diff/file?repository=-1&commit=&path=file.go&originalPath=",
			"/tasks/t1/diff/file?repository=0&commit=&path=&originalPath=",
			"/tasks/t1/diff/file?repository=0&commit=main&path=file.go&originalPath=",
			"/tasks/t1/diff/file?repository=1&commit=&path=file.go&originalPath=",
		}
		for _, rawURL := range urls {
			w := httptest.NewRecorder()
			r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, rawURL, nil)
			h.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s: status = %d, want %d", rawURL, w.Code, http.StatusBadRequest)
			}
		}
		if len(backend.fileDiffCalls) != 0 {
			t.Fatalf("FileDiff calls = %d, want none", len(backend.fileDiffCalls))
		}
	})
}

func TestTaskHandlersVNCWithoutDisplay(t *testing.T) {
	t.Parallel()

	s := newTestRouter(t, nil)
	insertTestTask(s, "t1", mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, ""))
	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/tasks/t1/vnc/ws", nil)
	r.SetPathValue("id", "t1")
	s.taskHandlers.handleVNCWebSocket(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusConflict)
	}
	err := decodeError(t, w)
	if err.Code != api.CodeConflict {
		t.Errorf("code = %q, want %q", err.Code, api.CodeConflict)
	}
}

func TestTaskHandlersHandoff(t *testing.T) {
	t.Parallel()

	s := newTestRouter(t, nil)
	tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "finish the feature"}, "")
	tk.SeedTimeline([]agent.Message{
		&agent.UserInputMessage{Text: "finish the feature"},
		&agent.TextMessage{Text: "The API remains to be connected."},
	})
	insertTestTask(s, "t1", tk)
	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/tasks/t1/handoff", nil)
	s.taskHandlers.routes().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp v1.TaskHandoffResp
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Continue this task in a new agent", "finish the feature", "The API remains to be connected."} {
		if !strings.Contains(resp.Prompt, want) {
			t.Errorf("prompt does not contain %q:\n%s", want, resp.Prompt)
		}
	}
}

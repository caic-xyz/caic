// Tests task HTTP preconditions, queued attachment access, diffs, and card-summary refreshes.

package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	logv3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/repo"
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

func newTaskDiffTestRouter(t *testing.T) (*testRouter, *diffRuntimeBackend, ksid.ID) {
	backend := &diffRuntimeBackend{FakeBackend: &runtimetest.FakeBackend{
		RepositoryStatusValue: runtime.RepositoryStatus{
			Branch:   "caic-1",
			Upstream: "origin/main",
			Ahead:    1,
			Behind:   1,
			Commits: []runtime.GitCommit{{
				SHA:          diffTestCommit,
				Subject:      "Add API",
				AuthoredDate: "2026-09-15",
				Stat:         []runtime.GitFileStat{{Path: "committed.go", LinesAdded: 4, LinesDeleted: 1}},
			}, {
				SHA:          "abcdef1234567890abcdef1234567890abcdef12",
				Subject:      "Missing upstream change",
				AuthoredDate: "2026-09-16",
				Behind:       true,
				Stat:         []runtime.GitFileStat{{Path: "upstream.go", LinesAdded: 1}},
			}},
			Uncommitted: []runtime.GitFileStatus{{Path: "renamed.go", OriginalPath: "old.go", WorktreeStatus: "R", LinesAdded: 2}},
		},
	}}
	s := newTestRouter(t, nil)
	testTaskHandlers(s).taskSvc.runtimes = newTestRuntime(t, backend)
	registerRouterCheckout(t, s.checkouts, "repo", newRouterTestCheckout(t.TempDir()))
	tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude)
	tk.Repos = []taskslog.RepoMount{{Name: "repo", Branch: "caic-1", ContainerPath: "/workspace/repo"}}
	tk.SetRuntimeConnectionInfo("test-runtime:ctr", runtime.ConnectionTarget{SSHHost: "ctr"}, "", "", 0)
	insertTestTask(s, tk.ID, tk)
	return s, backend, tk.ID
}

func TestTaskDiffHandlers(t *testing.T) {
	t.Parallel()

	t.Run("index omits patch reads", func(t *testing.T) {
		t.Parallel()

		s, backend, taskID := newTaskDiffTestRouter(t)
		w := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/tasks/"+taskID.String()+"/diff/index", nil)
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
		if len(resp.Repositories) != 1 || len(resp.Repositories[0].Commits) != 2 || len(resp.Repositories[0].Uncommitted) != 1 {
			t.Fatalf("index response = %+v, want one repository with committed and uncommitted files", resp)
		}
		commits := resp.Repositories[0].Commits
		if commits[0].Behind || !commits[1].Behind || commits[1].Subject != "Missing upstream change" {
			t.Fatalf("index commits = %+v, want ahead and missing upstream commits", commits)
		}
	})

	t.Run("committed patch forwards selector", func(t *testing.T) {
		t.Parallel()

		s, backend, taskID := newTaskDiffTestRouter(t)
		w := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/tasks/"+taskID.String()+"/diff/file?repository=0&commit="+diffTestCommit+"&path=committed.go&originalPath=", nil)
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

		s, backend, taskID := newTaskDiffTestRouter(t)
		w := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/tasks/"+taskID.String()+"/diff/file?repository=0&commit=&path=renamed.go&originalPath=old.go", nil)
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

		s, backend, taskID := newTaskDiffTestRouter(t)
		w := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/tasks/"+taskID.String()+"/diff?path=", nil)
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
		commits := resp.Repositories[0].Commits
		if len(commits) != 2 || commits[0].Behind || !commits[1].Behind || commits[1].Stat[0].Diff != "committed patch" {
			t.Fatalf("combined commits = %+v, want ahead and missing upstream commits with patches", commits)
		}
		if len(backend.fileDiffCalls) != 3 {
			t.Fatalf("FileDiff calls = %+v, want three", backend.fileDiffCalls)
		}
	})

	t.Run("compact repository status omits patch reads", func(t *testing.T) {
		t.Parallel()
		s, backend, taskID := newTaskDiffTestRouter(t)
		backend.RepositoryStatusValue.DiffStat = []runtime.GitFileStat{
			{Path: "committed.go", LinesAdded: 4, LinesDeleted: 1},
			{Path: "renamed.go", LinesAdded: 2},
		}
		backend.RepositoryStatusValue.Uncommitted[0].IndexStatus = "U"
		w := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/tasks/"+taskID.String()+"/repo-status", nil)
		testTaskHandlers(s).routes().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		var got v1.TaskRepoStatusResp
		if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		want := v1.TaskRepoStatusResp{Repositories: []v1.GitRepositoryState{{
			Name: "repo", Branch: "caic-1", Ahead: 1, Behind: 1,
			ChangedFiles: 2, LinesAdded: 6, LinesDeleted: 1, UncommittedFiles: 1, Conflicts: 1,
		}}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("status = %+v, want %+v", got, want)
		}
		if len(backend.fileDiffCalls) != 0 {
			t.Fatalf("FileDiff calls = %+v, want none", backend.fileDiffCalls)
		}
	})

	t.Run("rejects invalid selectors", func(t *testing.T) {
		t.Parallel()

		s, backend, taskID := newTaskDiffTestRouter(t)
		h := testTaskHandlers(s).routes()
		urls := []string{
			"/tasks/" + taskID.String() + "/diff/file?repository=bad&commit=&path=file.go&originalPath=",
			"/tasks/" + taskID.String() + "/diff/file?repository=-1&commit=&path=file.go&originalPath=",
			"/tasks/" + taskID.String() + "/diff/file?repository=0&commit=&path=&originalPath=",
			"/tasks/" + taskID.String() + "/diff/file?repository=0&commit=main&path=file.go&originalPath=",
			"/tasks/" + taskID.String() + "/diff/file?repository=1&commit=&path=file.go&originalPath=",
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

func TestTaskHandlersMissingTask(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"invalid-id", ksid.NewID().String()} {
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			s := newTestRouter(t, nil)
			for _, endpoint := range []string{"input", "purge"} {
				t.Run(endpoint, func(t *testing.T) {
					w := httptest.NewRecorder()
					r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/tasks/"+id+"/"+endpoint, strings.NewReader(`{"prompt":{"text":"hello"}}`))
					testTaskHandlers(s).routes().ServeHTTP(w, r)
					if w.Code != http.StatusNotFound {
						t.Fatalf("status = %d: %s", w.Code, w.Body.String())
					}
					if e := decodeError(t, w); e.Code != api.CodeNotFound {
						t.Fatalf("error = %+v, want NOT_FOUND", e)
					}
				})
			}
		})
	}
}

func TestTaskHandlersVNCWithoutDisplay(t *testing.T) {
	t.Parallel()

	s := newTestRouter(t, nil)
	tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude)
	insertTestTask(s, tk.ID, tk)
	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/tasks/t1/vnc/ws", nil)
	r.SetPathValue("id", tk.ID.String())
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
	insertTestTask(s, tk.ID, tk)
	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/tasks/"+tk.ID.String()+"/handoff", nil)
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

func TestGitReadsRefreshTaskSummary(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{"diff/index", "diff", "repo-status"} {
		t.Run(endpoint, func(t *testing.T) {
			t.Parallel()
			s, backend, taskID := newTaskDiffTestRouter(t)
			mgr := testTaskHandlers(s).taskSvc.taskMgr
			entry, _ := mgr.GetEntry(taskID)
			tk := entry.Task()
			tk.SetLiveRepositorySummary(&repo.GitSnapshot{
				Read:       repo.NewGitRead(tk.RuntimeInstanceID()),
				DiffStat:   logv3.DiffStat{{Path: "stale.go", LinesAdded: 273, LinesDeleted: 108}},
				RepoStates: []logv3.RepoState{{Branch: "caic-1", Ahead: 1, ChangedFiles: 20}},
			})
			backend.RepositoryStatusValue = runtime.RepositoryStatus{
				Branch: "caic-1", Behind: 2,
				DiffStat: []runtime.GitFileStat{{Path: "fresh.go", LinesAdded: 4, LinesDeleted: 1}},
			}
			read := func() {
				w := httptest.NewRecorder()
				r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/tasks/"+taskID.String()+"/"+endpoint, nil)
				testTaskHandlers(s).routes().ServeHTTP(w, r)
				if w.Code != http.StatusOK {
					t.Fatalf("status = %d: %s", w.Code, w.Body.String())
				}
			}
			changed := mgr.Changed()
			read()
			select {
			case <-changed:
			default:
				t.Fatal("summary update did not notify task-list subscribers")
			}
			snap := tk.Snapshot()
			if len(snap.DiffStat) != 1 || snap.DiffStat[0].Path != "fresh.go" || len(snap.RepoStates) != 1 || snap.RepoStates[0].Behind != 2 || snap.RepoStates[0].LinesAdded != 4 || snap.RepoStates[0].Ahead != 0 {
				t.Fatalf("fresh snapshot = %+v, %+v", snap.DiffStat, snap.RepoStates)
			}
			changed = mgr.Changed()
			read()
			select {
			case <-changed:
				t.Fatal("unchanged snapshot notified subscribers again")
			default:
			}
			backend.RepositoryStatusValue = runtime.RepositoryStatus{Branch: "caic-1"}
			read()
			select {
			case <-changed:
			default:
				t.Fatal("empty snapshot did not notify subscribers")
			}
			snap = tk.Snapshot()
			if len(snap.DiffStat) != 0 || len(snap.RepoStates) != 1 || snap.RepoStates[0] != (logv3.RepoState{Branch: "caic-1"}) {
				t.Fatalf("clean snapshot kept stale stats: %+v, %+v", snap.DiffStat, snap.RepoStates)
			}
		})
	}
}

func TestTaskHandlers(t *testing.T) {
	t.Parallel()
	t.Run("QueuedInputImage", func(t *testing.T) {
		t.Run("thumbnail_and_original", func(t *testing.T) {
			t.Parallel()
			s := newTestRouter(t, nil)
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "initial"}, harness.Claude)
			tk.SetState(taskslog.StateAsking)
			insertTestTask(s, tk.ID, tk)
			entry, _ := s.taskMgr.GetEntry(tk.ID)
			data := testJPEG(t, 960, 480)
			if _, err := entry.Lifecycle.QueueInput(t.Context(), agent.Prompt{Text: "next", Images: []logv3.ImageData{{MediaType: "image/jpeg", Data: base64.StdEncoding.EncodeToString(data)}}}); err != nil {
				t.Fatal(err)
			}
			apiTask, err := testTaskHandlers(s).taskSvc.getTask(t.Context(), entry, &api.EmptyReq{})
			if err != nil {
				t.Fatal(err)
			}
			if apiTask.PendingInput == nil || len(apiTask.PendingInput.Images) != 1 {
				t.Fatalf("pending input = %+v", apiTask.PendingInput)
			}
			attachment := apiTask.PendingInput.Images[0]
			for _, tc := range []struct {
				url   string
				width int
			}{{attachment.URL, 960}, {attachment.ThumbnailURL, 96}} {
				w := httptest.NewRecorder()
				req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, strings.TrimPrefix(tc.url, "/api/caic/v1"), nil)
				testTaskHandlers(s).routes().ServeHTTP(w, req)
				if w.Code != http.StatusOK {
					t.Fatalf("image status = %d: %s", w.Code, w.Body.String())
				}
				config, _, err := image.DecodeConfig(bytes.NewReader(w.Body.Bytes()))
				if err != nil || config.Width != tc.width || config.Height != tc.width/2 {
					t.Fatalf("image size = %+v, err = %v", config, err)
				}
				if tc.width == 960 && !bytes.Equal(w.Body.Bytes(), data) {
					t.Fatal("original image bytes changed")
				}
			}
		})
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				t.Run(method, func(t *testing.T) {
					t.Parallel()
					s := newTestRouter(t, nil)
					tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "initial"}, "")
					tk.SetState(taskslog.StateAsking)
					insertTestTask(s, tk.ID, tk)
					entry, _ := s.taskMgr.GetEntry(tk.ID)
					data := bytes.Repeat([]byte("queued attachment bytes"), 1000)
					if _, err := entry.Lifecycle.QueueInput(t.Context(), agent.Prompt{Images: []logv3.ImageData{{MediaType: "image/png", Data: base64.StdEncoding.EncodeToString(data)}}}); err != nil {
						t.Fatal(err)
					}
					inputID := entry.PendingInput().ID
					req := httptest.NewRequestWithContext(testHTTPContext(t), method, "/tasks/"+tk.ID.String()+"/input/"+inputID.String()+"/images/0", nil)
					w := httptest.NewRecorder()
					testTaskHandlers(s).routes().ServeHTTP(w, req)
					if w.Code != 200 {
						t.Fatalf("status=%d: %s", w.Code, w.Body.String())
					}
					if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
						t.Fatalf("unsafe headers: %v", w.Header())
					}
					if method == http.MethodHead {
						if w.Body.Len() != 0 {
							t.Fatal("HEAD sent image payload")
						}
					} else if !bytes.Equal(w.Body.Bytes(), data) {
						t.Fatal("attachment bytes changed")
					}
				})
			}
		})

		t.Parallel()
		t.Run("error", func(t *testing.T) {
			for _, tc := range []struct {
				name         string
				index        string
				method       string
				foreignOwner bool
				otherTask    bool
				cancel       bool
				corrupt      bool
				mediaType    string
				want         int
			}{
				{name: "foreign owner", index: "0", foreignOwner: true, want: 403},
				{name: "other task", index: "0", otherTask: true, want: 404},
				{name: "canceled", index: "0", cancel: true, want: 404},
				{name: "negative index", index: "-1", want: 404},
				{name: "missing index", index: "1", want: 404},
				{name: "invalid index", index: "no", want: 404},
				{name: "invalid encoding", index: "0", corrupt: true, want: 500},
				{name: "unsafe content type", index: "0", mediaType: "text/html", want: 415},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					s := newTestRouter(t, nil)
					tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "initial"}, "")
					tk.OwnerID = "owner"
					tk.SetState(taskslog.StateAsking)
					insertTestTask(s, tk.ID, tk)
					entry, _ := s.taskMgr.GetEntry(tk.ID)
					// Exercise both the initial read and the streamed continuation.
					data := bytes.Repeat([]byte("queued attachment bytes"), 1000)
					mediaType := tc.mediaType
					if mediaType == "" {
						mediaType = "image/png"
					}
					encoded := base64.StdEncoding.EncodeToString(data)
					if tc.corrupt {
						encoded = "invalid!"
					}
					if _, err := entry.Lifecycle.QueueInput(t.Context(), agent.Prompt{Images: []logv3.ImageData{{MediaType: mediaType, Data: encoded}}}); err != nil {
						t.Fatal(err)
					}
					inputID := entry.PendingInput().ID
					if tc.cancel {
						if err := entry.Lifecycle.CancelQueuedInput(inputID); err != nil {
							t.Fatal(err)
						}
					}
					taskID := tk.ID
					if tc.otherTask {
						other := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "other"}, "")
						other.OwnerID = "owner"
						insertTestTask(s, other.ID, other)
						taskID = other.ID
					}
					userID := "owner"
					if tc.foreignOwner {
						userID = "other"
					}
					ctx := auth.NewContext(testHTTPContext(t), &auth.User{ID: userID})
					method := tc.method
					if method == "" {
						method = http.MethodGet
					}
					req := httptest.NewRequestWithContext(ctx, method, "/tasks/"+taskID.String()+"/input/"+inputID.String()+"/images/"+tc.index, nil)
					w := httptest.NewRecorder()
					testTaskHandlers(s).routes().ServeHTTP(w, req)
					if w.Code != tc.want {
						t.Fatalf("status = %d, want %d: %s", w.Code, tc.want, w.Body.String())
					}
					if w.Header().Get("Cache-Control") != "no-store" {
						t.Fatal("attachment may be cached across accounts or cancellation")
					}
				})
			}
		})
	})
}

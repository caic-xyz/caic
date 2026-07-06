// Tests for Runner: task lifecycle orchestration through Start, Cleanup,
// StopTask, ReviveTask, and ForkTask.

package task

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/harness"
	"github.com/caic-xyz/caic/backend/internal/repowork"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// instantExitBackend embeds testBackend but spawns a process that exits
// immediately (no stdin read), so tests exercising EnsureSession's
// already-done branch don't have to wait out its 10-second liveness timer.
type instantExitBackend struct {
	testBackend
}

func newTestRepoWorkspace(baseBranch, dir string, backend runtime.Backend) *repowork.RepoWorkspace {
	workspace, err := repowork.NewRepoWorkspace(baseBranch, dir, filepath.Base(dir), time.Minute, backend, slog.With("repo", "test"))
	if err != nil {
		panic(err)
	}
	return workspace
}

func (b *instantExitBackend) Start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	b.capturedCtx = ctx
	b.capturedOpts = *opts
	cmd := exec.CommandContext(ctx, "true")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return agent.NewSession(cmd, agent.NewConn(stdin, opts.LogW, &testWire{parse: claudecode.New().NewWire().ParseMessage}), stdout, opts.MsgCh, nil), nil
}

func TestRunner(t *testing.T) {
	t.Parallel()

	t.Run("MakeMetadata", func(t *testing.T) {
		t.Parallel()
		t.Run("Basic", func(t *testing.T) {
			t.Parallel()
			tk := &Task{
				ID:      ksid.NewID(),
				Harness: harness.Claude,
			}
			metadata := MakeMetadata(tk)
			if len(metadata) != 3 {
				t.Fatalf("len = %d, want 3", len(metadata))
			}
			if metadata[runtime.MetadataTaskID] != tk.ID.String() {
				t.Errorf("metadata[%s] = %q", runtime.MetadataTaskID, metadata[runtime.MetadataTaskID])
			}
			if metadata[runtime.MetadataLegacyTaskID] != tk.ID.String() {
				t.Errorf("metadata[%s] = %q", runtime.MetadataLegacyTaskID, metadata[runtime.MetadataLegacyTaskID])
			}
			if metadata[runtime.MetadataHarness] != string(tk.Harness) {
				t.Errorf("metadata[%s] = %q", runtime.MetadataHarness, metadata[runtime.MetadataHarness])
			}
		})
		t.Run("WithGitHubToken", func(t *testing.T) {
			t.Parallel()
			tk := &Task{
				ID:          ksid.NewID(),
				Harness:     harness.Claude,
				GitHubToken: true,
			}
			metadata := MakeMetadata(tk)
			if len(metadata) != 4 {
				t.Fatalf("len = %d, want 4", len(metadata))
			}
			if metadata[runtime.MetadataGitHubToken] != "true" {
				t.Errorf("metadata[%s] = %q, want true", runtime.MetadataGitHubToken, metadata[runtime.MetadataGitHubToken])
			}
		})
	})

	t.Run("ProvisioningWriter", func(t *testing.T) {
		t.Parallel()
		tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
		_, ch, unsub := tk.Subscribe(t.Context())
		t.Cleanup(unsub)

		w := &provisioningWriter{ctx: t.Context(), t: tk}

		n, err := w.Write([]byte("hel"))
		if err != nil {
			t.Fatal(err)
		}
		_ = n
		select {
		case <-ch:
			t.Fatal("unexpected message for partial line")
		default:
		}

		_, err = w.Write([]byte("lo\n"))
		if err != nil {
			t.Fatal(err)
		}
		msg := recvMsg(t, ch)
		lm, ok := msg.(*agent.LogMessage)
		if !ok {
			t.Fatalf("expected *agent.LogMessage, got %T", msg)
		}
		if lm.Line != "hello" {
			t.Errorf("LogMessage.Line = %q, want %q", lm.Line, "hello")
		}

		_, err = w.Write([]byte("\n\n"))
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-ch:
			t.Fatal("unexpected message for empty lines")
		default:
		}

		_, err = w.Write([]byte("line1\nline2\n"))
		if err != nil {
			t.Fatal(err)
		}
		msg1 := recvMsg(t, ch)
		if lm, ok := msg1.(*agent.LogMessage); !ok || lm.Line != "line1" {
			t.Errorf("msg1 = %+v", msg1)
		}
		msg2 := recvMsg(t, ch)
		if lm, ok := msg2.(*agent.LogMessage); !ok || lm.Line != "line2" {
			t.Errorf("msg2 = %+v", msg2)
		}
	})

	t.Run("Start", func(t *testing.T) {
		t.Parallel()
		t.Run("PassesModelAndEffort", func(t *testing.T) {
			t.Parallel()
			backend := &testBackend{}
			r := &Runner{
				Workspace: newTestRepoWorkspace("", "", &stubContainer{}),
				Sessions: &SessionRunner{
					Backends: map[harness.Name]agent.Backend{"test": backend},
					Logs:     &LogStore{LogDir: t.TempDir()},
				},
			}
			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Harness:       "test",
				Model:         "model-1",
				Effort:        "high",
				StartedAt:     time.Now().UTC(),
			}
			h, err := r.Start(t.Context(), tk, "")
			if err != nil {
				t.Fatal(err)
			}
			stopCtx, stopCancel := context.WithTimeout(t.Context(), time.Second)
			_ = h.Session.Stop(stopCtx)
			stopCancel()
			h.CloseMsgCh()
			<-h.DispatchDone
			_ = h.LogW.Close()

			if backend.capturedOpts.Model != "model-1" {
				t.Errorf("Model = %q, want model-1", backend.capturedOpts.Model)
			}
			if backend.capturedOpts.Effort != "high" {
				t.Errorf("Effort = %q, want high", backend.capturedOpts.Effort)
			}
		})
		t.Run("SurfacesSetupFailure", func(t *testing.T) {
			t.Parallel()
			launchErr := errors.New("invalid context name cache-custom-mount:~/.cache/caic: invalid reference format")
			r := &Runner{
				Workspace: newTestRepoWorkspace("", "", &stubContainer{launchErr: launchErr}),
				Sessions:  &SessionRunner{},
			}
			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Harness:       "test",
				StartedAt:     time.Now().UTC(),
			}
			_, ch, unsub := tk.Subscribe(t.Context())
			t.Cleanup(unsub)

			if _, err := r.Start(t.Context(), tk, ""); err == nil {
				t.Fatal("want launch error")
			}
			if got := tk.GetState(); got != StateFailed {
				t.Errorf("state = %v, want %v", got, StateFailed)
			}
			msg := recvMsg(t, ch)
			lm, ok := msg.(*agent.LogMessage)
			if !ok {
				t.Fatalf("message = %T, want *agent.LogMessage", msg)
			}
			if !strings.Contains(lm.Line, launchErr.Error()) {
				t.Errorf("log line = %q, want launch error", lm.Line)
			}
		})
	})

	t.Run("Setup", func(t *testing.T) {
		t.Parallel()
		t.Run("CustomBaseBranch", func(t *testing.T) {
			t.Parallel()
			// Verify that setup creates the task branch from t.BaseBranch
			// when it differs from the workspace's default BaseBranch.
			clone := initTestRepo(t, "main")
			// Create a feature branch on origin with a distinct commit.
			runGit(t, clone, "checkout", "-b", "feature")
			if err := os.WriteFile(filepath.Join(clone, "feature.txt"), []byte("feat\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, clone, "add", ".")
			runGit(t, clone, "commit", "-m", "feature commit")
			runGit(t, clone, "push", "origin", "feature")
			runGit(t, clone, "checkout", "main")

			logDir := t.TempDir()
			stub := &stubContainer{}
			r := &Runner{
				Workspace: newTestRepoWorkspace("main", clone, stub),
				Sessions:  &SessionRunner{Logs: &LogStore{LogDir: logDir}},
			}
			r.initDefaults()

			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []RepoMount{{Name: "org/repo", BaseBranch: "feature"}},
				Harness:       harness.Claude,
			}

			if _, err := r.setup(t.Context(), tk, nil, ""); err != nil {
				t.Fatal(err)
			}

			// The task branch must contain the feature commit (feature.txt).
			branch := tk.Primary().Branch
			out, execErr := exec.CommandContext(t.Context(), "git", "-C", clone, "show", branch+":feature.txt").Output() //nolint:gosec // controlled test args
			if execErr != nil {
				t.Fatalf("feature.txt not in task branch %s: %v", branch, execErr)
			}
			if string(out) != "feat\n" {
				t.Errorf("feature.txt content = %q, want %q", string(out), "feat\n")
			}
		})
		t.Run("LocalOnlyBaseBranch", func(t *testing.T) {
			t.Parallel()
			// Verify that setup works when BaseBranch exists only locally
			// (not pushed to origin).
			clone := initTestRepo(t, "main")
			runGit(t, clone, "checkout", "-b", "local-only")
			if err := os.WriteFile(filepath.Join(clone, "local.txt"), []byte("local\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, clone, "add", ".")
			runGit(t, clone, "commit", "-m", "local commit")
			// Do NOT push to origin — branch exists only locally.
			runGit(t, clone, "checkout", "main")

			logDir := t.TempDir()
			stub := &stubContainer{}
			r := &Runner{
				Workspace: newTestRepoWorkspace("main", clone, stub),
				Sessions:  &SessionRunner{Logs: &LogStore{LogDir: logDir}},
			}
			r.initDefaults()

			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []RepoMount{{Name: "org/repo", BaseBranch: "local-only"}},
				Harness:       harness.Claude,
			}

			if _, err := r.setup(t.Context(), tk, nil, ""); err != nil {
				t.Fatal(err)
			}

			// The task branch must contain the local commit (local.txt).
			branch := tk.Primary().Branch
			out, execErr := exec.CommandContext(t.Context(), "git", "-C", clone, "show", branch+":local.txt").Output() //nolint:gosec // controlled test args
			if execErr != nil {
				t.Fatalf("local.txt not in task branch %s: %v", branch, execErr)
			}
			if string(out) != "local\n" {
				t.Errorf("local.txt content = %q, want %q", string(out), "local\n")
			}
		})
	})

	t.Run("Cleanup", func(t *testing.T) {
		t.Parallel()
		t.Run("NoSessionUsesLiveStats", func(t *testing.T) {
			t.Parallel()
			// Simulate an adopted task after server restart: no active session, but
			// live stats were restored from log messages. Cleanup should fall back to
			// LiveStats for the result cost.
			clone := initTestRepo(t, "main")
			r := &Runner{
				Workspace: newTestRepoWorkspace("main", clone, nil),
				Sessions:  &SessionRunner{},
			}

			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []RepoMount{{Name: "org/repo", Branch: "main"}},
			}
			tk.SetState(StateRunning)

			// Restore messages with cost info (simulates RestoreMessages from logs).
			tk.RestoreMessages([]agent.Message{
				&agent.ResultMessage{
					MessageType:  "result",
					TotalCostUSD: 0.42,
					NumTurns:     5,
					DurationMs:   12345,
				},
			})

			result := r.Cleanup(t.Context(), tk, StatePurged)
			if result.State != StatePurged {
				t.Errorf("state = %v, want %v", result.State, StatePurged)
			}
			if result.CostUSD != 0.42 {
				t.Errorf("CostUSD = %f, want 0.42", result.CostUSD)
			}
			if result.NumTurns != 5 {
				t.Errorf("NumTurns = %d, want 5", result.NumTurns)
			}
			if result.Duration != 12345*time.Millisecond {
				t.Errorf("Duration = %v, want %v", result.Duration, 12345*time.Millisecond)
			}
		})

		t.Run("StoppedTaskWritesTrailer", func(t *testing.T) {
			t.Parallel()
			// When a stopped task (no session handle) is purged, Cleanup must
			// reopen the existing log and write a caic_result trailer so the
			// task loads as "purged" (not "failed") on the next server restart.
			logDir := t.TempDir()
			clone := initTestRepo(t, "main")
			r := &Runner{
				Workspace: newTestRepoWorkspace("main", clone, nil),
				Sessions:  &SessionRunner{Logs: &LogStore{LogDir: logDir}},
			}

			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []RepoMount{{Name: "org/repo", Branch: "caic-0"}},
				Harness:       harness.Claude,
			}
			tk.SetState(StateStopped)

			// Create a pre-existing log file (written by StopTask scenario).
			logW, err := r.Sessions.Logs.Open(tk)
			if err != nil {
				t.Fatal(err)
			}
			_ = logW.Close()

			// Purge the stopped task — should reopen the log and write the trailer.
			r.Cleanup(t.Context(), tk, StatePurged)

			plainPath := filepath.Join(logDir, tk.ID.String()+"-org-repo-caic-0.jsonl")
			if _, err := os.Stat(plainPath); !os.IsNotExist(err) {
				t.Fatalf("plain log stat err = %v, want os.ErrNotExist", err)
			}
			if _, err := os.Stat(plainPath + ".zst"); err != nil {
				t.Fatal(err)
			}

			// Load the log and verify the trailer was written.
			lt, err := LoadLogs(logDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(lt) != 1 {
				t.Fatalf("LoadLogs returned %d tasks, want 1", len(lt))
			}
			if lt[0].State != StatePurged {
				t.Errorf("state = %v, want StatePurged", lt[0].State)
			}
			if lt[0].Result == nil {
				t.Fatal("Result is nil, want caic_result trailer")
			}
			if lt[0].Result.State != StatePurged {
				t.Errorf("Result.State = %v, want StatePurged", lt[0].Result.State)
			}
		})

		t.Run("UsesLiveDiffStat", func(t *testing.T) {
			t.Parallel()
			clone := initTestRepo(t, "main")
			r := &Runner{
				Workspace: newTestRepoWorkspace("main", clone, nil),
				Sessions:  &SessionRunner{},
			}

			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []RepoMount{{Name: "org/repo", Branch: "main"}},
			}
			tk.SetState(StateRunning)

			// Restore messages including a DiffStatMessage (simulates relay output).
			tk.RestoreMessages([]agent.Message{
				&agent.DiffStatMessage{
					MessageType: "caic_diff_stat",
					DiffStat: agent.DiffStat{
						{Path: "a.go", Added: 10, Deleted: 3},
						{Path: "b.go", Added: 5, Deleted: 0},
					},
				},
			})

			result := r.Cleanup(t.Context(), tk, StatePurged)
			if len(result.DiffStat) != 2 {
				t.Fatalf("DiffStat has %d entries, want 2", len(result.DiffStat))
			}
			if result.DiffStat[0].Path != "a.go" || result.DiffStat[0].Added != 10 {
				t.Errorf("DiffStat[0] = %+v, want {a.go 10 3}", result.DiffStat[0])
			}
		})
	})

	t.Run("StopTask", func(t *testing.T) {
		t.Parallel()
		for _, state := range []State{StatePurging, StatePurged, StateCrashed, StateFailed} {
			t.Run(state.String(), func(t *testing.T) {
				t.Parallel()
				stub := &stubContainer{}
				r := &Runner{
					Workspace: newTestRepoWorkspace("", "", stub),
					Sessions:  &SessionRunner{},
				}
				tk := &Task{
					ID:            ksid.NewID(),
					InitialPrompt: agent.Prompt{Text: "test"},
				}
				tk.SetRuntimeConnectionInfo("ctr-1", runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
				tk.SetState(state)

				r.StopTask(t.Context(), tk)

				if got := tk.GetState(); got != state {
					t.Errorf("state = %v, want %v", got, state)
				}
				if stub.stopped {
					t.Error("StopTask called backend Stop for terminal/cleanup state")
				}
			})
		}
	})

	t.Run("ReviveTask", func(t *testing.T) {
		t.Parallel()
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			t.Run("no_runtime", func(t *testing.T) {
				t.Parallel()
				r := &Runner{Workspace: newTestRepoWorkspace("", "", nil), Sessions: &SessionRunner{}}
				tk := &Task{ID: ksid.NewID()}
				if _, err := r.ReviveTask(t.Context(), tk); err == nil {
					t.Fatal("want error")
				}
			})
			t.Run("no_instance", func(t *testing.T) {
				t.Parallel()
				r := &Runner{Workspace: newTestRepoWorkspace("", "", &stubContainer{}), Sessions: &SessionRunner{}}
				tk := &Task{ID: ksid.NewID()}
				if _, err := r.ReviveTask(t.Context(), tk); err == nil {
					t.Fatal("want error")
				}
			})
			t.Run("wrong_state", func(t *testing.T) {
				t.Parallel()
				r := &Runner{Workspace: newTestRepoWorkspace("", "", &stubContainer{}), Sessions: &SessionRunner{}}
				tk := &Task{ID: ksid.NewID()}
				tk.SetRuntimeConnectionInfo("ctr-1", runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
				tk.SetState(StateRunning)
				if _, err := r.ReviveTask(t.Context(), tk); err == nil {
					t.Fatal("want error")
				}
			})
		})
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			ws := newTestRepoWorkspace("", "", &stubContainer{})
			backend := &instantExitBackend{}
			r := &Runner{
				Workspace: ws,
				Sessions: &SessionRunner{
					Backends:  map[harness.Name]agent.Backend{"test": backend},
					Workspace: ws,
					Logs:      &LogStore{LogDir: t.TempDir()},
				},
			}
			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Harness:       "test",
			}
			tk.SetRuntimeConnectionInfo("ctr-1", runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(StateStopped)

			h, err := r.ReviveTask(t.Context(), tk)
			if err != nil {
				t.Fatal(err)
			}
			if h == nil {
				t.Fatal("ReviveTask returned nil handle")
			}
			// The instantly-exiting session makes EnsureSession replace it with a
			// fresh idle relay; the task is left waiting for new input.
			if got := tk.GetState(); got != StateWaiting {
				t.Errorf("state = %v, want %v", got, StateWaiting)
			}
			tk.CloseAndDetachSession(t.Context())
		})
	})

	t.Run("ForkTask", func(t *testing.T) {
		t.Parallel()
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			t.Run("no_runtime", func(t *testing.T) {
				t.Parallel()
				r := &Runner{Workspace: newTestRepoWorkspace("", "", nil), Sessions: &SessionRunner{}}
				source := &Task{ID: ksid.NewID()}
				fork := &Task{ID: ksid.NewID()}
				if _, err := r.ForkTask(t.Context(), source, fork, &runtime.ForkOptions{}, ""); err == nil {
					t.Fatal("want error")
				}
			})
			t.Run("no_source_instance", func(t *testing.T) {
				t.Parallel()
				r := &Runner{Workspace: newTestRepoWorkspace("", "", &stubContainer{}), Sessions: &SessionRunner{}}
				source := &Task{ID: ksid.NewID()}
				fork := &Task{ID: ksid.NewID()}
				if _, err := r.ForkTask(t.Context(), source, fork, &runtime.ForkOptions{}, ""); err == nil {
					t.Fatal("want error")
				}
			})
		})
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			ws := newTestRepoWorkspace("", "", &stubContainer{})
			backend := &instantExitBackend{}
			r := &Runner{
				Workspace: ws,
				Sessions: &SessionRunner{
					Backends:  map[harness.Name]agent.Backend{"test": backend},
					Workspace: ws,
					Logs:      &LogStore{LogDir: t.TempDir()},
				},
			}
			source := &Task{ID: ksid.NewID(), Harness: "test"}
			source.SetRuntimeConnectionInfo("ctr-src", runtime.ConnectionTarget{SSHHost: "ctr-src"}, "", "", 0)
			fork := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "fork prompt"},
				Harness:       "test",
			}

			h, err := r.ForkTask(t.Context(), source, fork, &runtime.ForkOptions{}, "")
			if err != nil {
				t.Fatal(err)
			}
			if h == nil {
				t.Fatal("ForkTask returned nil handle")
			}
			if fork.RuntimeInstanceID() != "stub-fork" {
				t.Errorf("fork instance = %q, want stub-fork", fork.RuntimeInstanceID())
			}
			if got := fork.GetState(); got != StateRunning {
				t.Errorf("state = %v, want %v", got, StateRunning)
			}
			fork.CloseAndDetachSession(t.Context())
		})
	})
}

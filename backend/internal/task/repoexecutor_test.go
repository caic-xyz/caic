// Tests for the RepoExecutor: task execution and agent orchestration.

package task

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// testBackend implements agent.Backend for tests. It launches a process that
// reads one line from stdin then exits. capturedCtx records the context passed
// to Start so tests can assert context lifetime.
type testBackend struct {
	capturedCtx  context.Context
	capturedOpts agent.Options
}

func (b *testBackend) Harness() harness.Name { return "test" }

func (b *testBackend) Start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	b.capturedCtx = ctx
	b.capturedOpts = *opts
	// Read one line from stdin then exit. Session.Stop writes \x00\n which
	// satisfies the read, making Stop return immediately instead of timing out.
	cmd := exec.CommandContext(ctx, "python3", "-c", "input()")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return agent.NewSession(cmd, agent.NewConn(stdin, opts.LogW, &testWire{parse: claudecode.New().NewWire().ParseMessage}), stdout, opts.MsgCh, nil), nil
}

func (b *testBackend) AttachRelay(context.Context, *agent.Options) (*agent.Session, error) {
	return nil, errors.New("test backend does not support relay")
}

func (b *testBackend) ReadRelayOutput(context.Context, string) ([]agent.Message, int64, error) {
	return nil, 0, errors.New("test backend does not support relay")
}

func (b *testBackend) Models() []string   { return []string{"test-model"} }
func (b *testBackend) SetModels([]string) {}

// SupportsImages always returns false in the test backend.
func (b *testBackend) SupportsImages() bool { return false }

func (b *testBackend) AgentArgs(agent.HarnessArgs) []string { return nil }

func (b *testBackend) NewWire() agent.WireFormat { return claudecode.New() }

func (b *testBackend) SupportsCompact() bool { return false }

func (b *testBackend) ContextWindowLimit(string) int { return 180_000 }

type attachCaptureBackend struct {
	testBackend

	capturedAttachOpts agent.Options
}

func (b *attachCaptureBackend) AttachRelay(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	b.capturedAttachOpts = *opts
	cmd := exec.CommandContext(ctx, "python3", "-c", "import sys; sys.stdin.buffer.read()")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return agent.NewSession(cmd, agent.NewConn(stdin, opts.LogW, &testWire{parse: claudecode.New().NewWire().ParseMessage}), stdout, opts.MsgCh, nil), nil
}

// testWire implements agent.WireFormat for testing.
type testWire struct {
	parse func([]byte) ([]agent.Message, error)
}

func (*testWire) WritePrompt(w io.Writer, p agent.Prompt, logW io.Writer) error {
	msg := struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}{Type: "user"}
	msg.Message.Role = "user"
	msg.Message.Content = p.Text
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		return err
	}
	if logW != nil {
		_, _ = logW.Write(data)
	}
	return nil
}

func (w *testWire) ParseMessage(line []byte) ([]agent.Message, error) {
	return w.parse(line)
}

func TestRepoExecutor(t *testing.T) {
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
	t.Run("StartPassesModelAndEffort", func(t *testing.T) {
		t.Parallel()
		backend := &testBackend{}
		r := &RepoExecutor{
			LogDir:   t.TempDir(),
			Runtime:  &stubContainer{},
			Backends: map[harness.Name]agent.Backend{"test": backend},
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
	t.Run("StartSurfacesSetupFailure", func(t *testing.T) {
		t.Parallel()
		launchErr := errors.New("invalid context name cache-custom-mount:~/.cache/caic: invalid reference format")
		r := &RepoExecutor{
			Runtime: &stubContainer{launchErr: launchErr},
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
	t.Run("Init", func(t *testing.T) {
		t.Parallel()
		t.Run("Basic", func(t *testing.T) {
			t.Parallel()
			clone := initTestRepo(t, "main")
			r := &RepoExecutor{
				BaseBranch: "main",
				Dir:        clone,
			}
			if err := r.Init(t.Context()); err != nil {
				t.Fatal(err)
			}
			if r.nextID != 0 {
				t.Errorf("nextID = %d, want 0", r.nextID)
			}
		})
		t.Run("SkipsExisting", func(t *testing.T) {
			t.Parallel()
			clone := initTestRepo(t, "main")
			// Pre-create branches and push to remote.
			runGit(t, clone, "branch", "caic-0")
			runGit(t, clone, "push", "origin", "caic-0")
			runGit(t, clone, "branch", "caic-3")
			runGit(t, clone, "push", "origin", "caic-3")

			r := &RepoExecutor{
				BaseBranch: "main",
				Dir:        clone,
			}
			if err := r.Init(t.Context()); err != nil {
				t.Fatal(err)
			}
			if r.nextID != 4 {
				t.Errorf("nextID = %d, want 4", r.nextID)
			}
		})
		t.Run("SkipsLocalOnly", func(t *testing.T) {
			t.Parallel()
			// Local-only branches (e.g. from stopped tasks that were never
			// pushed) must also be accounted for.
			clone := initTestRepo(t, "main")
			runGit(t, clone, "branch", "caic-5")
			// Do NOT push — simulates a stopped task whose branch was
			// never synced to origin.

			r := &RepoExecutor{
				BaseBranch: "main",
				Dir:        clone,
			}
			if err := r.Init(t.Context()); err != nil {
				t.Fatal(err)
			}
			if r.nextID != 6 {
				t.Errorf("nextID = %d, want 6", r.nextID)
			}
		})
		t.Run("IgnoresNonCaicPrefix", func(t *testing.T) {
			t.Parallel()
			// Branches like "foo-caic-9" must not be matched.
			clone := initTestRepo(t, "main")
			runGit(t, clone, "branch", "foo-caic-9")
			runGit(t, clone, "branch", "caic-2")

			r := &RepoExecutor{
				BaseBranch: "main",
				Dir:        clone,
			}
			if err := r.Init(t.Context()); err != nil {
				t.Fatal(err)
			}
			if r.nextID != 3 {
				t.Errorf("nextID = %d, want 3", r.nextID)
			}
		})
	})

	t.Run("Setup", func(t *testing.T) {
		t.Parallel()
		t.Run("CustomBaseBranch", func(t *testing.T) {
			t.Parallel()
			// Verify that setup creates the task branch from t.BaseBranch
			// when it differs from the executor's default BaseBranch.
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
			r := &RepoExecutor{
				BaseBranch: "main",
				Dir:        clone,
				LogDir:     logDir,
				Runtime:    stub,
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
			r := &RepoExecutor{
				BaseBranch: "main",
				Dir:        clone,
				LogDir:     logDir,
				Runtime:    stub,
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
			r := &RepoExecutor{
				BaseBranch: "main",
				Dir:        clone,
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
			r := &RepoExecutor{
				BaseBranch: "main",
				Dir:        clone,
				LogDir:     logDir,
			}

			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []RepoMount{{Name: "org/repo", Branch: "caic-0"}},
				Harness:       harness.Claude,
			}
			tk.SetState(StateStopped)

			// Create a pre-existing log file (written by StopTask scenario).
			logW, err := r.logStore().Open(tk)
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
			r := &RepoExecutor{
				BaseBranch: "main",
				Dir:        clone,
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
				r := &RepoExecutor{Runtime: stub}
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

	t.Run("Reconnect", func(t *testing.T) {
		t.Parallel()
		t.Run("error_stateful_missing_session_id", func(t *testing.T) {
			t.Parallel()
			for _, harness := range []harness.Name{harness.Codex, harness.OpenCode} {
				t.Run(string(harness), func(t *testing.T) {
					t.Parallel()
					r := &RepoExecutor{}
					tk := &Task{
						ID:            ksid.NewID(),
						InitialPrompt: agent.Prompt{Text: "test"},
						Harness:       harness,
					}
					tk.SetRuntimeConnectionInfo("ctr-1", runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
					tk.SetState(StateRunning)
					_, err := r.Reconnect(t.Context(), tk, true)
					if err == nil {
						t.Fatal("Reconnect returned nil error")
					}
					if !strings.Contains(err.Error(), "session ID missing") {
						t.Fatalf("err = %v, want missing session ID", err)
					}
				})
			}
		})
		t.Run("passes_history_to_attach_backend", func(t *testing.T) {
			t.Parallel()
			backend := &attachCaptureBackend{}
			r := &RepoExecutor{
				LogDir:   filepath.Join(t.TempDir(), "logs"),
				Backends: map[harness.Name]agent.Backend{"test": backend},
			}
			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Harness:       "test",
			}
			tk.SetRuntimeConnectionInfo("ctr-1", runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(StateRunning)
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
			})

			h, err := r.Reconnect(t.Context(), tk, true)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = h.GracefulStop(t.Context(), time.Second)
				_ = h.Drain()
			})

			pending := backend.capturedAttachOpts.PendingUserActions
			if len(pending) != 1 {
				t.Fatalf("PendingUserActions len = %d, want 1", len(pending))
			}
			if pending[0].Kind != agent.PendingUserActionAskUserQuestion {
				t.Errorf("Kind = %q, want %q", pending[0].Kind, agent.PendingUserActionAskUserQuestion)
			}
			if pending[0].RequestID != "req-1" {
				t.Errorf("RequestID = %q, want req-1", pending[0].RequestID)
			}
			if len(pending[0].Ask.Questions) != 1 {
				t.Fatalf("Questions len = %d, want 1", len(pending[0].Ask.Questions))
			}
			if pending[0].Ask.Questions[0].Question != "Which?" {
				t.Errorf("question = %q, want Which?", pending[0].Ask.Questions[0].Question)
			}
		})
	})

	t.Run("LogStore", func(t *testing.T) {
		t.Parallel()
		t.Run("CreatesFile", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			logDir := filepath.Join(dir, "logs")
			r := &RepoExecutor{LogDir: logDir}
			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []RepoMount{{Name: "org/repo", Branch: "caic-0"}},
				Model:         "model-1",
				Effort:        "high",
			}
			w, err := r.logStore().Open(tk)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = w.Close() }()
			// Write something and close.
			_, _ = w.Write([]byte("test\n"))
			_ = w.Close()

			entries, err := os.ReadDir(logDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatalf("expected 1 file, got %d", len(entries))
			}
			name := entries[0].Name()
			want := tk.ID.String() + "-org-repo-caic-0.jsonl"
			if name != want {
				t.Errorf("filename = %q, want %q", name, want)
			}
			data, err := os.ReadFile(filepath.Join(logDir, name)) //nolint:gosec // path is test-controlled
			if err != nil {
				t.Fatal(err)
			}
			var meta map[string]any
			if err := json.Unmarshal(bytes.SplitN(data, []byte("\n"), 2)[0], &meta); err != nil {
				t.Fatal(err)
			}
			if meta["model"] != "model-1" {
				t.Errorf("meta model = %v, want model-1", meta["model"])
			}
			if meta["effort"] != "high" {
				t.Errorf("meta effort = %v, want high", meta["effort"])
			}
		})
		t.Run("ReopenNoDuplicateHeader", func(t *testing.T) {
			t.Parallel()
			// Reconnect appends via Reopen, which must not write a second
			// caic_meta header. Otherwise every server restart that re-adopts
			// a running instance duplicates the header.
			logDir := filepath.Join(t.TempDir(), "logs")
			r := &RepoExecutor{LogDir: logDir}
			tk := &Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}, Repos: []RepoMount{{Name: "org/repo", Branch: "caic-0"}}}

			// Initial Start writes the header.
			w, err := r.logStore().Open(tk)
			if err != nil {
				t.Fatal(err)
			}
			_ = w.Close()

			// Several reconnects (simulating repeated server restarts) append
			// without writing a new header.
			for range 3 {
				w, err := r.logStore().Reopen(tk)
				if err != nil {
					t.Fatal(err)
				}
				_ = w.Close()
			}

			name := tk.ID.String() + "-org-repo-caic-0.jsonl"
			data, err := os.ReadFile(filepath.Join(logDir, name)) //nolint:gosec // path is test-controlled
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(string(data), `"type":"caic_meta"`); got != 1 {
				t.Errorf("caic_meta count = %d, want 1", got)
			}
		})
		t.Run("ReopenMissingFile", func(t *testing.T) {
			t.Parallel()
			// Reopen must report os.ErrNotExist when no log exists yet so
			// Reconnect can fall back to Open (which writes the header).
			r := &RepoExecutor{LogDir: filepath.Join(t.TempDir(), "logs")}
			tk := &Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}, Repos: []RepoMount{{Name: "org/repo", Branch: "caic-0"}}}
			if _, err := r.logStore().Reopen(tk); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("Reopen err = %v, want os.ErrNotExist", err)
			}
		})
	})

	t.Run("RuntimeDir", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			dir  string
			task *Task
			want string
		}{
			{task: &Task{Repos: []RepoMount{{MountedPath: "~/src/caic-xyz/md"}}}, want: "/home/user/src/caic-xyz/md"},
			{dir: "/home/maruel/src/caic", want: "/home/user/src/caic"},
			{dir: "/home/alice/projects/myrepo", want: "/home/user/src/myrepo"},
			{dir: "/opt/repos/foo", want: "/home/user/src/foo"},
		}
		for _, tc := range tests {
			r := &RepoExecutor{Dir: tc.dir}
			tk := tc.task
			if tk == nil {
				tk = &Task{}
			}
			got := r.runtimeDir(tk)
			if got != tc.want {
				t.Errorf("runtimeDir(%q) = %q, want %q", tc.dir, got, tc.want)
			}
		}
	})

	t.Run("StartMessageDispatch", func(t *testing.T) {
		t.Parallel()
		t.Run("ResultMessage", func(t *testing.T) {
			t.Parallel()
			stub := &stubContainer{}
			r := &RepoExecutor{Runtime: stub, Dir: "/repo"}
			r.initDefaults()

			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, Repos: []RepoMount{{Branch: "caic-0"}}}
			tk.SetState(StateRunning)
			_, ch, unsub := tk.Subscribe(t.Context())
			defer unsub()

			msgCh, _ := r.startMessageDispatch(t.Context(), tk, false)

			rm := &agent.ResultMessage{MessageType: "result"}
			msgCh <- rm
			close(msgCh)

			// Wait for the dispatched message.
			timeout := time.After(time.Second)
			select {
			case got := <-ch:
				rr, ok := got.(*agent.ResultMessage)
				if !ok {
					t.Fatalf("expected *agent.ResultMessage, got %T", got)
				}
				if len(rr.DiffStat) != 1 || rr.DiffStat[0].Path != "main.go" {
					t.Errorf("DiffStat = %+v, want [{main.go 5 1}]", rr.DiffStat)
				}
			case <-timeout:
				t.Fatal("timed out waiting for message")
			}
			if !stub.fetched {
				t.Error("Fetch was not called on result message")
			}
		})

		t.Run("MutatingToolEmitsDiffStatWithoutFetch", func(t *testing.T) {
			t.Parallel()
			for _, tool := range []string{"Edit", "Bash", "Write", "NotebookEdit"} {
				t.Run(tool, func(t *testing.T) {
					t.Parallel()
					stub := &stubContainer{}
					r := &RepoExecutor{Runtime: stub, Dir: "/repo"}
					r.initDefaults()

					tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, Repos: []RepoMount{{Branch: "caic-0"}}}
					tk.SetState(StateRunning)
					_, ch, unsub := tk.Subscribe(t.Context())
					defer unsub()

					msgCh, _ := r.startMessageDispatch(t.Context(), tk, false)

					// Send a ToolUseMessage with a mutating tool.
					toolID := "tool_edit_1"
					msgCh <- &agent.ToolUseMessage{
						ToolUseID: toolID,
						Name:      tool,
						Input:     json.RawMessage(`{}`),
					}
					// Drain the tool_use message from the subscriber.
					recvMsg(t, ch)

					// Send the tool result.
					msgCh <- &agent.ToolResultMessage{
						ToolUseID: toolID,
					}

					msg := recvMsg(t, ch)
					if _, ok := msg.(*agent.ToolResultMessage); !ok {
						t.Fatalf("expected *agent.ToolResultMessage, got %T", msg)
					}
					msg = recvMsg(t, ch)
					ds, ok := msg.(*agent.DiffStatMessage)
					if !ok {
						t.Fatalf("expected *agent.DiffStatMessage, got %T", msg)
					}
					if len(ds.DiffStat) != 1 || ds.DiffStat[0].Path != "main.go" {
						t.Errorf("DiffStat = %+v, want [{main.go 5 1}]", ds.DiffStat)
					}
					if stub.fetched {
						t.Error("Fetch was called for mutating tool result")
					}
					close(msgCh)
				})
			}
		})

		t.Run("NonMutatingToolNoDiffStat", func(t *testing.T) {
			t.Parallel()
			stub := &stubContainer{}
			r := &RepoExecutor{Runtime: stub}
			r.initDefaults()

			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, Repos: []RepoMount{{Branch: "caic-0"}}}
			tk.SetState(StateRunning)
			_, ch, unsub := tk.Subscribe(t.Context())
			defer unsub()

			msgCh, _ := r.startMessageDispatch(t.Context(), tk, false)

			toolID := "tool_read_1"
			msgCh <- &agent.ToolUseMessage{
				ToolUseID: toolID,
				Name:      "Read",
				Input:     json.RawMessage(`{}`),
			}
			recvMsg(t, ch) // drain tool_use

			msgCh <- &agent.ToolResultMessage{
				ToolUseID: toolID,
			}
			// Only expect the ToolResultMessage, no DiffStatMessage.
			msg := recvMsg(t, ch)
			if _, ok := msg.(*agent.DiffStatMessage); ok {
				t.Error("unexpected DiffStatMessage emitted for non-mutating tool")
			}
			if stub.fetched {
				t.Error("Fetch was called for non-mutating tool")
			}
			close(msgCh)
		})

		t.Run("SkipSideEffects", func(t *testing.T) {
			t.Parallel()
			stub := &stubContainer{}
			r := &RepoExecutor{Runtime: stub, Dir: "/repo"}
			r.initDefaults()

			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, Repos: []RepoMount{{Branch: "caic-0"}}}
			tk.SetState(StateRunning)
			_, ch, unsub := tk.Subscribe(t.Context())
			defer unsub()

			msgCh, done := r.startMessageDispatch(t.Context(), tk, true)

			// Send a mutating tool use + result and a ResultMessage.
			toolID := "tool_edit_1"
			msgCh <- &agent.ToolUseMessage{ToolUseID: toolID, Name: "Edit", Input: json.RawMessage(`{}`)}
			recvMsg(t, ch)
			msgCh <- &agent.ToolResultMessage{ToolUseID: toolID}
			recvMsg(t, ch)
			msgCh <- &agent.ResultMessage{MessageType: "result"}
			recvMsg(t, ch)
			close(msgCh)
			<-done

			if stub.fetched {
				t.Error("Fetch was called despite skipSideEffects=true")
			}
		})

		t.Run("DispatchDrainBeforeClose", func(t *testing.T) {
			t.Parallel()
			r := &RepoExecutor{}
			r.initDefaults()

			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)

			msgCh, done := r.startMessageDispatch(t.Context(), tk, false)

			// Buffer several messages, then close without draining.
			msgs := []*agent.TextMessage{
				{Text: "first"},
				{Text: "second"},
				{Text: "third"},
			}
			for _, m := range msgs {
				msgCh <- m
			}
			close(msgCh)
			<-done

			// All messages must be in t.msgs after done fires.
			got := tk.Messages()
			if len(got) != len(msgs) {
				t.Fatalf("Messages() has %d items, want %d", len(got), len(msgs))
			}
			for i, m := range got {
				tm, ok := m.(*agent.TextMessage)
				if !ok {
					t.Fatalf("Messages()[%d] = %T, want *agent.TextMessage", i, m)
				}
				if tm.Text != msgs[i].Text {
					t.Errorf("Messages()[%d].Text = %q, want %q", i, tm.Text, msgs[i].Text)
				}
			}
		})
	})

	t.Run("RestartSession", func(t *testing.T) {
		t.Parallel()
		for _, startState := range []State{StateWaiting, StateAsking, StateHasPlan} {
			t.Run(startState.String(), func(t *testing.T) {
				t.Parallel()
				logDir := t.TempDir()
				backend := &testBackend{}

				r := &RepoExecutor{
					LogDir:   logDir,
					Runtime:  &stubContainer{},
					Backends: map[harness.Name]agent.Backend{"test": backend},
				}

				tk := &Task{
					ID:            ksid.NewID(),
					InitialPrompt: agent.Prompt{Text: "old prompt"},
					Repos:         []RepoMount{{Name: "org/repo", Branch: "caic-0"}},
					Harness:       "test",
				}
				tk.SetRuntimeConnectionInfo("fake-instance", runtime.ConnectionTarget{SSHHost: "fake-instance"}, "", "", 0)
				tk.SetState(startState)

				h, err := r.RestartSession(t.Context(), tk, agent.Prompt{Text: "new plan"})
				if err != nil {
					t.Fatal(err)
				}
				if h == nil {
					t.Fatal("RestartSession returned nil handle")
				}
				if tk.GetState() != StateRunning {
					t.Errorf("state = %v, want %v", tk.GetState(), StateRunning)
				}
				if tk.InitialPrompt.Text != "old prompt" {
					t.Errorf("Prompt.Text = %q, want %q (must not be mutated by RestartSession)", tk.InitialPrompt.Text, "old prompt")
				}

				// The context passed to AgentBackend.Start must still be valid after
				// RestartSession returns (it must not be a request-scoped context).
				select {
				case <-backend.capturedCtx.Done():
					t.Error("context passed to AgentBackend was canceled; must use a long-lived context")
				default:
				}

				// Verify the session is functional: wait briefly and check the context
				// is still alive (not canceled by a short-lived HTTP request context).
				time.Sleep(50 * time.Millisecond)
				select {
				case <-backend.capturedCtx.Done():
					t.Error("context was canceled shortly after RestartSession returned")
				default:
				}

				// Clean up: close the session.
				tk.CloseAndDetachSession(t.Context())
			})
		}
	})

	t.Run("RestartSession/LogContainsContextCleared", func(t *testing.T) {
		t.Parallel()
		logDir := t.TempDir()
		backend := &testBackend{}

		r := &RepoExecutor{
			LogDir:   logDir,
			Runtime:  &stubContainer{},
			Backends: map[harness.Name]agent.Backend{"test": backend},
		}

		tk := &Task{
			ID:            ksid.NewID(),
			InitialPrompt: agent.Prompt{Text: "test"},
			Repos:         []RepoMount{{Name: "org/repo", Branch: "caic-0"}},
			Harness:       "test",
		}
		tk.SetRuntimeConnectionInfo("fake-instance", runtime.ConnectionTarget{SSHHost: "fake-instance"}, "", "", 0)

		// Create an initial session with a log writer by using the backend
		// directly (RepoExecutor.Start needs a instance backend).
		logW, err := r.logStore().Open(tk)
		if err != nil {
			t.Fatal(err)
		}
		msgCh := make(chan agent.Message, 16)
		session, err := backend.Start(t.Context(), &agent.Options{MsgCh: msgCh, LogW: logW})
		if err != nil {
			t.Fatal(err)
		}
		alreadyDone := make(chan struct{})
		close(alreadyDone)
		h1 := &SessionHandle{Session: session, MsgCh: msgCh, DispatchDone: alreadyDone, LogW: logW}
		tk.AttachSession(h1)
		tk.SetState(StateRunning)

		// Simulate the agent writing a plan file.
		tk.addMessage(t.Context(), &agent.ToolUseMessage{
			ToolUseID: "tu1", Name: "Write",
			Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
		}, false)
		if snap := tk.Snapshot(); snap.PlanContent != "the plan" {
			t.Fatalf("PlanContent = %q before restart, want %q", snap.PlanContent, "the plan")
		}

		// Gracefully end the first session so we can restart.
		_ = h1.GracefulStop(t.Context(), 5*time.Second)
		tk.SetState(StateWaiting)

		// Restart: should write context_cleared to the log before closing it.
		h2, err := r.RestartSession(t.Context(), tk, agent.Prompt{Text: "execute plan"})
		if err != nil {
			t.Fatal(err)
		}
		defer tk.CloseAndDetachSession(t.Context())
		_ = h2

		// Read the raw log file and verify context_cleared is present.
		entries, err := os.ReadDir(logDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf("expected 1 log file, got %d", len(entries))
		}
		data, err := os.ReadFile(filepath.Join(logDir, entries[0].Name())) //nolint:gosec // test code, path is from t.TempDir()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `"context_cleared"`) {
			t.Error("log file does not contain context_cleared marker")
		}
	})

	t.Run("BranchDiffStat", func(t *testing.T) {
		t.Parallel()
		sc := &stubContainer{}
		r := &RepoExecutor{Runtime: sc, Dir: "/repo"}
		tk := &Task{Repos: []RepoMount{{GitRoot: "/repo", Branch: "feature"}}}
		tk.SetRuntimeConnectionInfo("ctr-1", runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
		ds := r.BranchDiffStat(t.Context(), tk)
		if !sc.fetched {
			t.Error("BranchDiffStat did not call Fetch")
		}
		if len(sc.fetchIDs) != 1 || sc.fetchIDs[0] != "ctr-1" {
			t.Errorf("fetch IDs = %v, want [ctr-1]", sc.fetchIDs)
		}
		if len(ds) != 1 || ds[0].Path != "main.go" || ds[0].Added != 5 || ds[0].Deleted != 1 {
			t.Errorf("BranchDiffStat = %+v, want [{main.go +5 -1}]", ds)
		}
	})
	t.Run("BranchDiffStatMultiRepoUsesInstanceID", func(t *testing.T) {
		t.Parallel()
		sc := &stubContainer{}
		r := &RepoExecutor{Runtime: sc, Dir: "/home/user/src/caic"}
		tk := &Task{
			Repos: []RepoMount{
				{GitRoot: "/home/user/src/caic", Branch: "caic-7", MountedPath: "/home/user/src/caic"},
				{GitRoot: "/home/user/src/genai", Branch: "caic-0", MountedPath: "/home/user/src/genai"},
			},
		}
		tk.SetRuntimeConnectionInfo("ctr-2", runtime.ConnectionTarget{SSHHost: "ctr-2"}, "", "", 0)

		ds := r.BranchDiffStat(t.Context(), tk)

		if len(ds) != 2 {
			t.Fatalf("BranchDiffStat len = %d, want 2", len(ds))
		}
		if len(sc.diffIDs) != 2 {
			t.Fatalf("diff calls = %d, want 2", len(sc.diffIDs))
		}
		for i, id := range sc.diffIDs {
			if id != "ctr-2" {
				t.Errorf("diff call %d id = %q, want ctr-2", i, id)
			}
		}
		if sc.diffIdxs[0] != 0 || sc.diffIdxs[1] != 1 {
			t.Errorf("diff indexes = %v, want [0 1]", sc.diffIdxs)
		}
		if ds[1].Path != "genai/main.go" {
			t.Errorf("second path = %q, want genai/main.go", ds[1].Path)
		}
	})
	t.Run("BranchDiffStatNoContainer", func(t *testing.T) {
		t.Parallel()
		r := &RepoExecutor{}
		if ds := r.BranchDiffStat(t.Context(), &Task{}); ds != nil {
			t.Errorf("BranchDiffStat with no instance = %+v, want nil", ds)
		}
	})
	t.Run("BranchDiffStatNoDir", func(t *testing.T) {
		t.Parallel()
		r := &RepoExecutor{Runtime: &stubContainer{}, Dir: ""}
		if ds := r.BranchDiffStat(t.Context(), &Task{}); ds != nil {
			t.Errorf("BranchDiffStat with no dir = %+v, want nil", ds)
		}
	})
}

func TestTaskRuntime(t *testing.T) {
	t.Parallel()
	t.Run("valid_preserves_mounted_path", func(t *testing.T) {
		t.Parallel()
		r := &RepoExecutor{Dir: "/home/user/src/caic-xyz/caic"}
		tk := &Task{
			Repos: []RepoMount{
				{Name: "caic-xyz/caic", Branch: "caic-7", MountedPath: "/home/user/src/caic-xyz/caic"},
				{Name: "caic-xyz/md", Branch: "caic-0", GitRoot: "/home/user/src/caic-xyz/md", MountedPath: "/home/user/src/caic-xyz/md"},
			},
		}
		tk.SetRuntimeConnectionInfo("ctr-1", runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)

		id, repos, err := r.taskRuntime(tk)
		if err != nil {
			t.Fatalf("taskRuntime: %v", err)
		}
		if id != "ctr-1" {
			t.Errorf("id = %q, want ctr-1", id)
		}
		if len(repos) != 2 {
			t.Fatalf("repos len = %d, want 2", len(repos))
		}
		if repos[0].HostPath != "/home/user/src/caic-xyz/caic" {
			t.Errorf("primary HostPath = %q, want executor dir", repos[0].HostPath)
		}
		if repos[0].MountPath != "/home/user/src/caic-xyz/caic" {
			t.Errorf("primary MountPath = %q, want qualified mount", repos[0].MountPath)
		}
		if repos[1].MountPath != "/home/user/src/caic-xyz/md" {
			t.Errorf("extra MountPath = %q, want qualified mount", repos[1].MountPath)
		}
	})
	t.Run("valid_no_repos", func(t *testing.T) {
		t.Parallel()
		r := &RepoExecutor{Dir: "/repo"}
		tk := &Task{}
		tk.SetRuntimeConnectionInfo("ctr-1", runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
		id, repos, err := r.taskRuntime(tk)
		if err != nil {
			t.Fatalf("taskRuntime: %v", err)
		}
		if id != "ctr-1" {
			t.Errorf("id = %q, want ctr-1", id)
		}
		if repos != nil {
			t.Fatalf("repos = %+v, want nil", repos)
		}
	})
	t.Run("error_no_instance", func(t *testing.T) {
		t.Parallel()
		if _, _, err := (&RepoExecutor{}).taskRuntime(&Task{}); err == nil {
			t.Fatal("want error")
		}
	})
}

func TestExtractRepoDS(t *testing.T) {
	t.Parallel()
	ds := agent.DiffStat{
		{Path: "a/b/main.go", Added: 10, Deleted: 3},
		{Path: "a/b/util.go", Added: 5, Deleted: 0},
	}
	t.Run("Multi", func(t *testing.T) {
		t.Parallel()
		got := extractRepoDS(ds, "a/b", true)
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2", len(got))
		}
		if got[0].Path != "main.go" || got[1].Path != "util.go" {
			t.Errorf("got paths = %q, %q", got[0].Path, got[1].Path)
		}
	})
	t.Run("Single", func(t *testing.T) {
		t.Parallel()
		got := extractRepoDS(ds, "a/b", false)
		if len(got) != 2 || got[0].Path != "a/b/main.go" {
			t.Errorf("single repo should return unchanged, got %+v", got)
		}
	})
}

func TestDiffContentArgs(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		path  string
		repo  runtime.Repo
		multi bool
		want  []string
	}{
		"single repo full diff": {
			want: []string{"--src-prefix=", "--dst-prefix="},
		},
		"single repo path": {
			path: "main.go",
			want: []string{"--src-prefix=", "--dst-prefix=", "--", "main.go"},
		},
		"multi repo full diff": {
			repo:  runtime.Repo{MountPath: "~/src/caic"},
			multi: true,
			want:  []string{"--src-prefix=a/caic/", "--dst-prefix=b/caic/"},
		},
		"multi repo path": {
			path:  "b/main.go",
			repo:  runtime.Repo{MountPath: "~/src/caic-xyz/caic"},
			multi: true,
			want:  []string{"--src-prefix=a/caic-xyz/caic/", "--dst-prefix=b/caic-xyz/caic/", "--", "b/main.go"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := diffContentArgs(tc.path, &tc.repo, tc.multi); !slices.Equal(got, tc.want) {
				t.Errorf("diffContentArgs() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDiffRepoPrefix(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		repo runtime.Repo
		want string
	}{
		"tilde source mount": {
			repo: runtime.Repo{MountPath: "~/src/caic"},
			want: "caic",
		},
		"tilde collision mount": {
			repo: runtime.Repo{MountPath: "~/src/caic-xyz/caic"},
			want: "caic-xyz/caic",
		},
		"home source mount": {
			repo: runtime.Repo{MountPath: "/home/user/src/caic-xyz/caic"},
			want: "caic-xyz/caic",
		},
		"host fallback": {
			repo: runtime.Repo{HostPath: "/home/user/src/caic"},
			want: "caic",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := diffRepoPrefix(&tc.repo); got != tc.want {
				t.Errorf("diffRepoPrefix() = %q, want %q", got, tc.want)
			}
		})
	}
}

// stubContainer implements runtime.Backend for testing. Diff returns a fixed
// numstat line; Fetch records that it was called.
type stubContainer struct {
	fetched   bool
	launchErr error // If set, Launch returns this error.
	fetchErr  error // If set, Fetch returns this error.
	stopped   bool
	diffIDs   []runtime.InstanceID
	fetchIDs  []runtime.InstanceID
	diffIdxs  []int
}

func (s *stubContainer) Launch(_ context.Context, _ []runtime.Repo, _ *runtime.StartOptions) (runtime.InstanceID, error) {
	if s.launchErr != nil {
		return "", s.launchErr
	}
	return "stub", nil
}

func (s *stubContainer) Connect(_ context.Context, id runtime.InstanceID, _ *runtime.StartOptions) (runtime.ConnectionInfo, error) {
	return runtime.ConnectionInfo{AgentTarget: runtime.ConnectionTarget{SSHHost: string(id)}}, nil
}

func (s *stubContainer) Diff(_ context.Context, id runtime.InstanceID, repoIdx int, _ ...string) (string, error) {
	s.diffIDs = append(s.diffIDs, id)
	s.diffIdxs = append(s.diffIdxs, repoIdx)
	return "5\t1\tmain.go\n", nil
}

func (s *stubContainer) Fetch(_ context.Context, id runtime.InstanceID) error {
	s.fetched = true
	s.fetchIDs = append(s.fetchIDs, id)
	if s.fetchErr != nil {
		return s.fetchErr
	}
	return nil
}

func (s *stubContainer) Stop(_ context.Context, _ runtime.InstanceID) error {
	s.stopped = true
	return nil
}
func (s *stubContainer) Purge(_ context.Context, _ runtime.InstanceID) error {
	return nil
}
func (s *stubContainer) Revive(_ context.Context, _ runtime.InstanceID) error {
	return nil
}

func (s *stubContainer) Fork(_ context.Context, _ runtime.InstanceID, _ []runtime.Repo, _ *runtime.ForkOptions) (runtime.InstanceID, runtime.ConnectionInfo, []runtime.Repo, error) {
	return "stub-fork", runtime.ConnectionInfo{AgentTarget: runtime.ConnectionTarget{SSHHost: "stub-fork"}}, nil, nil
}
func (s *stubContainer) VNCPort(_ context.Context, _ runtime.InstanceID) int { return 0 }

func (s *stubContainer) Processes(_ context.Context, _ runtime.InstanceID) ([]runtime.ProcessInfo, error) {
	return nil, nil
}

func (s *stubContainer) Signal(_ context.Context, _ runtime.InstanceID, _ int, _ string) error {
	return nil
}

// recvMsg reads a single message from ch, respecting the test context and a
// 1-second safety timeout.
func recvMsg(t *testing.T, ch <-chan agent.Message) agent.Message {
	select {
	case m := <-ch:
		return m
	case <-t.Context().Done():
		t.Fatal("test context canceled waiting for message")
		return nil
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message")
		return nil
	}
}

// initTestRepo creates a bare "remote" and a local clone with one commit on
// baseBranch. Returns the clone directory. origin points to the bare repo so
// git fetch/push work locally.
func initTestRepo(t *testing.T, baseBranch string) string { //nolint:unparam // baseBranch is parameterized for clarity.
	dir := t.TempDir()
	bare := filepath.Join(dir, "remote.git")
	clone := filepath.Join(dir, "clone")

	runGit(t, "", "init", "--bare", bare)
	runGit(t, "", "init", clone)
	runGit(t, clone, "config", "user.name", "Test")
	runGit(t, clone, "config", "user.email", "test@test.com")
	runGit(t, clone, "checkout", "-b", baseBranch)

	if err := os.WriteFile(filepath.Join(clone, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, clone, "add", ".")
	runGit(t, clone, "commit", "-m", "init")
	runGit(t, clone, "remote", "add", "origin", bare)
	runGit(t, clone, "push", "-u", "origin", baseBranch)
	return clone
}

func runGit(t *testing.T, dir string, args ...string) {
	cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // test helper with controlled args
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

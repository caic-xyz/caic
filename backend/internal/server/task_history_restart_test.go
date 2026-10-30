// Tests raw-history generation recovery across an actual HTTP server process restart.

package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
)

func TestHistoryProcessRestart(t *testing.T) {
	if path := os.Getenv("CAIC_HISTORY_RESTART_LOG"); path != "" {
		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: claudecode.New()})
		tk := mustNewTask(t, ksid.ID(1), agent.Prompt{Text: "restart history"}, harness.Claude)
		tk.SetState(taskslog.StateRunning)
		insertTestTask(s, tk.ID, tk)
		e, _ := s.taskMgr.GetEntry(tk.ID)
		e.LogPath.Set(path)
		var config net.ListenConfig
		listener, err := config.Listen(t.Context(), "tcp", os.Getenv("CAIC_HISTORY_RESTART_ADDR"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(listener.Addr().String()); err != nil {
			t.Fatal(err)
		}
		routes := s.taskHandlers.routes()
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			routes.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), httpLoggerKey{}, slog.New(slog.DiscardHandler))))
		})
		server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
		if err := server.Serve(listener); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Parallel()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(path, []byte(`{"t":"caic_meta","version":3,"harness":"claude","prompt":"restart history"}`+"\n"+historyTextRecord+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	start := func(address string) (string, func()) {
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestHistoryProcessRestart$")
		cmd.Env = append(os.Environ(), "CAIC_HISTORY_RESTART_LOG="+path, "CAIC_HISTORY_RESTART_ADDR="+address)
		output, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		var once sync.Once
		stop := func() {
			once.Do(func() {
				cancel()
				if err := cmd.Wait(); err == nil {
					t.Error("history server exited without cancellation")
				} else if _, ok := errors.AsType[*exec.ExitError](err); !ok {
					t.Errorf("wait for history server: %v", err)
				}
			})
		}
		t.Cleanup(stop)
		var actual string
		if err := json.NewDecoder(output).Decode(&actual); err != nil {
			t.Fatal(err)
		}
		return actual, stop
	}
	address, stop := start("127.0.0.1:0")
	client := &http.Client{Timeout: 5 * time.Second}
	request := func(path string) (int, []byte) {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+address+path, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("response: %v, %v", readErr, closeErr)
		}
		return response.StatusCode, body
	}
	bootstrap := func() v1.HistoryBootstrap {
		status, body := request("/tasks/" + ksid.ID(1).String() + "/history/manifest")
		if status != http.StatusOK {
			t.Fatalf("bootstrap %d: %s", status, body)
		}
		var manifest v1.HistoryManifest
		if err := json.Unmarshal(body, &manifest); err != nil {
			t.Fatal(err)
		}
		return manifest.Bootstrap
	}
	before := bootstrap()
	_, original := request(strings.TrimPrefix(recentHistoryURL(&before), "/api/caic/v1"))
	stop()
	address, _ = start(address)
	status, _ := request(strings.TrimPrefix(recentHistoryURL(&before), "/api/caic/v1"))
	if status != http.StatusConflict {
		t.Fatalf("old generation status %d", status)
	}
	after := bootstrap()
	if after.Generation == before.Generation || after.Through != before.Through {
		t.Fatal("restart did not rediscover a new generation over the same raw prefix")
	}
	_, recovered := request(strings.TrimPrefix(recentHistoryURL(&after), "/api/caic/v1"))
	if !bytes.Equal(recovered, original) {
		t.Fatal("process reconstruction changed canonical bytes")
	}
	r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+address+"/tasks/"+ksid.ID(1).String()+"/history/events?cursor="+before.Cursor, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	frame := readHistoryFrame(t, bufio.NewReader(response.Body))
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if frame.name != "reset" {
		t.Fatalf("stale process cursor returned %q", frame.name)
	}
}

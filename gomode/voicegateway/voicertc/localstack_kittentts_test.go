// Tests for the KittenTTS subprocess adapter.

package voicertc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestKittenTTSAdapter(t *testing.T) {
	t.Parallel()

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := kittenTTSTestContext(t)
		t.Cleanup(cancel)
		a, err := newKittenTTSAdapterWithCommand(ctx, kittenTTSTestCommand("valid"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
		})
		chunks, err := collectKittenTTS(ctx, a, "hello")
		if err != nil {
			t.Fatal(err)
		}
		pcm := bytes.Join(chunks, nil)
		if !bytes.Equal(pcm, []byte{1, 2, 3, 4}) {
			t.Fatalf("pcm = %v, want [1 2 3 4]", pcm)
		}
	})

	t.Run("concurrent", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := kittenTTSTestContext(t)
		t.Cleanup(cancel)
		a, err := newKittenTTSAdapterWithCommand(ctx, kittenTTSTestCommand("concurrent"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
		})
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for i := range 2 {
			wg.Go(func() {
				chunks, err := collectKittenTTS(ctx, a, fmt.Sprintf("hello %d", i))
				if err != nil {
					errs <- err
					return
				}
				pcm := bytes.Join(chunks, nil)
				if !bytes.Equal(pcm, []byte{1, 2, 3, 4}) {
					errs <- fmt.Errorf("pcm = %v, want [1 2 3 4]", pcm)
				}
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := kittenTTSTestContext(t)
		t.Cleanup(cancel)
		a, err := newKittenTTSAdapterWithCommand(ctx, kittenTTSTestCommand("error"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
		})
		_, err = collectKittenTTS(ctx, a, "hello")
		if err == nil || !strings.Contains(err.Error(), "synthesis failed") {
			t.Fatalf("err = %v, want synthesis failed", err)
		}
	})

	t.Run("startup error", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := kittenTTSTestContext(t)
		t.Cleanup(cancel)
		_, err := newKittenTTSAdapterWithCommand(ctx, kittenTTSTestCommand("startup-error"))
		if err == nil || !strings.Contains(err.Error(), "missing model") {
			t.Fatalf("err = %v, want missing model", err)
		}
	})
}

func TestKittenTTSCommand(t *testing.T) { //nolint:paralleltest // Uses t.Setenv to validate platform-specific os.UserCacheDir behavior.
	want := isolatedKittenTTSCacheDir(t)

	cmd, err := kittenTTSCommand(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Dir != want {
		t.Fatalf("cmd.Dir = %q, want %q", cmd.Dir, want)
	}
}

func isolatedKittenTTSCacheDir(t *testing.T) string {
	base := t.TempDir()
	switch runtime.GOOS {
	case "darwin":
		t.Setenv("HOME", base)
		return filepath.Join(base, "Library", "Caches", "caic", "kittentts")
	case "windows":
		t.Setenv("LOCALAPPDATA", base)
		return filepath.Join(base, "caic", "kittentts")
	default:
		t.Setenv("XDG_CACHE_HOME", base)
		return filepath.Join(base, "caic", "kittentts")
	}
}

func TestKittenTTSAdapterHelperProcess(t *testing.T) { //nolint:paralleltest // helper subprocess exits instead of running as a normal test.
	if os.Getenv("CAIC_KITTEN_TTS_HELPER") != "1" {
		t.Parallel()
		return
	}
	args := os.Args
	mode := args[len(args)-1]
	switch mode {
	case "valid":
		kittenTTSHelperServe(false, false)
	case "concurrent":
		kittenTTSHelperServe(false, true)
	case "error":
		kittenTTSHelperServe(true, false)
	case "startup-error":
		fmt.Println(`{"kind":"error","error":"missing model"}`)
	default:
		fmt.Printf(`{"kind":"error","error":"unknown mode %s"}`+"\n", mode)
	}
	os.Exit(0)
}

func kittenTTSTestContext(t *testing.T) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
}

func kittenTTSTestCommand(mode string) kittenTTSCommandFactory {
	return func(ctx context.Context) (*exec.Cmd, error) {
		args := []string{"-test.run=TestKittenTTSAdapterHelperProcess", "--", mode}
		cmd := exec.CommandContext(ctx, os.Args[0], args...) //nolint:gosec // test helper executes this test binary.
		cmd.Env = append(os.Environ(), "CAIC_KITTEN_TTS_HELPER=1")
		return cmd, nil
	}
}

func kittenTTSHelperServe(alwaysFail, requireConcurrent bool) {
	var active atomic.Int32
	var maxActive atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(kittenTTSSynthesize, func(w http.ResponseWriter, r *http.Request) {
		now := active.Add(1)
		for {
			old := maxActive.Load()
			if now <= old || maxActive.CompareAndSwap(old, now) {
				break
			}
		}
		defer active.Add(-1)
		if requireConcurrent {
			time.Sleep(100 * time.Millisecond)
		}
		if alwaysFail {
			writeKittenTTSTestJSON(w, http.StatusInternalServerError, kittenTTSResponse{Error: "synthesis failed"})
			return
		}
		var req kittenTTSRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeKittenTTSTestJSON(w, http.StatusBadRequest, kittenTTSResponse{Error: err.Error()})
			return
		}
		if requireConcurrent && maxActive.Load() < 2 {
			time.Sleep(150 * time.Millisecond)
			if maxActive.Load() < 2 {
				writeKittenTTSTestJSON(w, http.StatusInternalServerError, kittenTTSResponse{Error: "requests were serialized"})
				return
			}
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		writeKittenTTSStream(w, []byte{1, 2})
		writeKittenTTSStream(w, []byte{3, 4})
	})
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Printf(`{"kind":"error","error":%q}`+"\n", err.Error())
		os.Exit(1)
	}
	fmt.Printf(`{"kind":"ready","url":"http://%s","voices":["Jasper"]}`+"\n", ln.Addr().String())
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	if err := srv.Serve(ln); err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
}

func writeKittenTTSTestJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "encode response: %v\n", err)
	}
}

func writeKittenTTSStream(w http.ResponseWriter, pcm []byte) {
	if _, err := w.Write(pcm); err != nil {
		fmt.Fprintf(os.Stderr, "write stream response: %v\n", err)
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func collectKittenTTS(ctx context.Context, a *kittenTTSAdapter, text string) ([][]byte, error) {
	var chunks [][]byte
	for pcm, err := range a.synthesize(ctx, text) {
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, pcm)
	}
	return chunks, nil
}

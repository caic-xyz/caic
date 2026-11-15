// Tests configured Pi launch arguments and opt-in native SDK resource, override, and resume behavior.

package pi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestCompactionLauncher exercises an installed Pi SDK without inference.
// Run CAIC_PI_SDK_TEST=1 go test ./backend/internal/agent/pi -run TestCompactionLauncher -v.
// Ordinary tests do not require a global Pi installation or make network requests.
func TestCompactionLauncher(t *testing.T) {
	t.Parallel()
	if os.Getenv("CAIC_PI_SDK_TEST") != "1" {
		t.Skip("requires an installed Pi 1.5+ SDK; set CAIC_PI_SDK_TEST=1")
	}
	t.Run("warning/malformed_settings", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		models := `{"providers":{"caic":{"baseUrl":"http://127.0.0.1:1","apiKey":"test-only","api":"openai-completions","models":[{"id":"fixture","name":"Fixture","contextWindow":32768,"maxTokens":4096}]}}}`
		if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(models), 0o600); err != nil {
			t.Fatal(err)
		}
		launcher := filepath.Join(dir, "compaction.mjs")
		data, err := compactionLauncher.ReadFile("compaction.mjs")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(launcher, data, 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		t.Cleanup(cancel)
		cmd := exec.CommandContext(ctx, "node", launcher, "--reserve-tokens", "0") //nolint:gosec // Test-owned embedded launcher and fixed arguments.
		cmd.Env = append(os.Environ(), "PI_CODING_AGENT_DIR="+dir, "PI_OFFLINE=1", "PI_SKIP_VERSION_CHECK=1")
		cmd.Dir = dir
		output, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(output), "Warning: Invalid settings file "+filepath.Join(dir, "settings.json")) {
			t.Fatalf("invalid settings result = %v: %s", err, output)
		}
		if data, err := os.ReadFile(filepath.Join(dir, "settings.json")); err != nil || string(data) != "{" { //nolint:gosec // Private test fixture path.
			t.Fatalf("malformed settings changed: %q: %v", data, err)
		}
	})
	for _, resume := range []bool{false, true} {
		name := "new"
		if resume {
			name = "resume"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			cwd := filepath.Join(dir, "work")
			sessions := filepath.Join(dir, "sessions")
			for _, p := range []string{cwd, sessions} {
				if err := os.MkdirAll(p, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "100")
				w.WriteHeader(http.StatusOK)
				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Errorf("flush fixture response: %v", err)
					return
				}
				<-r.Context().Done()
			}))
			t.Cleanup(server.Close)
			settings := []byte(`{"compaction":{"reserveTokens":5000,"keepRecentTokens":6000,"modelOverrides":{"caic/scoped":{"reserveTokens":7000,"keepRecentTokens":8000}}},"httpIdleTimeoutMs":200,"httpProxy":"http://127.0.0.1:1","defaultProvider":"caic","defaultModel":"default","defaultThinkingLevel":"high","enabledModels":["caic/scoped:low","caic/missing"],"sessionDir":` + strconv.Quote(filepath.Join(dir, "wrong-sessions")) + `}`)
			models := []byte(`{"providers":{"caic":{"baseUrl":"http://127.0.0.1:1","apiKey":"test-only","api":"openai-completions","models":[{"id":"default","name":"Default fixture","reasoning":true,"input":["text"],"contextWindow":32768,"maxTokens":4096},{"id":"scoped","name":"Scoped fixture","reasoning":true,"input":["text"],"contextWindow":32768,"maxTokens":4096}]}}}`)
			if err := os.WriteFile(filepath.Join(dir, "models.json"), models, 0o600); err != nil {
				t.Fatal(err)
			}
			settingsPath := filepath.Join(dir, "settings.json")
			if err := os.WriteFile(settingsPath, settings, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(cwd, ".pi"), 0o700); err != nil {
				t.Fatal(err)
			}
			project := []byte(`{"compaction":{"keepRecentTokens":9000,"modelOverrides":{"caic/scoped":{"reserveTokens":10000,"keepRecentTokens":11000}}}}`)
			projectPath := filepath.Join(cwd, ".pi", "settings.json")
			if err := os.WriteFile(projectPath, project, 0o600); err != nil {
				t.Fatal(err)
			}
			observed := filepath.Join(dir, "observed.jsonl")
			ext := filepath.Join(dir, "probe.mjs")
			source := `import fs from "node:fs";
export default function(pi) {
 console.log("caic extension startup log");
 pi.registerCommand("caic_test_reload",{description:"Reload test resources",handler:async(_args,ctx)=>{await ctx.reload();}});
 pi.on("session_start",async(_event,ctx)=>{
   let bodyError;
   try { const response=await fetch(` + strconv.Quote(server.URL) + `); await response.text(); }
   catch(err) {bodyError=err.cause?.code??err.code;}
   fs.appendFileSync(` + strconv.Quote(observed) + `,JSON.stringify({settings:pi.getSettings(),tools:pi.getAllTools().map(x=>x.name),bodyError,proxy:process.env.HTTP_PROXY})+"\n");
   ctx.ui.notify("caic probe ready", "info");
 });
}`
			if err := os.WriteFile(ext, []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			launcher := filepath.Join(dir, "compaction.mjs")
			data, err := compactionLauncher.ReadFile("compaction.mjs")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(launcher, data, 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{launcher, "--reserve-tokens", "0", "--extension", ext}
			if resume {
				header := `{"type":"session","version":3,"id":"resume-fixture","timestamp":"2026-01-01T00:00:00Z","cwd":` + strconv.Quote(cwd) + `}` + "\n" + `{"type":"message","id":"user","parentId":null,"timestamp":"2026-01-01T00:00:01Z","message":{"role":"user","content":"earlier work","timestamp":1767225601000}}` + "\n"
				if err := os.WriteFile(filepath.Join(sessions, "resume.jsonl"), []byte(header), 0o600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--session", "resume-fixture")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			t.Cleanup(cancel)
			cmd := exec.CommandContext(ctx, "node", args...) //nolint:gosec // The executable and arguments are the embedded launcher and private test fixtures.
			cmd.Dir = cwd
			for _, value := range os.Environ() {
				key, _, _ := strings.Cut(value, "=")
				if strings.EqualFold(key, "HTTP_PROXY") || strings.EqualFold(key, "HTTPS_PROXY") {
					continue
				}
				cmd.Env = append(cmd.Env, value)
			}
			cmd.Env = append(cmd.Env, "PI_CODING_AGENT_DIR="+dir, "PI_CODING_AGENT_SESSION_DIR="+sessions, "PI_OFFLINE=1", "PI_SKIP_VERSION_CHECK=1", "NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost")
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cancel()
				if cmd.ProcessState == nil {
					_ = cmd.Wait()
				}
			})
			scanner := bufio.NewScanner(stdout)
			var output bytes.Buffer
			ready := 0
			readUntil := func(id string) {
				for scanner.Scan() {
					line := scanner.Bytes()
					output.Write(line)
					output.WriteByte('\n')
					var event struct {
						ID      string `json:"id"`
						Message string `json:"message"`
						Method  string `json:"method"`
					}
					if err := json.Unmarshal(line, &event); err != nil {
						t.Fatalf("native output is not JSON: %s", line)
					}
					if event.Method == "notify" && event.Message == "caic probe ready" {
						ready++
					}
					if event.ID == id || (id == "ready" && ready == 2) {
						return
					}
				}
				t.Fatalf("native stream ended waiting for %s: %v", id, scanner.Err())
			}
			writeCommand := func(command string) {
				if _, err := io.WriteString(stdin, command+"\n"); err != nil {
					t.Fatal(err)
				}
			}
			writeCommand(`{"type":"get_state","id":"initial"}`)
			readUntil("initial")
			writeCommand(`{"type":"prompt","id":"reload","message":"/caic_test_reload"}`)
			readUntil("ready")
			writeCommand(`{"type":"get_state","id":"final"}`)
			readUntil("final")
			if err := stdin.Close(); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatalf("SDK launcher: %v: %s", err, stderr.String())
			}

			if !strings.Contains(stderr.String(), "caic extension startup log") {
				t.Fatalf("extension startup output was not diverted to stderr: %s", stderr.String())
			}
			if !strings.Contains(stderr.String(), `Warning: No models match pattern "caic/missing"`) {
				t.Fatalf("native model scope warning missing: %s", stderr.String())
			}
			if _, err := os.Stat(filepath.Join(dir, "wrong-sessions")); !os.IsNotExist(err) {
				t.Fatalf("settings sessionDir used instead of environment override: %v", err)
			}
			type stateResponse struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Success bool   `json:"success"`
				Data    struct {
					SessionID     string `json:"sessionId"`
					MessageCount  int    `json:"messageCount"`
					ThinkingLevel string `json:"thinkingLevel"`
					Model         struct {
						ID       string `json:"id"`
						Provider string `json:"provider"`
					} `json:"model"`
				} `json:"data"`
			}
			scan := bufio.NewScanner(bytes.NewReader(output.Bytes()))
			states := 0
			for scan.Scan() {
				var state stateResponse
				if err := json.Unmarshal(scan.Bytes(), &state); err != nil {
					t.Fatalf("native output is not JSON: %s", scan.Bytes())
				}
				if state.Command == "get_state" {
					states++
					if !state.Success {
						t.Fatalf("get_state failed: %s", scan.Bytes())
					}
					wantModel, wantThinking := "scoped", "low"
					if resume {
						wantModel, wantThinking = "default", "high"
					}
					if state.Data.Model.Provider != "caic" || state.Data.Model.ID != wantModel || state.Data.ThinkingLevel != wantThinking {
						t.Fatalf("native model scope = %+v, want %s/%s", state.Data, wantModel, wantThinking)
					}
					if resume && (state.Data.SessionID != "resume-fixture" || state.Data.MessageCount != 1) {
						t.Fatalf("resume state = %+v", state.Data)
					}
				}
			}
			if err := scan.Err(); err != nil {
				t.Fatal(err)
			}
			if states != 2 {
				t.Fatalf("get_state responses = %d, want 2: %s", states, output.Bytes())
			}
			proof, err := os.ReadFile(observed) //nolint:gosec // The path is a private fixture in t.TempDir.
			if err != nil {
				t.Fatal(err)
			}
			scan = bufio.NewScanner(bytes.NewReader(proof))
			samples := 0
			for scan.Scan() {
				var sample struct {
					Settings struct {
						Compaction struct {
							ReserveTokens    int `json:"reserveTokens"`
							KeepRecentTokens int `json:"keepRecentTokens"`
							ModelOverrides   map[string]struct {
								ReserveTokens    int `json:"reserveTokens"`
								KeepRecentTokens int `json:"keepRecentTokens"`
							} `json:"modelOverrides"`
						} `json:"compaction"`
					} `json:"settings"`
					Tools     []string `json:"tools"`
					BodyError string   `json:"bodyError"`
					Proxy     string   `json:"proxy"`
				}
				if err := json.Unmarshal(scan.Bytes(), &sample); err != nil {
					t.Fatal(err)
				}
				samples++
				if sample.BodyError != "UND_ERR_BODY_TIMEOUT" || sample.Proxy != "http://127.0.0.1:1" {
					t.Fatalf("native HTTP settings not applied: timeout=%q proxy=%q", sample.BodyError, sample.Proxy)
				}
				c := sample.Settings.Compaction
				if c.ReserveTokens != 0 || c.KeepRecentTokens != 9000 || c.ModelOverrides["caic/scoped"].ReserveTokens != 0 || c.ModelOverrides["caic/scoped"].KeepRecentTokens != 11000 {
					t.Fatalf("native effective settings = %+v", c)
				}
				for _, tool := range []string{"read", "bash", "codemode", "tool_search"} {
					if !slices.Contains(sample.Tools, tool) {
						t.Fatalf("native resource %q missing: %v", tool, sample.Tools)
					}
				}
			}
			if samples < 2 {
				t.Fatalf("load/reload observations = %d, want at least 2", samples)
			}
			for path, want := range map[string][]byte{settingsPath: settings, projectPath: project} {
				got, err := os.ReadFile(path) //nolint:gosec // The paths are private fixtures in t.TempDir.
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("SDK launcher modified %s", path)
				}
			}
		})
	}
}

// Tests for the check-agent-logs command.

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/maruel/genai/acp"

	"github.com/klauspost/compress/zstd"
)

func TestRun(t *testing.T) {
	t.Parallel()
	t.Run("deletes old obsolete logs", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		old := time.Now().Add(-7 * 24 * time.Hour)
		paths := []string{
			writeLog(t, filepath.Join(dir, "v1.jsonl"), []string{`{"type":"caic_meta","version":1}`, `not a valid body`}, false),
			writeLog(t, filepath.Join(dir, "v2.jsonl.zst"), []string{`{"t":"caic_meta","version":2}`, `not a valid body`}, true),
		}
		for _, path := range paths {
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
		var out bytes.Buffer
		if err := run([]string{"-dir", dir}, &out); err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("obsolete log still exists: %s (stat error: %v)", path, err)
			}
		}
		want := fmt.Sprintf("deleted obsolete v1 task log %s\ndeleted obsolete v2 task log %s\nchecked 0 v3 task logs; found 0 schema issues\n", paths[0], paths[1])
		if out.String() != want {
			t.Errorf("output = %q, want %q", out.String(), want)
		}
	})
	t.Run("deletes selected obsolete logs", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name      string
			args      []string
			selection string
			checked   int
		}{
			{name: "default cutoff", checked: 1},
			{name: "custom cutoff", args: []string{"-since=1h"}, checked: 1},
			{name: "all", args: []string{"-all"}, checked: 1},
			{name: "explicit file", args: []string{"-since=0"}, selection: "files"},
			{name: "explicit directory", args: []string{"-since=0"}, selection: "directory", checked: 1},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				paths := make([]string, 0, 4)
				var want strings.Builder
				for i, header := range []string{`{"type":"caic_meta","version":1,"prompt":"p"}`, `{"t":"caic_meta","version":2,"repos":[]}`} {
					for _, compressed := range []bool{false, true} {
						name := fmt.Sprintf("v%d.jsonl", i+1)
						if compressed {
							name += ".zst"
						}
						path := writeLog(t, filepath.Join(dir, name), []string{header, "invalid body"}, compressed)
						paths = append(paths, path)
						fmt.Fprintf(&want, "deleted obsolete v%d task log %s\n", i+1, path)
					}
				}
				ignored := writeLog(t, filepath.Join(dir, "unrelated.txt"), []string{`{"type":"caic_meta","version":1}`}, false)
				current := writeLog(t, filepath.Join(dir, "v3.jsonl"), []string{taskMeta("pi")}, false)
				args := append([]string(nil), tc.args...)
				switch tc.selection {
				case "files":
					args = append(args, paths...)
				case "directory":
					args = append(args, dir)
				default:
					args = append(args, "-dir", dir)
				}
				fmt.Fprintf(&want, "checked %d v3 task logs; found 0 schema issues\n", tc.checked)
				var out bytes.Buffer
				if err := run(args, &out); err != nil {
					t.Fatal(err)
				}
				if out.String() != want.String() {
					t.Errorf("output = %q, want %q", out.String(), want.String())
				}
				for _, path := range paths {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Errorf("obsolete log still exists: %s (stat error: %v)", path, err)
					}
				}
				for _, path := range []string{ignored, current} {
					if _, err := os.Stat(path); err != nil {
						t.Errorf("preserved file %s: %v", path, err)
					}
				}
			})
		}
	})
	t.Run("v3 age cutoff", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name string
				args []string
				age  time.Duration
			}{
				{name: "default", age: 96 * time.Hour},
				{name: "custom", args: []string{"-since=1h"}, age: 2 * time.Hour},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					dir := t.TempDir()
					path := writeLog(t, filepath.Join(dir, "old.jsonl.zst"), []string{taskMeta("pi"), "invalid body"}, true)
					old := time.Now().Add(-tc.age)
					if err := os.Chtimes(path, old, old); err != nil {
						t.Fatal(err)
					}
					var out bytes.Buffer
					if err := run(append(tc.args, "-dir", dir), &out); err != nil {
						t.Fatal(err)
					}
					if out.String() != "checked 0 v3 task logs; found 0 schema issues\n" {
						t.Errorf("output = %q", out.String())
					}
					if _, err := os.Stat(path); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			for _, mode := range []string{"all", "explicit file", "explicit directory", "custom cutoff"} {
				t.Run(mode, func(t *testing.T) {
					t.Parallel()
					dir := t.TempDir()
					path := writeLog(t, filepath.Join(dir, "v3.jsonl"), []string{taskMeta("pi"), "invalid body"}, false)
					old := time.Now().Add(-96 * time.Hour)
					if err := os.Chtimes(path, old, old); err != nil {
						t.Fatal(err)
					}
					var args []string
					switch mode {
					case "all":
						args = []string{"-all", "-dir", dir}
					case "explicit file":
						args = []string{"-since=0", path}
					case "explicit directory":
						args = []string{"-since=0", dir}
					case "custom cutoff":
						args = []string{"-since=120h", "-dir", dir}
					}
					if err := run(args, io.Discard); err == nil || !strings.Contains(err.Error(), path+":2:") {
						t.Errorf("run error = %v, want body validation error", err)
					}
					if _, err := os.Stat(path); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	})
	t.Run("overlapping explicit paths", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := writeLog(t, filepath.Join(dir, "obsolete.jsonl"), []string{`{"t":"caic_meta","version":2}`}, false)
		current := writeLog(t, filepath.Join(dir, "v3.jsonl"), []string{taskMeta("pi")}, false)
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(cwd, path)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := run([]string{path, dir, path, rel, current}, &out); err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("deleted obsolete v2 task log %s\nchecked 1 v3 task logs; found 0 schema issues\n", path)
		if out.String() != want {
			t.Errorf("output = %q, want %q", out.String(), want)
		}
	})
	t.Run("preserves unconfirmed headers", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct{ name, header string }{
			{"empty", ""},
			{"malformed", `{"type":"caic_meta","version":1`},
			{"trailing", `{"type":"caic_meta","version":1} {}`},
			{"array", `[{"type":"caic_meta","version":1}]`},
			{"null", `null`},
			{"missing version", `{"t":"caic_meta"}`},
			{"missing discriminator", `{"version":1}`},
			{"future", `{"t":"caic_meta","version":4}`},
			{"native", `{"type":"agent_start","version":1}`},
			{"other compact record", `{"t":"agent","version":2}`},
			{"wrong v1 discriminator", `{"t":"caic_meta","version":1}`},
			{"wrong v2 discriminator", `{"type":"caic_meta","version":2}`},
			{"both discriminators", `{"type":"caic_meta","t":"caic_meta","version":1}`},
			{"null discriminator", `{"t":"caic_meta","type":null,"version":2}`},
			{"duplicate version", `{"t":"caic_meta","version":3,"version":2}`},
			{"same duplicate version", `{"type":"caic_meta","version":1,"version":1}`},
			{"escaped duplicate version", `{"t":"caic_meta","version":2,"ver\u0073ion":2}`},
			{"case variant version", `{"type":"caic_meta","version":1,"Version":3}`},
			{"case variant discriminator", `{"type":"caic_meta","TYPE":"agent","version":1}`},
			{"unicode case variant version", `{"t":"caic_meta","version":2,"verſion":3}`},
			{"duplicate discriminator", `{"t":"agent","t":"caic_meta","version":2}`},
			{"string version", `{"t":"caic_meta","version":"2"}`},
			{"fractional version", `{"t":"caic_meta","version":2.0}`},
			{"malformed version", `{"type":"caic_meta","version":null}`},
			{"invalid discriminator", `{"type":[],"version":1}`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				for _, compressed := range []bool{false, true} {
					dir := t.TempDir()
					name := "a-unconfirmed.jsonl"
					if compressed {
						name += ".zst"
					}
					path := writeLog(t, filepath.Join(dir, name), []string{tc.header}, compressed)
					before, err := os.ReadFile(filepath.Clean(path))
					if err != nil {
						t.Fatal(err)
					}
					old := time.Now().Add(-96 * time.Hour)
					if err := os.Chtimes(path, old, old); err != nil {
						t.Fatal(err)
					}
					obsolete := writeLog(t, filepath.Join(dir, "z-obsolete.jsonl"), []string{`{"t":"caic_meta","version":2}`}, false)
					if err := run([]string{"-dir", dir}, io.Discard); err == nil || !strings.Contains(err.Error(), path+":1:") {
						t.Errorf("run error = %v, want header validation error", err)
					}
					after, err := os.ReadFile(filepath.Clean(path))
					if err != nil || !bytes.Equal(before, after) {
						t.Errorf("unconfirmed log changed; read error = %v", err)
					}
					if _, err := os.Stat(obsolete); !os.IsNotExist(err) {
						t.Errorf("obsolete log not deleted after unrelated header failure: %v", err)
					}
				}
			})
		}
	})
	t.Run("reports output failure", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		obsolete := []string{
			writeLog(t, filepath.Join(dir, "a-v1.jsonl"), []string{`{"type":"caic_meta","version":1}`}, false),
			writeLog(t, filepath.Join(dir, "b-v2.jsonl.zst"), []string{`{"t":"caic_meta","version":2}`}, true),
		}
		current := writeLog(t, filepath.Join(dir, "c-v3.jsonl"), []string{taskMeta("pi"), `{"t":"agent","ts":1.000,"msg":{"type":"agent_start"}}`}, false)
		drift := writeLog(t, filepath.Join(dir, "d-v3.jsonl"), []string{taskMeta("pi"), `{"t":"agent","ts":1.000,"msg":{"type":"agent_start","future":true}}`}, false)
		r, out := io.Pipe()
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := out.Close(); err != nil {
				t.Error(err)
			}
		})
		err := run([]string{dir}, out)
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("run error = %v, want closed pipe", err)
		}
		if err == nil || !strings.Contains(err.Error(), "agent log schema validation failed") {
			t.Errorf("run error = %v, want later v3 schema failure", err)
		}
		for _, path := range obsolete {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("obsolete log not deleted after reporting failure: %s: %v", path, err)
			}
		}
		for _, path := range []string{current, drift} {
			if _, err := os.Stat(path); err != nil {
				t.Errorf("v3 log not preserved: %s: %v", path, err)
			}
		}
	})
	t.Run("continues after selection failure", func(t *testing.T) {
		t.Parallel()
		for _, mode := range []string{"missing root", "unreadable root", "unreadable file", "removal failure"} {
			t.Run(mode, func(t *testing.T) {
				t.Parallel()
				if mode != "missing root" && (runtime.GOOS == "windows" || os.Geteuid() == 0) {
					t.Skip("file permissions do not force failure on this platform or as root")
				}
				dir := t.TempDir()
				failed := filepath.Join(dir, "a-failure.jsonl")
				wantErr := os.ErrNotExist
				if mode != "missing root" {
					wantErr = os.ErrPermission
					if mode == "unreadable file" {
						writeLog(t, failed, []string{`{"t":"caic_meta","version":2}`}, false)
					} else {
						if err := os.Mkdir(failed, 0o700); err != nil {
							t.Fatal(err)
						}
						writeLog(t, filepath.Join(failed, "v1.jsonl"), []string{`{"type":"caic_meta","version":1}`}, false)
					}
					f, err := os.Open(filepath.Clean(failed))
					if err != nil {
						t.Fatal(err)
					}
					restorePerm := os.FileMode(0o700)
					if mode == "unreadable file" {
						restorePerm = 0o600
					}
					t.Cleanup(func() {
						if err := f.Chmod(restorePerm); err != nil {
							t.Error(err)
						}
						if err := f.Close(); err != nil {
							t.Error(err)
						}
					})
					var perm os.FileMode
					if mode == "removal failure" {
						perm = 0o500
					}
					if err := f.Chmod(perm); err != nil {
						t.Fatal(err)
					}
				}
				healthy := filepath.Join(dir, "b-healthy")
				if err := os.Mkdir(healthy, 0o700); err != nil {
					t.Fatal(err)
				}
				obsolete := []string{
					writeLog(t, filepath.Join(healthy, "v1.jsonl"), []string{`{"type":"caic_meta","version":1}`}, false),
					writeLog(t, filepath.Join(healthy, "v2.jsonl.zst"), []string{`{"t":"caic_meta","version":2}`}, true),
				}
				current := writeLog(t, filepath.Join(healthy, "v3.jsonl"), []string{taskMeta("pi"), `{"t":"agent","ts":1.000,"msg":{"type":"agent_start"}}`}, false)
				var out bytes.Buffer
				args := []string{failed, healthy}
				if mode == "missing root" {
					args = append(args, filepath.Join(dir, "z-missing.jsonl"))
				}
				err := run(args, &out)
				if !errors.Is(err, wantErr) {
					t.Errorf("run error = %v, want %v", err, wantErr)
				}
				if mode == "missing root" {
					for _, path := range []string{args[0], args[2]} {
						if err == nil || !strings.Contains(err.Error(), path) {
							t.Errorf("run error = %v, want discovery error for %s", err, path)
						}
					}
				}
				want := fmt.Sprintf("deleted obsolete v1 task log %s\ndeleted obsolete v2 task log %s\nchecked 1 v3 task logs; found 0 schema issues\n", obsolete[0], obsolete[1])
				if out.String() != want {
					t.Errorf("output = %q, want %q", out.String(), want)
				}
				for _, path := range obsolete {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Errorf("obsolete log not deleted after selection failure: %s: %v", path, err)
					}
				}
				if _, err := os.Stat(current); err != nil {
					t.Errorf("v3 log not preserved: %v", err)
				}
				if mode != "missing root" {
					if _, err := os.Stat(failed); err != nil {
						t.Errorf("failed selection not preserved: %v", err)
					}
				}
				if mode == "removal failure" {
					if _, err := os.Stat(filepath.Join(failed, "v1.jsonl")); err != nil {
						t.Errorf("obsolete log not preserved after removal failure: %v", err)
					}
				}
			})
		}
	})
	t.Run("reports compressed read failure", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "broken.jsonl.zst")
		if err := os.WriteFile(path, []byte("not a zstd stream"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := run([]string{path}, io.Discard); err == nil || !strings.Contains(err.Error(), "read "+path) {
			t.Errorf("run error = %v, want compressed read error", err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("clean JSONL and zstd", func(t *testing.T) {
		t.Parallel()
		lines := []string{
			taskMeta("pi"),
			`{"t":"agent","ts":1.000,"msg":{"type":"agent_start"}}`,
			`{"t":"turn_commit_snapshot","baseline":true,"repository_commits":[{"repository_path":"/home/user/src/repo","branch_name":"caic-1","commit_hash":"1111111111111111111111111111111111111111"}],"change_stat":{"files":1,"added":2,"deleted":1,"binary_files":0}}`,
		}
		for _, compressed := range []bool{false, true} {
			name := "task.jsonl"
			if compressed {
				name += ".zst"
			}
			path := writeLog(t, filepath.Join(t.TempDir(), name), lines, compressed)
			var out bytes.Buffer
			if err := run([]string{path}, &out); err != nil {
				t.Fatal(err)
			}
			if got := out.String(); got != "checked 1 v3 task logs; found 0 schema issues\n" {
				t.Errorf("output = %q", got)
			}
		}
	})

	t.Run("v3 input and relay records", func(t *testing.T) {
		t.Parallel()
		path := writeLog(t, filepath.Join(t.TempDir(), "task.jsonl"), []string{
			taskMeta("pi"),
			`{"t":"input","ts":1.000,"msg":{"type":"prompt","message":"hello"}}`,
			`{"t":"agent","ts":1.001,"msg":{"type":"agent_start"}}`,
		}, false)
		var out bytes.Buffer
		if err := run([]string{path}, &out); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("v3 directional client input DTOs", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name    string
			harness string
			input   string
		}{
			{
				name:    "antigravity",
				harness: "antigravity",
				input:   `{"event":"user","message":{"content":[{"type":"text","text":"hello"}]}}`,
			},
			{
				name:    "claude",
				harness: "claude",
				input:   `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`,
			},
			{
				name:    "codex",
				harness: "codex",
				input:   `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"caic","title":"caic","version":"1"},"capabilities":{"optOutNotificationMethods":[]}}}`,
			},
			{
				name:    "opencode",
				harness: "opencode",
				input:   `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{"terminal":false},"clientInfo":{"name":"caic","title":"caic","version":"1"}}}`,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				path := writeLog(t, filepath.Join(t.TempDir(), "task.jsonl"), []string{
					taskMeta(tc.harness),
					`{"t":"input","ts":1.000,"msg":` + tc.input + `}`,
				}, false)
				var out bytes.Buffer
				if err := run([]string{path}, &out); err != nil {
					t.Fatal(err)
				}
			})
		}
	})

	t.Run("Antigravity output and schema drift", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name   string
			record string
			want   string
		}{
			{"valid", `{"t":"agent","ts":1.000,"msg":{"event":"init","conversation_id":"s","init":{"model":"gemini-3.8-flash-low"}}}`, ""},
			{"string input", `{"t":"input","ts":1.000,"msg":{"event":"user","message":{"content":"hello"}}}`, ""},
			{"output drift", `{"t":"agent","ts":1.000,"msg":{"event":"step_update","step_update":{"state":"DONE","future":true}}}`, `unknown field "future"`},
			{"input drift", `{"t":"input","ts":1.000,"msg":{"event":"user","message":{"content":[{"type":"text","text":"hello","future":true}]}}}`, `unknown field "future"`},
			{"input message drift", `{"t":"input","ts":1.000,"msg":{"event":"user","message":{"content":"hello","future":true}}}`, `unknown field "future"`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				path := writeLog(t, filepath.Join(t.TempDir(), "task.jsonl"), []string{taskMeta("antigravity"), tc.record}, false)
				var out bytes.Buffer
				err := run([]string{path}, &out)
				if tc.want == "" {
					if err != nil {
						t.Fatal(err)
					}
				} else if err == nil || !strings.Contains(out.String(), tc.want) || !strings.Contains(out.String(), "providers/antigravity/dto.go") {
					t.Fatalf("schema drift: output=%s err=%v", out.String(), err)
				}
			})
		}
	})

	t.Run("Claude Code init", func(t *testing.T) {
		t.Parallel()
		path := writeLog(t, filepath.Join(t.TempDir(), "task.jsonl"), []string{
			taskMeta("claude"),
			`{"t":"agent","ts":1.000,"msg":{"type":"system","subtype":"init","cwd":"/tmp","session_id":"session","tools":["Bash"],"model":"sonnet","claude_code_version":"2","uuid":"uuid"}}`,
		}, false)
		var out bytes.Buffer
		if err := run([]string{path}, &out); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("unknown Pi field", func(t *testing.T) {
		t.Parallel()
		path := writeLog(t, filepath.Join(t.TempDir(), "task.jsonl"), []string{
			taskMeta("pi"),
			`{"t":"agent","ts":1.000,"msg":{"type":"agent_start","newField":true}}`,
		}, false)
		var out bytes.Buffer
		err := run([]string{path}, &out)
		if err == nil || err.Error() != "agent log schema validation failed" {
			t.Fatalf("run error = %v", err)
		}
		got := out.String()
		for _, want := range []string{"harness=pi", "dto=*pi.AgentStartEvent", `json: unknown field "newField"`, "providers/pi/dto.go"} {
			if !strings.Contains(got, want) {
				t.Errorf("output missing %q:\n%s", want, got)
			}
		}
	})

	t.Run("Codex emittedAtMs", func(t *testing.T) {
		t.Parallel()
		path := writeLog(t, filepath.Join(t.TempDir(), "task.jsonl"), []string{
			taskMeta("codex"),
			`{"t":"agent","ts":1.000,"msg":{"method":"thread/started","params":{"thread":{"id":"thread"}},"emittedAtMs":1}}`,
		}, false)
		var out bytes.Buffer
		if err := run([]string{path}, &out); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCheckCodex(t *testing.T) {
	t.Parallel()
	t.Run("warning schema", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string
			data string
			want string
		}{
			{"valid", `{"method":"warning","params":{"threadId":"thread","message":"Warning text"}}`, ""},
			{"global", `{"method":"warning","params":{"threadId":null,"message":"Warning text"}}`, ""},
			{"error", `{"method":"warning","params":{"message":"Warning text","future":true}}`, `unknown field "future"`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				dto, err := checkCodex([]byte(tc.data))
				if dto != "*codex.WarningNotification" {
					t.Fatalf("DTO = %q, err = %v", dto, err)
				}
				if tc.want == "" {
					if err != nil {
						t.Fatal(err)
					}
				} else if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err = %v, want %q", err, tc.want)
				}
			})
		}
	})
	for _, tc := range []struct {
		name string
		data string
	}{
		{
			name: "thread status changed",
			data: `{"method":"thread/status/changed","params":{"threadId":"thread","status":{"type":"idle"}}}`,
		},
		{
			name: "thread goal cleared",
			data: `{"method":"thread/goal/cleared","params":{"threadId":"thread"}}`,
		},
		{
			name: "model rerouted",
			data: `{"method":"model/rerouted","params":{"fromModel":"old","toModel":"new","reason":"high_risk_cyber_activity"}}`,
		},
		{
			name: "MCP startup status",
			data: `{"method":"mcpServer/startupStatus/updated","params":{"threadId":"thread","name":"node","status":"starting"}}`,
		},
		{
			name: "account rate limits",
			data: `{"method":"account/rateLimits/updated","params":{"rateLimits":{}}}`,
		},
		{
			name: "remote control status",
			data: `{"method":"remoteControl/status/changed","params":{"status":"disabled","serverName":"host","installationId":"installation","environmentId":null}}`,
		},
		{
			name: "account updated",
			data: `{"method":"account/updated","params":{"authMode":"chatgpt","planType":"prolite"}}`,
		},
		{
			name: "deprecation notice",
			data: `{"method":"deprecationNotice","params":{"summary":"Use pagination","details":null}}`,
		},
		{
			name: "skills changed",
			data: `{"method":"skills/changed","params":{}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := checkCodex([]byte(tc.data)); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("unknown method without params", func(t *testing.T) {
		t.Parallel()
		if _, err := checkCodex([]byte(`{"method":"future/notification"}`)); err == nil || !strings.Contains(err.Error(), "missing params") {
			t.Fatalf("checkCodex error = %v, want missing params", err)
		}
	})
	t.Run("response", func(t *testing.T) {
		t.Parallel()
		if _, err := checkCodex([]byte(`{"id":1,"result":{}}`)); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCheckPi(t *testing.T) {
	t.Parallel()
	for _, typ := range []string{
		"agent_settled",
		"auto_retry_end",
		"auto_retry_start",
		"compaction_end",
		"compaction_start",
		"entry_appended",
		"extension_error",
		"queue_update",
		"summarization_retry_attempt_start",
		"summarization_retry_finished",
		"summarization_retry_scheduled",
		"thinking_level_changed",
	} {
		t.Run(typ, func(t *testing.T) {
			t.Parallel()
			if _, err := checkPi([]byte(`{"type":"` + typ + `"}`)); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("content blocks", func(t *testing.T) {
		t.Parallel()
		valid := []byte(`{"type":"message_start","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}`)
		if _, err := checkPi(valid); err != nil {
			t.Fatal(err)
		}
		invalid := []byte(`{"type":"message_start","message":{"role":"assistant","content":[{"type":"text","text":"hello","unexpected":true}]}}`)
		if _, err := checkPi(invalid); err == nil || !strings.Contains(err.Error(), `json: unknown field "unexpected"`) {
			t.Fatalf("checkPi error = %v, want strict content-block error", err)
		}
	})
	t.Run("context replacement content", func(t *testing.T) {
		t.Parallel()
		valid := []byte(`{"type":"entry_appended","entry":{"type":"context_edit","targetId":"target","replacement":{"content":[{"type":"text","text":"replacement"}]}}}`)
		if _, err := checkPi(valid); err != nil {
			t.Fatal(err)
		}
		invalid := []byte(`{"type":"entry_appended","entry":{"type":"context_edit","targetId":"target","replacement":{"content":[{"type":"text","unexpected":true}]}}}`)
		if _, err := checkPi(invalid); err == nil || !strings.Contains(err.Error(), `json: unknown field "unexpected"`) {
			t.Fatalf("checkPi error = %v, want strict replacement content error", err)
		}
	})
	t.Run("opaque tool arguments", func(t *testing.T) {
		t.Parallel()
		data := []byte(`{"type":"tool_execution_start","toolCallId":"call","toolName":"tool","args":{"content":[{"unexpected":true}]}}`)
		if _, err := checkPi(data); err != nil {
			t.Fatalf("checkPi error = %v, want opaque arguments accepted", err)
		}
	})
}

func TestCheckOpenCode(t *testing.T) {
	t.Parallel()
	t.Run("valid session update", func(t *testing.T) {
		t.Parallel()
		data := []byte(`{"method":"session/update","params":{"sessionId":"session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello"}}}}`)
		if _, err := checkOpenCode(data, false, nil); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("missing notification params", func(t *testing.T) {
		t.Parallel()
		data := []byte(`{"method":"session/update"}`)
		if _, err := checkOpenCode(data, false, nil); err == nil || !strings.Contains(err.Error(), "missing params") {
			t.Fatalf("checkOpenCode error = %v, want missing params", err)
		}
	})

	t.Run("requests", func(t *testing.T) {
		t.Parallel()
		methods := make(map[openCodeRequestKey]acp.Method)
		valid := []byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"session","prompt":[]}}`)
		if _, err := checkOpenCode(valid, true, methods); err != nil {
			t.Fatal(err)
		}
		permission := []byte(`{"jsonrpc":"2.0","id":2,"method":"session/request_permission","params":{"sessionId":"session","toolCall":{"toolCallId":"call","status":"pending","title":"title","kind":"other","locations":[]},"options":[]}}`)
		if _, err := checkOpenCode(permission, false, methods); err != nil {
			t.Fatal(err)
		}
		if _, err := checkOpenCode([]byte(`{"jsonrpc":"2.0","id":3,"method":"future/request","params":{}}`), true, methods); err == nil || !strings.Contains(err.Error(), "unrecognized OpenCode request method") {
			t.Fatalf("unknown request error = %v", err)
		}
		if _, err := checkOpenCode([]byte(`{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"session","prompt":[],"unexpected":true}}`), true, methods); err == nil || !strings.Contains(err.Error(), `unknown field "unexpected"`) {
			t.Fatalf("malformed request error = %v", err)
		}
	})

	t.Run("unmatched response", func(t *testing.T) {
		t.Parallel()
		if _, err := checkOpenCode([]byte(`{"jsonrpc":"2.0","id":99,"result":{"stopReason":"end_turn"}}`), false, make(map[openCodeRequestKey]acp.Method)); err == nil || !strings.Contains(err.Error(), "unmatched OpenCode response") {
			t.Fatalf("checkOpenCode error = %v, want unmatched response", err)
		}
	})

	t.Run("strict response result", func(t *testing.T) {
		t.Parallel()
		methods := make(map[openCodeRequestKey]acp.Method)
		request := []byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"session","prompt":[]}}`)
		if _, err := checkOpenCode(request, true, methods); err != nil {
			t.Fatal(err)
		}
		response := []byte(`{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`)
		if _, err := checkOpenCode(response, false, methods); err != nil {
			t.Fatal(err)
		}
		if _, err := checkOpenCode(request, true, methods); err != nil {
			t.Fatal(err)
		}
		invalidResponse := []byte(`{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn","unexpected":true}}`)
		if _, err := checkOpenCode(invalidResponse, false, methods); err == nil || !strings.Contains(err.Error(), `unknown field "unexpected"`) {
			t.Fatalf("checkOpenCode error = %v, want strict PromptResult error", err)
		}
	})

	t.Run("session and configuration updates", func(t *testing.T) {
		t.Parallel()
		for _, update := range []string{`{"sessionUpdate":"session_info_update","title":"DSH task"}`, `{"sessionUpdate":"config_option_update","configOptions":[]}`} {
			data := []byte(`{"method":"session/update","params":{"sessionId":"session","update":` + update + `}}`)
			if _, err := checkOpenCode(data, false, nil); err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("unknown update field", func(t *testing.T) {
		t.Parallel()
		path := writeLog(t, filepath.Join(t.TempDir(), "task.jsonl"), []string{
			taskMeta("opencode"),
			`{"t":"agent","ts":1.000,"msg":{"method":"session/update","params":{"sessionId":"session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello"},"newField":true}}}}`,
		}, false)
		var out bytes.Buffer
		if err := run([]string{path}, &out); err == nil {
			t.Fatal("run succeeded, want schema validation error")
		}
		for _, want := range []string{"harness=opencode", "dto=*acp.AgentMessageChunkUpdate", `json: unknown field "newField"`, "acp/dto.go"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("output missing %q:\n%s", want, out.String())
			}
		}
	})
}

func TestCheckFile(t *testing.T) {
	t.Parallel()
	t.Run("valid directional Pi and OpenCode records", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name    string
			harness string
			records []string
		}{
			{"Pi commands and events", "pi", []string{
				`{"t":"input","ts":1.000,"msg":{"type":"prompt","message":"hello","images":[{"type":"image","mimeType":"image/png","data":"aW1hZ2U="}]}}`,
				`{"t":"agent","ts":2.000,"msg":{"type":"agent_start"}}`,
				`{"t":"agent","ts":3.000,"msg":{"type":"response","command":"prompt","success":true}}`,
			}},
			{"OpenCode requests in both directions sharing an ID", "opencode", []string{
				`{"t":"input","ts":1.000,"msg":{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"session","prompt":[]}}}`,
				`{"t":"agent","ts":2.000,"msg":{"jsonrpc":"2.0","id":1,"method":"session/request_permission","params":{"sessionId":"session","toolCall":{"toolCallId":"call","status":"pending","title":"title","kind":"other","locations":[]},"options":[]}}}`,
				`{"t":"input","ts":3.000,"msg":{"jsonrpc":"2.0","id":1,"result":{"outcome":{"outcome":"selected","optionId":"allow"}}}}`,
				`{"t":"agent","ts":4.000,"msg":{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}}`,
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				path := writeLog(t, filepath.Join(t.TempDir(), "task.jsonl"), append([]string{taskMeta(tc.harness)}, tc.records...), false)
				result, err := checkFile(path, time.Time{})
				if err != nil || !result.checked || len(result.findings) != 0 {
					t.Fatalf("directional records = %#v, %v", result, err)
				}
			})
		}
	})
	t.Run("error wrong-direction Pi and OpenCode records", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name    string
			harness string
			record  string
		}{
			{"Pi event as input", "pi", `{"t":"input","ts":1.000,"msg":{"type":"agent_start"}}`},
			{"Pi command as output", "pi", `{"t":"agent","ts":1.000,"msg":{"type":"prompt","message":"hello"}}`},
			{"Pi response as input", "pi", `{"t":"input","ts":1.000,"msg":{"type":"response","command":"prompt","success":true}}`},
			{"OpenCode update as input", "opencode", `{"t":"input","ts":1.000,"msg":{"method":"session/update","params":{"sessionId":"session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello"}}}}}`},
			{"OpenCode permission request as input", "opencode", `{"t":"input","ts":1.000,"msg":{"jsonrpc":"2.0","id":1,"method":"session/request_permission","params":{"sessionId":"session","toolCall":{"toolCallId":"call"},"options":[]}}}`},
			{"OpenCode client request as output", "opencode", `{"t":"agent","ts":1.000,"msg":{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"session","prompt":[]}}}`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				path := writeLog(t, filepath.Join(t.TempDir(), "task.jsonl"), []string{taskMeta(tc.harness), tc.record}, false)
				result, err := checkFile(path, time.Time{})
				if err != nil {
					t.Fatal(err)
				}
				if len(result.findings) != 1 || result.findings[0].line != 2 || result.findings[0].harness != tc.harness {
					t.Fatalf("direction violation findings = %#v", result.findings)
				}
			})
		}
	})
	t.Run("error response direction and input schema drift", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name    string
			harness string
			records []string
			want    string
		}{
			{"Pi input image drift", "pi", []string{
				`{"t":"input","ts":1.000,"msg":{"type":"prompt","message":"hello","images":[{"type":"image","mimeType":"image/png","data":"aW1hZ2U=","future":true}]}}`,
			}, `unknown field "future"`},
			{"OpenCode prompt response as input", "opencode", []string{
				`{"t":"input","ts":1.000,"msg":{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"session","prompt":[]}}}`,
				`{"t":"input","ts":2.000,"msg":{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}}`,
			}, "unmatched OpenCode response"},
			{"OpenCode permission response as output", "opencode", []string{
				`{"t":"agent","ts":1.000,"msg":{"jsonrpc":"2.0","id":1,"method":"session/request_permission","params":{"sessionId":"session","toolCall":{"toolCallId":"call"},"options":[]}}}`,
				`{"t":"agent","ts":2.000,"msg":{"jsonrpc":"2.0","id":1,"result":{"outcome":{"outcome":"selected","optionId":"allow"}}}}`,
			}, "unmatched OpenCode response"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				path := writeLog(t, filepath.Join(t.TempDir(), "task.jsonl"), append([]string{taskMeta(tc.harness)}, tc.records...), false)
				result, err := checkFile(path, time.Time{})
				if err != nil {
					t.Fatal(err)
				}
				if len(result.findings) != 1 || result.findings[0].line != len(tc.records)+1 || !strings.Contains(result.findings[0].err.Error(), tc.want) {
					t.Fatalf("direction or schema findings = %#v", result.findings)
				}
			})
		}
	})
	t.Run("rejects noncanonical v3 record", func(t *testing.T) {
		t.Parallel()
		path := writeLog(t, filepath.Join(t.TempDir(), "task.jsonl"), []string{
			taskMeta("pi"),
			`{"t":"agent","ts":1,"msg":{"type":"agent_start"}}`,
		}, false)
		if _, err := checkFile(path, time.Time{}); err == nil || !strings.Contains(err.Error(), "timestamp must have exactly three fractional digits") {
			t.Errorf("checkFile error = %v, want noncanonical timestamp", err)
		}
	})

	t.Run("reports relay diagnostic", func(t *testing.T) {
		t.Parallel()
		path := writeLog(t, filepath.Join(t.TempDir(), "task.jsonl"), []string{
			taskMeta("pi"),
			`{"t":"agent","ts":1.000,"msg":"native record exceeds relay limit"}`,
		}, false)
		var out bytes.Buffer
		err := run([]string{path}, &out)
		if err == nil || !strings.Contains(out.String(), "dto=relay diagnostic") || !strings.Contains(out.String(), "relay_v2.py") {
			t.Errorf("run error = %v, output = %q", err, out.String())
		}
	})
}

func TestParseFlags(t *testing.T) {
	t.Run("requires a selected directory without home", func(t *testing.T) {
		t.Setenv("HOME", "")
		t.Setenv("USERPROFILE", "")
		dir := t.TempDir()
		t.Chdir(dir)
		path := writeLog(t, filepath.Join(dir, "preserved.jsonl"), []string{`{"type":"caic_meta","version":1}`}, false)
		var out bytes.Buffer
		if err := run(nil, &out); err == nil || !strings.Contains(err.Error(), "resolve default task-log directory") {
			t.Errorf("default selection error = %v, want home-resolution error", err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("unselected current-directory log changed: %v", err)
		}
		if out.Len() != 0 {
			t.Errorf("default selection produced output: %s", &out)
		}
	})
	t.Run("selects default home directory", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		dir := filepath.Join(home, ".cache", "caic", "tasks")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		obsolete := writeLog(t, filepath.Join(dir, "obsolete.jsonl"), []string{`{"t":"caic_meta","version":2}`}, false)
		cwd := t.TempDir()
		t.Chdir(cwd)
		preserved := writeLog(t, filepath.Join(cwd, "preserved.jsonl"), []string{`{"type":"caic_meta","version":1}`}, false)
		var out bytes.Buffer
		if err := run(nil, &out); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(obsolete); !os.IsNotExist(err) {
			t.Errorf("default-home obsolete log still exists: %v", err)
		}
		if _, err := os.Stat(preserved); err != nil {
			t.Errorf("unselected current-directory log changed: %v", err)
		}
	})
	t.Run("explicit selection without home", func(t *testing.T) {
		t.Setenv("HOME", "")
		t.Setenv("USERPROFILE", "")
		for _, fileSelection := range []bool{false, true} {
			dir := t.TempDir()
			path := writeLog(t, filepath.Join(dir, "obsolete.jsonl"), []string{`{"type":"caic_meta","version":1}`}, false)
			args := []string{"-dir", dir}
			if fileSelection {
				args = []string{path}
			}
			var out bytes.Buffer
			if err := run(args, &out); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("explicitly selected log still exists: %v", err)
			}
		}
	})
	if _, err := parseFlags([]string{"-dir="}); err == nil || !strings.Contains(err.Error(), "-dir must not be empty") {
		t.Fatalf("empty directory error = %v", err)
	}
	if _, err := parseFlags([]string{"-since=-1s"}); err == nil {
		t.Fatal("parseFlags accepted a negative duration")
	}
}

func taskMeta(harness string) string {
	return `{"t":"caic_meta","version":3,"prompt":"p","repos":[],"harness":"` + harness + `"}`
}

func writeLog(t *testing.T, path string, lines []string, compressed bool) string {
	data := []byte(strings.Join(lines, "\n") + "\n")
	if !compressed {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// #nosec G304 -- path is allocated under t.TempDir.
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	z, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := z.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckDSH(t *testing.T) {
	t.Parallel()
	t.Run("resume and configuration", func(t *testing.T) {
		t.Parallel()
		methods := make(map[openCodeRequestKey]acp.Method)
		for _, record := range []struct {
			input bool
			data  string
		}{
			{true, `{"jsonrpc":"2.0","id":1,"method":"session/resume","params":{"sessionId":"persisted","cwd":"/workspace","mcpServers":[]}}`},
			{false, `{"jsonrpc":"2.0","id":1,"result":{"configOptions":[]}}`},
			{false, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"persisted","update":{"sessionUpdate":"config_option_update","configOptions":[]}}}`},
		} {
			if finding := checkMessage("dsh.jsonl", 2, "dsh", []byte(record.data), record.input, methods); finding != nil {
				t.Fatal(finding.String())
			}
		}
	})

	t.Run("unknown resume field", func(t *testing.T) {
		t.Parallel()
		finding := checkMessage("dsh.jsonl", 2, "dsh", []byte(`{"jsonrpc":"2.0","id":1,"method":"session/resume","params":{"sessionId":"persisted","cwd":"/workspace","unexpected":true}}`), true, make(map[openCodeRequestKey]acp.Method))
		if finding == nil || !strings.Contains(finding.String(), `unknown field "unexpected"`) || !strings.Contains(finding.String(), "acp/dto.go") {
			t.Fatalf("finding = %v, want strict ACP field diagnostic", finding)
		}
	})
}

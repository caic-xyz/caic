// Tests for the check-agent-logs command.

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	opencodedto "github.com/maruel/genai/providers/opencode"
)

func TestRun(t *testing.T) {
	t.Parallel()
	t.Run("clean JSONL and zstd", func(t *testing.T) {
		t.Parallel()
		lines := []string{
			v2Meta("pi"),
			`{"t":"agent","ts":1.000,"msg":{"type":"agent_start"}}`,
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
			if got := out.String(); got != "checked 1 v2 task logs; found 0 schema issues\n" {
				t.Errorf("output = %q", got)
			}
		}
	})

	t.Run("Claude Code init", func(t *testing.T) {
		t.Parallel()
		path := writeLog(t, filepath.Join(t.TempDir(), "task.jsonl"), []string{
			v2Meta("claude"),
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
			v2Meta("pi"),
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
			v2Meta("codex"),
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
	for _, tc := range []struct {
		name string
		data string
	}{
		{
			name: "thread status changed",
			data: `{"method":"thread/status/changed","params":{"threadId":"thread","status":{"type":"idle"}}}`,
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
		if _, err := checkOpenCode(data, nil); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("caic injections", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string
			data string
		}{
			{name: "session", data: `{"type":"caic_session","session_id":"session","model":"model","agent_version":"1"}`},
			{name: "init", data: `{"type":"caic_init","session_id":"session","model":"model","version":"1"}`},
			{name: "diff stat", data: `{"type":"caic_diff_stat","diff_stat":[],"ts":1.000}`},
			{name: "exit", data: `{"type":"caic_exit","exit_code":0,"cmd":["opencode"],"ts":1.000}`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				if _, err := checkOpenCode([]byte(tc.data), nil); err != nil {
					t.Fatal(err)
				}
				invalid := strings.TrimSuffix(tc.data, "}") + `,"unexpected":true}`
				if _, err := checkOpenCode([]byte(invalid), nil); err == nil || !strings.Contains(err.Error(), `unknown field "unexpected"`) {
					t.Fatalf("checkOpenCode(%s) error = %v, want strict unknown-field error", invalid, err)
				}
			})
		}
	})

	t.Run("unknown caic injection", func(t *testing.T) {
		t.Parallel()
		if _, err := checkOpenCode([]byte(`{"type":"caic_future","value":true}`), nil); err == nil || !strings.Contains(err.Error(), `unrecognized OpenCode caic injection type "caic_future"`) {
			t.Fatalf("checkOpenCode error = %v, want unknown caic injection error", err)
		}
	})

	t.Run("missing notification params", func(t *testing.T) {
		t.Parallel()
		data := []byte(`{"method":"session/update"}`)
		if _, err := checkOpenCode(data, nil); err == nil || !strings.Contains(err.Error(), "missing params") {
			t.Fatalf("checkOpenCode error = %v, want missing params", err)
		}
	})

	t.Run("requests", func(t *testing.T) {
		t.Parallel()
		methods := make(map[string]opencodedto.Method)
		valid := []byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"session","prompt":[]}}`)
		if _, err := checkOpenCode(valid, methods); err != nil {
			t.Fatal(err)
		}
		permission := []byte(`{"jsonrpc":"2.0","id":2,"method":"session/request_permission","params":{"sessionId":"session","toolCall":{"toolCallId":"call","status":"pending","title":"title","kind":"other","locations":[]},"options":[]}}`)
		if _, err := checkOpenCode(permission, methods); err != nil {
			t.Fatal(err)
		}
		if _, err := checkOpenCode([]byte(`{"jsonrpc":"2.0","id":3,"method":"future/request","params":{}}`), methods); err == nil || !strings.Contains(err.Error(), "unrecognized OpenCode request method") {
			t.Fatalf("unknown request error = %v", err)
		}
		if _, err := checkOpenCode([]byte(`{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"session","prompt":[],"unexpected":true}}`), methods); err == nil || !strings.Contains(err.Error(), `unknown field "unexpected"`) {
			t.Fatalf("malformed request error = %v", err)
		}
	})

	t.Run("unmatched response", func(t *testing.T) {
		t.Parallel()
		if _, err := checkOpenCode([]byte(`{"jsonrpc":"2.0","id":99,"result":{"stopReason":"end_turn"}}`), make(map[string]opencodedto.Method)); err == nil || !strings.Contains(err.Error(), "unmatched OpenCode response") {
			t.Fatalf("checkOpenCode error = %v, want unmatched response", err)
		}
	})

	t.Run("strict response result", func(t *testing.T) {
		t.Parallel()
		methods := make(map[string]opencodedto.Method)
		request := []byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"session","prompt":[]}}`)
		if _, err := checkOpenCode(request, methods); err != nil {
			t.Fatal(err)
		}
		response := []byte(`{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`)
		if _, err := checkOpenCode(response, methods); err != nil {
			t.Fatal(err)
		}
		if _, err := checkOpenCode(request, methods); err != nil {
			t.Fatal(err)
		}
		invalidResponse := []byte(`{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn","unexpected":true}}`)
		if _, err := checkOpenCode(invalidResponse, methods); err == nil || !strings.Contains(err.Error(), `unknown field "unexpected"`) {
			t.Fatalf("checkOpenCode error = %v, want strict PromptResult error", err)
		}
	})

	t.Run("known updates without DTO", func(t *testing.T) {
		t.Parallel()
		for _, update := range []string{"session_info_update", "config_option_update"} {
			data := []byte(`{"method":"session/update","params":{"sessionId":"session","update":{"sessionUpdate":"` + update + `"}}}`)
			if _, err := checkOpenCode(data, nil); err == nil || !strings.Contains(err.Error(), "has no genai DTO") {
				t.Fatalf("checkOpenCode(%s) error = %v, want missing DTO error", update, err)
			}
		}
	})

	t.Run("unknown update field", func(t *testing.T) {
		t.Parallel()
		path := writeLog(t, filepath.Join(t.TempDir(), "task.jsonl"), []string{
			v2Meta("opencode"),
			`{"t":"agent","ts":1.000,"msg":{"method":"session/update","params":{"sessionId":"session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello"},"newField":true}}}}`,
		}, false)
		var out bytes.Buffer
		if err := run([]string{path}, &out); err == nil {
			t.Fatal("run succeeded, want schema validation error")
		}
		for _, want := range []string{"harness=opencode", "dto=*opencode.AgentMessageChunkUpdate", `json: unknown field "newField"`, "providers/opencode/dto.go"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("output missing %q:\n%s", want, out.String())
			}
		}
	})
}

func TestCheckFile(t *testing.T) {
	t.Parallel()
	t.Run("rejects noncanonical v2 record", func(t *testing.T) {
		t.Parallel()
		path := writeLog(t, filepath.Join(t.TempDir(), "task.jsonl"), []string{
			v2Meta("pi"),
			`{"t":"agent","ts":1,"msg":{"type":"agent_start"}}`,
		}, false)
		if _, _, err := checkFile(path); err == nil || !strings.Contains(err.Error(), "timestamp must have exactly three fractional digits") {
			t.Errorf("checkFile error = %v, want noncanonical timestamp", err)
		}
	})

	t.Run("reports relay diagnostic", func(t *testing.T) {
		t.Parallel()
		path := writeLog(t, filepath.Join(t.TempDir(), "task.jsonl"), []string{
			v2Meta("pi"),
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
	t.Parallel()
	if _, err := parseFlags([]string{"-since=-1s"}); err == nil {
		t.Fatal("parseFlags accepted a negative duration")
	}
}

func v2Meta(harness string) string {
	return `{"t":"caic_meta","version":2,"prompt":"p","repos":[],"harness":"` + harness + `"}`
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

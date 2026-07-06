// Tests for ExportDiscussion task-log-to-markdown conversion.

package agent

import (
	"encoding/json"
	"errors"
	"io"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) {
	return 0, errors.New("read failed")
}

type testZstdReadCloser struct {
	dec  *zstd.Decoder
	file *os.File
}

func (r *testZstdReadCloser) Read(p []byte) (int, error) {
	return r.dec.Read(p)
}

func (r *testZstdReadCloser) Close() error {
	r.dec.Close()
	return r.file.Close()
}

// exportParseFn is a minimal parser for export tests. It recognises the
// message types that ExportDiscussion renders and ignores the rest.
func exportParseFn(line []byte) ([]Message, error) {
	var env exportLine
	if err := json.Unmarshal(line, &env); err != nil {
		return nil, err
	}
	switch env.Type {
	case "user_input":
		var m UserInputMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []Message{&m}, nil
	case "text":
		var w struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(line, &w); err != nil {
			return nil, err
		}
		return []Message{&TextMessage{Text: w.Text}}, nil
	case "thinking":
		var w struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(line, &w); err != nil {
			return nil, err
		}
		return []Message{&ThinkingMessage{Text: w.Text}}, nil
	case "tool_use":
		var w struct {
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(line, &w); err != nil {
			return nil, err
		}
		return []Message{&ToolUseMessage{ToolUseID: w.ID, Name: w.Name, Input: w.Input}}, nil
	case "tool_result":
		var w struct {
			ToolUseID string `json:"tool_use_id"`
			Error     string `json:"error"`
		}
		if err := json.Unmarshal(line, &w); err != nil {
			return nil, err
		}
		m := &ToolResultMessage{ToolUseID: w.ToolUseID}
		if w.Error != "" {
			m.Error = w.Error
		}
		return []Message{m}, nil
	case "system":
		var m SystemMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []Message{&m}, nil
	case "subagent_start":
		var m SubagentStartMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []Message{&m}, nil
	case "subagent_end":
		var m SubagentEndMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []Message{&m}, nil
	default:
		return nil, nil
	}
}

// writeJSONL writes lines to a temp JSONL file and returns its path.
func writeJSONL(t *testing.T, lines []string) string {
	path := filepath.Join(t.TempDir(), "test.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func seqOf(lines ...string) iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		for _, line := range lines {
			if !yield([]byte(line)) {
				return
			}
		}
	}
}

func writeCompressedJSONL(t *testing.T, lines iter.Seq[[]byte]) string {
	compressed := filepath.Join(t.TempDir(), "test.jsonl.zst")
	out, err := os.OpenFile(filepath.Clean(compressed), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := zstd.NewWriter(out)
	if err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	var writeErr error
	for line := range lines {
		if _, err := enc.Write(line); err != nil {
			writeErr = err
			break
		}
		if _, err := enc.Write([]byte("\n")); err != nil {
			writeErr = err
			break
		}
	}
	if err := errors.Join(writeErr, enc.Close(), out.Close()); err != nil {
		t.Fatal(err)
	}
	return compressed
}

func openTestLogReader(t *testing.T, path string) io.ReadCloser {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, ".zst") {
		return f
	}
	d, err := zstd.NewReader(f)
	if err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	return &testZstdReadCloser{dec: d, file: f}
}

func exportDiscussionPath(t *testing.T, path string) (string, error) {
	r := openTestLogReader(t, path)
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	return ExportDiscussion(r, path, exportParseFn)
}

// metaLine returns a caic_meta JSON line with the given prompt and harness.
func metaLine(prompt, harnessName string) string {
	return `{"type":"caic_meta","version":1,"prompt":` + jsonStr(prompt) + `,"harness":` + jsonStr(harnessName) + `,"repos":[],"started_at":"2025-01-15T10:00:00Z"}`
}

// jsonStr returns a JSON-quoted string.
func jsonStr(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestExportDiscussion(t *testing.T) {
	t.Parallel()

	t.Run("valid", func(t *testing.T) {
		t.Parallel()

		t.Run("full_conversation", func(t *testing.T) {
			t.Parallel()
			path := writeJSONL(t, []string{
				metaLine("fix the bug", "pi"),
				`{"type":"user_input","text":"fix the bug"}`,
				`{"type":"thinking","text":"Let me analyze the issue carefully and find the root cause."}`,
				`{"type":"text","text":"I'll look at the error logs first."}`,
				`{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"cat /var/log/app.log"}}`,
				`{"type":"tool_result","tool_use_id":"t1"}`,
				`{"type":"tool_use","id":"t2","name":"Edit","input":{"path":"main.go","old_text":"buggy","new_text":"fixed"}}`,
				`{"type":"tool_result","tool_use_id":"t2"}`,
				`{"type":"text","text":"The bug is fixed. The issue was a nil pointer dereference."}`,
				`{"type":"caic_result","state":"completed","cost_usd":0.05,"duration":120,"num_turns":3}`,
			})

			md, err := exportDiscussionPath(t, path)
			if err != nil {
				t.Fatal(err)
			}

			// Metadata
			assertContains(t, md, "# Task Discussion")
			assertContains(t, md, "**Prompt**: fix the bug")
			assertContains(t, md, "**Harness**: pi")
			assertContains(t, md, "**State**: completed")
			assertContains(t, md, "**Cost**: $0.0500")
			assertContains(t, md, "**Duration**: 2.0m")
			assertContains(t, md, "**Turns**: 3")

			// Conversation
			assertContains(t, md, "## User\n\nfix the bug")
			assertContains(t, md, "## Assistant\n\nI'll look at the error logs first.")
			assertContains(t, md, "The bug is fixed")
			assertContains(t, md, "### 🔧 Tool: `Bash`")
			assertContains(t, md, "```bash\ncat /var/log/app.log\n```")
			assertContains(t, md, "### 🔧 Tool: `Edit`")
			assertContains(t, md, "**File**: `main.go`")
			assertContains(t, md, "💭 Thinking")
		})

		t.Run("metadata_only", func(t *testing.T) {
			t.Parallel()
			path := writeJSONL(t, []string{
				metaLine("hello", "claude"),
			})

			md, err := exportDiscussionPath(t, path)
			if err != nil {
				t.Fatal(err)
			}
			assertContains(t, md, "**Prompt**: hello")
			assertContains(t, md, "**Harness**: claude")
			assertContains(t, md, "# Task Discussion")
			assertContains(t, md, "---")
		})

		t.Run("compressed_log", func(t *testing.T) {
			t.Parallel()
			path := writeCompressedJSONL(t, seqOf(
				metaLine("compress me", "pi"),
				`{"type":"user_input","text":"hello"}`,
				`{"type":"text","text":"done"}`,
				`{"type":"caic_result","state":"completed"}`,
			))

			md, err := exportDiscussionPath(t, path)
			if err != nil {
				t.Fatal(err)
			}
			assertContains(t, md, "**Prompt**: compress me")
			assertContains(t, md, "## Assistant\n\ndone")
		})

		t.Run("with_PR_info", func(t *testing.T) {
			t.Parallel()
			path := writeJSONL(t, []string{
				metaLine("add feature", "claude"),
				`{"type":"text","text":"done"}`,
				`{"type":"caic_pr","forge_owner":"octo","forge_repo":"app","forge_pr":42}`,
			})

			md, err := exportDiscussionPath(t, path)
			if err != nil {
				t.Fatal(err)
			}
			assertContains(t, md, "**PR**: octo/app#42")
		})

		t.Run("with_flags", func(t *testing.T) {
			t.Parallel()
			line := `{"type":"caic_meta","version":1,"prompt":"test","harness":"pi","repos":[],"started_at":"2025-01-15T10:00:00Z","tailscale":true,"display":true}`
			path := writeJSONL(t, []string{line})

			md, err := exportDiscussionPath(t, path)
			if err != nil {
				t.Fatal(err)
			}
			assertContains(t, md, "**Flags**: tailscale, display")
		})

		t.Run("with_repos", func(t *testing.T) {
			t.Parallel()
			line := `{"type":"caic_meta","version":1,"prompt":"test","harness":"pi","repos":[{"name":"app","base_branch":"main","branch":"fix-bug"}],"started_at":"2025-01-15T10:00:00Z"}`
			path := writeJSONL(t, []string{line})

			md, err := exportDiscussionPath(t, path)
			if err != nil {
				t.Fatal(err)
			}
			assertContains(t, md, "**Repo**: app (`main..fix-bug`)")
		})

		t.Run("with_error_result", func(t *testing.T) {
			t.Parallel()
			path := writeJSONL(t, []string{
				metaLine("task", "pi"),
				`{"type":"text","text":"something went wrong"}`,
				`{"type":"caic_result","state":"failed","error":"container died","cost_usd":0.01,"duration":10}`,
			})

			md, err := exportDiscussionPath(t, path)
			if err != nil {
				t.Fatal(err)
			}
			assertContains(t, md, "**State**: failed")
			assertContains(t, md, "**Error**: container died")
		})

		t.Run("tool_error", func(t *testing.T) {
			t.Parallel()
			path := writeJSONL(t, []string{
				metaLine("task", "pi"),
				`{"type":"tool_result","tool_use_id":"t1","error":"command not found"}`,
			})

			md, err := exportDiscussionPath(t, path)
			if err != nil {
				t.Fatal(err)
			}
			assertContains(t, md, "⚠️ Tool error: command not found")
		})

		t.Run("subagent_events", func(t *testing.T) {
			t.Parallel()
			path := writeJSONL(t, []string{
				metaLine("task", "pi"),
				`{"type":"subagent_start","task_id":"sub1","description":"analyze logs"}`,
				`{"type":"subagent_end","task_id":"sub1","status":"completed"}`,
			})

			md, err := exportDiscussionPath(t, path)
			if err != nil {
				t.Fatal(err)
			}
			assertContains(t, md, "### 🤖 Subagent: analyze logs")
			assertContains(t, md, "### Subagent completed")
		})

		t.Run("compaction_boundary", func(t *testing.T) {
			t.Parallel()
			path := writeJSONL(t, []string{
				metaLine("task", "pi"),
				`{"type":"system","subtype":"compact_boundary"}`,
			})

			md, err := exportDiscussionPath(t, path)
			if err != nil {
				t.Fatal(err)
			}
			assertContains(t, md, "Context compaction boundary")
		})

		t.Run("skips_caic_control_records", func(t *testing.T) {
			t.Parallel()
			path := writeJSONL(t, []string{
				metaLine("task", "pi"),
				`{"type":"caic_diff_stat","diff_stat":[]}`,
				`{"type":"caic_exit","exit_code":0}`,
				`{"type":"caic_model_info","context_window":200000}`,
				`{"type":"text","text":"visible"}`,
			})

			md, err := exportDiscussionPath(t, path)
			if err != nil {
				t.Fatal(err)
			}
			assertContains(t, md, "visible")
			assertNotContains(t, md, "caic_diff_stat")
			assertNotContains(t, md, "caic_exit")
			assertNotContains(t, md, "caic_model_info")
		})

		t.Run("thinking_truncated", func(t *testing.T) {
			t.Parallel()
			long := strings.Repeat("a", 1000)
			path := writeJSONL(t, []string{
				metaLine("task", "pi"),
				`{"type":"thinking","text":` + jsonStr(long) + `}`,
			})

			md, err := exportDiscussionPath(t, path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(md, strings.Repeat("a", 500)+"...") {
				t.Error("thinking text was not truncated")
			}
		})

		t.Run("duration_hours", func(t *testing.T) {
			t.Parallel()
			path := writeJSONL(t, []string{
				metaLine("task", "pi"),
				`{"type":"caic_result","state":"completed","duration":7200}`,
			})

			md, err := exportDiscussionPath(t, path)
			if err != nil {
				t.Fatal(err)
			}
			assertContains(t, md, "**Duration**: 2.0h")
		})

		t.Run("duration_seconds", func(t *testing.T) {
			t.Parallel()
			path := writeJSONL(t, []string{
				metaLine("task", "pi"),
				`{"type":"caic_result","state":"completed","duration":45}`,
			})

			md, err := exportDiscussionPath(t, path)
			if err != nil {
				t.Fatal(err)
			}
			assertContains(t, md, "**Duration**: 45s")
		})

		t.Run("skips_empty_tool_use", func(t *testing.T) {
			t.Parallel()
			path := writeJSONL(t, []string{
				metaLine("task", "pi"),
				`{"type":"text","text":"Before"}`,
				`{"type":"tool_use","id":"t1","name":"Read","input":{}}`,
				`{"type":"tool_use","id":"t2","name":"Read","input":{"path":"main.go"}}`,
				`{"type":"text","text":"After"}`,
			})

			md, err := exportDiscussionPath(t, path)
			if err != nil {
				t.Fatal(err)
			}
			// The empty ToolUseMessage should be skipped.
			assertContains(t, md, "**File**: `main.go`")
			// There should be exactly one Tool use line.
			if strings.Count(md, "### 🔧 Tool:") != 1 {
				t.Errorf("expected 1 tool use, got %d", strings.Count(md, "### 🔧 Tool:"))
			}
		})

		t.Run("skips_null_tool_use_input", func(t *testing.T) {
			t.Parallel()
			path := writeJSONL(t, []string{
				metaLine("task", "pi"),
				`{"type":"tool_use","id":"t1","name":"Read","input":null}`,
				`{"type":"text","text":"done"}`,
			})

			md, err := exportDiscussionPath(t, path)
			if err != nil {
				t.Fatal(err)
			}
			assertNotContains(t, md, "### 🔧 Tool:")
		})

		t.Run("write_tool_preview", func(t *testing.T) {
			t.Parallel()
			path := writeJSONL(t, []string{
				metaLine("task", "pi"),
				`{"type":"tool_use","id":"t1","name":"Write","input":{"path":"out.txt","content":"hello world"}}`,
			})

			md, err := exportDiscussionPath(t, path)
			if err != nil {
				t.Fatal(err)
			}
			assertContains(t, md, "**File**: `out.txt`")
			assertContains(t, md, "hello world")
		})

		t.Run("read_tool_path", func(t *testing.T) {
			t.Parallel()
			path := writeJSONL(t, []string{
				metaLine("task", "pi"),
				`{"type":"tool_use","id":"t1","name":"Read","input":{"path":"main.go"}}`,
			})

			md, err := exportDiscussionPath(t, path)
			if err != nil {
				t.Fatal(err)
			}
			assertContains(t, md, "**File**: `main.go`")
		})

		t.Run("grep_tool_pattern", func(t *testing.T) {
			t.Parallel()
			path := writeJSONL(t, []string{
				metaLine("task", "pi"),
				`{"type":"tool_use","id":"t1","name":"Grep","input":{"pattern":"TODO","path":"."}}`,
			})

			md, err := exportDiscussionPath(t, path)
			if err != nil {
				t.Fatal(err)
			}
			assertContains(t, md, "**Pattern**: `TODO`")
		})

		t.Run("title", func(t *testing.T) {
			t.Parallel()
			meta := &MetaMessage{
				Prompt:    "do something",
				Title:     "My Task Title",
				Harness:   harness.Pi,
				StartedAt: time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC),
			}
			md := renderDiscussion(meta, nil, nil, nil)
			if !strings.Contains(md, "**Title**: My Task Title") {
				t.Errorf("missing title in output:\n%s", md)
			}
		})
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()

		t.Run("reader_error", func(t *testing.T) {
			t.Parallel()
			_, err := ExportDiscussion(errorReader{}, "broken.jsonl", exportParseFn)
			if err == nil {
				t.Fatal("expected error for reader failure")
			}
			if !strings.Contains(err.Error(), "scan broken.jsonl") {
				t.Errorf("error = %q, want it to mention 'scan broken.jsonl'", err.Error())
			}
		})

		t.Run("no_caic_meta_header", func(t *testing.T) {
			t.Parallel()
			path := writeJSONL(t, []string{
				`{"type":"text","text":"orphan message"}`,
			})
			_, err := exportDiscussionPath(t, path)
			if err == nil {
				t.Fatal("expected error for missing caic_meta")
			}
			if !strings.Contains(err.Error(), "no caic_meta header") {
				t.Errorf("error = %q, want it to mention 'no caic_meta header'", err.Error())
			}
		})

		t.Run("empty_file", func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "empty.jsonl")
			if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := exportDiscussionPath(t, path)
			if err == nil {
				t.Fatal("expected error for empty file")
			}
		})

		t.Run("unparseable_lines_skipped", func(t *testing.T) {
			t.Parallel()
			path := writeJSONL(t, []string{
				metaLine("task", "pi"),
				`{not valid json`,
				`{"type":"text","text":"visible"}`,
			})

			md, err := exportDiscussionPath(t, path)
			if err != nil {
				t.Fatal(err)
			}
			assertContains(t, md, "visible")
		})
	})
}

func TestIsInputEmpty(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		raw   json.RawMessage
		empty bool
	}{
		{"nil", nil, true},
		{"empty_slice", json.RawMessage{}, true},
		{"empty_obj", json.RawMessage(`{}`), true},
		{"null", json.RawMessage(`null`), true},
		{"empty_str", json.RawMessage(`""`), true},
		{"real_obj", json.RawMessage(`{"path":"x"}`), false},
		{"real_str", json.RawMessage(`"hello"`), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isInputEmpty(tt.raw); got != tt.empty {
				t.Errorf("isInputEmpty(%s) = %v, want %v", tt.raw, got, tt.empty)
			}
		})
	}
}

func TestTrunc(t *testing.T) {
	t.Parallel()

	t.Run("short", func(t *testing.T) {
		t.Parallel()
		if got := trunc("hello", 10); got != "hello" {
			t.Errorf("trunc(%q, 10) = %q, want %q", "hello", got, "hello")
		}
	})

	t.Run("exact", func(t *testing.T) {
		t.Parallel()
		if got := trunc("hello", 5); got != "hello" {
			t.Errorf("trunc(%q, 5) = %q, want %q", "hello", got, "hello")
		}
	})

	t.Run("truncated", func(t *testing.T) {
		t.Parallel()
		if got := trunc("hello world", 5); got != "hello..." {
			t.Errorf("trunc(%q, 5) = %q, want %q", "hello world", got, "hello...")
		}
	})

	t.Run("unicode", func(t *testing.T) {
		t.Parallel()
		if got := trunc("héllo", 3); got != "hél..." {
			t.Errorf("trunc(%q, 3) = %q, want %q", "héllo", got, "hél...")
		}
	})
}

func assertContains(t *testing.T, s, substr string) {
	if !strings.Contains(s, substr) {
		t.Errorf("expected output to contain %q, but it didn't.\nGot:\n%s", substr, truncForTest(s, 500))
	}
}

func assertNotContains(t *testing.T, s, substr string) {
	if strings.Contains(s, substr) {
		t.Errorf("expected output NOT to contain %q, but it did", substr)
	}
}

func truncForTest(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "... (truncated)"
}

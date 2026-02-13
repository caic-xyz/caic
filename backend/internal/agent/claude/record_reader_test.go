package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadRecords(t *testing.T) {
	t.Run("QueueOperation", func(t *testing.T) {
		input := `{"type":"queue-operation","operation":"dequeue","timestamp":"2026-02-11T13:25:20.373Z","sessionId":"7fdc580d-83db-42d9-8e19-63c61a9f89ed"}` + "\n"
		records, err := ReadRecords(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		if len(records) != 1 {
			t.Fatalf("got %d records, want 1", len(records))
		}
		if records[0].Type != TypeQueueOperation {
			t.Fatalf("got type %q, want %q", records[0].Type, TypeQueueOperation)
		}
		qo, err := records[0].AsQueueOperation()
		if err != nil {
			t.Fatal(err)
		}
		if qo.Operation != "dequeue" {
			t.Errorf("got operation %q, want %q", qo.Operation, "dequeue")
		}
		if qo.SessionID != "7fdc580d-83db-42d9-8e19-63c61a9f89ed" {
			t.Errorf("got sessionId %q", qo.SessionID)
		}
	})

	t.Run("UserSimpleContent", func(t *testing.T) {
		input := `{"parentUuid":null,"type":"user","message":{"role":"user","content":"hello world"},"uuid":"abc","timestamp":"2026-01-01T00:00:00Z","sessionId":"sess1"}` + "\n"
		records, err := ReadRecords(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		u, err := records[0].AsUser()
		if err != nil {
			t.Fatal(err)
		}
		text, ok := u.Message.ContentText()
		if !ok {
			t.Fatal("expected string content")
		}
		if text != "hello world" {
			t.Errorf("got %q", text)
		}
		if u.ParentUUID != nil {
			t.Errorf("expected nil parentUuid, got %v", *u.ParentUUID)
		}
	})

	t.Run("UserToolResult", func(t *testing.T) {
		input := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_123","content":[{"type":"text","text":"result"}]}]},"uuid":"abc","timestamp":"2026-01-01T00:00:00Z","sessionId":"sess1","parentUuid":"parent1"}` + "\n"
		records, err := ReadRecords(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		u, err := records[0].AsUser()
		if err != nil {
			t.Fatal(err)
		}
		_, ok := u.Message.ContentText()
		if ok {
			t.Fatal("expected array content, not string")
		}
		blocks, ok := u.Message.ContentBlocks()
		if !ok {
			t.Fatal("expected array content")
		}
		if len(blocks) != 1 {
			t.Fatalf("got %d blocks, want 1", len(blocks))
		}
		if blocks[0].Type != "tool_result" {
			t.Errorf("got type %q", blocks[0].Type)
		}
		if blocks[0].ToolUseID != "toolu_123" {
			t.Errorf("got tool_use_id %q", blocks[0].ToolUseID)
		}
	})

	t.Run("UserWithTodos", func(t *testing.T) {
		input := `{"type":"user","message":{"role":"user","content":"do stuff"},"uuid":"u","timestamp":"t","sessionId":"s","parentUuid":null,"todos":[{"content":"Fix bug","status":"in_progress","activeForm":"Fixing bug"}]}` + "\n"
		records, err := ReadRecords(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		u, err := records[0].AsUser()
		if err != nil {
			t.Fatal(err)
		}
		if len(u.Todos) != 1 {
			t.Fatalf("got %d todos, want 1", len(u.Todos))
		}
		if u.Todos[0].Status != "in_progress" {
			t.Errorf("got status %q", u.Todos[0].Status)
		}
	})

	t.Run("UserWithToolUseResult", func(t *testing.T) {
		input := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"done"}]}]},"uuid":"u","timestamp":"t","sessionId":"s","parentUuid":"p","toolUseResult":{"status":"completed","prompt":"do it","agentId":"a1","totalDurationMs":5000,"totalTokens":100,"totalToolUseCount":3},"sourceToolAssistantUUID":"sa1"}` + "\n"
		records, err := ReadRecords(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		u, err := records[0].AsUser()
		if err != nil {
			t.Fatal(err)
		}
		tr, ok := u.ToolUseResultObject()
		if !ok {
			t.Fatal("expected toolUseResult object")
		}
		if tr.Status != "completed" {
			t.Errorf("got status %q", tr.Status)
		}
		if tr.TotalDurationMs != 5000 {
			t.Errorf("got totalDurationMs %d", tr.TotalDurationMs)
		}
		if u.SourceToolAssistantUUID != "sa1" {
			t.Errorf("got sourceToolAssistantUUID %q", u.SourceToolAssistantUUID)
		}
	})

	t.Run("Assistant", func(t *testing.T) {
		input := `{"type":"assistant","parentUuid":"p1","message":{"id":"msg_123","type":"message","model":"claude-opus-4-6","role":"assistant","content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":20,"cache_creation_input_tokens":5,"cache_read_input_tokens":3,"service_tier":"standard"}},"uuid":"a1","timestamp":"2026-01-01T00:00:00Z","sessionId":"sess1"}` + "\n"
		records, err := ReadRecords(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		a, err := records[0].AsAssistant()
		if err != nil {
			t.Fatal(err)
		}
		if a.Message.Model != "claude-opus-4-6" {
			t.Errorf("got model %q", a.Message.Model)
		}
		if len(a.Message.Content) != 2 {
			t.Fatalf("got %d content blocks, want 2", len(a.Message.Content))
		}
		if a.Message.Content[0].Type != "text" || a.Message.Content[0].Text != "hello" {
			t.Errorf("unexpected text block: %+v", a.Message.Content[0])
		}
		if a.Message.Content[1].Type != "tool_use" || a.Message.Content[1].Name != "Bash" {
			t.Errorf("unexpected tool_use block: %+v", a.Message.Content[1])
		}
		if *a.Message.StopReason != "end_turn" {
			t.Errorf("got stop_reason %v", a.Message.StopReason)
		}
		if a.Message.Usage.InputTokens != 10 {
			t.Errorf("got input_tokens %d", a.Message.Usage.InputTokens)
		}
	})

	t.Run("AssistantThinking", func(t *testing.T) {
		input := `{"type":"assistant","message":{"id":"msg_1","type":"message","model":"claude-opus-4-6","role":"assistant","content":[{"type":"thinking","thinking":"Let me think...","signature":"abc123"}],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"service_tier":"standard"}},"uuid":"a1","timestamp":"2026-01-01T00:00:00Z","sessionId":"sess1","parentUuid":"p1"}` + "\n"
		records, err := ReadRecords(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		a, err := records[0].AsAssistant()
		if err != nil {
			t.Fatal(err)
		}
		if a.Message.Content[0].Type != "thinking" {
			t.Errorf("got type %q", a.Message.Content[0].Type)
		}
		if a.Message.Content[0].Thinking != "Let me think..." {
			t.Errorf("got thinking %q", a.Message.Content[0].Thinking)
		}
	})

	t.Run("AssistantError", func(t *testing.T) {
		input := `{"type":"assistant","message":{"id":"m1","type":"message","model":"<synthetic>","role":"assistant","content":[{"type":"text","text":"API Error: 529"}],"stop_reason":"stop_sequence","stop_sequence":"","usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"service_tier":null},"container":null,"context_management":null},"uuid":"a1","timestamp":"t","sessionId":"s","parentUuid":"p","error":"unknown","isApiErrorMessage":true}` + "\n"
		records, err := ReadRecords(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		a, err := records[0].AsAssistant()
		if err != nil {
			t.Fatal(err)
		}
		if a.Error != "unknown" {
			t.Errorf("got error %q", a.Error)
		}
		if !a.IsAPIErrorMessage {
			t.Error("expected isApiErrorMessage=true")
		}
	})

	t.Run("System", func(t *testing.T) {
		input := `{"type":"system","subtype":"compact_boundary","content":"Conversation compacted","level":"info","isMeta":false,"timestamp":"2026-01-01T00:00:00Z","uuid":"s1","sessionId":"sess1","parentUuid":null,"compactMetadata":{"trigger":"auto","preTokens":168229}}` + "\n"
		records, err := ReadRecords(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		s, err := records[0].AsSystem()
		if err != nil {
			t.Fatal(err)
		}
		if s.Subtype != "compact_boundary" {
			t.Errorf("got subtype %q", s.Subtype)
		}
		if s.CompactMetadata == nil {
			t.Fatal("expected compactMetadata")
		}
		if s.CompactMetadata.PreTokens != 168229 {
			t.Errorf("got preTokens %d", s.CompactMetadata.PreTokens)
		}
	})

	t.Run("Summary", func(t *testing.T) {
		input := `{"type":"summary","summary":"test summary","leafUuid":"leaf1"}` + "\n"
		records, err := ReadRecords(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		s, err := records[0].AsSummary()
		if err != nil {
			t.Fatal(err)
		}
		if s.Summary != "test summary" {
			t.Errorf("got summary %q", s.Summary)
		}
		if s.LeafUUID != "leaf1" {
			t.Errorf("got leafUuid %q", s.LeafUUID)
		}
	})

	t.Run("FileHistorySnapshot", func(t *testing.T) {
		input := `{"type":"file-history-snapshot","messageId":"m1","snapshot":{"messageId":"m1","trackedFileBackups":{".bashrc":{"backupFileName":"abc@v1","version":1,"backupTime":"2026-01-01T00:00:00Z"}},"timestamp":"2026-01-01T00:00:00Z"},"isSnapshotUpdate":false}` + "\n"
		records, err := ReadRecords(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		f, err := records[0].AsFileHistorySnapshot()
		if err != nil {
			t.Fatal(err)
		}
		if f.MessageID != "m1" {
			t.Errorf("got messageId %q", f.MessageID)
		}
		b, ok := f.Snapshot.TrackedFileBackups[".bashrc"]
		if !ok {
			t.Fatal("expected .bashrc backup")
		}
		if b.Version != 1 {
			t.Errorf("got version %d", b.Version)
		}
	})

	t.Run("Progress", func(t *testing.T) {
		input := `{"type":"progress","parentUuid":"p1","sessionId":"sess1","uuid":"pr1","timestamp":"2026-01-01T00:00:00Z","data":{"type":"hook_progress","hookEvent":"PostToolUse","hookName":"PostToolUse:Glob","command":"callback"},"parentToolUseID":"toolu_1","toolUseID":"toolu_1"}` + "\n"
		records, err := ReadRecords(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		p, err := records[0].AsProgress()
		if err != nil {
			t.Fatal(err)
		}
		if p.ToolUseID != "toolu_1" {
			t.Errorf("got toolUseID %q", p.ToolUseID)
		}
		pd, err := p.ProgressData()
		if err != nil {
			t.Fatal(err)
		}
		if pd.Type != "hook_progress" {
			t.Errorf("got data type %q", pd.Type)
		}
		if pd.HookEvent != "PostToolUse" {
			t.Errorf("got hookEvent %q", pd.HookEvent)
		}
	})

	t.Run("SkipsMalformed", func(t *testing.T) {
		var buf bytes.Buffer
		handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
		old := slog.Default()
		slog.SetDefault(slog.New(handler))
		defer slog.SetDefault(old)

		input := "not json\n" + `{"type":"summary","summary":"ok","leafUuid":"l"}` + "\n"
		records, err := ReadRecords(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		if len(records) != 1 {
			t.Fatalf("got %d records, want 1", len(records))
		}
		if records[0].Type != TypeSummary {
			t.Errorf("got type %q", records[0].Type)
		}
		if !strings.Contains(buf.String(), "malformed") {
			t.Errorf("expected malformed warning, got: %s", buf.String())
		}
	})

	t.Run("EmptyLines", func(t *testing.T) {
		input := "\n\n" + `{"type":"summary","summary":"ok","leafUuid":"l"}` + "\n\n"
		records, err := ReadRecords(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		if len(records) != 1 {
			t.Fatalf("got %d records, want 1", len(records))
		}
	})

	t.Run("RealFiles", func(t *testing.T) {
		base := os.ExpandEnv("$HOME/.claude/projects")
		var files []string
		err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && filepath.Ext(path) == ".jsonl" {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Skipf("skipping: %v", err)
		}
		if len(files) == 0 {
			t.Skip("no JSONL files found")
		}
		for _, path := range files {
			f, err := os.Open(filepath.Clean(path))
			if err != nil {
				t.Errorf("open %s: %v", path, err)
				continue
			}
			records, err := ReadRecords(f)
			_ = f.Close()
			if err != nil {
				t.Errorf("read %s: %v", path, err)
				continue
			}
			for i, r := range records {
				if _, err := DecodeRecord(&r); err != nil {
					t.Errorf("%s record %d (type=%q): %v", path, i, r.Type, err)
				}
			}
		}
		t.Logf("successfully parsed %d JSONL files", len(files))
	})
}

func TestDecodeRecord(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantType string
	}{
		{
			name:     "queue-operation",
			input:    `{"type":"queue-operation","operation":"dequeue","timestamp":"t","sessionId":"s"}`,
			wantType: "*claude.QueueOperation",
		},
		{
			name:     "user",
			input:    `{"type":"user","message":{"role":"user","content":"hi"},"uuid":"u","timestamp":"t","sessionId":"s","parentUuid":null}`,
			wantType: "*claude.UserRecord",
		},
		{
			name:     "assistant",
			input:    `{"type":"assistant","message":{"id":"m","type":"message","model":"m","role":"assistant","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"service_tier":"standard"}},"uuid":"u","timestamp":"t","sessionId":"s","parentUuid":"p"}`,
			wantType: "*claude.AssistantRecord",
		},
		{
			name:     "system",
			input:    `{"type":"system","subtype":"turn_duration","durationMs":1000,"timestamp":"t","uuid":"u","sessionId":"s","parentUuid":"p"}`,
			wantType: "*claude.SystemRecord",
		},
		{
			name:     "progress",
			input:    `{"type":"progress","data":{"type":"hook_progress"},"timestamp":"t","uuid":"u","sessionId":"s","parentUuid":"p"}`,
			wantType: "*claude.ProgressRecord",
		},
		{
			name:     "summary",
			input:    `{"type":"summary","summary":"s","leafUuid":"l"}`,
			wantType: "*claude.SummaryRecord",
		},
		{
			name:     "file-history-snapshot",
			input:    `{"type":"file-history-snapshot","messageId":"m","snapshot":{"messageId":"m","trackedFileBackups":{},"timestamp":"t"},"isSnapshotUpdate":false}`,
			wantType: "*claude.FileHistorySnapshotRecord",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			records, err := ReadRecords(strings.NewReader(tt.input + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			result, err := DecodeRecord(&records[0])
			if err != nil {
				t.Fatal(err)
			}
			got := fmt.Sprintf("%T", result)
			if got != tt.wantType {
				t.Errorf("got type %s, want %s", got, tt.wantType)
			}
		})
	}

	t.Run("UnknownType", func(t *testing.T) {
		var buf bytes.Buffer
		handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
		old := slog.Default()
		slog.SetDefault(slog.New(handler))
		defer slog.SetDefault(old)

		input := `{"type":"brand-new-type","data":"something"}` + "\n"
		records, err := ReadRecords(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		result, err := DecodeRecord(&records[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := result.(*Record); !ok {
			t.Errorf("expected *Record for unknown type, got %T", result)
		}
		if !strings.Contains(buf.String(), "unknown record type") {
			t.Errorf("expected unknown record type warning, got: %s", buf.String())
		}
	})
}

func TestUnknownFields(t *testing.T) {
	t.Run("TopLevel", func(t *testing.T) {
		var buf bytes.Buffer
		handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
		old := slog.Default()
		slog.SetDefault(slog.New(handler))
		defer slog.SetDefault(old)

		input := `{"type":"queue-operation","operation":"enqueue","timestamp":"2026-01-01T00:00:00Z","sessionId":"s1","newField":"surprise","anotherNew":42}` + "\n"
		records, err := ReadRecords(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		qo, err := records[0].AsQueueOperation()
		if err != nil {
			t.Fatal(err)
		}
		if qo.Operation != "enqueue" {
			t.Errorf("got operation %q", qo.Operation)
		}
		if len(qo.Extra) != 2 {
			t.Fatalf("got %d extra fields, want 2: %v", len(qo.Extra), qo.Extra)
		}
		if _, ok := qo.Extra["newField"]; !ok {
			t.Error("missing newField in Extra")
		}
		if _, ok := qo.Extra["anotherNew"]; !ok {
			t.Error("missing anotherNew in Extra")
		}
		logged := buf.String()
		if !strings.Contains(logged, "unknown fields") {
			t.Errorf("expected warning log, got: %s", logged)
		}
		if !strings.Contains(logged, "anotherNew") {
			t.Errorf("expected field name in log, got: %s", logged)
		}
	})

	t.Run("Nested", func(t *testing.T) {
		var buf bytes.Buffer
		handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
		old := slog.Default()
		slog.SetDefault(slog.New(handler))
		defer slog.SetDefault(old)

		input := `{"type":"assistant","message":{"id":"m1","type":"message","model":"test","role":"assistant","content":[{"type":"text","text":"hi","futureField":true}],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"service_tier":"standard","newMetric":99}},"uuid":"a1","timestamp":"2026-01-01T00:00:00Z","sessionId":"s1","parentUuid":"p1"}` + "\n"
		records, err := ReadRecords(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		a, err := records[0].AsAssistant()
		if err != nil {
			t.Fatal(err)
		}
		if len(a.Message.Content[0].Extra) != 1 {
			t.Errorf("expected 1 extra in ContentBlock, got %d", len(a.Message.Content[0].Extra))
		}
		if len(a.Message.Usage.Extra) != 1 {
			t.Errorf("expected 1 extra in Usage, got %d", len(a.Message.Usage.Extra))
		}
		logged := buf.String()
		if !strings.Contains(logged, "futureField") {
			t.Errorf("expected futureField warning, got: %s", logged)
		}
		if !strings.Contains(logged, "newMetric") {
			t.Errorf("expected newMetric warning, got: %s", logged)
		}
	})

	t.Run("NoExtraForKnownData", func(t *testing.T) {
		input := `{"type":"queue-operation","operation":"dequeue","timestamp":"t","sessionId":"s"}` + "\n"
		records, err := ReadRecords(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		qo, err := records[0].AsQueueOperation()
		if err != nil {
			t.Fatal(err)
		}
		if len(qo.Extra) != 0 {
			t.Errorf("expected no extra fields, got %v", qo.Extra)
		}
	})

	t.Run("PreservedInExtra", func(t *testing.T) {
		input := `{"type":"queue-operation","operation":"dequeue","timestamp":"t","sessionId":"s","futureVersion":{"nested":"value"}}` + "\n"
		records, err := ReadRecords(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		qo, err := records[0].AsQueueOperation()
		if err != nil {
			t.Fatal(err)
		}
		raw, ok := qo.Extra["futureVersion"]
		if !ok {
			t.Fatal("expected futureVersion in Extra")
		}
		var nested map[string]string
		if err := json.Unmarshal(raw, &nested); err != nil {
			t.Fatal(err)
		}
		if nested["nested"] != "value" {
			t.Errorf("got nested value %q", nested["nested"])
		}
	})
}

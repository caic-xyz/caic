// Tests for voice gateway transcript and tool activity log persistence.

package voicertc

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	voicev1 "github.com/caic-xyz/caic/gomode/voicegateway/api/v1"
)

func TestActivityLog(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	log, err := openActivityLog(dir, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := log.close(); err != nil {
			t.Fatal(err)
		}
	})

	for _, event := range []struct {
		source  activityLogSource
		message string
	}{
		{activityLogSourceClient, `{"kind":"session.setup","context":{"systemInstruction":"Initial context","text":"Current task"}}`},
		{activityLogSourceGateway, `{"kind":"transcript.delta","speaker":"user","text":"hello"}`},
		{activityLogSourceGateway, `{"kind":"tool.call","id":"call-1","name":"tasks_list","args":{}}`},
		{activityLogSourceClient, `{"kind":"tool.result","id":"call-1","name":"tasks_list","result":{"tasks":[]}}`},
		{activityLogSourceGateway, `{"kind":"speech.ended","speaker":"assistant"}`},
	} {
		if err := log.record(event.source, []byte(event.message)); err != nil {
			t.Fatal(err)
		}
	}

	if err := log.close(); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(dir, "????-??-??-??-??-session-1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("activity log paths = %v, want one timestamped session log", paths)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"src":"client"`)) || bytes.Contains(data, []byte(`"dir"`)) {
		t.Fatalf("log fields = %s, want src without dir", data)
	}
	var records []activityLogRecord
	for line := range bytes.SplitSeq(data, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var record activityLogRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if len(records) != 4 {
		t.Fatalf("records = %d, want 4", len(records))
	}
	if records[0].Source != activityLogSourceClient || records[0].Kind != voicev1.MessageKindSessionSetup || !bytes.Contains(records[0].Message, []byte("Initial context")) {
		t.Fatalf("first record = %+v, want initial context", records[0])
	}
	if records[1].Kind != voicev1.MessageKindTranscriptDelta {
		t.Fatalf("second record = %+v, want user transcript", records[1])
	}
	if records[2].Kind != voicev1.MessageKindToolCall || records[3].Kind != voicev1.MessageKindToolResult || records[3].Source != activityLogSourceClient {
		t.Fatalf("tool records = %+v, want call then client result", records[2:])
	}
}

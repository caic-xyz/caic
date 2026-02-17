package claude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
)

// ReadRecords reads all JSONL records from r.
// Each line is parsed as a Record. Malformed lines are logged and skipped.
func ReadRecords(r io.Reader) ([]Record, error) {
	var records []Record
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024) // up to 10 MB per line
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			slog.Warn("skipping malformed JSONL line", "line", lineNo, "error", err)
			continue
		}
		records = append(records, rec)
	}
	if err := scanner.Err(); err != nil {
		return records, fmt.Errorf("reading JSONL: %w", err)
	}
	return records, nil
}

// DecodeRecord fully decodes a Record into its concrete type.
// Returns one of: *QueueOperation, *UserRecord, *AssistantRecord,
// *SystemRecord, *ProgressRecord, *SummaryRecord, *FileHistorySnapshotRecord.
// For unknown record types, the raw JSON is logged and returned as-is.
func DecodeRecord(r *Record) (any, error) {
	switch r.Type {
	case TypeQueueOperation:
		return r.AsQueueOperation()
	case TypeUser:
		return r.AsUser()
	case TypeAssistant:
		return r.AsAssistant()
	case TypeSystem:
		return r.AsSystem()
	case TypeProgress:
		return r.AsProgress()
	case TypeSummary:
		return r.AsSummary()
	case TypeFileHistorySnapshot:
		return r.AsFileHistorySnapshot()
	default:
		slog.Warn("unknown record type", "type", r.Type) //nolint:gosec // structured logging, no injection
		return r, nil
	}
}

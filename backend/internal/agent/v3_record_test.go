// Focused tests and benchmarks for strict canonical v3 task-log record decoding.

package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/taskslog"

	logdata "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
)

func TestInputDeliveryTimestamp(t *testing.T) {
	t.Parallel()
	for name, message := range map[string]*SystemMessage{
		"started":   InputDeliveryStarted(),
		"completed": InputDeliveryCompleted(),
		"failed":    InputDeliveryFailed(errors.New("closed")),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if message.Ts == 0 {
				t.Fatal("input delivery message has no timestamp")
			}
			encoded, err := MarshalLogMessage(message)
			if err != nil {
				t.Fatal(err)
			}
			frame, err := taskslog.DecodeFrame(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if frame.Kind != taskslog.KindInputDelivery && frame.Kind != taskslog.KindInputDeliveryFailed {
				t.Fatalf("kind = %q", frame.Kind)
			}
			if frame.ProducerTime.IsZero() {
				t.Fatal("input delivery timestamp was not decoded")
			}
		})
	}
}

func TestHostDiffStatTimestamp(t *testing.T) {
	t.Parallel()
	msg := NewDiffStatMessage(logdata.DiffStat{{Path: "main.go", LinesAdded: 1}}, nil)
	if msg.Ts == 0 {
		t.Fatal("host Git summary has no timestamp")
	}
	encoded, err := MarshalLogMessage(msg)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := taskslog.DecodeFrame(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Kind != taskslog.KindGitSummary || frame.ProducerTime.IsZero() {
		t.Fatalf("frame = %+v", frame)
	}
}

type v3AgentRecordFixture struct {
	ReaderCases    []v3ReaderCase    `json:"reader_cases"`
	EncoderVectors []v3EncoderVector `json:"encoder_vectors"`
}

type v3ReaderCase struct {
	Name        string `json:"name"`
	Timestamp   string `json:"timestamp"`
	NativeBytes string `json:"native_bytes"`
	RecordBytes string `json:"record_bytes"`
}

type v3EncoderVector struct {
	Name              string `json:"name"`
	ObservedUnixNS    string `json:"observed_unix_ns"`
	ExpectedTimestamp string `json:"expected_timestamp"`
	NativeBytes       string `json:"native_bytes"`
	RecordBytes       string `json:"record_bytes"`
}

func TestHostGitSummaryDoesNotAdvanceRelayPosition(t *testing.T) {
	t.Parallel()

	encoded, err := MarshalLogMessage(&DiffStatMessage{
		MessageType: "caic_diff_stat",
		DiffStat:    logdata.DiffStat{{Path: "main.go", LinesAdded: 2}},
		Repos:       []logdata.RepoState{{RepoIndex: 0, Branch: "main"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	parser, err := NewLogRecordParser(&testMessageParser{output: testParseFn})
	if err != nil {
		t.Fatal(err)
	}
	record, err := parser.ParseRecord(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if record.Provenance.Source == RecordSourceRelay || !record.Control || len(record.Messages) != 1 {
		t.Fatalf("host summary = %+v, want one control outside relay history", record)
	}
	summary, ok := record.Messages[0].Message.(*DiffStatMessage)
	if !ok || len(summary.DiffStat) != 1 || summary.DiffStat[0].LinesAdded != 2 || len(summary.Repos) != 1 || summary.Repos[0].Branch != "main" {
		t.Fatalf("restored summary = %+v", record.Messages[0].Message)
	}
}

func assertV3FixtureRecord(t *testing.T, name, timestamp, nativeBytes, recordBytes string) {
	if name == "" || timestamp == "" || nativeBytes == "" || recordBytes == "" {
		t.Fatalf("fixture record has an empty required field: name=%q timestamp=%q native=%q record=%q", name, timestamp, nativeBytes, recordBytes)
	}
	if !strings.HasSuffix(recordBytes, "\n") || strings.Count(recordBytes, "\n") != 1 {
		t.Fatalf("record must contain exactly one terminating LF: %q", recordBytes)
	}
	wantRecord := `{"t":"agent","ts":` + timestamp + `,"msg":` + nativeBytes + "}\n"
	if recordBytes != wantRecord {
		t.Fatalf("record bytes = %q, want %q", recordBytes, wantRecord)
	}
	frame, err := taskslog.DecodeFrame([]byte(strings.TrimSuffix(recordBytes, "\n")))
	wantTime := frame.ProducerTime
	if err != nil {
		t.Fatalf("timestamp %q is not canonical: %v", timestamp, err)
	}

	// These same literal encoder vectors are exercised by the maintained
	// Python relay, pinning both Go envelopes to the relay's physical bytes.
	encoded, err := (&logdata.NativeRecord{Type: "agent", Time: wantTime, Message: []byte(nativeBytes)}).MarshalJSON()
	if err != nil || string(encoded)+"\n" != recordBytes {
		t.Fatalf("v3 encoded %q: %v", encoded, err)
	}
	encoded, err = (&logdata.NativeRecord{Type: "input", Time: wantTime, Message: []byte(nativeBytes)}).MarshalJSON()
	wantInput := strings.Replace(recordBytes, `"t":"agent"`, `"t":"input"`, 1)
	if err != nil || string(encoded)+"\n" != wantInput {
		t.Fatalf("v3 input encoded %q: %v", encoded, err)
	}

	calls := 0
	p, err := NewLogRecordParser(&testMessageParser{output: func(msg []byte) ([]Message, error) {
		calls++
		if string(msg) != nativeBytes {
			return nil, fmt.Errorf("native payload = %q, want %q", msg, nativeBytes)
		}
		return []Message{&TextMessage{Text: name}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	line := []byte(strings.TrimSuffix(recordBytes, "\n"))
	msgs, err := p.ParseRecord(line)
	if err != nil {
		t.Fatal(err)
	}
	if msgs.Control || calls != 1 || len(msgs.Messages) != 1 || !msgs.Messages[0].ProducerTime.Equal(wantTime) {
		t.Fatalf("calls = %d, record = %#v, want producer time %v", calls, msgs, wantTime)
	}
}

func TestV3AgentRecord(t *testing.T) {
	t.Parallel()

	t.Run("canonical_values", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name    string
			payload string
		}{
			{name: "object", payload: `{"type":"native","nested":{"items":[1,true,"x"]}}`},
			{name: "array", payload: `[1,{"key":"value"},false]`},
			{name: "escaped_string", payload: `"quote: \" slash: \\ delimiter: ,\"msg\":"`},
			{name: "number", payload: `-12.50e+2`},
			{name: "boolean", payload: `true`},
			{name: "internal_whitespace", payload: "{\t\"type\" : \"native\", \r\"value\" : [ 1, 2 ] }"},
			{name: "diagnostic_string", payload: `"invalid native JSON: {not-json}"`},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				calls := 0
				var aliased bool
				record := []byte(`{"t":"agent","ts":1700000000.123,"msg":` + tc.payload + `}`)
				payloadStart := len(record) - 1 - len(tc.payload)
				p, err := NewLogRecordParser(&testMessageParser{output: func(msg []byte) ([]Message, error) {
					calls++
					aliased = len(msg) != 0 && &msg[0] == &record[payloadStart]
					if string(msg) != tc.payload {
						return nil, fmt.Errorf("native payload = %q, want %q", msg, tc.payload)
					}
					return []Message{&RawMessage{MessageType: "native"}}, nil
				}})
				if err != nil {
					t.Fatal(err)
				}
				msgs, err := p.ParseRecord(record)
				if err != nil {
					t.Fatal(err)
				}
				if calls != 1 {
					t.Fatalf("native callback count = %d, want 1", calls)
				}
				if !aliased {
					t.Fatal("native payload did not alias the input record")
				}
				wantTime := time.Unix(1_700_000_000, 123_000_000).UTC()
				if msgs.Control || len(msgs.Messages) != 1 || !msgs.Messages[0].ProducerTime.Equal(wantTime) {
					t.Fatalf("parsed record = %#v, want producer time %v", msgs, wantTime)
				}
			})
		}
	})

	t.Run("fixture", func(t *testing.T) {
		t.Parallel()
		data, err := os.ReadFile("testdata/v3_agent_records.json")
		if err != nil {
			t.Fatal(err)
		}
		var fixture v3AgentRecordFixture
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&fixture); err != nil {
			t.Fatal(err)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			t.Fatalf("trailing fixture data: %v", err)
		}
		if len(fixture.ReaderCases) == 0 {
			t.Fatal("fixture reader_cases is empty")
		}
		if len(fixture.EncoderVectors) == 0 {
			t.Fatal("fixture encoder_vectors is empty")
		}

		readerNames := make(map[string]struct{}, len(fixture.ReaderCases))
		for _, tc := range fixture.ReaderCases {
			if _, duplicate := readerNames[tc.Name]; duplicate {
				t.Fatalf("duplicate reader case name %q", tc.Name)
			}
			readerNames[tc.Name] = struct{}{}
			t.Run("reader_"+tc.Name, func(t *testing.T) {
				t.Parallel()
				assertV3FixtureRecord(t, tc.Name, tc.Timestamp, tc.NativeBytes, tc.RecordBytes)
			})
		}

		requiredVectors := map[string]struct {
			observedUnixNS    string
			expectedTimestamp string
		}{
			"above_half":   {observedUnixNS: "1234501000", expectedTimestamp: "1.235"},
			"below_half":   {observedUnixNS: "1234499000", expectedTimestamp: "1.234"},
			"exact_half":   {observedUnixNS: "1234500000", expectedTimestamp: "1.235"},
			"second_carry": {observedUnixNS: "1999500000", expectedTimestamp: "2.000"},
		}
		seen := make(map[string]struct{}, len(fixture.EncoderVectors))
		for _, vector := range fixture.EncoderVectors {
			want, ok := requiredVectors[vector.Name]
			if !ok {
				t.Fatalf("unexpected encoder vector name %q", vector.Name)
			}
			if _, duplicate := seen[vector.Name]; duplicate {
				t.Fatalf("duplicate encoder vector name %q", vector.Name)
			}
			seen[vector.Name] = struct{}{}
			if vector.ObservedUnixNS != want.observedUnixNS || vector.ExpectedTimestamp != want.expectedTimestamp {
				t.Fatalf(
					"encoder vector %q = (%q, %q), want (%q, %q)",
					vector.Name,
					vector.ObservedUnixNS,
					vector.ExpectedTimestamp,
					want.observedUnixNS,
					want.expectedTimestamp,
				)
			}
			observed, err := strconv.ParseInt(vector.ObservedUnixNS, 10, 64)
			if err != nil || observed <= 0 || strconv.FormatInt(observed, 10) != vector.ObservedUnixNS {
				t.Fatalf("encoder vector %q has invalid observed_unix_ns %q", vector.Name, vector.ObservedUnixNS)
			}
			t.Run("encoder_"+vector.Name, func(t *testing.T) {
				t.Parallel()
				assertV3FixtureRecord(
					t,
					vector.Name,
					vector.ExpectedTimestamp,
					vector.NativeBytes,
					vector.RecordBytes,
				)
			})
		}
		if len(seen) != len(requiredVectors) {
			t.Fatalf("encoder vector names = %v, want all %v", seen, requiredVectors)
		}
	})

	t.Run("control_discriminators", func(t *testing.T) {
		t.Parallel()
		p, err := NewLogRecordParser(&testMessageParser{output: func([]byte) ([]Message, error) {
			return []Message{&UsageMessage{}}, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		meta := []byte(`{"t":"caic_meta","version":3,"prompt":"p","repos":[],"harness":"claude"}`)
		msgs, err := p.ParseRecord(meta)
		if err != nil {
			t.Fatal(err)
		}
		if !msgs.Control || len(msgs.Messages) != 1 {
			t.Fatalf("meta record = %#v", msgs)
		}
		parsedMeta, ok := msgs.Messages[0].Message.(*MetaMessage)
		if !ok || parsedMeta.MessageType != "caic_meta" || parsedMeta.Version != 3 {
			t.Fatalf("meta message = %#v", msgs.Messages[0].Message)
		}
		msgs, err = p.ParseRecord([]byte(`{"t":"caic_meta","version":3,"prompt":"p","repos":[],"harness":"claude","model":"gpt-5.6","effort":"high"}`))
		if err != nil {
			t.Fatal(err)
		}
		parsedMeta, ok = msgs.Messages[0].Message.(*MetaMessage)
		if !ok || parsedMeta.RequestedModel != "gpt-5.6" || parsedMeta.RequestedEffort != "high" {
			t.Fatalf("v3 meta settings = %#v", msgs.Messages[0].Message)
		}
		msgs, err = p.ParseRecord([]byte(`{"t":"session","session_id":"s","model":"gpt-5.6","agent_version":"1"}`))
		if err != nil {
			t.Fatal(err)
		}
		init, ok := msgs.Messages[0].Message.(*MetaSessionMessage)
		if !ok || init.ReportedModel != "gpt-5.6" {
			t.Fatalf("v3 session = %#v", msgs.Messages[0].Message)
		}
		if _, err := p.ParseRecord([]byte(`{"t":"caic_meta","type":"caic_meta","version":3,"prompt":"p","repos":[],"harness":"claude"}`)); err == nil {
			t.Fatal("v3 meta accepted the top-level type field")
		}
		if _, err := p.ParseRecord([]byte(`{"t":"caic_meta","version":3,"prompt":"p","repos":[],"harness":"claude","unknown":true}`)); err == nil || !strings.Contains(err.Error(), `json: unknown field "unknown"`) {
			t.Fatalf("v3 meta error = %v, want strict unknown-field error", err)
		}
		if _, err := p.ParseRecord([]byte(`{"t":"model_info","context_window":1000000}`)); err != nil {
			t.Fatal(err)
		}
		msgs, err = p.ParseRecord([]byte(`{"t":"agent","ts":1.000,"msg":{}}`))
		if err != nil {
			t.Fatal(err)
		}
		usage, ok := msgs.Messages[0].Message.(*UsageMessage)
		if msgs.Control || !ok || usage.ContextWindow != 1000000 {
			t.Fatalf("usage after t control = %#v", msgs)
		}
	})

	t.Run("callback_results", func(t *testing.T) {
		t.Parallel()
		t.Run("one_to_many", func(t *testing.T) {
			t.Parallel()
			p, err := NewLogRecordParser(&testMessageParser{output: func([]byte) ([]Message, error) {
				return []Message{
					&TextMessage{Text: "one"},
					&ToolUseMessage{ToolUseID: "tool"},
					&ToolResultMessage{ToolUseID: "tool"},
				}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			msgs, err := p.ParseRecord([]byte(`{"t":"agent","ts":1.001,"msg":{}}`))
			if err != nil {
				t.Fatal(err)
			}
			wantTime := time.Unix(1, int64(time.Millisecond)).UTC()
			if msgs.Control || len(msgs.Messages) != 3 {
				t.Fatalf("record = %#v, want three agent messages", msgs)
			}
			for i, msg := range msgs.Messages {
				if !msg.ProducerTime.Equal(wantTime) {
					t.Fatalf("message %d producer time = %v, want %v", i, msg.ProducerTime, wantTime)
				}
			}
		})
		t.Run("zero", func(t *testing.T) {
			t.Parallel()
			calls := 0
			p, err := NewLogRecordParser(&testMessageParser{output: func([]byte) ([]Message, error) {
				calls++
				return nil, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			msgs, err := p.ParseRecord([]byte(`{"t":"agent","ts":0.001,"msg":false}`))
			if err != nil {
				t.Fatal(err)
			}
			if msgs.Control || calls != 1 || len(msgs.Messages) != 0 {
				t.Fatalf("calls = %d, record = %#v", calls, msgs)
			}
		})
	})

	t.Run("rejections", func(t *testing.T) {
		t.Parallel()
		invalidUTF8 := append([]byte(`{"t":"agent","ts":1.000,"msg":"`), 0xff)
		invalidUTF8 = append(invalidUTF8, []byte(`"}`)...)
		cases := []struct {
			name      string
			line      []byte
			wantCalls int
		}{
			{name: "null", line: []byte(`{"t":"agent","ts":1.000,"msg":null}`)},
			{name: "malformed_utf8", line: invalidUTF8},
			{name: "malformed_json", line: []byte(`{"t":"agent","ts":1.000,"msg":{"x":}}`)},
			{name: "leading_outer_space", line: []byte(` {"t":"agent","ts":1.000,"msg":{}}`)},
			{name: "trailing_outer_space", line: []byte(`{"t":"agent","ts":1.000,"msg":{}} `)},
			{name: "leading_msg_space", line: []byte(`{"t":"agent","ts":1.000,"msg": {}}`)},
			{name: "trailing_msg_space", line: []byte(`{"t":"agent","ts":1.000,"msg":{} }`)},
			{name: "raw_lf", line: []byte("{\"t\":\"agent\",\"ts\":1.000,\"msg\":[1,\n2]}")},
			{name: "integer_timestamp", line: []byte(`{"t":"agent","ts":1,"msg":{}}`)},
			{name: "one_fraction_digit", line: []byte(`{"t":"agent","ts":1.0,"msg":{}}`)},
			{name: "two_fraction_digits", line: []byte(`{"t":"agent","ts":1.00,"msg":{}}`)},
			{name: "four_fraction_digits", line: []byte(`{"t":"agent","ts":1.0000,"msg":{}}`)},
			{name: "positive_sign", line: []byte(`{"t":"agent","ts":+1.000,"msg":{}}`)},
			{name: "negative_sign", line: []byte(`{"t":"agent","ts":-1.000,"msg":{}}`)},
			{name: "exponent_timestamp", line: []byte(`{"t":"agent","ts":1e0,"msg":{}}`)},
			{name: "leading_zero_timestamp", line: []byte(`{"t":"agent","ts":01.000,"msg":{}}`)},
			{name: "zero_timestamp", line: []byte(`{"t":"agent","ts":0.000,"msg":{}}`)},
			{name: "integer_overflow", line: []byte(`{"t":"agent","ts":9223372036854775808.000,"msg":{}}`)},
			{name: "time_range_overflow", line: []byte(`{"t":"agent","ts":9223372036854775807.000,"msg":{}}`)},
			{name: "missing_discriminator", line: []byte(`{"ts":1.000,"msg":{}}`)},
			{name: "unknown_t", line: []byte(`{"t":"future","ts":1.000,"msg":{}}`)},
			{name: "old_type_agent", line: []byte(`{"type":"agent","ts":1.000,"msg":{}}`)},
			{name: "old_type_control", line: []byte(`{"type":"diff_stat","diff_stat":[]}`)},
			{name: "old_type_meta", line: []byte(`{"type":"caic_meta","version":3,"prompt":"p","repos":[],"harness":"claude"}`)},
			{name: "type_null", line: []byte(`{"t":"diff_stat","type":null,"diff_stat":[]}`)},
			{name: "type_boolean", line: []byte(`{"t":"diff_stat","type":true,"diff_stat":[]}`)},
			{name: "type_number", line: []byte(`{"t":"diff_stat","type":1,"diff_stat":[]}`)},
			{name: "type_object", line: []byte(`{"t":"diff_stat","type":{},"diff_stat":[]}`)},
			{name: "type_array", line: []byte(`{"t":"diff_stat","type":[],"diff_stat":[]}`)},
			{name: "t_and_type_control", line: []byte(`{"t":"diff_stat","type":"diff_stat","diff_stat":[]}`)},
			{name: "case_variant_t", line: []byte(`{"T":"diff_stat","diff_stat":[]}`)},
			{name: "duplicate_control_t", line: []byte(`{"t":"future","t":"diff_stat","diff_stat":[]}`)},
			{name: "duplicate_control_field", line: []byte(`{"t":"diff_stat","diff_stat":[],"diff_stat":[]}`)},
			{name: "unknown_control_field", line: []byte(`{"t":"diff_stat","unexpected":1,"diff_stat":[]}`)},
			{name: "bare_native", line: []byte(`{"type":"assistant"}`)},
			{name: "missing_timestamp", line: []byte(`{"t":"agent","msg":{}}`)},
			{name: "missing_message", line: []byte(`{"t":"agent","ts":1.000}`)},
			{name: "reordered_fields", line: []byte(`{"ts":1.000,"t":"agent","msg":{}}`)},
			{name: "duplicate_t", line: []byte(`{"t":"agent","t":"agent","ts":1.000,"msg":{}}`)},
			{name: "duplicate_timestamp", line: []byte(`{"t":"agent","ts":1.000,"ts":1.000,"msg":{}}`)},
			{name: "duplicate_message", line: []byte(`{"t":"agent","ts":1.000,"msg":{},"msg":{}}`)},
			{name: "extra_field", line: []byte(`{"t":"agent","ts":1.000,"msg":{},"extra":true}`)},
			{name: "trailing_data", line: []byte(`{"t":"agent","ts":1.000,"msg":{}}true`)},
			{name: "delimiter_spoof", line: []byte(`{"t":"agent","ts":"1.000,\"msg\":{}","msg":{}}`)},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				calls := 0
				p, err := NewLogRecordParser(&testMessageParser{output: func([]byte) ([]Message, error) {
					calls++
					return nil, nil
				}})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := p.ParseRecord(tc.line); err == nil {
					t.Fatal("expected corruption error")
				}
				if calls != tc.wantCalls {
					t.Fatalf("native callback count = %d, want %d", calls, tc.wantCalls)
				}
			})
		}
	})

	t.Run("permissive_callback_cannot_accept_malformed_msg", func(t *testing.T) {
		t.Parallel()
		calls := 0
		p, err := NewLogRecordParser(&testMessageParser{output: func([]byte) ([]Message, error) {
			calls++
			return nil, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.ParseRecord([]byte(`{"t":"agent","ts":1.000,"msg":{"x":}}`)); err == nil {
			t.Fatal("malformed native payload was accepted")
		}
		if calls != 0 {
			t.Fatalf("native callback count = %d, want 0", calls)
		}
	})

	t.Run("encoded_size", func(t *testing.T) {
		t.Parallel()
		prefix := []byte(`{"t":"agent","ts":1.000,"msg":"`)
		suffix := []byte(`"}`)
		payloadLen := (32 << 20) - 2 - len(prefix) - len(suffix)
		line := make([]byte, 0, (32<<20)-2)
		line = append(line, prefix...)
		line = append(line, bytes.Repeat([]byte{'a'}, payloadLen)...)
		line = append(line, suffix...)
		calls := 0
		p, err := NewLogRecordParser(&testMessageParser{output: func([]byte) ([]Message, error) {
			calls++
			return nil, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.ParseRecord(line); err != nil {
			t.Fatalf("maximum accepted record: %v", err)
		}
		if calls != 1 {
			t.Fatalf("maximum accepted callback count = %d, want 1", calls)
		}
		line = append(line[:len(line)-len(suffix)], 'a')
		line = append(line, suffix...)
		if _, err := p.ParseRecord(line); err == nil || !strings.Contains(err.Error(), "encoded size") {
			t.Fatalf("oversized record error = %v", err)
		}
		if calls != 1 {
			t.Fatalf("oversized record invoked callback; count = %d", calls)
		}
	})

	t.Run("state_is_atomic", func(t *testing.T) {
		t.Parallel()
		action := &PendingUserActionMessage{
			MessageType: PendingUserActionMessageType,
			Action: logdata.PendingUserAction{
				Kind: PendingUserActionAskUserQuestion, RequestID: "request", ToolUseID: "tool",
			},
		}
		batch := []Message{action, nil}
		p, err := NewLogRecordParser(&testMessageParser{output: func([]byte) ([]Message, error) {
			return batch, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		line := []byte(`{"t":"agent","ts":1.000,"msg":{}}`)
		if _, err := p.ParseRecord(line); err == nil || !strings.Contains(err.Error(), "native message 1 is nil") {
			t.Fatalf("invalid batch error = %v", err)
		}
		batch = []Message{action}
		msgs, err := p.ParseRecord(line)
		if err != nil {
			t.Fatal(err)
		}
		if msgs.Control || len(msgs.Messages) != 1 || msgs.Messages[0].Message != action {
			t.Fatalf("rejected batch changed parser state: %#v", msgs)
		}
	})

	t.Run("callback_error", func(t *testing.T) {
		t.Parallel()
		wantErr := errors.New("native failure")
		calls := 0
		p, err := NewLogRecordParser(&testMessageParser{output: func([]byte) ([]Message, error) {
			calls++
			return nil, wantErr
		}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.ParseRecord([]byte(`{"t":"agent","ts":1.000,"msg":{}}`))
		if !errors.Is(err, wantErr) || calls != 1 {
			t.Fatalf("error = %v, calls = %d", err, calls)
		}
	})
}

func TestHistoricalContextClearedRecord(t *testing.T) {
	t.Parallel()
	// RestartSession wrote this exact SystemMessage projection in both formats.
	const historical = `{"session_id":"","subtype":"context_cleared","t":"context_cleared","uuid":""}`

	encoded, err := MarshalLogMessage(ContextCleared())
	if err != nil || string(encoded) != historical {
		t.Fatalf("historical writer = %s, %v", encoded, err)
	}
	p, err := NewLogRecordParser(&testMessageParser{output: func([]byte) ([]Message, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	record, err := p.ParseRecord([]byte(historical))
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Messages) != 1 {
		t.Fatalf("restored marker = %#v", record)
	}
	marker, ok := record.Messages[0].Message.(*SystemMessage)
	if !ok || marker.Subtype != "context_cleared" {
		t.Fatalf("restored marker = %#v", record)
	}
	if _, err := p.ParseRecord([]byte(`{"t":"context_cleared","unrelated":true}`)); err == nil {
		t.Fatal("unknown field accepted")
	}
	for _, bad := range []string{`{"t":"context_cleared","session_id":false}`, `{"t":"context_cleared","subtype":"unrelated"}`} {
		if _, err := p.ParseRecord([]byte(bad)); err == nil {
			t.Fatalf("malformed marker accepted: %s", bad)
		}
	}
}

type benchmarkNativeMessage struct {
	Type string `json:"type"`
}

func benchmarkNativeParser(line []byte) ([]Message, error) {
	var msg benchmarkNativeMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil, err
	}
	return nil, nil
}

func BenchmarkV3AgentRecord(b *testing.B) {
	cases := []struct {
		name    string
		payload []byte
	}{
		{name: "small", payload: []byte(`{"type":"delta","text":"hello"}`)},
		{name: "nested", payload: []byte(`{"type":"event","data":{"items":[1,true,{"escaped":"quote: \""}]}}`)},
		{
			name:    "large_string",
			payload: fmt.Appendf(nil, `{"type":"tool_result","content":"%s"}`, bytes.Repeat([]byte{'x'}, 64<<10)),
		},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			p, err := NewLogRecordParser(&testMessageParser{output: benchmarkNativeParser})
			if err != nil {
				b.Fatal(err)
			}
			record := make([]byte, 0, len(tc.payload)+64)
			record = append(record, `{"t":"agent","ts":1700000000.123,"msg":`...)
			record = append(record, tc.payload...)
			record = append(record, '}')
			b.ReportAllocs()
			b.SetBytes(int64(len(record) + 1))
			for b.Loop() {
				if _, err := p.ParseRecord(record); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

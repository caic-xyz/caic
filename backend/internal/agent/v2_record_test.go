// Focused tests for strict canonical v2 task-log record decoding.

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
)

type v2AgentRecordFixture struct {
	ReaderCases    []v2ReaderCase    `json:"reader_cases"`
	EncoderVectors []v2EncoderVector `json:"encoder_vectors"`
}

type v2ReaderCase struct {
	Name        string `json:"name"`
	Timestamp   string `json:"timestamp"`
	NativeBytes string `json:"native_bytes"`
	RecordBytes string `json:"record_bytes"`
}

type v2EncoderVector struct {
	Name              string `json:"name"`
	ObservedUnixNS    string `json:"observed_unix_ns"`
	ExpectedTimestamp string `json:"expected_timestamp"`
	NativeBytes       string `json:"native_bytes"`
	RecordBytes       string `json:"record_bytes"`
}

func assertV2FixtureRecord(t *testing.T, name, timestamp, nativeBytes, recordBytes string) {
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
	wantTime, err := parseV2ProducerTime([]byte(timestamp))
	if err != nil {
		t.Fatalf("timestamp %q is not canonical: %v", timestamp, err)
	}

	calls := 0
	p, err := NewLogRecordParser(LogVersionV2, func(msg []byte) ([]Message, error) {
		calls++
		if string(msg) != nativeBytes {
			return nil, fmt.Errorf("native payload = %q, want %q", msg, nativeBytes)
		}
		return []Message{&TextMessage{Text: name}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	line := []byte(strings.TrimSuffix(recordBytes, "\n"))
	msgs, err := parseV2Record(p, line)
	if err != nil {
		t.Fatal(err)
	}
	if msgs.Control || calls != 1 || len(msgs.Messages) != 1 || !msgs.Messages[0].ProducerTime.Equal(wantTime) {
		t.Fatalf("calls = %d, record = %#v, want producer time %v", calls, msgs, wantTime)
	}
}

func TestV2AgentRecord(t *testing.T) {
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
				p, err := NewLogRecordParser(LogVersionV2, func(msg []byte) ([]Message, error) {
					calls++
					aliased = len(msg) != 0 && &msg[0] == &record[payloadStart]
					if string(msg) != tc.payload {
						return nil, fmt.Errorf("native payload = %q, want %q", msg, tc.payload)
					}
					return []Message{&RawMessage{MessageType: "native"}}, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				msgs, err := parseV2Record(p, record)
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
		data, err := os.ReadFile("testdata/v2_agent_records.json")
		if err != nil {
			t.Fatal(err)
		}
		var fixture v2AgentRecordFixture
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
				assertV2FixtureRecord(t, tc.Name, tc.Timestamp, tc.NativeBytes, tc.RecordBytes)
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
				assertV2FixtureRecord(
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
		p, err := NewLogRecordParser(LogVersionV2, func([]byte) ([]Message, error) {
			return []Message{&UsageMessage{}}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		meta := []byte(`{"t":"caic_meta","version":2,"prompt":"p","repos":[],"harness":"claude"}`)
		msgs, err := parseV2Record(p, meta)
		if err != nil {
			t.Fatal(err)
		}
		if !msgs.Control || len(msgs.Messages) != 1 {
			t.Fatalf("meta record = %#v", msgs)
		}
		parsedMeta, ok := msgs.Messages[0].Message.(*MetaMessage)
		if !ok || parsedMeta.MessageType != "caic_meta" || parsedMeta.Version != 2 {
			t.Fatalf("meta message = %#v", msgs.Messages[0].Message)
		}
		if _, err := parseV2Record(p, []byte(`{"t":"caic_meta","type":"caic_meta","version":2,"prompt":"p","repos":[],"harness":"claude"}`)); err == nil {
			t.Fatal("v2 meta accepted the V1 type field")
		}
		if _, err := parseV2Record(p, []byte(`{"t":"caic_meta","version":2,"prompt":"p","repos":[],"harness":"claude","unknown":true}`)); err == nil || !strings.Contains(err.Error(), `json: unknown field "unknown"`) {
			t.Fatalf("v2 meta error = %v, want strict unknown-field error", err)
		}
		if _, err := parseV2Record(p, []byte(`{"t":"model_info","context_window":1000000}`)); err != nil {
			t.Fatal(err)
		}
		msgs, err = parseV2Record(p, []byte(`{"t":"agent","ts":1.000,"msg":{}}`))
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
			p, err := NewLogRecordParser(LogVersionV2, func([]byte) ([]Message, error) {
				return []Message{
					&TextMessage{Text: "one"},
					&ToolUseMessage{ToolUseID: "tool"},
					&ToolResultMessage{ToolUseID: "tool"},
				}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			msgs, err := parseV2Record(p, []byte(`{"t":"agent","ts":1.001,"msg":{}}`))
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
			p, err := NewLogRecordParser(LogVersionV2, func([]byte) ([]Message, error) {
				calls++
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			msgs, err := parseV2Record(p, []byte(`{"t":"agent","ts":0.001,"msg":false}`))
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
			{name: "old_type_meta", line: []byte(`{"type":"caic_meta","version":2,"prompt":"p","repos":[],"harness":"claude"}`)},
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
				p, err := NewLogRecordParser(LogVersionV2, func([]byte) ([]Message, error) {
					calls++
					return nil, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := parseV2Record(p, tc.line); err == nil {
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
		p, err := NewLogRecordParser(LogVersionV2, func([]byte) ([]Message, error) {
			calls++
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseV2Record(p, []byte(`{"t":"agent","ts":1.000,"msg":{"x":}}`)); err == nil {
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
		payloadLen := v2MaxEncodedRecordLen - 2 - len(prefix) - len(suffix)
		line := make([]byte, 0, v2MaxEncodedRecordLen-2)
		line = append(line, prefix...)
		line = append(line, bytes.Repeat([]byte{'a'}, payloadLen)...)
		line = append(line, suffix...)
		calls := 0
		p, err := NewLogRecordParser(LogVersionV2, func([]byte) ([]Message, error) {
			calls++
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseV2Record(p, line); err != nil {
			t.Fatalf("maximum accepted record: %v", err)
		}
		if calls != 1 {
			t.Fatalf("maximum accepted callback count = %d, want 1", calls)
		}
		line = append(line[:len(line)-len(suffix)], 'a')
		line = append(line, suffix...)
		if _, err := parseV2Record(p, line); err == nil || !strings.Contains(err.Error(), "encoded size") {
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
			Action: PendingUserAction{
				Kind: PendingUserActionAskUserQuestion, RequestID: "request", ToolUseID: "tool",
			},
		}
		batch := []Message{action, nil}
		p, err := NewLogRecordParser(LogVersionV2, func([]byte) ([]Message, error) {
			return batch, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		line := []byte(`{"t":"agent","ts":1.000,"msg":{}}`)
		if _, err := parseV2Record(p, line); err == nil || !strings.Contains(err.Error(), "native message 1 is nil") {
			t.Fatalf("invalid batch error = %v", err)
		}
		batch = []Message{action}
		msgs, err := parseV2Record(p, line)
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
		p, err := NewLogRecordParser(LogVersionV2, func([]byte) ([]Message, error) {
			calls++
			return nil, wantErr
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = parseV2Record(p, []byte(`{"t":"agent","ts":1.000,"msg":{}}`))
		if !errors.Is(err, wantErr) || calls != 1 {
			t.Fatalf("error = %v, calls = %d", err, calls)
		}
	})
}

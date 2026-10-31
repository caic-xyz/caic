// Microbenchmarks for strict canonical v3 task-log record decoding through the parser API.

package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

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

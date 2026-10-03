// Tests native JSON field decoding and the lifetime of borrowed envelope payloads.

package agent

import (
	"bytes"
	"testing"
)

func TestParseJSONRPCEnvelope(t *testing.T) {
	t.Parallel()
	t.Run("borrowed payloads", func(t *testing.T) {
		t.Parallel()
		line := []byte(` {"method":"session/update","id": "request\"1", "params": {"text":"hello"}} `)
		env, err := ParseJSONRPCEnvelope(line)
		if err != nil {
			t.Fatal(err)
		}
		if env.Method != "session/update" || string(env.ID) != `"request\"1"` || string(env.Params) != `{"text":"hello"}` {
			t.Fatalf("envelope = %#v", env)
		}
		line[bytes.Index(line, []byte("hello"))] = 'j'
		if string(env.Params) != `{"text":"jello"}` {
			t.Fatal("params were copied")
		}
	})
	t.Run("repeated fields", func(t *testing.T) {
		t.Parallel()
		env, err := ParseJSONRPCEnvelope([]byte(`{"method":"first","method":null,"type":"old","type":"new","params":{},"params":null}`))
		if err != nil {
			t.Fatal(err)
		}
		if env.Method != "first" || env.Type != "new" || string(env.Params) != "null" {
			t.Fatalf("envelope = %#v", env)
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		for _, line := range []string{`[]`, `{"method":1}`, `{"type":false}`} {
			t.Run(line, func(t *testing.T) {
				t.Parallel()
				if _, err := ParseJSONRPCEnvelope([]byte(line)); err == nil {
					t.Fatal("expected error")
				}
			})
		}
	})
}

func TestJSONField(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, line, key, want string }{
		{"string", `{"key": "escaped\"text"}`, "key", `"escaped\"text"`},
		{"nested", `{"outer":{"key":"value"}}`, "outer", `{"key":"value"}`},
		{"number", `{"key": 123}`, "key", "123"},
		{"null", `{"key":null}`, "key", "null"},
		{"missing", `{}`, "key", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := JSONField([]byte(tc.line), tc.key)
			if err != nil || string(got) != tc.want {
				t.Fatalf("field = %q, err = %v", got, err)
			}
		})
	}
}

func TestJSONString(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct{ name, line, want string }{
			{"escape", `{"key":"line\nend"}`, "line\nend"},
			{"surrogate", `{"key":"\ud800"}`, "\ufffd"},
			{"invalid UTF-8", "{\"key\":\"\xff\xff\"}", "\ufffd\ufffd"},
			{"null", `{"key":null}`, ""},
			{"missing", `{}`, ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				got, err := JSONString([]byte(tc.line), "key")
				if err != nil || got != tc.want {
					t.Fatalf("string = %q, err = %v", got, err)
				}
			})
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		if _, err := JSONString([]byte(`{"key":false}`), "key"); err == nil {
			t.Fatal("expected error")
		}
	})
}

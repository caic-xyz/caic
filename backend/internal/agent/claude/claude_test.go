package claude

import (
	"bytes"
	"strings"
	"testing"
)

func TestWritePrompt(t *testing.T) {
	var buf bytes.Buffer
	var logBuf bytes.Buffer
	var b Backend
	if err := b.WritePrompt(&buf, "hello", &logBuf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != logBuf.String() {
		t.Errorf("stdin and log differ:\nstdin: %q\nlog:   %q", buf.String(), logBuf.String())
	}
	if !strings.Contains(buf.String(), `"content":"hello"`) {
		t.Errorf("unexpected output: %s", buf.String())
	}
}

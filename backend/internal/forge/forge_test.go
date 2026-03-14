// Tests for forge package utilities.
package forge

import (
	"strings"
	"testing"
)

func TestReadLog(t *testing.T) {
	t.Run("strips ANSI color codes", func(t *testing.T) {
		input := "\x1b[31mERROR\x1b[0m: build failed\n\x1b[32mOK\x1b[0m"
		got, err := ReadLog(strings.NewReader(input))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "ERROR: build failed\nOK"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("preserves plain text", func(t *testing.T) {
		input := "##[group]Run tests\ngo test ./...\n##[endgroup]"
		got, err := ReadLog(strings.NewReader(input))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != input {
			t.Errorf("got %q, want %q", got, input)
		}
	})

	t.Run("strips bold and cursor codes", func(t *testing.T) {
		input := "\x1b[1mBold\x1b[0m \x1b[2Kcleared"
		got, err := ReadLog(strings.NewReader(input))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "Bold cleared"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

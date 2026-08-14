// Tests for forge package utilities.

package forge

import (
	"strings"
	"testing"
	"time"
)

func TestReadLog(t *testing.T) {
	t.Parallel()
	t.Run("strips ANSI color codes", func(t *testing.T) {
		t.Parallel()
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
		t.Parallel()
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
		t.Parallel()
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

func TestCheckFromRun(t *testing.T) {
	t.Parallel()
	t.Run("preserves timing", func(t *testing.T) {
		t.Parallel()
		now := time.Now()
		completed := now.Add(time.Minute)
		run := CheckRun{
			Name: "build", Status: CheckRunStatusCompleted,
			Conclusion: CheckRunConclusionSuccess, StartedAt: now, CompletedAt: completed,
		}
		c := CheckFromRun("o", "r", &run)
		if c.StartedAt.IsZero() {
			t.Error("startedAt should be set")
		}
		if c.CompletedAt.IsZero() {
			t.Error("completedAt should be set")
		}
		if c.Status != CheckRunStatusCompleted {
			t.Errorf("status = %q, want completed", c.Status)
		}
		if c.Owner != "o" || c.Repo != "r" {
			t.Errorf("owner/repo = %s/%s, want o/r", c.Owner, c.Repo)
		}
	})
}

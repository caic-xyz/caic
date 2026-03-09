package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpandTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("bare tilde", func(t *testing.T) {
		got := expandTilde("~")
		if got != home {
			t.Errorf("expandTilde(~) = %q, want %q", got, home)
		}
	})

	t.Run("tilde with path", func(t *testing.T) {
		got := expandTilde("~/repos")
		want := filepath.Join(home, "repos")
		if got != want {
			t.Errorf("expandTilde(~/repos) = %q, want %q", got, want)
		}
	})

	t.Run("absolute path unchanged", func(t *testing.T) {
		got := expandTilde("/opt/repos")
		if got != "/opt/repos" {
			t.Errorf("expandTilde(/opt/repos) = %q, want /opt/repos", got)
		}
	})

	t.Run("empty string unchanged", func(t *testing.T) {
		got := expandTilde("")
		if got != "" {
			t.Errorf("expandTilde(\"\") = %q, want \"\"", got)
		}
	})

	t.Run("tilde with backslash", func(t *testing.T) {
		got := expandTilde(`~\repos`)
		want := filepath.Join(home, "repos")
		if got != want {
			t.Errorf(`expandTilde(~\repos) = %q, want %q`, got, want)
		}
	})

	t.Run("relative path unchanged", func(t *testing.T) {
		got := expandTilde("repos/foo")
		if got != "repos/foo" {
			t.Errorf("expandTilde(repos/foo) = %q, want repos/foo", got)
		}
	})
}

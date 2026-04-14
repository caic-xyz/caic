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
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"bare tilde", "~", home},
		{"tilde with slash", "~/repos", filepath.Join(home, "repos")},
		{"tilde with backslash", `~\repos`, filepath.Join(home, "repos")},
		{"absolute path unchanged", "/opt/repos", "/opt/repos"},
		{"empty string resolves to cwd", "", cwd},
		{"relative path made absolute", "repos/foo", filepath.Join(cwd, "repos", "foo")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := expandTilde(tt.input)
			if err != nil {
				t.Fatalf("expandTilde(%q) error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("expandTilde(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

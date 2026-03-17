package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadFileFromEnv(t *testing.T) {
	const envVar = "TEST_READ_FILE_OR_ENV"

	t.Run("empty", func(t *testing.T) {
		t.Setenv(envVar, "")
		if got := readFileFromEnv(envVar); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})

	t.Run("absolute path", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "key.pem")
		if err := os.WriteFile(f, []byte("ABS-PEM"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(envVar, f)
		if got := readFileFromEnv(envVar); got != "ABS-PEM" {
			t.Fatalf("want ABS-PEM, got %q", got)
		}
	})

	t.Run("relative path resolves to config dir", func(t *testing.T) {
		cfgDir := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", cfgDir)
		caicDir := filepath.Join(cfgDir, "caic")
		if err := os.MkdirAll(caicDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(caicDir, "key.pem"), []byte("REL-PEM"), 0o600); err != nil {
			t.Fatal(err)
		}

		t.Setenv(envVar, "key.pem")
		if got := readFileFromEnv(envVar); got != "REL-PEM" {
			t.Fatalf("want REL-PEM, got %q", got)
		}
	})

	t.Run("missing file returns empty", func(t *testing.T) {
		t.Setenv(envVar, "/nonexistent/path/key.pem")
		if got := readFileFromEnv(envVar); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})
}

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

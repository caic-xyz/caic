// Tests for command utilities, restart watching, and shutdown exit policy.

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIsNormalShutdown(t *testing.T) {
	t.Parallel()
	failure := errors.New("watcher failed")
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"cancellation", context.Canceled, true},
		{"wrapped cancellation", fmt.Errorf("shutdown: %w", context.Canceled), true},
		{"joined cancellation", errors.Join(context.Canceled, context.Canceled), true},
		{"watcher failure", failure, false},
		{"joined failure", errors.Join(context.Canceled, failure), false},
		{"wrapped joined failure", fmt.Errorf("shutdown: %w", errors.Join(context.Canceled, failure)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isNormalShutdown(tc.err); got != tc.want {
				t.Fatalf("isNormalShutdown(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestWatchForRestart(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"create", "directory creation", "remove", "replace", "watcher failure", "write"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if action == "directory creation" {
				dir = filepath.Join(dir, "missing", "nested")
			}
			p := filepath.Join(dir, "config.toml")
			if action != "create" && action != "directory creation" {
				if err := os.WriteFile(p, []byte("old config"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancelCause(t.Context())
			w, err := watchForRestart(ctx, cancel, dir)
			if err != nil {
				cancel(nil)
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cancel(nil)
				if action != "watcher failure" {
					if err := w.Close(); err != nil {
						t.Error(err)
					}
				}
			})
			switch action {
			case "directory creation":
				err = os.MkdirAll(dir, 0o700)
			case "watcher failure":
				err = w.Close()
			case "remove":
				err = os.Remove(p)
			case "replace":
				if err = os.WriteFile(p+".new", []byte("new config"), 0o600); err == nil {
					err = os.Rename(p+".new", p)
				}
			default:
				err = os.WriteFile(p, []byte("new config"), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-ctx.Done():
				cause := context.Cause(ctx)
				if action == "watcher failure" {
					// app.Serve can return joined cancellation and cleanup errors.
					if cause == nil || isNormalShutdown(errors.Join(context.Canceled, cause)) {
						t.Fatalf("shutdown cause = %v, want failure", cause)
					}
				} else if !isNormalShutdown(cause) {
					t.Fatalf("shutdown cause = %v, want normal cancellation", cause)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("watcher did not trigger shutdown")
			}
		})
	}
}

func TestExpandTilde(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
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

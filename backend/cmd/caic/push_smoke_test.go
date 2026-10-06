// Runtime smoke coverage fetches task commits and pushes through ordinary HEAD-only hooks.

//go:build smoke

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/md/git"

	"github.com/caic-xyz/caic/backend/internal/logtest"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/smoketest"
)

func TestSmokePush(t *testing.T) {
	smoke := startSmokeServer(t)
	var repos []v1.Repo
	getJSON(t, smoke.baseURL, "/api/caic/v1/server/repos", &repos)
	root := filepath.Join(smoke.rootDir, repos[0].Path)
	g := &git.Checkout{Root: root, Logger: logtest.Logger(t)}
	audit := filepath.Join(t.TempDir(), "verified")
	hook, err := os.ReadFile("../../../scripts/hooks/pre-push")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte("verify:\n\t@test \"$$(cat smoke-push.txt)\" = runtime-task\n\t@git rev-parse HEAD > '"+audit+"'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"-c", "user.name=Smoke", "-c", "user.email=smoke@example.com", "commit", "-m", "Install push verification"}, {"push", "origin", "main"}} {
		if _, err := g.RunGit(t.Context(), args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "hooks", "pre-push"), hook, 0o700); err != nil {
		t.Fatal(err)
	}
	before, err := g.RevParse(t.Context(), "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	var task v1.Task
	postJSON(t, smoke.baseURL, "/api/caic/v1/tasks", v1.CreateTaskReq{
		InitialPrompt: v1.Prompt{Text: "runtime push smoke"}, Repos: []v1.RepoSpec{{Name: repos[0].Path}}, Harness: v1.HarnessCodex, RuntimeName: smoketest.SmokeRuntime(),
	}, &task)
	id := task.ID.String()
	task = waitForTaskState(t, smoke, id, "waiting")
	container := string(runtime.ID(task.Runtime.ID).InstanceID())
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, smoketest.SmokeRuntime(), "exec", container, "sh", "-c", "set -eu; cd -- \"$1\"; printf '%s\\n' runtime-task > smoke-push.txt; git add smoke-push.txt; git -c user.name=Smoke -c user.email=smoke@example.com commit -m 'Runtime task change'; git rev-parse HEAD", "sh", root) //nolint:gosec // fixture-owned runtime, container, and repository.
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("container commit: %v: %s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	tip := lines[len(lines)-1]
	if err := os.WriteFile(filepath.Join(root, "host-dirty"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	var resp v1.SyncResp
	postJSON(t, smoke.baseURL, "/api/caic/v1/tasks/"+id+"/sync", v1.SyncReq{}, &resp)
	if resp.Status != "synced" {
		t.Fatalf("sync: %+v", resp)
	}
	verified, err := os.ReadFile(audit)
	if err != nil || strings.TrimSpace(string(verified)) != tip {
		t.Fatalf("hook verified %q, want %s: %v", verified, tip, err)
	}
	remote, err := g.RunGit(t.Context(), "ls-remote", "origin", "refs/heads/"+task.Repos[0].Branch)
	if err != nil || !strings.HasPrefix(remote, tip+"\t") {
		t.Fatalf("remote: %q: %v", remote, err)
	}
	head, err := g.RevParse(t.Context(), "HEAD")
	if err != nil || head != before {
		t.Fatalf("host HEAD changed: %q, %v", head, err)
	}
	if dirty, err := os.ReadFile(filepath.Join(root, "host-dirty")); err != nil || string(dirty) != "keep" {
		t.Fatalf("host untracked file changed: %q, %v", dirty, err)
	}
	entries, err := os.ReadDir(filepath.Join(smoke.cfg.Dirs.CacheDir, "push"))
	if err != nil {
		t.Fatal(err)
	}
	idle := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "operation-") {
			if _, err := os.Stat(filepath.Join(smoke.cfg.Dirs.CacheDir, "push", e.Name(), "idle-v1")); err != nil {
				t.Fatal("completed push checkout not reusable", err)
			}
			idle++
		}
	}
	if idle != 1 {
		t.Fatalf("expected one idle push checkout, got %d", idle)
	}
}

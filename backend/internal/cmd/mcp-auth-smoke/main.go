// Smoke-test local MCP OAuth compatibility with shipped MCP clients.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/forge/forgemanager"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/repos"
	"github.com/caic-xyz/caic/backend/internal/runtime/mdruntime"
	"github.com/caic-xyz/caic/backend/internal/server"
	"github.com/caic-xyz/caic/backend/internal/server/ipgeo"
	"github.com/caic-xyz/caic/backend/internal/tasks"
)

const smokeSessionTTL = 30 * 24 * time.Hour

type smokeClient string

const (
	smokeClientClaude smokeClient = "claude"
	smokeClientCodex  smokeClient = "codex"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err := run(ctx, os.Args[1:])
	stop()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp-auth-smoke", flag.ContinueOnError)
	client := fs.String("client", string(smokeClientCodex), "MCP client to test: codex or claude")
	keepTemp := fs.Bool("keep-temp", false, "keep temporary files after the run")
	if err := fs.Parse(args); err != nil {
		return err
	}
	selected := smokeClient(*client)
	if selected != smokeClientCodex && selected != smokeClientClaude {
		return fmt.Errorf("unsupported client %q", *client)
	}
	if _, err := exec.LookPath(string(selected)); err != nil {
		return fmt.Errorf("%s not found in PATH: %w", selected, err)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		return fmt.Errorf("python3 not found in PATH: %w", err)
	}

	tmp, err := os.MkdirTemp("", "caic-mcp-auth-smoke-*")
	if err != nil {
		return err
	}
	if !*keepTemp {
		defer func() { _ = os.RemoveAll(tmp) }()
	}

	baseURL, sessionCookie, shutdown, err := startAuthServer(ctx, filepath.Join(tmp, "state"))
	if err != nil {
		return err
	}
	defer shutdown()

	browserPath := filepath.Join(tmp, "authorize.py")
	browserLog := filepath.Join(tmp, "browser.log")
	if err := writeBrowserHelper(browserPath); err != nil {
		return err
	}
	endpoint := baseURL + "/api/caic/v1/mcp"
	env := append(os.Environ(),
		"BROWSER="+browserPath,
		"BROWSER_LOG="+browserLog,
		"CAIC_SESSION_COOKIE="+sessionCookie,
	)

	switch selected {
	case smokeClientCodex:
		err = smokeCodex(ctx, env, tmp, endpoint)
	case smokeClientClaude:
		err = smokeClaude(ctx, env, tmp, endpoint)
	}
	if err != nil {
		return err
	}

	fmt.Printf("ok: %s authenticated to %s and listed caic MCP tools\n", selected, endpoint)
	if *keepTemp {
		fmt.Printf("temp: %s\n", tmp)
	}
	return nil
}

func smokeCodex(ctx context.Context, baseEnv []string, tmp, endpoint string) error {
	codexHome := filepath.Join(tmp, "codex-home")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		return err
	}
	env := append([]string(nil), baseEnv...)
	env = append(env, "CODEX_HOME="+codexHome)
	if _, err := runCommand(ctx, "codex", env, "mcp", "add", "caic", "--url", endpoint); err != nil {
		return fmt.Errorf("codex mcp add: %w", err)
	}
	if _, err := runCommand(ctx, "codex", env, "mcp", "login", "caic", "--scopes", "caic:mcp.read,caic:tasks.read"); err != nil {
		return fmt.Errorf("codex mcp login: %w", err)
	}
	listOut, err := runCommand(ctx, "codex", env, "mcp", "list")
	if err != nil {
		return fmt.Errorf("codex mcp list: %w", err)
	}
	if !bytes.Contains(listOut, []byte("caic")) || !bytes.Contains(listOut, []byte("OAuth")) {
		return fmt.Errorf("codex mcp list did not report OAuth caic server:\n%s", listOut)
	}
	if err := verifyCodexTokenToolsList(ctx, filepath.Join(codexHome, ".credentials.json"), endpoint); err != nil {
		return err
	}
	return nil
}

func smokeClaude(ctx context.Context, baseEnv []string, tmp, endpoint string) error {
	home := filepath.Join(tmp, "claude-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	env := append([]string(nil), baseEnv...)
	env = append(env, "HOME="+home)
	if _, err := runCommand(ctx, "claude", env, "mcp", "add", "--transport", "http", "--scope", "local", "caic", endpoint); err != nil {
		return fmt.Errorf("claude mcp add: %w", err)
	}
	getOut, err := runCommand(ctx, "claude", env, "mcp", "get", "caic")
	if err != nil {
		return fmt.Errorf("claude mcp get: %w", err)
	}
	if bytes.Contains(getOut, []byte("Status: ! Needs authentication")) {
		return fmt.Errorf("claude reached caic but did not start OAuth from mcp get; use interactive /mcp authentication for this Claude Code version:\n%s", getOut)
	}
	if !bytes.Contains(getOut, []byte("Status: ✓ Connected")) {
		return fmt.Errorf("claude mcp get did not report connected caic server:\n%s", getOut)
	}
	return nil
}

func startAuthServer(ctx context.Context, stateDir string) (baseURL, sessionCookie string, shutdown func(), retErr error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", "", nil, err
	}
	checker, err := ipgeo.NewChecker(ctx, "0.0.0.0/0,::/0", "", "")
	if err != nil {
		return "", "", nil, err
	}
	backend := &mdruntime.Backend{}
	taskMgr := tasks.New(tasks.Config{ServerCtx: ctx})
	repoSvc := repos.NewService("", "", "", nil, repos.NewRegistry(nil), taskMgr, backend, nil)
	prefs, err := preferences.Open(filepath.Join(stateDir, "preferences.json"))
	if err != nil {
		return "", "", nil, err
	}
	store, err := auth.Open(filepath.Join(stateDir, "users.json"))
	if err != nil {
		return "", "", nil, err
	}
	user, err := store.UpsertUser(&auth.User{Provider: auth.ProviderGitHub, ProviderID: "1", Username: "alice", AccessToken: "forge-token"})
	if err != nil {
		return "", "", nil, err
	}
	secret := []byte("0123456789abcdef0123456789abcdef")
	session, err := auth.IssueToken(&user, secret, smokeSessionTTL)
	if err != nil {
		return "", "", nil, err
	}
	router, err := server.New(ctx, server.Dependencies{
		Repos:          repoSvc,
		ProcessBackend: backend,
		TaskManager:    taskMgr,
		Preferences:    prefs,
		IPGeoChecker:   checker,
		Forge:          forgemanager.New("", "", nil),
		AuthStore:      store,
		SessionSecret:  secret,
	})
	if err != nil {
		return "", "", nil, err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", nil, err
	}
	serverCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- router.Serve(serverCtx, ln)
	}()
	shutdown = func() {
		cancel()
		if err := <-done; err != nil {
			slog.WarnContext(ctx, "mcp auth smoke server shutdown", "err", err)
		}
	}
	return "http://" + ln.Addr().String(), session, shutdown, nil
}

func verifyCodexTokenToolsList(ctx context.Context, credentialsPath, endpoint string) error {
	data, err := os.ReadFile(credentialsPath) //nolint:gosec // Smoke test reads its own temporary credentials file.
	if err != nil {
		return fmt.Errorf("read codex credentials: %w", err)
	}
	var credentials map[string]codexCredential
	if err := json.Unmarshal(data, &credentials); err != nil {
		return fmt.Errorf("parse codex credentials: %w", err)
	}
	var token string
	for _, credential := range credentials {
		if credential.ServerURL == endpoint && credential.AccessToken != "" {
			token = credential.AccessToken
			break
		}
	}
	if token == "" {
		return errors.New("codex credentials did not contain an access token for caic")
	}
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("tools/list with codex token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read tools/list response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tools/list status = %d, want 200: %s", resp.StatusCode, respBody)
	}
	if !bytes.Contains(respBody, []byte(`"tools"`)) || !bytes.Contains(respBody, []byte(`"tasks_list"`)) {
		return fmt.Errorf("tools/list did not return caic tools: %s", respBody)
	}
	return nil
}

type codexCredential struct {
	ServerURL   string `json:"server_url"`
	AccessToken string `json:"access_token"`
}

func writeBrowserHelper(path string) error {
	script := `#!/usr/bin/env python3
import os
import re
import sys
import urllib.parse
import urllib.request

url = sys.argv[-1]
cookie = os.environ["CAIC_SESSION_COOKIE"]
log_path = os.environ.get("BROWSER_LOG")

def log(message):
    if log_path:
        with open(log_path, "a", encoding="utf-8") as f:
            print(message, file=f)

log("authorize " + url)
headers = {"Cookie": "caic_session=" + cookie}
req = urllib.request.Request(url, headers=headers)
with urllib.request.urlopen(req, timeout=30) as resp:
    html = resp.read().decode("utf-8", "replace")
match = re.search(r'name="consent_token" value="([^"]+)"', html)
if not match:
    log("consent token missing")
    sys.exit(2)
post_url = urllib.parse.urljoin(url, "/api/caic/v1/oauth/authorize")
data = urllib.parse.urlencode({"consent_token": match.group(1)}).encode()
req = urllib.request.Request(post_url, data=data, headers={**headers, "Content-Type": "application/x-www-form-urlencoded"})
with urllib.request.urlopen(req, timeout=30) as resp:
    body = resp.read()
log("callback bytes " + str(len(body)))
`
	return os.WriteFile(path, []byte(script), 0o700) //nolint:gosec // The browser helper must be executable.
}

func runCommand(ctx context.Context, name string, env []string, args ...string) ([]byte, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, name, args...) //nolint:gosec // Smoke test intentionally launches a checked client CLI by fixed name.
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if cmdCtx.Err() != nil {
		err = errors.Join(err, cmdCtx.Err())
	}
	if err != nil {
		return out.Bytes(), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out.String())
	}
	return out.Bytes(), nil
}

package main

import (
	"context"
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
	"strconv"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claude"
	"github.com/caic-xyz/caic/backend/internal/agent/fake"
	"github.com/caic-xyz/caic/backend/internal/server"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/fsnotify/fsnotify"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-colorable"
	"github.com/mattn/go-isatty"
)

// expandTilde replaces a leading "~/" or bare "~" with the current user's home directory.
func expandTilde(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		rest := strings.TrimLeft(path[1:], `/\`)
		return filepath.Join(home, rest)
	}
	return path
}

// envDefault returns the value of the named environment variable, or def if unset/empty.
func envDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// localizeAddr defaults to localhost when the address specifies a port but no
// host (e.g. ":8080" → "localhost:8080"). This avoids accidentally listening
// on all interfaces.
func localizeAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" {
		return net.JoinHostPort("localhost", port)
	}
	return addr
}

func mainImpl() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	flag.Usage = func() {
		w := flag.CommandLine.Output()
		_, _ = fmt.Fprintf(w, `Usage: caic [flags]

caic manages multiple coding agents in parallel. Each task runs in an isolated
container with the agent communicating over SSH.

Flags:
`)
		flag.PrintDefaults()
		_, _ = fmt.Fprintf(w, `
Environment variables (flags take precedence when set):

  Core:
    CAIC_HTTP                   HTTP listen address (e.g. :8080)
    CAIC_ROOT                   Parent directory containing git repos
    CAIC_LOG_LEVEL              Log level: debug, info, warn, error (default: info)
    CAIC_EXTERNAL_URL           Public base URL; required for OAuth login and webhooks

  LLM features (title generation, commit descriptions):
    CAIC_LLM_PROVIDER           Provider: anthropic, gemini, openaichat, etc.
    CAIC_LLM_MODEL              Model name (e.g. claude-haiku-4-5-20251001)

  GitHub — choose one of PAT or OAuth; GitHub App is independent:
    GITHUB_TOKEN                PAT for PR/CI; headless/single-user (mutually exclusive with GITHUB_OAUTH_CLIENT_ID)
    GITHUB_OAUTH_CLIENT_ID      OAuth app client ID; multi-user login (mutually exclusive with GITHUB_TOKEN)
    GITHUB_OAUTH_CLIENT_SECRET  OAuth app client secret
    GITHUB_OAUTH_ALLOWED_USERS  Comma-separated GitHub usernames allowed to log in (required with OAuth)
    GITHUB_APP_ID               GitHub App ID for org-wide webhooks and installation tokens
    GITHUB_APP_PRIVATE_KEY_PEM  Path to RSA private key PEM file, or the PEM content directly
    GITHUB_APP_ALLOWED_OWNERS   Comma-separated owners/orgs allowed to install the app; rejects others
    GITHUB_WEBHOOK_SECRET       HMAC-SHA256 secret; enables POST /api/v1/github/webhook

  GitLab — choose one of PAT or OAuth:
    GITLAB_TOKEN                PAT for MR/CI; headless/single-user (mutually exclusive with GITLAB_OAUTH_CLIENT_ID)
    GITLAB_OAUTH_CLIENT_ID      OAuth app client ID; multi-user login (mutually exclusive with GITLAB_TOKEN)
    GITLAB_OAUTH_CLIENT_SECRET  OAuth app client secret
    GITLAB_OAUTH_ALLOWED_USERS  Comma-separated GitLab usernames allowed to log in (required with OAuth)
    GITLAB_URL                  GitLab instance URL (default: https://gitlab.com)

  Agents:
    GEMINI_API_KEY              Gemini API key for the Gemini Live voice agent
    TAILSCALE_API_KEY           Tailscale API key for Tailscale ephemeral node

See contrib/caic.env for a template with all variables and documentation.
`)
	}

	addr := flag.String("http", os.Getenv("CAIC_HTTP"), "start web UI on this address (e.g. :8080)")
	root := flag.String("root", os.Getenv("CAIC_ROOT"), "parent directory containing git repos")
	logLevel := flag.String("log-level", envDefault("CAIC_LOG_LEVEL", "info"), "log level (debug, info, warn, error)")
	fakeMode := flag.Bool("fake", false, "use fake container/agent ops (for e2e tests); creates a temp repo when -root is omitted")
	flag.Parse()
	if args := flag.Args(); len(args) > 0 {
		return fmt.Errorf("unexpected arguments: %v", args)
	}
	*root = expandTilde(*root)

	initLogging(*logLevel)

	cfg := &server.Config{
		GeminiAPIKey:            os.Getenv("GEMINI_API_KEY"),
		TailscaleAPIKey:         os.Getenv("TAILSCALE_API_KEY"),
		LLMProvider:             os.Getenv("CAIC_LLM_PROVIDER"),
		LLMModel:                os.Getenv("CAIC_LLM_MODEL"),
		ConfigDir:               configDir(),
		CacheDir:                cacheDir(),
		GitHubToken:             os.Getenv("GITHUB_TOKEN"),
		GitLabToken:             os.Getenv("GITLAB_TOKEN"),
		ExternalURL:             os.Getenv("CAIC_EXTERNAL_URL"),
		GitHubOAuthClientID:     os.Getenv("GITHUB_OAUTH_CLIENT_ID"),
		GitHubOAuthClientSecret: os.Getenv("GITHUB_OAUTH_CLIENT_SECRET"),
		GitLabOAuthClientID:     os.Getenv("GITLAB_OAUTH_CLIENT_ID"),
		GitLabOAuthClientSecret: os.Getenv("GITLAB_OAUTH_CLIENT_SECRET"),
		GitLabURL:               os.Getenv("GITLAB_URL"),
		GitHubOAuthAllowedUsers: os.Getenv("GITHUB_OAUTH_ALLOWED_USERS"),
		GitLabOAuthAllowedUsers: os.Getenv("GITLAB_OAUTH_ALLOWED_USERS"),
		GitHubWebhookSecret:     []byte(os.Getenv("GITHUB_WEBHOOK_SECRET")),
		GitHubAppID:             parseInt64(os.Getenv("GITHUB_APP_ID")),
		GitHubAppPrivateKeyPEM:  []byte(readFileOrEnv("GITHUB_APP_PRIVATE_KEY_PEM")),
		GitHubAppAllowedOwners:  os.Getenv("GITHUB_APP_ALLOWED_OWNERS"),
	}

	if key := cfg.GeminiAPIKey; key != "" {
		suffix := key
		if len(suffix) > 4 {
			suffix = suffix[len(suffix)-4:]
		}
		slog.Info("GEMINI_API_KEY configured", "suffix", suffix) //nolint:gosec // G706: logging last 4 chars of key, not a secret
	} else {
		slog.Warn("GEMINI_API_KEY not set")
	}
	if key := cfg.TailscaleAPIKey; key != "" {
		suffix := key
		if len(suffix) > 4 {
			suffix = suffix[len(suffix)-4:]
		}
		slog.Info("TAILSCALE_API_KEY configured", "suffix", suffix) //nolint:gosec // G706: logging last 4 chars of key, not a secret
	} else {
		slog.Warn("TAILSCALE_API_KEY not set")
	}
	slog.Info("LLM", "provider", cfg.LLMProvider, "model", cfg.LLMModel) //nolint:gosec // G706: config values, not user input

	if err := cfg.Validate(); err != nil {
		return err
	}
	if *fakeMode {
		return serveFake(ctx, *addr, *root, cfg)
	}
	if *addr == "" {
		return errors.New("HTTP address is required: set -http flag or CAIC_HTTP env var")
	}
	*addr = localizeAddr(*addr)
	if *root == "" {
		return errors.New("root directory is required: set -root flag or CAIC_ROOT env var")
	}

	// Exit when executable is rebuilt (systemd restarts the service).
	if err := watchExecutable(ctx, cancel); err != nil {
		slog.Warn("failed to watch executable", "err", err)
	}
	return serveHTTP(ctx, *addr, *root, cfg)
}

// initLogging configures slog with tint for colored, concise output.
// Timestamps are omitted under systemd (JOURNAL_STREAM), and zero-value
// attributes are dropped.
func initLogging(level string) {
	ll := &slog.LevelVar{}
	switch level {
	case "debug":
		ll.Set(slog.LevelDebug)
	case "info":
		// default
	case "warn":
		ll.Set(slog.LevelWarn)
	case "error":
		ll.Set(slog.LevelError)
	}
	// Skip timestamps when running under systemd (it adds its own).
	underSystemd := os.Getenv("JOURNAL_STREAM") != ""
	slog.SetDefault(slog.New(tint.NewHandler(colorable.NewColorable(os.Stderr), &tint.Options{
		Level:      ll,
		TimeFormat: "15:04:05.000",
		NoColor:    !isatty.IsTerminal(os.Stderr.Fd()),
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if underSystemd && a.Key == slog.TimeKey && len(groups) == 0 {
				return slog.Attr{}
			}
			val := a.Value.Any()
			skip := false
			switch t := val.(type) {
			case string:
				skip = t == ""
			case bool:
				skip = !t
			case uint64:
				skip = t == 0
			case int64:
				skip = t == 0
			case float64:
				skip = t == 0
			case time.Time:
				skip = t.IsZero()
			case time.Duration:
				skip = t == 0
			case nil:
				skip = true
			}
			if skip {
				return slog.Attr{}
			}
			return a
		},
	})))
}

func serveHTTP(ctx context.Context, addr, rootDir string, cfg *server.Config) error {
	srv, err := server.New(ctx, rootDir, cfg)
	if err != nil {
		return err
	}
	err = srv.ListenAndServe(ctx, addr)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func main() {
	if err := mainImpl(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "caic: %v\n", err)
		os.Exit(1)
	}
}

// serveFake starts the HTTP server with fake container/agent ops and a temp
// git repo. Used for e2e testing without md CLI or SSH.
func serveFake(ctx context.Context, addr, rootDir string, cfg *server.Config) error {
	if addr == "" {
		addr = "localhost:8090"
	} else {
		addr = localizeAddr(addr)
	}

	// When -root is not provided, create a temp git repo.
	if rootDir == "" {
		tmpDir, err := os.MkdirTemp("", "caic-e2e-*")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(tmpDir) }()
		clone, err := initFakeRepo(tmpDir)
		if err != nil {
			return fmt.Errorf("init fake repo: %w", err)
		}
		rootDir = filepath.Dir(clone)
	}

	// Use a temp dir for XDG_CONFIG_HOME so md can write its keys without
	// hitting the read-only ~/.config/md mount in the dev container.
	mdConfigDir, err := os.MkdirTemp("", "caic-e2e-md-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(mdConfigDir) }()
	if err := os.Setenv("XDG_CONFIG_HOME", mdConfigDir); err != nil {
		return fmt.Errorf("set XDG_CONFIG_HOME: %w", err)
	}
	// Override config/cache dirs for the fake server.
	fakeConfigDir, err := os.MkdirTemp("", "caic-e2e-cfg-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(fakeConfigDir) }()
	cfg.ConfigDir = fakeConfigDir
	cfg.CacheDir = filepath.Join(os.TempDir(), "caic-e2e-logs")
	srv, err := server.New(ctx, rootDir, cfg)
	if err != nil {
		return fmt.Errorf("new server: %w", err)
	}
	fb := &fakeBackend{}
	srv.SetRunnerOps(&fakeContainer{}, map[agent.Harness]agent.Backend{fb.Harness(): fb})

	err = srv.ListenAndServe(ctx, addr)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// initFakeRepo creates a bare remote and a clone with one commit on main.
func initFakeRepo(tmpDir string) (string, error) {
	bare := filepath.Join(tmpDir, "remote.git")
	clone := filepath.Join(tmpDir, "clone")
	for _, args := range [][]string{
		{"init", "--bare", bare},
		{"init", clone},
		{"-C", clone, "config", "user.name", "Test"},
		{"-C", clone, "config", "user.email", "test@test.com"},
		{"-C", clone, "checkout", "-b", "main"},
	} {
		if err := runGit(args...); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(filepath.Join(clone, "README.md"), []byte("hello\n"), 0o600); err != nil {
		return "", err
	}
	for _, args := range [][]string{
		{"-C", clone, "add", "."},
		{"-C", clone, "commit", "-m", "init"},
		{"-C", clone, "remote", "add", "origin", bare},
		{"-C", clone, "push", "-u", "origin", "main"},
	} {
		if err := runGit(args...); err != nil {
			return "", err
		}
	}
	return clone, nil
}

func runGit(args ...string) error {
	out, err := exec.Command("git", args...).CombinedOutput() //nolint:gosec // args are hardcoded git subcommands
	if err != nil {
		return fmt.Errorf("git %v: %w\n%s", args, err, out)
	}
	return nil
}

// fakeContainer implements task.ContainerBackend with no-op operations.
type fakeContainer struct{}

var _ task.ContainerBackend = (*fakeContainer)(nil)

func (*fakeContainer) Start(_ context.Context, _, branch string, _ []string, _ task.StartOptions) (_, _ string, _ error) {
	return "md-test-" + strings.ReplaceAll(branch, "/", "-"), "", nil
}

func (*fakeContainer) Diff(_ context.Context, _, _ string, _ ...string) (string, error) {
	return "", nil
}

func (*fakeContainer) Fetch(_ context.Context, _, _ string) error { return nil }
func (*fakeContainer) Kill(_ context.Context, _, _ string) error  { return nil }

// fakeBackend implements agent.Backend with a shell process that emits
// streaming text deltas followed by complete messages, simulating
// --include-partial-messages output. It supports multiple turns: each
// line read from stdin triggers the next joke response, cycling through
// a fixed list.
type fakeBackend struct{}

var _ agent.Backend = (*fakeBackend)(nil)

func (*fakeBackend) Harness() agent.Harness { return "fake" }

func (*fakeBackend) Start(_ context.Context, opts *agent.Options, msgCh chan<- agent.Message, logW io.Writer) (*agent.Session, error) {
	cmd := exec.Command("python3", "-u", "-c", string(fake.Script)) //nolint:gosec // fake.Script is an embedded constant
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s := agent.NewSession(cmd, stdin, stdout, msgCh, logW, claude.Wire, nil)
	if opts.InitialPrompt.Text != "" {
		if err := s.Send(opts.InitialPrompt); err != nil {
			s.Close()
			return nil, fmt.Errorf("write prompt: %w", err)
		}
	}
	return s, nil
}

func (*fakeBackend) AttachRelay(context.Context, string, int64, chan<- agent.Message, io.Writer) (*agent.Session, error) {
	return nil, errors.New("fake backend does not support relay")
}

func (*fakeBackend) ReadRelayOutput(context.Context, string) ([]agent.Message, int64, error) {
	return nil, 0, errors.New("fake backend does not support relay")
}

func (*fakeBackend) ParseMessage(line []byte) ([]agent.Message, error) {
	return claude.ParseMessage(line)
}

func (*fakeBackend) Models() []string { return []string{"fake-model"} }

func (*fakeBackend) SupportsImages() bool { return true }

func (*fakeBackend) ContextWindowLimit(string) int { return 180_000 }

// cacheDir returns the caic log/cache directory, using $XDG_CACHE_HOME/caic/
// with a fallback to ~/.cache/caic/.
func cacheDir() string {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "caic")
}

// configDir returns the caic config directory: $XDG_CONFIG_HOME/caic/ with a fallback
// to ~/.config/caic/.
func configDir() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "caic")
}

func parseInt64(s string) int64 {
	if s == "" {
		return 0
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		slog.Warn("invalid int64 env value", "val", s) //nolint:gosec // G706: config value, not user input
		return 0
	}
	return id
}

// readFileOrEnv returns the contents of the file at path if it exists,
// otherwise returns path itself (allowing the env var to hold the PEM directly).
func readFileOrEnv(envVar string) string {
	v := os.Getenv(envVar)
	if v == "" {
		return ""
	}
	data, err := os.ReadFile(v) //nolint:gosec // path from trusted env var
	if err != nil {
		// Not a file path — treat the env var value as the PEM content directly.
		return v
	}
	return string(data)
}

// watchExecutable watches the current executable for modifications and calls
// stop to trigger graceful shutdown when detected. Combined with systemd's
// Restart=always, this enables seamless restarts after a rebuild.
func watchExecutable(ctx context.Context, stop context.CancelFunc) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := w.Add(exe); err != nil {
		_ = w.Close()
		return err
	}
	go func() {
		defer func() { _ = w.Close() }()
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-w.Events:
				if !ok {
					return
				}
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Chmod) {
					slog.Info("executable modified, shutting down")
					stop()
					return
				}
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				slog.Warn("error watching executable", "err", err)
			}
		}
	}()
	return nil
}

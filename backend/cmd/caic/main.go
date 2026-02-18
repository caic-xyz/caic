package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/lmittmann/tint"
	"github.com/maruel/caic/backend/internal/agent"
	"github.com/maruel/caic/backend/internal/agent/claude"
	"github.com/maruel/caic/backend/internal/agent/fake"
	"github.com/maruel/caic/backend/internal/server"
	"github.com/maruel/caic/backend/internal/task"
	"github.com/mattn/go-colorable"
	"github.com/mattn/go-isatty"
)

// envDefault returns the value of the named environment variable, or def if unset/empty.
func envDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// envInt returns the integer value of the named environment variable, or def if unset/empty/invalid.
func envInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n := 0
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return def
	}
	return n
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

  CAIC_HTTP                 HTTP listen address (e.g. :8080)
  CAIC_ROOT                 Parent directory containing git repos
  CAIC_MAX_TURNS            Max agentic turns per task (0=unlimited)
  CAIC_LOG_LEVEL            Log level: debug, info, warn, error (default: info)
  CAIC_COMMITDESC_PROVIDER  AI provider for commit description generation
  CAIC_COMMITDESC_MODEL     Model name for commit description generation
  GEMINI_API_KEY            Gemini API key for the Gemini agent backend
  TAILSCALE_API_KEY         Tailscale API key for Tailscale integration

See contrib/caic.env for a template with all variables and documentation.
`)
	}

	maxTurns := flag.Int("max-turns", envInt("CAIC_MAX_TURNS", 0), "max agentic turns per task (0=unlimited)")
	addr := flag.String("http", os.Getenv("CAIC_HTTP"), "start web UI on this address (e.g. :8080)")
	root := flag.String("root", os.Getenv("CAIC_ROOT"), "parent directory containing git repos")
	logLevel := flag.String("log-level", envDefault("CAIC_LOG_LEVEL", "info"), "log level (debug, info, warn, error)")
	fakeMode := flag.Bool("fake", false, "use fake container/agent ops (for e2e tests); creates a temp repo when -root is omitted")
	flag.Parse()
	if args := flag.Args(); len(args) > 0 {
		return fmt.Errorf("unexpected arguments: %v", args)
	}

	initLogging(*logLevel)

	cfg := &server.Config{
		GeminiAPIKey:          os.Getenv("GEMINI_API_KEY"),
		TailscaleAPIKey:       os.Getenv("TAILSCALE_API_KEY"),
		CommitDescGenProvider: os.Getenv("CAIC_COMMITDESC_PROVIDER"),
		CommitDescGenModel:    os.Getenv("CAIC_COMMITDESC_MODEL"),
	}

	if key := cfg.GeminiAPIKey; key != "" {
		suffix := key
		if len(suffix) > 4 {
			suffix = suffix[len(suffix)-4:]
		}
		slog.Info("GEMINI_API_KEY configured", "suffix", suffix)
	} else {
		slog.Warn("GEMINI_API_KEY not set")
	}
	if key := cfg.TailscaleAPIKey; key != "" {
		suffix := key
		if len(suffix) > 4 {
			suffix = suffix[len(suffix)-4:]
		}
		slog.Info("TAILSCALE_API_KEY configured", "suffix", suffix)
	} else {
		slog.Warn("TAILSCALE_API_KEY not set")
	}
	slog.Info("commit description", "provider", cfg.CommitDescGenProvider, "model", cfg.CommitDescGenModel)

	if *fakeMode {
		return serveFake(ctx, *addr, *root, cfg)
	}
	if *addr == "" {
		return errors.New("HTTP address is required: set -http flag or CAIC_HTTP env var")
	}
	if *root == "" {
		return errors.New("root directory is required: set -root flag or CAIC_ROOT env var")
	}

	logDir := cacheDir()

	// Exit when executable is rebuilt (systemd restarts the service).
	if err := watchExecutable(ctx, cancel); err != nil {
		slog.Warn("failed to watch executable", "err", err)
	}
	return serveHTTP(ctx, *addr, *root, *maxTurns, logDir, cfg)
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

func serveHTTP(ctx context.Context, addr, rootDir string, maxTurns int, logDir string, cfg *server.Config) error {
	srv, err := server.New(ctx, rootDir, maxTurns, logDir, cfg)
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
		addr = ":8090"
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

	logDir := filepath.Join(os.TempDir(), "caic-e2e-logs")
	srv, err := server.New(ctx, rootDir, 1, logDir, cfg)
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
	if opts.Prompt != "" {
		if err := s.Send(opts.Prompt); err != nil {
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

func (*fakeBackend) ParseMessage(line []byte) (agent.Message, error) {
	return agent.ParseMessage(line)
}

func (*fakeBackend) Models() []string { return []string{"fake-model"} }

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

// Entry point for the caic server: parses flags, loads config, and starts the HTTP server.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-colorable"
	"github.com/mattn/go-isatty"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/autoupdate"
	"github.com/caic-xyz/caic/backend/internal/forge/github"
	"github.com/caic-xyz/caic/backend/internal/server"
)

// expandTilde replaces a leading "~/" or bare "~" with the current user's home directory.
func expandTilde(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return path, err
		}
		rest := strings.TrimLeft(path[1:], `/\`)
		return filepath.Join(home, rest), nil
	}
	return filepath.Abs(path)
}

// localizeAddr defaults to localhost when the address specifies a port but no
// host (e.g. ":2242" → "localhost:2242"). This avoids accidentally listening
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		select {
		case s := <-sig:
			slog.Info("shutdown", "signal", s)
			cancel()
		case <-ctx.Done():
		}
		signal.Stop(sig)
	}()

	flag.Usage = func() {
		w := flag.CommandLine.Output()
		_, _ = fmt.Fprintf(w, `Usage: caic [flags]

caic manages multiple coding agents in parallel. Each task runs in an isolated
container with the agent communicating over SSH.

Configuration is loaded from config.toml inside the config directory
(default: ~/.config/caic/). See contrib/config.toml for a documented template.

Flags:
`)
		flag.PrintDefaults()
	}

	cfgDirFlag := flag.String("config-dir", "", "config directory (default: ~/.config/caic)")
	versionFlag := flag.Bool("version", false, "print version and exit")
	printURLFlag := flag.Bool("print-url", false, "print the server URL and exit")
	traceFlag := flag.String("trace", "", "preload a JSONL trace file as a terminal task (fake mode only)")
	flag.Parse()
	if *versionFlag {
		fmt.Println(autoupdate.Version)
		return nil
	}
	if args := flag.Args(); len(args) > 0 {
		return fmt.Errorf("unexpected arguments: %v", args)
	}

	// Resolve config directory: -config-dir flag > XDG default.
	cfgDir := configDir()
	if *cfgDirFlag != "" {
		d, err := expandTilde(*cfgDirFlag)
		if err != nil {
			return fmt.Errorf("config-dir: %w", err)
		}
		cfgDir = d
	}

	// Suppress log noise when only printing the URL.
	if *printURLFlag {
		slog.SetLogLoggerLevel(slog.LevelError)
	}

	// Load configuration from TOML file.
	tc, err := loadTOMLConfig(cfgDir)
	if err != nil {
		return err
	}
	cfg, addr, root, logLevel, err := tomlToServerConfig(ctx, &tc, cfgDir)
	if err != nil {
		return err
	}
	if *printURLFlag {
		a := localizeAddr(addr)
		var lc net.ListenConfig
		ln, err := lc.Listen(ctx, "tcp", a)
		if err != nil {
			return fmt.Errorf("port %s is not available (already in use?)", a)
		}
		_ = ln.Close()
		fmt.Printf("http://%s\n", a)
		return nil
	}

	// Validate geo_db file exists if explicitly set in config.
	if tc.Server.GeoDB != nil {
		if _, err := os.Stat(cfg.IPGeoDB); err != nil {
			return fmt.Errorf("geo_db file not found at %q: %w", cfg.IPGeoDB, err)
		}
	}

	if root, err = expandTilde(root); err != nil {
		return err
	}
	for _, p := range []*string{&tc.Debug.CPUProfile, &tc.Debug.Trace, &tc.Debug.MemProfile} {
		if *p == "" {
			continue
		}
		if *p, err = expandTilde(*p); err != nil {
			return fmt.Errorf("debug path: %w", err)
		}
	}

	initLogging(logLevel, tc.Debug.NoLogTime)

	// File-based profiling: CPU, heap, and execution trace.
	if tc.Debug.CPUProfile != "" {
		f, err := os.Create(tc.Debug.CPUProfile)
		if err != nil {
			return fmt.Errorf("create CPU profile: %w", err)
		}
		defer func() { _ = f.Close() }()
		if err := pprof.StartCPUProfile(f); err != nil {
			return fmt.Errorf("start CPU profile: %w", err)
		}
		defer pprof.StopCPUProfile()
		slog.Info("CPU profiling enabled", "file", tc.Debug.CPUProfile)
	}
	if tc.Debug.Trace != "" {
		f, err := os.Create(tc.Debug.Trace)
		if err != nil {
			return fmt.Errorf("create trace: %w", err)
		}
		defer func() { _ = f.Close() }()
		if err := trace.Start(f); err != nil {
			return fmt.Errorf("start trace: %w", err)
		}
		defer trace.Stop()
		trace.Log(ctx, "trace", "started")
		slog.Info("execution trace enabled", "file", tc.Debug.Trace)
	}
	if tc.Debug.MemProfile != "" {
		defer func() {
			runtime.GC()
			f, err := os.Create(tc.Debug.MemProfile)
			if err != nil {
				slog.Error("create heap profile", "err", err)
				return
			}
			if err := pprof.WriteHeapProfile(f); err != nil {
				slog.Error("write heap profile", "err", err)
			} else {
				slog.Info("heap profile written", "file", tc.Debug.MemProfile)
			}
			_ = f.Close()
		}()
	}

	slog.Info("gemini", "apikey", auth.MaskedToken(cfg.GeminiAPIKey))
	slog.Info("tailscale", "apikey", auth.MaskedToken(cfg.TailscaleAPIKey))
	slog.Info("LLM", "provider", cfg.LLMProvider, "model", cfg.LLMModel)

	if err := cfg.Validate(); err != nil {
		return err
	}
	if isFakeMode {
		return serveFake(ctx, addr, cfg, *traceFlag)
	}
	addr = localizeAddr(addr)

	// Open the listener early to detect port conflicts before lengthy
	// initialisation (container discovery, repo scanning, etc.).
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	defer func() { _ = ln.Close() }()
	slog.Info("port acquired", "addr", ln.Addr())

	// Exit when executable or config is modified (systemd/launchd restarts the service).
	if err := watchForRestart(ctx, cancel, cfgDir); err != nil {
		return fmt.Errorf("failed to set up file watcher: %w", err)
	}
	// Auto-update: checks GitHub Releases on a cron schedule and replaces the binary.
	if v := autoupdate.Version; v != "" && !strings.HasPrefix(v, "devel-") {
		sched, err := autoUpdateSchedule(&tc)
		if err != nil {
			return err
		}
		if sched != nil {
			go autoupdate.Run(ctx, github.NewClient(cfg.GitHubToken, http.DefaultTransport), sched)
		}
	}
	return serveHTTP(ctx, ln, root, cfg)
}

// roundDur rounds d to 3 significant digits.
func roundDur(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	ns := int64(d)
	unit := int64(1)
	for ns/unit >= 1000 {
		unit *= 10
	}
	return time.Duration((ns + unit/2) / unit * unit)
}

// initLogging configures slog with tint for colored, concise output.
// Timestamps are omitted when noLogTime is true, and zero-value
// attributes are dropped.
func initLogging(level string, noLogTime bool) {
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
	homeDir, _ := os.UserHomeDir()
	slog.SetDefault(slog.New(tint.NewHandler(colorable.NewColorable(os.Stderr), &tint.Options{
		Level:      ll,
		TimeFormat: "15:04:05.000",
		NoColor:    !isatty.IsTerminal(os.Stderr.Fd()),
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if noLogTime && a.Key == slog.TimeKey && len(groups) == 0 {
				return slog.Attr{}
			}
			val := a.Value.Any()
			skip := false
			switch t := val.(type) {
			case string:
				skip = t == ""
				if !skip && homeDir != "" && strings.HasPrefix(t, homeDir) {
					a = slog.String(a.Key, "~"+t[len(homeDir):])
				}
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
				if !skip {
					a = slog.Duration(a.Key, roundDur(t))
				}
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

func serveHTTP(ctx context.Context, ln net.Listener, rootDir string, cfg *server.Config) error {
	srv, err := server.New(ctx, rootDir, cfg)
	if err != nil {
		return err
	}
	return srv.Serve(ctx, ln)
}

func main() {
	if err := mainImpl(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "caic: %v\n", err)
		os.Exit(1)
	}
}

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

// resolveGitHubTokenFromGH attempts to obtain a GitHub token from the gh CLI
// (gh auth token). Returns "" if the CLI is not available or fails.
func resolveGitHubTokenFromGH(ctx context.Context) string {
	ghPath, err := exec.LookPath("gh")
	if err != nil {
		return ""
	}
	out, err := exec.CommandContext(ctx, ghPath, "auth", "token").Output() //nolint:gosec // ghPath resolved via LookPath
	if err != nil {
		slog.Warn("GITHUB_TOKEN", "msg", "gh CLI found but gh auth token failed", "err", err, "out", string(out))
		return ""
	}
	return strings.TrimSpace(string(out))
}

// watchForRestart watches the executable and config.toml for modifications and
// calls stop to trigger graceful shutdown when either changes. Combined with
// systemd's Restart=always or launchd's KeepAlive, this enables seamless
// restarts after a rebuild or config change.
func watchForRestart(ctx context.Context, stop context.CancelFunc, cfgDir string) error {
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
	// Watch the config directory (not just the file) so we catch
	// rename-into-place writes (common with editors like vim).
	configPath := filepath.Join(cfgDir, "config.toml")
	if _, statErr := os.Stat(configPath); statErr == nil {
		if err := w.Add(cfgDir); err != nil {
			_ = w.Close()
			return err
		}
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
				var reason string
				switch {
				case event.Name == exe:
					switch runtime.GOOS {
					case "darwin":
						// macOS replaces the binary via CREATE (rename-into-place).
						if !event.Has(fsnotify.Create) {
							continue
						}
					default:
						if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Chmod) {
							continue
						}
					}
					reason = "executable modified"
				case filepath.Base(event.Name) == "config.toml":
					if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Create) {
						continue
					}
					reason = "config.toml modified"
				default:
					continue
				}
				slog.Info("shutdown", "reason", reason, "ev", event)
				stop()
				return
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				slog.Warn("fsnotify", "err", err)
			}
		}
	}()
	return nil
}

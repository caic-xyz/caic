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
	"strconv"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/server"
	"github.com/fsnotify/fsnotify"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-colorable"
	"github.com/mattn/go-isatty"
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
    GITHUB_TOKEN                PAT for PR/CI; single-user (mutually exclusive with GITHUB_OAUTH_CLIENT_ID); auto-detected from gh CLI if unset
    GITHUB_OAUTH_CLIENT_ID      OAuth app client ID; multi-user login (mutually exclusive with GITHUB_TOKEN)
    GITHUB_OAUTH_CLIENT_SECRET  OAuth app client secret
    GITHUB_OAUTH_ALLOWED_USERS  Comma-separated GitHub usernames allowed to log in (required with OAuth)
    GITHUB_APP_ID               GitHub App ID for org-wide webhooks and installation tokens
    GITHUB_APP_PRIVATE_KEY_PEM  Path to PEM file (relative to ~/.config/caic/)
    GITHUB_APP_ALLOWED_OWNERS   Comma-separated owners/orgs allowed to install the app; rejects others
    GITHUB_WEBHOOK_SECRET       HMAC-SHA256 secret; enables POST /webhooks/github

  GitLab — choose one of PAT or OAuth:
    GITLAB_TOKEN                PAT for MR/CI; single-user (mutually exclusive with GITLAB_OAUTH_CLIENT_ID)
    GITLAB_OAUTH_CLIENT_ID      OAuth app client ID; multi-user login (mutually exclusive with GITLAB_TOKEN)
    GITLAB_OAUTH_CLIENT_SECRET  OAuth app client secret
    GITLAB_OAUTH_ALLOWED_USERS  Comma-separated GitLab usernames allowed to log in (required with OAuth)
    GITLAB_URL                  GitLab instance URL (default: https://gitlab.com)
    GITLAB_WEBHOOK_SECRET       Shared secret; enables POST /webhooks/gitlab

  Agents:
    GEMINI_API_KEY              Gemini API key for the Gemini Live voice agent
    TAILSCALE_API_KEY           Tailscale API key for Tailscale ephemeral node

  IP geolocation (optional):
    CAIC_IPGEO_DB               Path to a MaxMind MMDB file; relative paths resolve against ~/.config/caic/ (e.g. GeoLite2-Country.mmdb)
    CAIC_IPGEO_ALLOWLIST        Comma-separated allowlist (default: "local,tailscale,github"): ISO country codes (e.g. CA,US), "local", "tailscale", "github", or CIDR ranges (e.g. 34.74.90.64/28); requires CAIC_IPGEO_DB when country codes are present

See contrib/caic.env for a template with all variables and documentation.
`)
	}

	addr := flag.String("http", envDefault("CAIC_HTTP", ":8080"), "start web UI on this address (e.g. :8080)")
	root := flag.String("root", envDefault("CAIC_ROOT", "."), "parent directory containing git repos")
	logLevel := flag.String("log-level", envDefault("CAIC_LOG_LEVEL", "info"), "log level (debug, info, warn, error)")
	flag.Parse()
	if args := flag.Args(); len(args) > 0 {
		return fmt.Errorf("unexpected arguments: %v", args)
	}
	var err error
	if *root, err = expandTilde(*root); err != nil {
		return err
	}

	initLogging(*logLevel)

	cfg := &server.Config{
		GeminiAPIKey:            os.Getenv("GEMINI_API_KEY"),
		TailscaleAPIKey:         os.Getenv("TAILSCALE_API_KEY"),
		LLMProvider:             os.Getenv("CAIC_LLM_PROVIDER"),
		LLMModel:                os.Getenv("CAIC_LLM_MODEL"),
		ConfigDir:               configDir(),
		CacheDir:                cacheDir(),
		GitHubToken:             resolveGitHubToken(),
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
		GitHubAppPrivateKeyPEM:  []byte(readFileFromEnv("GITHUB_APP_PRIVATE_KEY_PEM")),
		GitHubAppAllowedOwners:  os.Getenv("GITHUB_APP_ALLOWED_OWNERS"),
		GitLabWebhookSecret:     []byte(os.Getenv("GITLAB_WEBHOOK_SECRET")),
		IPGeoDB:                 resolvePathFromEnv("CAIC_IPGEO_DB"),
		IPGeoAllowlist:          envDefault("CAIC_IPGEO_ALLOWLIST", "local,tailscale,github"),
	}

	slog.Info("gemini", "apikey", maskedToken(cfg.GeminiAPIKey))                                            //nolint:gosec // G706: value from env, not user input
	slog.Info("tailscale", "apikey", maskedToken(cfg.TailscaleAPIKey))                                      //nolint:gosec // G706: value from env, not user input
	slog.Info("LLM", "provider", cfg.LLMProvider, "model", cfg.LLMModel)                                    //nolint:gosec // G706: value from env, not user input
	slog.Info("github", "pat", maskedToken(cfg.GitHubToken), "oauth", maskedToken(cfg.GitHubOAuthClientID)) //nolint:gosec // G706: value from env, not user input
	slog.Info("gitlab", "pat", maskedToken(cfg.GitLabToken), "oauth", maskedToken(cfg.GitLabOAuthClientID)) //nolint:gosec // G706: value from env, not user input

	if err := cfg.Validate(); err != nil {
		return err
	}
	if isFakeMode {
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
	homeDir, _ := os.UserHomeDir()
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

// maskedToken is a credential string that logs as "xxx...1234" (last 4 chars
// visible, remainder replaced with "x"). Implements slog.LogValuer so the
// masking happens inside the type and no nolint directives are needed.
type maskedToken string

func (m maskedToken) LogValue() slog.Value {
	s := string(m)
	if s == "" {
		return slog.StringValue("")
	}
	if len(s) <= 4 {
		return slog.StringValue(s)
	}
	return slog.StringValue(strings.Repeat("x", len(s)-4) + s[len(s)-4:])
}

// resolveGitHubToken returns the GitHub token to use. It returns GITHUB_TOKEN
// if set. Otherwise, when OAuth is not configured, it attempts to obtain a
// token from the gh CLI (gh auth token). Returns "" if neither is available.
func resolveGitHubToken() string {
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		return t
	}
	// Don't try gh when OAuth is configured — the two modes are mutually
	// exclusive and mixing them would cause a startup error.
	if os.Getenv("GITHUB_OAUTH_CLIENT_ID") != "" {
		return ""
	}
	ghPath, err := exec.LookPath("gh")
	if err != nil {
		return ""
	}
	out, err := exec.Command(ghPath, "auth", "token").Output() //nolint:gosec // ghPath resolved via LookPath
	if err != nil {
		slog.Warn("GITHUB_TOKEN", "msg", "gh CLI found but gh auth token failed", "err", err, "out", string(out))
		return ""
	}
	return strings.TrimSpace(string(out))
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

// resolvePathFromEnv returns the path stored in the given env var, resolving
// relative paths against the config directory (~/.config/caic/).
// Returns "" if the env var is unset.
func resolvePathFromEnv(envVar string) string {
	v := os.Getenv(envVar)
	if v == "" {
		return ""
	}
	if !filepath.IsAbs(v) {
		return filepath.Join(configDir(), v)
	}
	return v
}

// readFileFromEnv reads the file path stored in the given env var and returns its
// contents. Relative paths are resolved against the config directory
// (~/.config/caic/).
func readFileFromEnv(envVar string) string {
	v := os.Getenv(envVar)
	if v == "" {
		return ""
	}
	path := v
	if !filepath.IsAbs(path) {
		path = filepath.Join(configDir(), path)
	}
	data, err := os.ReadFile(path) //nolint:gosec // path from trusted env var
	if err != nil {
		slog.Error("failed to read file from env var", "env", envVar, "path", path, "err", err) //nolint:gosec // path from trusted env var
		return ""
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

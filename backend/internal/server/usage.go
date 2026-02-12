// Claude Code OAuth usage quota fetcher with caching, credential file
// watching, and exponential backoff on errors.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/maruel/caic/backend/internal/server/dto"
)

const (
	usageAPIURL   = "https://api.anthropic.com/api/oauth/usage"
	usageCacheTTL = 30 * time.Second

	// Exponential backoff parameters for fetch errors.
	backoffMin = 30 * time.Second
	backoffMax = 1 * time.Hour
)

// usageFetcher fetches and caches Claude Code usage quota data. It watches
// ~/.claude/.credentials.json for changes and applies exponential backoff when
// fetches fail.
type usageFetcher struct {
	client *http.Client

	mu       sync.Mutex
	token    string
	cached   *dto.UsageResp
	fetchAt  time.Time     // when cached was last successfully fetched
	backoff  time.Duration // current backoff; 0 means no backoff
	errorAt  time.Time     // when the last error occurred
	watcher  *fsnotify.Watcher
	credPath string // resolved path to .credentials.json
}

// newUsageFetcher creates a fetcher and starts watching
// ~/.claude/.credentials.json for token changes. The watcher goroutine exits
// when ctx is cancelled.
func newUsageFetcher(ctx context.Context) *usageFetcher {
	home, err := os.UserHomeDir()
	if err != nil {
		slog.Warn("cannot determine home dir; usage disabled", "err", err)
		return nil
	}
	credPath := filepath.Join(home, ".claude", ".credentials.json")

	token := os.Getenv("CLAUDE_OAUTH_TOKEN")
	if token == "" {
		token = readCredentialsToken()
	}
	if token == "" {
		slog.Warn("no Claude OAuth token found; usage endpoint disabled (will watch for credentials)")
	}

	f := &usageFetcher{
		client:   &http.Client{Timeout: 10 * time.Second},
		token:    token,
		credPath: credPath,
	}

	if err := f.startWatcher(ctx); err != nil {
		slog.Warn("failed to watch credentials file", "err", err)
	}
	return f
}

// startWatcher sets up fsnotify on the credentials file. It watches the parent
// directory so it catches creates/renames (atomic writes).
func (f *usageFetcher) startWatcher(ctx context.Context) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	// Watch the directory so we catch atomic-write patterns (write to
	// tmp + rename) that don't fire events on the file itself.
	dir := filepath.Dir(f.credPath)
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		return err
	}
	f.watcher = w
	go f.watchLoop(ctx)
	return nil
}

func (f *usageFetcher) watchLoop(ctx context.Context) {
	defer func() { _ = f.watcher.Close() }()
	base := filepath.Base(f.credPath)
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-f.watcher.Events:
			if !ok {
				return
			}
			if filepath.Base(ev.Name) != base {
				continue
			}
			if !ev.Has(fsnotify.Write) && !ev.Has(fsnotify.Create) {
				continue
			}
			f.onCredentialsChanged()
		case err, ok := <-f.watcher.Errors:
			if !ok {
				return
			}
			slog.Warn("credentials watcher error", "err", err)
		}
	}
}

func (f *usageFetcher) onCredentialsChanged() {
	token := readCredentialsToken()
	if token == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if token == f.token {
		return
	}
	f.token = token
	f.backoff = 0
	f.errorAt = time.Time{}
	f.cached = nil
	f.fetchAt = time.Time{}
	slog.Info("credentials updated, token refreshed")
}

// hasToken reports whether a token is available.
func (f *usageFetcher) hasToken() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.token != ""
}

// get returns the cached usage data, refreshing if stale. Respects
// exponential backoff on prior errors.
func (f *usageFetcher) get() *dto.UsageResp {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.token == "" {
		return nil
	}
	// Still within cache TTL?
	if f.cached != nil && time.Since(f.fetchAt) < usageCacheTTL {
		return f.cached
	}
	// In backoff window?
	if f.backoff > 0 && time.Since(f.errorAt) < f.backoff {
		return f.cached
	}
	resp, err := f.fetch()
	if err != nil {
		slog.Warn("failed to fetch usage", "err", err)
		f.errorAt = time.Now()
		if f.backoff == 0 {
			f.backoff = backoffMin
		} else {
			f.backoff *= 2
			if f.backoff > backoffMax {
				f.backoff = backoffMax
			}
		}
		return f.cached
	}
	f.backoff = 0
	f.cached = resp
	f.fetchAt = time.Now()
	return resp
}

func (f *usageFetcher) fetch() (*dto.UsageResp, error) {
	req, err := http.NewRequest(http.MethodGet, usageAPIURL, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("usage API returned %d: %s", resp.StatusCode, body)
	}

	var raw struct {
		FiveHour *struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    string  `json:"resets_at"`
		} `json:"five_hour"`
		SevenDay *struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    string  `json:"resets_at"`
		} `json:"seven_day"`
		ExtraUsage *struct {
			IsEnabled    bool    `json:"is_enabled"`
			MonthlyLimit float64 `json:"monthly_limit"`
			UsedCredits  float64 `json:"used_credits"`
			Utilization  float64 `json:"utilization"`
		} `json:"extra_usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode usage: %w", err)
	}

	out := &dto.UsageResp{}
	if raw.FiveHour != nil {
		out.FiveHour = dto.UsageWindow{
			Utilization: raw.FiveHour.Utilization,
			ResetsAt:    raw.FiveHour.ResetsAt,
		}
	}
	if raw.SevenDay != nil {
		out.SevenDay = dto.UsageWindow{
			Utilization: raw.SevenDay.Utilization,
			ResetsAt:    raw.SevenDay.ResetsAt,
		}
	}
	if raw.ExtraUsage != nil {
		out.ExtraUsage = dto.ExtraUsage{
			IsEnabled:    raw.ExtraUsage.IsEnabled,
			MonthlyLimit: raw.ExtraUsage.MonthlyLimit,
			UsedCredits:  raw.ExtraUsage.UsedCredits,
			Utilization:  raw.ExtraUsage.Utilization,
		}
	}
	return out, nil
}

// readCredentialsToken reads the OAuth token from ~/.claude/.credentials.json.
func readCredentialsToken() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json")) //nolint:gosec // fixed well-known path
	if err != nil {
		return ""
	}
	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(data, &creds) != nil {
		return ""
	}
	return creds.ClaudeAiOauth.AccessToken
}

// Claude Code OAuth subscription quota fetcher with caching, credential file watching, and exponential backoff.

package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/fsnotify/fsnotify"
)

const claudeCodeUsageAPIURL = "https://api.anthropic.com/api/oauth/usage"

// anthropicUsagePayload mirrors the Anthropic GET /api/oauth/usage response.
type anthropicUsagePayload struct {
	FiveHour   *anthropicUsageWindow `json:"five_hour"`
	SevenDay   *anthropicUsageWindow `json:"seven_day"`
	ExtraUsage *anthropicExtraUsage  `json:"extra_usage"`
}

type anthropicUsageWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

type anthropicExtraUsage struct {
	IsEnabled    bool    `json:"is_enabled"`
	MonthlyLimit float64 `json:"monthly_limit"`
	UsedCredits  float64 `json:"used_credits"`
	Utilization  float64 `json:"utilization"`
}

// ClaudeCodeFetcher fetches and caches Claude Code OAuth subscription quota data. It
// watches ~/.claude/.credentials.json for token changes and applies
// exponential backoff when fetches fail.
type ClaudeCodeFetcher struct {
	baseFetcher

	client   *http.Client
	watcher  *fsnotify.Watcher
	credPath string
	token    string
}

// NewClaudeCodeFetcher creates a fetcher and starts watching
// ~/.claude/.credentials.json for token changes. Returns nil if the home
// directory cannot be determined. The watcher goroutine exits when ctx is
// cancelled.
func NewClaudeCodeFetcher(ctx context.Context) *ClaudeCodeFetcher {
	home, err := os.UserHomeDir()
	if err != nil {
		slog.WarnContext(ctx, "cannot determine home dir; Claude Code usage disabled", "err", err)
		return nil
	}
	credPath := filepath.Join(home, ".claude", ".credentials.json")
	token := readClaudeCodeToken(credPath)
	if token == "" {
		slog.InfoContext(ctx, "no Claude Code OAuth token found; usage disabled (will watch for credentials)")
	}

	f := &ClaudeCodeFetcher{
		baseFetcher: newBaseFetcher(agent.QuotaProviderClaudeCode, "Claude Code", AuthKindOAuth, "https://claude.ai/settings/usage"),
		client:      &http.Client{Timeout: 10 * time.Second},
		token:       token,
		credPath:    credPath,
	}

	if err := f.startWatcher(ctx); err != nil {
		slog.WarnContext(ctx, "failed to watch Claude Code credentials file", "err", err)
	}
	return f
}

// Get returns the cached quota data, refreshing if stale. Returns nil when
// no token is available.
func (f *ClaudeCodeFetcher) Get(ctx context.Context) *ProviderQuota {
	return f.getIf(ctx, func() bool { return f.token != "" }, f.fetch)
}

func (f *ClaudeCodeFetcher) fetch(ctx context.Context) (*ProviderQuota, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeCodeUsageAPIURL, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("User-Agent", "caic")
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("usage API returned %d: %s", resp.StatusCode, body)
	}

	var raw anthropicUsagePayload
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode usage: %w", err)
	}

	out := f.quota()
	if raw.FiveHour != nil {
		t, _ := time.Parse(time.RFC3339, raw.FiveHour.ResetsAt)
		out.RateLimits = append(out.RateLimits, QuotaRateLimit{
			Window:   "5h",
			UsedPct:  raw.FiveHour.Utilization,
			ResetsAt: t,
		})
	}
	if raw.SevenDay != nil {
		t, _ := time.Parse(time.RFC3339, raw.SevenDay.ResetsAt)
		out.RateLimits = append(out.RateLimits, QuotaRateLimit{
			Window:   "7d",
			UsedPct:  raw.SevenDay.Utilization,
			ResetsAt: t,
		})
	}
	if raw.ExtraUsage != nil {
		out.ExtraUsage = QuotaExtraUsage{
			Currency:     "USD",
			IsEnabled:    raw.ExtraUsage.IsEnabled,
			MonthlyLimit: raw.ExtraUsage.MonthlyLimit / 100,
			UsedCredits:  raw.ExtraUsage.UsedCredits / 100,
			UsedPct:      raw.ExtraUsage.Utilization,
		}
	}
	return out, nil
}

func (f *ClaudeCodeFetcher) startWatcher(ctx context.Context) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	dir := filepath.Dir(f.credPath)
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		return err
	}
	f.watcher = w
	go f.watchLoop(ctx)
	return nil
}

func (f *ClaudeCodeFetcher) watchLoop(ctx context.Context) {
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
			slog.WarnContext(ctx, "credentials watcher error", "err", err)
		}
	}
}

func (f *ClaudeCodeFetcher) onCredentialsChanged() {
	token := readClaudeCodeToken(f.credPath)
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
	slog.Info("credentials updated, Claude Code token refreshed")
}

func readClaudeCodeToken(credPath string) string {
	data, err := os.ReadFile(credPath) //nolint:gosec // credPath from os.UserHomeDir
	if err != nil {
		return ""
	}
	var creds anthropicCreds
	_ = json.Unmarshal(data, &creds)
	return creds.ClaudeAiOauth.AccessToken
}

type anthropicCreds struct {
	ClaudeAiOauth struct {
		AccessToken string `json:"accessToken"`
	} `json:"claudeAiOauth"`
}

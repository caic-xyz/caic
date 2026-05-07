// Anthropic OAuth usage quota fetcher with caching, credential file watching,
// and exponential backoff on errors. Implements ProviderFetcher.
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
	"sync"
	"time"

	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
	"github.com/fsnotify/fsnotify"
)

const anthropicUsageAPIURL = "https://api.anthropic.com/api/oauth/usage"

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

// AnthropicFetcher fetches and caches Anthropic OAuth usage quota data. It
// watches ~/.claude/.credentials.json for token changes and applies
// exponential backoff when fetches fail.
type AnthropicFetcher struct {
	client   *http.Client
	watcher  *fsnotify.Watcher
	credPath string

	mu      sync.Mutex
	token   string
	cached  *v1.ProviderQuota
	fetchAt time.Time
	backoff time.Duration
	errorAt time.Time
}

// NewAnthropicFetcher creates a fetcher and starts watching
// ~/.claude/.credentials.json for token changes. Returns nil if the home
// directory cannot be determined. The watcher goroutine exits when ctx is
// cancelled.
func NewAnthropicFetcher(ctx context.Context) *AnthropicFetcher {
	home, err := os.UserHomeDir()
	if err != nil {
		slog.Warn("cannot determine home dir; Anthropic usage disabled", "err", err)
		return nil
	}
	credPath := filepath.Join(home, ".claude", ".credentials.json")
	token := readAnthropicToken(credPath)
	if token == "" {
		slog.Info("no Claude OAuth token found; Anthropic usage disabled (will watch for credentials)")
	}

	f := &AnthropicFetcher{
		client:   &http.Client{Timeout: 10 * time.Second},
		token:    token,
		credPath: credPath,
	}

	if err := f.startWatcher(ctx); err != nil {
		slog.Warn("failed to watch Anthropic credentials file", "err", err)
	}
	return f
}

// Provider returns the provider identifier.
func (f *AnthropicFetcher) Provider() string { return "anthropic" }

// Label returns the human-readable provider name.
func (f *AnthropicFetcher) Label() string { return "Anthropic" }

// AuthKind returns the authentication method.
func (f *AnthropicFetcher) AuthKind() string { return "oauth" }

// UsageURL returns the link to the provider's usage/billing page.
func (f *AnthropicFetcher) UsageURL() string { return "https://claude.ai/settings/usage" }

// Get returns the cached quota data, refreshing if stale. Returns nil when
// no token is available.
func (f *AnthropicFetcher) Get(ctx context.Context) *v1.ProviderQuota {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.token == "" {
		return nil
	}
	if f.cached != nil && time.Since(f.fetchAt) < CacheTTL {
		return f.cached
	}
	if f.backoff > 0 && time.Since(f.errorAt) < f.backoff {
		return f.cached
	}
	resp, err := f.fetch(ctx)
	if err != nil {
		slog.WarnContext(ctx, "failed to fetch Anthropic usage", "err", err)
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

func (f *AnthropicFetcher) fetch(ctx context.Context) (*v1.ProviderQuota, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, anthropicUsageAPIURL, http.NoBody)
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

	out := &v1.ProviderQuota{
		Provider: f.Provider(),
		Label:    f.Label(),
		AuthKind: f.AuthKind(),
	}
	if raw.FiveHour != nil {
		t, _ := time.Parse(time.RFC3339, raw.FiveHour.ResetsAt)
		out.RateLimits = append(out.RateLimits, v1.QuotaRateLimit{
			Window:   "5h",
			UsedPct:  raw.FiveHour.Utilization,
			ResetsAt: t,
		})
	}
	if raw.SevenDay != nil {
		t, _ := time.Parse(time.RFC3339, raw.SevenDay.ResetsAt)
		out.RateLimits = append(out.RateLimits, v1.QuotaRateLimit{
			Window:   "7d",
			UsedPct:  raw.SevenDay.Utilization,
			ResetsAt: t,
		})
	}
	if raw.ExtraUsage != nil {
		out.ExtraUsage = v1.QuotaExtraUsage{
			Currency:     "USD",
			IsEnabled:    raw.ExtraUsage.IsEnabled,
			MonthlyLimit: raw.ExtraUsage.MonthlyLimit / 100,
			UsedCredits:  raw.ExtraUsage.UsedCredits / 100,
			UsedPct:      raw.ExtraUsage.Utilization,
		}
	}
	return out, nil
}

func (f *AnthropicFetcher) startWatcher(ctx context.Context) error {
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

func (f *AnthropicFetcher) watchLoop(ctx context.Context) {
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

func (f *AnthropicFetcher) onCredentialsChanged() {
	token := readAnthropicToken(f.credPath)
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
	slog.Info("credentials updated, Anthropic token refreshed")
}

func readAnthropicToken(credPath string) string {
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

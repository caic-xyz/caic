// Codex usage quota fetcher with caching, credential file watching, and
// exponential backoff on errors.
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

const codexUsageAPIURL = "https://chatgpt.com/backend-api/wham/usage"

// CodexFetcher fetches and caches Codex rate-limit usage data. It watches
// ~/.codex/auth.json for changes and applies exponential backoff when fetches
// fail.
type CodexFetcher struct {
	client *http.Client

	mu        sync.Mutex
	token     string
	accountID string
	cached    *v1.CodexUsage
	fetchAt   time.Time     // when cached was last successfully fetched
	backoff   time.Duration // current backoff; 0 means no backoff
	errorAt   time.Time     // when the last error occurred
	watcher   *fsnotify.Watcher
	authPath  string // resolved path to auth.json
}

// NewCodexFetcher creates a fetcher and starts watching
// ~/.codex/auth.json for token changes. Returns nil if the home directory
// cannot be determined. The watcher goroutine exits when ctx is cancelled.
func NewCodexFetcher(ctx context.Context) *CodexFetcher {
	home, err := os.UserHomeDir()
	if err != nil {
		slog.Warn("cannot determine home dir; codex usage disabled", "err", err)
		return nil
	}
	authPath := filepath.Join(home, ".codex", "auth.json")
	token, accountID := readCodexAuthToken(authPath)
	if token == "" {
		slog.Info("no Codex OAuth token found; codex usage endpoint disabled (will watch for credentials)")
	}

	f := &CodexFetcher{
		client:    &http.Client{Timeout: 10 * time.Second},
		token:     token,
		accountID: accountID,
		authPath:  authPath,
	}

	if err := f.startWatcher(ctx); err != nil {
		slog.Warn("failed to watch codex auth file", "err", err)
	}
	return f
}

func (f *CodexFetcher) startWatcher(ctx context.Context) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	dir := filepath.Dir(f.authPath)
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		return err
	}
	f.watcher = w
	go f.watchLoop(ctx)
	return nil
}

func (f *CodexFetcher) watchLoop(ctx context.Context) {
	defer func() { _ = f.watcher.Close() }()
	base := filepath.Base(f.authPath)
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
			f.onAuthChanged()
		case err, ok := <-f.watcher.Errors:
			if !ok {
				return
			}
			slog.Warn("codex auth watcher error", "err", err)
		}
	}
}

func (f *CodexFetcher) onAuthChanged() {
	token, accountID := readCodexAuthToken(f.authPath)
	if token == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if token == f.token {
		return
	}
	f.token = token
	f.accountID = accountID
	f.backoff = 0
	f.errorAt = time.Time{}
	f.cached = nil
	f.fetchAt = time.Time{}
	slog.Info("codex credentials updated, token refreshed")
}

// Get returns the cached Codex usage data, refreshing if stale.
func (f *CodexFetcher) Get(ctx context.Context) *v1.CodexUsage {
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
		slog.Warn("failed to fetch codex usage", "err", err)
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

func (f *CodexFetcher) fetch(ctx context.Context) (*v1.CodexUsage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageAPIURL, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("User-Agent", "caic")
	if f.accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", f.accountID)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("codex usage API returned %d: %s", resp.StatusCode, body)
	}

	var raw codexUsagePayload
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode codex usage: %w", err)
	}

	out := &v1.CodexUsage{PlanType: raw.PlanType}
	if raw.RateLimit != nil {
		if raw.RateLimit.PrimaryWindow != nil {
			out.Primary = &v1.CodexRateLimitWindow{
				UsedPercent:        raw.RateLimit.PrimaryWindow.UsedPercent,
				LimitWindowSeconds: raw.RateLimit.PrimaryWindow.LimitWindowSeconds,
				ResetAfterSeconds:  raw.RateLimit.PrimaryWindow.ResetAfterSeconds,
				ResetAt:            raw.RateLimit.PrimaryWindow.ResetAt,
			}
		}
		if raw.RateLimit.SecondaryWindow != nil {
			out.Secondary = &v1.CodexRateLimitWindow{
				UsedPercent:        raw.RateLimit.SecondaryWindow.UsedPercent,
				LimitWindowSeconds: raw.RateLimit.SecondaryWindow.LimitWindowSeconds,
				ResetAfterSeconds:  raw.RateLimit.SecondaryWindow.ResetAfterSeconds,
				ResetAt:            raw.RateLimit.SecondaryWindow.ResetAt,
			}
		}
	}
	if raw.Credits != nil {
		out.Credits = v1.CodexCredits{
			HasCredits: raw.Credits.HasCredits,
			Unlimited:  raw.Credits.Unlimited,
			Balance:    raw.Credits.Balance,
		}
	}
	return out, nil
}

// readCodexAuthToken reads the OAuth access token from ~/.codex/auth.json.
func readCodexAuthToken(authPath string) (token, accountID string) {
	data, err := os.ReadFile(authPath) //nolint:gosec // authPath is derived from os.UserHomeDir, not user input
	if err != nil {
		return "", ""
	}
	var auth codexAuthJSON
	_ = json.Unmarshal(data, &auth)
	return auth.Tokens.AccessToken, auth.Tokens.AccountID
}

// codexAuthJSON mirrors the Codex CLI auth.json structure.
type codexAuthJSON struct {
	Tokens codexTokenData `json:"tokens"`
}

type codexTokenData struct {
	AccessToken string `json:"access_token"`
	AccountID   string `json:"account_id"`
}

// codexUsagePayload mirrors the Codex RateLimitStatusPayload response.
type codexUsagePayload struct {
	PlanType  string                 `json:"plan_type"`
	RateLimit *codexRateLimitDetails `json:"rate_limit"`
	Credits   *codexCreditDetails    `json:"credits"`
}

type codexRateLimitDetails struct {
	Allowed         bool                 `json:"allowed"`
	LimitReached    bool                 `json:"limit_reached"`
	PrimaryWindow   *codexWindowSnapshot `json:"primary_window"`
	SecondaryWindow *codexWindowSnapshot `json:"secondary_window"`
}

type codexWindowSnapshot struct {
	UsedPercent        int `json:"used_percent"`
	LimitWindowSeconds int `json:"limit_window_seconds"`
	ResetAfterSeconds  int `json:"reset_after_seconds"`
	ResetAt            int `json:"reset_at"`
}

type codexCreditDetails struct {
	HasCredits bool   `json:"has_credits"`
	Unlimited  bool   `json:"unlimited"`
	Balance    string `json:"balance"`
}

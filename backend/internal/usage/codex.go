// Codex OAuth usage quota fetcher with caching, credential file watching, and exponential backoff.

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
	"strconv"
	"time"

	"github.com/fsnotify/fsnotify"
)

const codexUsageAPIURL = "https://chatgpt.com/backend-api/wham/usage"

// codexWindowSnapshot mirrors a single Codex rate-limit window in the
// usage API response.
type codexWindowSnapshot struct {
	UsedPercent        int `json:"used_percent"`
	LimitWindowSeconds int `json:"limit_window_seconds"`
	ResetAfterSeconds  int `json:"reset_after_seconds"`
	ResetAt            int `json:"reset_at"`
}

// codexUsagePayload mirrors the Codex GET /backend-api/wham/usage response.
type codexUsagePayload struct {
	PlanType  string `json:"plan_type"`
	RateLimit *struct {
		PrimaryWindow   *codexWindowSnapshot `json:"primary_window"`
		SecondaryWindow *codexWindowSnapshot `json:"secondary_window"`
	} `json:"rate_limit"`
	Credits *struct {
		HasCredits bool   `json:"has_credits"`
		Unlimited  bool   `json:"unlimited"`
		Balance    string `json:"balance"`
	} `json:"credits"`
}

// codexAuthJSON mirrors the Codex CLI auth.json structure.
type codexAuthJSON struct {
	Tokens struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	} `json:"tokens"`
}

// CodexFetcher fetches and caches Codex rate-limit and credit usage data. It
// watches ~/.codex/auth.json for token changes.
type CodexFetcher struct {
	baseFetcher

	client    *http.Client
	watcher   *fsnotify.Watcher
	authPath  string
	token     string
	accountID string
}

// NewCodexFetcher creates a fetcher and starts watching ~/.codex/auth.json.
// Returns nil if the home directory cannot be determined.
func NewCodexFetcher(ctx context.Context) *CodexFetcher {
	home, err := os.UserHomeDir()
	if err != nil {
		slog.WarnContext(ctx, "cannot determine home dir; Codex usage disabled", "err", err)
		return nil
	}
	authPath := filepath.Join(home, ".codex", "auth.json")
	token, accountID := readCodexAuth(authPath)
	if token == "" {
		slog.InfoContext(ctx, "no Codex OAuth token found; Codex usage disabled (will watch for credentials)")
	}

	f := &CodexFetcher{
		client:    &http.Client{Timeout: 10 * time.Second},
		token:     token,
		accountID: accountID,
		authPath:  authPath,
	}

	if err := f.startWatcher(ctx); err != nil {
		slog.WarnContext(ctx, "failed to watch Codex auth file", "err", err)
	}
	return f
}

// Provider returns the provider identifier.
func (f *CodexFetcher) Provider() string { return "codex" }

// Label returns the human-readable provider name.
func (f *CodexFetcher) Label() string { return "Codex" }

// AuthKind returns the authentication method.
func (f *CodexFetcher) AuthKind() string { return "oauth" }

// UsageURL returns the link to the provider's usage/billing page.
func (f *CodexFetcher) UsageURL() string { return "https://chatgpt.com/codex/cloud/settings/analytics" }

// Get returns the cached quota data, refreshing if stale.
func (f *CodexFetcher) Get(ctx context.Context) *ProviderQuota {
	f.mu.Lock()
	if f.token == "" {
		f.mu.Unlock()
		return nil
	}
	f.mu.Unlock()
	return f.get(ctx, f.fetch, "Codex")
}

func (f *CodexFetcher) fetch(ctx context.Context) (*ProviderQuota, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageAPIURL, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("User-Agent", "caic")
	if f.accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", f.accountID)
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
		return nil, fmt.Errorf("decode Codex usage: %w", err)
	}

	out := &ProviderQuota{
		Provider: f.Provider(),
		Label:    f.Label(),
		AuthKind: f.AuthKind(),
	}
	if raw.RateLimit != nil {
		if w := raw.RateLimit.PrimaryWindow; w != nil {
			out.RateLimits = append(out.RateLimits, QuotaRateLimit{
				Window:   "5h",
				UsedPct:  float64(w.UsedPercent),
				ResetsAt: time.Unix(int64(w.ResetAt), 0).UTC(),
			})
		}
		if w := raw.RateLimit.SecondaryWindow; w != nil {
			out.RateLimits = append(out.RateLimits, QuotaRateLimit{
				Window:   "7d",
				UsedPct:  float64(w.UsedPercent),
				ResetsAt: time.Unix(int64(w.ResetAt), 0).UTC(),
			})
		}
	}
	if raw.Credits != nil && raw.Credits.Balance != "" {
		bal, _ := strconv.ParseFloat(raw.Credits.Balance, 64)
		out.Balance = QuotaBalance{
			Currency: "USD",
			Total:    bal,
		}
		if !raw.Credits.HasCredits && !raw.Credits.Unlimited {
			out.Balance.Total = 0
		}
	}
	return out, nil
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
			slog.WarnContext(ctx, "Codex auth watcher error", "err", err)
		}
	}
}

func (f *CodexFetcher) onAuthChanged() {
	token, accountID := readCodexAuth(f.authPath)
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
	slog.Info("Codex credentials updated, token refreshed")
}

func readCodexAuth(authPath string) (token, accountID string) {
	data, err := os.ReadFile(authPath) //nolint:gosec // authPath from os.UserHomeDir
	if err != nil {
		return "", ""
	}
	var auth codexAuthJSON
	_ = json.Unmarshal(data, &auth)
	return auth.Tokens.AccessToken, auth.Tokens.AccountID
}

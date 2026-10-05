// Antigravity subscription quota fetcher delegates authentication to agy and caches its structured usage command.

package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/maruel/genai/providers/antigravity"
)

// AntigravityFetcher polls agy's account quota without running a model turn.
// It runs on the caic server, where agy owns sign-in and token renewal. Bucket
// IDs identify independent model-pool/window limits; token costs remain unknown.
type AntigravityFetcher struct {
	baseFetcher

	path string
}

// NewAntigravityFetcher creates a fetcher when agy is available on PATH.
func NewAntigravityFetcher() *AntigravityFetcher {
	path, err := exec.LookPath("agy")
	if err != nil {
		return nil
	}
	return &AntigravityFetcher{
		baseFetcher: newBaseFetcher(agent.QuotaProviderAntigravity, AuthKindOAuth, ""),
		path:        path,
	}
}

// Get returns cached quota, refreshing stale data with bounded execution time
// and exponential backoff. A failed refresh retains the last good snapshot.
func (f *AntigravityFetcher) Get(ctx context.Context) *ProviderQuota {
	return f.get(ctx, f.fetch)
}

func (f *AntigravityFetcher) fetch(ctx context.Context) (*ProviderQuota, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, f.path, "--output-format", "json", "--print=/usage") //nolint:gosec // executable resolved from PATH; arguments are fixed
	// Do not let the server's checkout supply project configuration or plugins.
	cmd.Dir = os.TempDir()
	var out agyQuotaOutput
	cmd.Stdout = &out
	// Do not copy raw CLI diagnostics into server logs; they can contain auth data.
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("agy /usage: %w", err)
	}
	var result antigravity.JSONOutput
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		return nil, fmt.Errorf("decode agy /usage: %w", err)
	}
	if result.Status != antigravity.StatusSuccess {
		return nil, fmt.Errorf("agy /usage returned status %q", result.Status)
	}
	if result.Command.Name != "usage" {
		return nil, errors.New("agy /usage returned no usage command")
	}
	var data antigravity.UsageData
	if err := json.Unmarshal(result.Command.Data, &data); err != nil {
		return nil, fmt.Errorf("decode agy quota: %w", err)
	}
	if len(data.Groups) == 0 {
		return nil, errors.New("agy /usage returned no quota groups")
	}
	q := f.quota()
	var ids []string
	for _, group := range data.Groups {
		for _, bucket := range group.Buckets {
			if bucket.Disabled {
				continue // A disabled bucket does not restrict the account.
			}
			if bucket.RemainingFraction == nil {
				q.UnassessedWindows = append(q.UnassessedWindows, UnassessedQuotaWindow{
					Group:  group.Name,
					Window: bucket.ID,
				})
				continue // Preserve uncertainty without turning an amount into a fraction.
			}
			if bucket.ID == "" || slices.Contains(ids, bucket.ID) {
				return nil, errors.New("agy quota contains an empty or duplicate bucket ID")
			}
			ids = append(ids, bucket.ID)
			remaining := *bucket.RemainingFraction
			if remaining < 0 || remaining > 1 {
				return nil, errors.New("agy quota fraction is outside [0, 1]")
			}
			var reset time.Time
			if bucket.ResetTime != "" {
				var err error
				reset, err = time.Parse(time.RFC3339Nano, bucket.ResetTime)
				if err != nil {
					return nil, fmt.Errorf("decode agy quota reset: %w", err)
				}
			}
			label := bucket.ID
			switch bucket.ID {
			case "gemini-weekly":
				label = "7d"
			case "gemini-5h":
				label = "5h"
			case "3p-weekly":
				label = "3p-7d"
			}
			q.RateLimits = append(q.RateLimits, QuotaRateLimit{
				Label:       label,
				Window:      bucket.ID,
				Utilization: 1 - remaining,
				ResetsAt:    reset,
			})
		}
	}
	return q, nil
}

// agyQuotaOutput bounds the small account-quota document to 1 MiB.
type agyQuotaOutput struct{ bytes.Buffer }

func (b *agyQuotaOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 1<<20 {
		return 0, errors.New("agy quota output exceeds 1 MiB")
	}
	return b.Buffer.Write(p)
}

// Tests Antigravity CLI quota discovery, parsing, caching, and failed refreshes.

package usage

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

const agyQuotaResponse = `{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[{"name":"Gemini Models","buckets":[{"id":"gemini-weekly","window":"weekly","remaining_fraction":0.94,"reset_time":"2099-10-09T02:11:21Z"},{"id":"gemini-5h","window":"5h","remaining_fraction":0.89,"reset_time":"2099-10-06T02:01:58Z"}]},{"name":"Claude and GPT models","buckets":[{"id":"3p-weekly","window":"weekly","remaining_fraction":1,"reset_time":"2099-10-12T22:05:45Z"},{"id":"3p-5h","window":"5h","remaining_fraction":0,"reset_time":"2099-10-06T03:05:45Z"}]}]}}}`

func init() {
	if os.Getenv("CAIC_TEST_AGY_QUOTA") != "1" {
		return
	}
	if !slices.Equal(os.Args[1:], []string{"--output-format", "json", "--print=/usage"}) {
		fmt.Fprintln(os.Stderr, "unexpected agy arguments")
		os.Exit(2)
	}
	switch os.Getenv("CAIC_TEST_AGY_MODE") {
	case "exit":
		os.Exit(1)
	case "wait":
		time.Sleep(time.Minute)
	case "large":
		fmt.Print(strings.Repeat("x", 2<<20))
	default:
		fmt.Print(os.Getenv("CAIC_TEST_AGY_RESPONSE"))
	}
	os.Exit(0)
}

func TestAntigravityFetcher(t *testing.T) {
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, filepath.Join(dir, "agy")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("CAIC_TEST_AGY_QUOTA", "1")
	t.Setenv("CAIC_TEST_AGY_RESPONSE", agyQuotaResponse)

	t.Run("Get", func(t *testing.T) {
		f := NewAntigravityFetcher()
		if f == nil {
			t.Fatal("fetcher unavailable")
		}
		q := f.Get(t.Context())
		if q == nil || q.Provider != agent.QuotaProviderAntigravity || q.AuthKind != AuthKindOAuth || len(q.RateLimits) != 4 {
			t.Fatalf("quota = %#v", q)
		}
		for i, want := range []struct {
			id    string
			label string
			used  float64
		}{{"gemini-5h", "5h", .11}, {"gemini-weekly", "7d", .06}, {"3p-5h", "3p-5h", 1}, {"3p-weekly", "3p-7d", 0}} {
			got := q.RateLimits[i]
			if got.Window != want.id || got.Label != want.label || math.Abs(got.Utilization-want.used) > 1e-9 || got.ResetsAt.IsZero() {
				t.Errorf("bucket = %#v, want %#v", got, want)
			}
		}
		t.Setenv("CAIC_TEST_AGY_MODE", "exit")
		if got := f.Get(t.Context()); got != q {
			t.Fatal("fresh quota was not cached")
		}
		f.fetchAt = time.Now().Add(-CacheTTL)
		stale := f.Get(t.Context())
		if stale == nil || !stale.FetchError || stale.FetchedAt != q.FetchedAt || len(stale.RateLimits) != 4 || q.FetchError {
			t.Fatalf("failed refresh = %#v", stale)
		}
		t.Setenv("CAIC_TEST_AGY_MODE", "")
		if got := f.Get(t.Context()); got == nil || !got.FetchError {
			t.Fatal("failed refresh did not back off")
		}
	})
	t.Run("disabled quota", func(t *testing.T) {
		t.Setenv("CAIC_TEST_AGY_RESPONSE", strings.Replace(agyQuotaResponse, `"remaining_fraction":0.94`, `"remaining_fraction":0,"disabled":true`, 1))
		q := NewAntigravityFetcher().Get(t.Context())
		if q == nil || len(q.RateLimits) != 3 || q.RateLimits[0].Window != "gemini-5h" {
			t.Fatalf("quota = %#v", q)
		}
	})
	t.Run("unknown reset", func(t *testing.T) {
		t.Setenv("CAIC_TEST_AGY_RESPONSE", strings.Replace(agyQuotaResponse, `,"reset_time":"2099-10-09T02:11:21Z"`, "", 1))
		q := NewAntigravityFetcher().Get(t.Context())
		if q == nil || len(q.RateLimits) != 4 || !q.RateLimits[1].ResetsAt.IsZero() {
			t.Fatalf("quota = %#v", q)
		}
	})
	t.Run("unknown quota", func(t *testing.T) {
		t.Setenv("CAIC_TEST_AGY_RESPONSE", strings.Replace(agyQuotaResponse, `"remaining_fraction":0.94`, `"remaining_fraction":null`, 1))
		q := NewAntigravityFetcher().Get(t.Context())
		if q == nil || len(q.RateLimits) != 3 || q.RateLimits[0].Window != "gemini-5h" || len(q.UnassessedWindows) != 1 || q.UnassessedWindows[0].Group != "Gemini Models" || q.UnassessedWindows[0].Window != "gemini-weekly" {
			t.Fatalf("quota = %#v", q)
		}
	})
	t.Run("unassessed additional windows", func(t *testing.T) {
		for _, bucket := range []string{
			`{"id":"3p-monthly","remaining_amount":0}`,
			`{"id":"3p-monthly"}`,
			`{"remaining_amount":0}`,
		} {
			t.Run(bucket, func(t *testing.T) {
				input := strings.Replace(agyQuotaResponse, `"name":"Claude and GPT models","buckets":[`, `"name":"Claude and GPT models","buckets":[`+bucket+`,`, 1)
				t.Setenv("CAIC_TEST_AGY_RESPONSE", input)
				q := NewAntigravityFetcher().Get(t.Context())
				if q == nil || len(q.RateLimits) != 4 || len(q.UnassessedWindows) != 1 || q.UnassessedWindows[0].Group != "Claude and GPT models" {
					t.Fatalf("quota lost unassessed bucket: %#v", q)
				}
			})
		}
	})
	t.Run("error", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			response string
			mode     string
		}{
			{"process failure", agyQuotaResponse, "exit"},
			{"oversized output", "", "large"},
			{"invalid JSON", "{", ""},
			{"command failure", `{"status":"ERROR","error":"sign in required"}`, ""},
			{"wrong command", strings.Replace(agyQuotaResponse, `"name":"usage"`, `"name":"models"`, 1), ""},
			{"missing data", `{"status":"SUCCESS","command":{"name":"usage"}}`, ""},
			{"bad fraction", strings.Replace(agyQuotaResponse, `"remaining_fraction":0.94`, `"remaining_fraction":1.1`, 1), ""},
			{"bad reset", strings.Replace(agyQuotaResponse, "2099-10-09T02:11:21Z", "invalid", 1), ""},
			{"missing bucket ID", strings.Replace(agyQuotaResponse, `"id":"gemini-weekly"`, `"id":""`, 1), ""},
			{"duplicate bucket ID", strings.Replace(agyQuotaResponse, `"id":"gemini-5h"`, `"id":"gemini-weekly"`, 1), ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Setenv("CAIC_TEST_AGY_RESPONSE", tc.response)
				t.Setenv("CAIC_TEST_AGY_MODE", tc.mode)
				f := NewAntigravityFetcher()
				if _, err := f.fetch(t.Context()); err == nil {
					t.Fatal("fetch accepted invalid quota")
				}
			})
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		t.Setenv("CAIC_TEST_AGY_MODE", "wait")
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		if _, err := NewAntigravityFetcher().fetch(ctx); err == nil {
			t.Fatal("fetch ignored cancellation")
		}
	})
	t.Run("unavailable", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if NewAntigravityFetcher() != nil {
			t.Fatal("registered without agy")
		}
	})
}

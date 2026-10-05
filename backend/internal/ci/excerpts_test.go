// Tests bounded CI prompts across provider outcomes and many failed checks.

package ci

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/forge/github"
	"github.com/maruel/genai"
)

func TestFailureSummaryBounded(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for range 300 {
			if _, err := io.WriteString(w, strings.Repeat("output ", 1024)+"\n"); err != nil {
				return
			}
		}
		_, _ = io.WriteString(w, "FAIL useful diagnostic\n")
	}))
	t.Cleanup(srv.Close)
	client := github.NewClient("", ciLogRedirect{url: srv.URL})
	for _, mode := range []string{"absent", "failed", "empty", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			var p genai.Provider
			switch mode {
			case "failed":
				p = &ciProvider{err: errors.New("unavailable")}
			case "empty":
				p = &ciProvider{}
			case "oversized":
				p = &ciProvider{text: "useful summary\n" + strings.Repeat("\u754c", 100000)}
			}
			checks := make([]forge.Check, 20)
			for i := range checks {
				checks[i] = forge.Check{Name: "tests", Owner: "owner", Repo: "repo", RunID: 1, JobID: int64(i + 1), Conclusion: forge.CheckRunConclusionFailure, Labels: []string{"linux"}}
			}
			summary := FailureSummary(t.Context(), slog.New(slog.DiscardHandler), client, p, forgecache.Result{Checks: checks})
			got := summary.String()
			wrapped := summary.ForPR("https://github.com/owner/repo/pull/123", 123, strings.Repeat("valid-long-branch-segment/", 100)+"fix")
			if len(wrapped) > 256<<10 {
				t.Errorf("final repair prompt exceeds 256KiB: %d", len(wrapped))
			}
			for _, want := range []string{"https://github.com/owner/repo/pull/123", "branch name abbreviated", "and push the fix:", "remaining failing check(s) omitted", "https://github.com/owner/repo/actions/runs/1/job/1"} {
				if !strings.Contains(wrapped, want) {
					t.Errorf("repair prompt lost %q", want)
				}
			}
			longURL := "https://example.com/" + strings.Repeat("nested/", 580) + "pull/123"
			fullLink := summary.ForPR(longURL, 123, strings.Repeat("segment/", 100)+"fix")
			if len(fullLink) > 256<<10 || !strings.Contains(fullLink, longURL) {
				t.Error("repair wrapper lost bounded PR link or exceeded budget")
			}
			omittedLink := summary.ForPR(strings.Repeat("x", 5000), 123, "fix")
			if !strings.Contains(omittedLink, "PR link omitted: too long") || len(omittedLink) > 256<<10 {
				t.Error("oversized PR link omitted silently or exceeded budget")
			}

			if mode != "oversized" && !strings.Contains(wrapped, "FAIL useful diagnostic") {
				t.Error("repair wrapper lost failure context")
			}

			if len(got) > maxFailurePromptBytes || !utf8.ValidString(got) {
				t.Fatalf("invalid %d byte prompt", len(got))
			}
			for _, want := range []string{"https://github.com/owner/repo/actions/runs/1/job/1", "remaining failing check(s) omitted", "Please fix"} {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q", want)
				}
			}
			if mode != "oversized" && !strings.Contains(got, "FAIL useful diagnostic") {
				t.Error("missing diagnostic")
			}
		})
	}
}

func TestFailureMetadataBounds(t *testing.T) {
	t.Parallel()
	c := forge.Check{Name: strings.Repeat("\u754c", 10000), Conclusion: forge.CheckRunConclusionFailure, Labels: make([]string, 100000)}
	client := github.NewClient("", ciLogRedirect{})
	got := failureCheckHeader(client, &c)
	if len(got) > 8192 || !utf8.ValidString(got) || !strings.Contains(got, "additional labels omitted") {
		t.Fatalf("invalid header size %d", len(got))
	}
	for n := range 10 {
		got := failureMetadata("\u754c\u754c\u754c\u754c", n)
		if len(got) > n || !utf8.ValidString(got) {
			t.Fatalf("metadata limit %d: %q", n, got)
		}
	}
}

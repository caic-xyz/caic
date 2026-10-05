// Benchmarks CI failure prompts from streamed disk logs and their retained summary heap.

package ci

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/maruel/genai"

	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/forge/github"
)

// BenchmarkFailureSummary measures the acquisition-to-prompt path. Input logs
// are disk fixtures streamed by HTTP, not whole strings retained by the fixture.
func BenchmarkFailureSummary(b *testing.B) {
	for _, size := range []int64{1 << 20, 16 << 20} {
		for _, mode := range []string{"no_provider", "provider_failure"} {
			b.Run(fmt.Sprintf("4x%dMiB/%s", size>>20, mode), func(b *testing.B) {
				f, result := ciBenchmarkSource(b, size)
				var p genai.Provider
				if mode == "provider_failure" {
					p = &ciProvider{err: errors.New("provider unavailable")}
				}
				log := slog.New(slog.DiscardHandler)
				b.ReportAllocs()
				b.SetBytes(4 * size)
				b.ResetTimer()
				for b.Loop() {
					summary := FailureSummary(b.Context(), log, f, p, result).String()
					if summary == "" {
						b.Fatal("empty prompt")
					}
				}
			})
		}
	}
}

// BenchmarkFailureSummaryRetainedHeap samples post-GC heap while the final
// prompt remains reachable at the caller boundary. This is retained heap delta,
// not peak RSS; deliberate GC is excluded from the throughput benchmark above.
func BenchmarkFailureSummaryRetainedHeap(b *testing.B) {
	for _, mode := range []string{"no_provider", "provider_failure"} {
		b.Run(mode, func(b *testing.B) {
			f, result := ciBenchmarkSource(b, 16<<20)
			var p genai.Provider
			if mode == "provider_failure" {
				p = &ciProvider{err: errors.New("provider unavailable")}
			}
			log := slog.New(slog.DiscardHandler)
			for range b.N {
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				summary := FailureSummary(b.Context(), log, f, p, result).String()
				runtime.GC()
				runtime.ReadMemStats(&after)
				b.ReportMetric(float64(max(after.HeapAlloc, before.HeapAlloc)-before.HeapAlloc), "retained-heap-B")
				b.ReportMetric(float64(len(summary)), "prompt-B")
				runtime.KeepAlive(summary)
			}
		})
	}
}

// BenchmarkFailurePromptRepairRetainedHeap samples the complete shared manual/
// automatic PR prompt at its caller boundary. Oversized provider output is
// generated per call, so the provider does not itself retain the large result.
// This is after-only retained-heap evidence, not a matched baseline or peak.
func BenchmarkFailurePromptRepairRetainedHeap(b *testing.B) {
	for _, mode := range []string{"no_provider", "oversized_provider"} {
		b.Run(mode, func(b *testing.B) {
			f, result := ciBenchmarkSource(b, 16<<20)
			var p genai.Provider
			if mode == "oversized_provider" {
				p = &ciProvider{text: "useful summary\n", repeat: 1 << 20}
			}
			log := slog.New(slog.DiscardHandler)
			for range b.N {
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				prompt := FailureSummary(b.Context(), log, f, p, result).ForPR("https://github.com/owner/repo/pull/123", 123, strings.Repeat("valid-branch-segment/", 100)+"fix")
				runtime.GC()
				runtime.ReadMemStats(&after)
				b.ReportMetric(float64(max(after.HeapAlloc, before.HeapAlloc)-before.HeapAlloc), "retained-heap-B")
				b.ReportMetric(float64(len(prompt)), "prompt-B")
				runtime.KeepAlive(prompt)
			}
		})
	}
}

func ciBenchmarkSource(b *testing.B, size int64) (forge.Forge, forgecache.Result) {
	root, err := os.OpenRoot(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := root.Close(); err != nil {
			b.Error(err)
		}
	})
	file, err := root.Create("log.txt")
	if err != nil {
		b.Fatal(err)
	}
	line := strings.Repeat("plain CI output ", 256) + "\n"
	for n := int64(0); n < size; {
		part := line[:min(int64(len(line)), size-n)]
		k, err := file.WriteString(part)
		if err != nil {
			b.Fatal(err)
		}
		n += int64(k)
	}
	if _, err := file.WriteString("\nFAIL: important final context\n"); err != nil {
		b.Fatal(err)
	}
	if err := file.Close(); err != nil {
		b.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, err := root.Open("log.txt")
		if err != nil {
			b.Error(err)
			return
		}
		_, err = io.Copy(w, f)
		err = errors.Join(err, f.Close())
		if err != nil {
			b.Error(err)
		}
	}))
	b.Cleanup(srv.Close)
	client := github.NewClient("", ciLogRedirect{url: srv.URL})
	checks := make([]forge.Check, 4)
	for i := range checks {
		checks[i] = forge.Check{Name: fmt.Sprintf("job-%d", i), Owner: "owner", Repo: "repo", RunID: 1, JobID: int64(i + 1), Conclusion: forge.CheckRunConclusionFailure, Labels: []string{"linux"}}
	}
	return client, forgecache.Result{Status: forge.CIStatusFailure, Checks: checks}
}

type ciLogRedirect struct{ url string }

func (tr ciLogRedirect) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{tr.url}}, Body: http.NoBody}, nil
}

type ciProvider struct {
	genai.Provider

	err    error
	text   string
	repeat int
}

func (p *ciProvider) GenSync(context.Context, genai.Messages, ...genai.GenOption) (genai.Result, error) {
	text := p.text
	if p.repeat > 0 {
		text += strings.Repeat("summary noise ", p.repeat)
	}
	return genai.Result{Message: genai.NewTextMessage(text)}, p.err
}

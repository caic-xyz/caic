// Tests bounded streamed CI excerpts, diagnostic selection, UTF-8 and source failures.

package forge

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLogExcerptBounds(t *testing.T) {
	t.Parallel()
	for _, groups := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "grouped"}[groups], func(t *testing.T) {
			t.Parallel()
			input := "HEAD context\n" + strings.Repeat("setup\n", 20000) + "##[group]tests\n##[error]sole diagnostic\n" + strings.Repeat("irrelevant noise\n", 20000) + "TAIL context\n##[endgroup]\n"
			got, err := ReadLogExcerpt(t.Context(), strings.NewReader(input), groups)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) > MaxLogExcerptBytes || !utf8.ValidString(got) {
				t.Fatalf("invalid excerpt: %d bytes", len(got))
			}
			for _, want := range []string{"sole diagnostic", "TAIL context", "omitted"} {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q", want)
				}
			}
		})
	}
}

func TestLogExcerptLongLineAndANSI(t *testing.T) {
	t.Parallel()
	input := strings.Repeat("x", (64<<10)-1) + "\x1b[31mred\x1b[0m\n" + strings.Repeat("\u754c", 1<<20) + "\nlast\n"
	got, err := ReadLogExcerpt(t.Context(), strings.NewReader(input), false)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(got) || len(got) > MaxLogExcerptBytes || strings.Contains(got, "\x1b[31m") || !strings.Contains(got, "last") {
		t.Fatalf("invalid bounded ANSI excerpt: %d bytes", len(got))
	}
	for _, s := range []string{strings.Repeat("\u754c", 10000), strings.Repeat("\xff", 10000)} {
		for _, n := range []int{0, 1, 2, 70, 4097, 9000} {
			got := LimitLogExcerpt(s, n)
			if len(got) > n || !utf8.ValidString(got) {
				t.Fatalf("limit %d: invalid %d-byte excerpt", n, len(got))
			}
		}
	}
}

func TestLogExcerptErrors(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("late read failure")
	got, err := ReadLogExcerpt(t.Context(), io.MultiReader(strings.NewReader(strings.Repeat("noise\n", 30000)), excerptErrorReader{sentinel}), false)
	if got != "" || !errors.Is(err, sentinel) {
		t.Fatalf("partial success: %q %v", got, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got, err := ReadLogExcerpt(ctx, strings.NewReader("log"), false); got != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %q %v", got, err)
	}
	if _, err := ReadLogExcerpt(t.Context(), strings.NewReader("\x1b["+strings.Repeat("1", 100)+"m"), false); err == nil {
		t.Fatal("oversized ANSI accepted")
	}
	for _, n := range []int64{MaxLogBytes, MaxLogBytes + 1} {
		got, err := ReadLogExcerpt(t.Context(), io.LimitReader(excerptRepeatReader{}, n), false)
		if (err != nil) != (n > MaxLogBytes) {
			t.Fatalf("source size %d: %v", n, err)
		}
		if err != nil && got != "" {
			t.Fatal("oversized source returned partial log")
		}
	}
}

type excerptErrorReader struct{ err error }

func (r excerptErrorReader) Read([]byte) (int, error) { return 0, r.err }

type excerptRepeatReader struct{}

func (excerptRepeatReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestPlainDiagnosticMiddle(t *testing.T) {
	t.Parallel()
	for _, diagnostic := range []string{"FAIL: TestMiddle", "error: compiler diagnostic", "fatal: cannot compile", "panic: runtime failure", "Traceback (most recent call last):", "src/main.go:42:7: undefined: missing", "AssertionError: wrong value"} {
		t.Run(diagnostic, func(t *testing.T) {
			t.Parallel()
			input := strings.Repeat("successful setup\n", 10000) + diagnostic + "\nuseful failure context\n" + strings.Repeat("trailing noise\n", 10000)
			got, err := ReadLogExcerpt(t.Context(), strings.NewReader(input), false)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(got, diagnostic) || !strings.Contains(got, "useful failure context") {
				t.Fatalf("middle diagnostic lost: %q", diagnostic)
			}
		})
	}
}

func TestPlainDiagnosticNoise(t *testing.T) {
	t.Parallel()
	input := strings.Repeat("successful setup\n", 10000) + "error: sole useful diagnostic\nuseful failure context\n" + strings.Repeat("12:34:56 successful step; 0 tests failed; failure count: 0; error rate 0\n", 10000)
	got, err := ReadLogExcerpt(t.Context(), strings.NewReader(input), false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "sole useful diagnostic") || !strings.Contains(got, "useful failure context") {
		t.Fatal("noise displaced useful diagnostic")
	}
}

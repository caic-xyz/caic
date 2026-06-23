// Tests for local-stack assistant text-to-speech sentence fragmenting.

package voicertc

import (
	"slices"
	"strings"
	"testing"
)

func TestSentenceFragmenter(t *testing.T) {
	t.Parallel()

	t.Run("flushes stable punctuation and holds trailing text", func(t *testing.T) {
		t.Parallel()
		f := newSentenceFragmenter(80)
		got := make([]string, 0, 2)
		got = append(got, f.push("Hello")...)
		got = append(got, f.push(" world. Ne")...)
		got = append(got, f.push("xt")...)
		if want := []string{"Hello world. "}; !slices.Equal(got, want) {
			t.Fatalf("push fragments = %#v, want %#v", got, want)
		}
		got = append(got, f.flush()...)
		if want := []string{"Hello world. ", "Next"}; !slices.Equal(got, want) {
			t.Fatalf("all fragments = %#v, want %#v", got, want)
		}
		if strings.Join(got, "") != "Hello world. Next" {
			t.Fatalf("joined fragments = %q, want exact input", strings.Join(got, ""))
		}
	})

	t.Run("holds punctuation at buffer end until final flush", func(t *testing.T) {
		t.Parallel()
		f := newSentenceFragmenter(80)
		if got := f.push("Done!"); len(got) != 0 {
			t.Fatalf("push fragments = %#v, want none", got)
		}
		if got, want := f.flush(), []string{"Done!"}; !slices.Equal(got, want) {
			t.Fatalf("flush = %#v, want %#v", got, want)
		}
	})

	t.Run("does not split decimal-like punctuation", func(t *testing.T) {
		t.Parallel()
		f := newSentenceFragmenter(80)
		got := f.push("Version 1.2 is ready")
		if len(got) != 0 {
			t.Fatalf("push fragments = %#v, want none", got)
		}
		got = append(got, f.flush()...)
		if want := []string{"Version 1.2 is ready"}; !slices.Equal(got, want) {
			t.Fatalf("all fragments = %#v, want %#v", got, want)
		}
	})

	t.Run("flushes on size limit", func(t *testing.T) {
		t.Parallel()
		f := newSentenceFragmenter(12)
		got := f.push("alpha beta gamma")
		if want := []string{"alpha beta "}; !slices.Equal(got, want) {
			t.Fatalf("push fragments = %#v, want %#v", got, want)
		}
		got = append(got, f.flush()...)
		if want := []string{"alpha beta ", "gamma"}; !slices.Equal(got, want) {
			t.Fatalf("all fragments = %#v, want %#v", got, want)
		}
		if strings.Join(got, "") != "alpha beta gamma" {
			t.Fatalf("joined fragments = %q, want exact input", strings.Join(got, ""))
		}
	})

	t.Run("splits long text without whitespace deterministically", func(t *testing.T) {
		t.Parallel()
		f := newSentenceFragmenter(5)
		got := f.push("abcdefghijk")
		if want := []string{"abcde", "fghij"}; !slices.Equal(got, want) {
			t.Fatalf("push fragments = %#v, want %#v", got, want)
		}
		got = append(got, f.flush()...)
		if want := []string{"abcde", "fghij", "k"}; !slices.Equal(got, want) {
			t.Fatalf("all fragments = %#v, want %#v", got, want)
		}
		if strings.Join(got, "") != "abcdefghijk" {
			t.Fatalf("joined fragments = %q, want exact input", strings.Join(got, ""))
		}
	})
}

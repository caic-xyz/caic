// Tests streamed GitLab job excerpts and download read/close failures.

package gitlab

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/caic-xyz/caic/backend/internal/forge"
)

func TestGetJobLog(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"excerpt", "full", "read_error", "close_error", "status"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			sentinel := errors.New("download failure")
			body := &jobLogBody{Reader: strings.NewReader(strings.Repeat("\u754c", 30000) + "\nFAIL diagnostic\n")}
			status := http.StatusOK
			switch mode {
			case "read_error":
				body.Reader = io.MultiReader(body.Reader, jobErrorReader{sentinel})
			case "close_error":
				body.closeErr = sentinel
			case "status":
				status = http.StatusBadRequest
			}
			c := &Client{HTTPClient: &http.Client{Transport: jobLogTransport{body: body, status: status}}}
			got, err := c.GetJobLog(t.Context(), "owner", "repo", 1, mode != "full")
			if !body.closed {
				t.Error("download body not closed")
			}
			if mode == "read_error" || mode == "close_error" {
				if !errors.Is(err, sentinel) {
					t.Fatalf("lost failure: %v", err)
				}
				return
			}
			if mode == "status" {
				if err == nil || len(err.Error()) > 4200 {
					t.Fatalf("invalid status error: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !utf8.ValidString(got) || !strings.Contains(got, "FAIL diagnostic") {
				t.Fatal("lost valid diagnostic")
			}
			if mode == "full" {
				if len(got) <= forge.MaxLogExcerptBytes {
					t.Fatal("full retrieval clipped")
				}
			} else if len(got) > forge.MaxLogExcerptBytes {
				t.Fatal("excerpt exceeds budget")
			}
		})
	}
}

type jobLogTransport struct {
	body   io.ReadCloser
	status int
}

func (tr jobLogTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: tr.status, Header: make(http.Header), Body: tr.body}, nil
}

type jobLogBody struct {
	io.Reader

	closeErr error
	closed   bool
}

func (b *jobLogBody) Close() error { b.closed = true; return b.closeErr }

type jobErrorReader struct{ err error }

func (r jobErrorReader) Read([]byte) (int, error) { return 0, r.err }

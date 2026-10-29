// Tests for GitHub API error handling and log extraction.

package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// NewClientForTest creates a Client pointing at baseURL instead of api.github.com.
func NewClientForTest(token, baseURL string) *Client {
	c := NewClient(token, http.DefaultTransport)
	c.baseURL = baseURL
	return c
}

func TestClient(t *testing.T) {
	t.Parallel()
	t.Run("GetJobLog", func(t *testing.T) {
		t.Parallel()
		t.Run("follows redirect without Authorization header", func(t *testing.T) {
			t.Parallel()
			logContent := "##[group]Run tests\n##[error]FAIL: TestFoo\n##[endgroup]"

			// Blob storage server: must NOT receive an Authorization header.
			blobSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					http.Error(w, "unexpected Authorization header on pre-signed URL", http.StatusForbidden)
					return
				}
				_, _ = w.Write([]byte(logContent))
			}))
			defer blobSrv.Close()

			// GitHub API server: returns 302 to the blob server.
			apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "" {
					http.Error(w, "missing Authorization", http.StatusUnauthorized)
					return
				}
				http.Redirect(w, r, blobSrv.URL+"/log", http.StatusFound)
			}))
			defer apiSrv.Close()

			client := NewClientForTest("token", apiSrv.URL)
			log, err := client.GetJobLog(t.Context(), "owner", "repo", 42, false)
			if err != nil {
				t.Fatalf("GetJobLog failed: %v", err)
			}
			if !strings.Contains(log, "FAIL: TestFoo") {
				t.Errorf("unexpected log: %q", log)
			}
		})
		for _, mode := range []string{"late_read", "cancel"} {
			t.Run("download failure "+mode, func(t *testing.T) {
				t.Parallel()
				started := make(chan struct{})
				blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Length", "200000")
					_, _ = io.WriteString(w, strings.Repeat("noise\n", 12000))
					if err := http.NewResponseController(w).Flush(); err != nil {
						t.Error(err)
						return
					}
					close(started)
					if mode == "cancel" {
						<-r.Context().Done()
					}
				}))
				t.Cleanup(blob.Close)
				api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, blob.URL, http.StatusFound) }))
				t.Cleanup(api.Close)
				client := NewClientForTest("token", api.URL)
				ctx, cancel := context.WithCancel(t.Context())
				var workers sync.WaitGroup
				t.Cleanup(func() { cancel(); workers.Wait() })
				if mode == "cancel" {
					workers.Go(func() {
						select {
						case <-started:
							cancel()
						case <-ctx.Done():
						}
					})
				}
				got, err := client.GetJobLog(ctx, "owner", "repo", 1, true)
				if err == nil || got != "" {
					t.Fatalf("partial download succeeded: %d bytes %v", len(got), err)
				}
				if mode == "cancel" && !errors.Is(err, context.Canceled) {
					t.Fatalf("lost cancellation: %v", err)
				}
			})
		}
	})

	t.Run("GetDefaultBranchSHA", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/repos/owner/repo/git/ref/heads/main" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`<!doctype html><html><body><p>Hello future GitHubber!</p><p><strong>No server is currently available to service your request.</strong></p><script>veryLongScriptThatMustNotAppear()</script></body></html>`))
		}))
		t.Cleanup(srv.Close)

		client := NewClientForTest("token", srv.URL)
		client.HTTPClient.Transport = http.DefaultTransport
		_, err := client.GetDefaultBranchSHA(t.Context(), "owner", "repo", "main")
		if err == nil {
			t.Fatal("GetDefaultBranchSHA returned nil error")
		}
		want := "github get ref: status 503: Hello future GitHubber! No server is currently available to service your request."
		if err.Error() != want {
			t.Errorf("GetDefaultBranchSHA() error = %q, want %q", err, want)
		}
	})
}

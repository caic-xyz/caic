// Tests for GitHub-specific log extraction.
package github

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// NewClientForTest creates a Client pointing at baseURL instead of api.github.com.
func NewClientForTest(token, baseURL string) *Client {
	c := NewClient(token, http.DefaultTransport)
	c.baseURL = baseURL
	return c
}

func TestGetJobLog(t *testing.T) {
	t.Run("follows redirect without Authorization header", func(t *testing.T) {
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
}

func TestExtractGitHubSteps(t *testing.T) {
	t.Run("extracts failing step", func(t *testing.T) {
		log := strings.Join([]string{
			"2024-01-01T00:00:00.1234567Z ##[group]Set up job",
			"2024-01-01T00:00:01.0000000Z Preparing runner",
			"2024-01-01T00:00:02.0000000Z ##[endgroup]",
			"2024-01-01T00:00:03.0000000Z ##[group]Run tests",
			"2024-01-01T00:00:04.0000000Z go test ./...",
			"2024-01-01T00:00:05.0000000Z --- FAIL: TestFoo (0.01s)",
			"2024-01-01T00:00:05.0000000Z ##[error]Process completed with exit code 1.",
			"2024-01-01T00:00:06.0000000Z ##[endgroup]",
			"2024-01-01T00:00:07.0000000Z ##[group]Post checkout",
			"2024-01-01T00:00:08.0000000Z Cleaning up",
			"2024-01-01T00:00:09.0000000Z ##[endgroup]",
		}, "\n")

		result := extractGitHubSteps(log)

		if !strings.Contains(result, "Run tests") {
			t.Error("expected result to contain failing step name 'Run tests'")
		}
		if !strings.Contains(result, "FAIL: TestFoo") {
			t.Error("expected result to contain test failure output")
		}
		if strings.Contains(result, "Set up job") {
			t.Error("expected result to NOT contain successful step 'Set up job'")
		}
		if strings.Contains(result, "Post checkout") {
			t.Error("expected result to NOT contain successful step 'Post checkout'")
		}
	})

	t.Run("multiple failing steps", func(t *testing.T) {
		log := strings.Join([]string{
			"2024-01-01T00:00:00.1234567Z ##[group]Lint",
			"2024-01-01T00:00:01.0000000Z ##[error]lint error found",
			"2024-01-01T00:00:02.0000000Z ##[endgroup]",
			"2024-01-01T00:00:03.0000000Z ##[group]Test",
			"2024-01-01T00:00:04.0000000Z ##[error]test failed",
			"2024-01-01T00:00:05.0000000Z ##[endgroup]",
		}, "\n")

		result := extractGitHubSteps(log)

		if !strings.Contains(result, "Step: Lint") {
			t.Error("expected 'Lint' step")
		}
		if !strings.Contains(result, "Step: Test") {
			t.Error("expected 'Test' step")
		}
	})

	t.Run("no groups returns raw log", func(t *testing.T) {
		raw := "some plain log\nwithout groups"
		result := extractGitHubSteps(raw)
		if result != raw {
			t.Errorf("expected raw log back, got %q", result)
		}
	})

	t.Run("no errors returns raw log", func(t *testing.T) {
		log := strings.Join([]string{
			"2024-01-01T00:00:00.1234567Z ##[group]Build",
			"2024-01-01T00:00:01.0000000Z compiling...",
			"2024-01-01T00:00:02.0000000Z ##[endgroup]",
		}, "\n")
		result := extractGitHubSteps(log)
		if result != log {
			t.Error("expected raw log when no errors found")
		}
	})

	t.Run("lines without timestamps", func(t *testing.T) {
		log := "##[group]Build\n##[error]fail\n##[endgroup]"
		result := extractGitHubSteps(log)
		if !strings.Contains(result, "Step: Build") {
			t.Error("expected step extraction to work without timestamps")
		}
	})
}

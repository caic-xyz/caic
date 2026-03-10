// Tests for GitHub webhook event handlers.
package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/cicache"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/github"
)

// stubAppClient implements githubAppClient for tests.
type stubAppClient struct {
	forgeClient forge.Forge
	forgeErr    error
}

func (s *stubAppClient) ForgeClient(_ context.Context, _ int64) (forge.Forge, error) {
	return s.forgeClient, s.forgeErr
}
func (s *stubAppClient) DeleteInstallation(_ context.Context, _ int64) error { return nil }
func (s *stubAppClient) RepoInstallation(_ context.Context, _, _ string) (int64, error) {
	return 0, nil
}
func (s *stubAppClient) PostComment(_ context.Context, _ int64, _, _ string, _ int, _ string) error {
	return nil
}

// stubForge implements forge.Forge for tests. Only GetCheckRuns and
// GetDefaultBranchSHA are used by handleCheckSuiteEvent.
type stubForge struct {
	headSHA   string
	checkRuns []forge.CheckRun
}

func (f *stubForge) GetDefaultBranchSHA(_ context.Context, _, _, _ string) (string, error) {
	return f.headSHA, nil
}
func (f *stubForge) GetCheckRuns(_ context.Context, _, _, _ string) ([]forge.CheckRun, error) {
	return f.checkRuns, nil
}
func (f *stubForge) CreatePR(_ context.Context, _, _, _, _, _, _ string) (forge.PR, error) {
	return forge.PR{}, nil
}
func (f *stubForge) PRURL(_, _ string, _ int) string         { return "" }
func (f *stubForge) PRLabel(_ int) string                    { return "" }
func (f *stubForge) CIJobURL(_, _ string, _, _ int64) string { return "" }
func (f *stubForge) CIHomeURL(_ string) string               { return "" }
func (f *stubForge) BranchCompareURL(_, _ string) string     { return "" }
func (f *stubForge) Name() string                            { return "stub" }
func (f *stubForge) GetJobLog(_ context.Context, _, _ string, _ int64, _ int) (string, error) {
	return "", nil
}

// signGitHub computes X-Hub-Signature-256 for the given body and secret.
func signGitHub(body, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestHandleCheckSuiteEvent(t *testing.T) {
	successRuns := []forge.CheckRun{
		{Name: "ci", Status: forge.CheckRunStatusCompleted, Conclusion: forge.CheckRunConclusionSuccess},
	}
	failureRuns := []forge.CheckRun{
		{Name: "ci", Status: forge.CheckRunStatusCompleted, Conclusion: forge.CheckRunConclusionFailure},
	}

	t.Run("updates CI status when SHA matches HEAD", func(t *testing.T) {
		s := minimalServer(t)
		s.repos = []repoInfo{{RelPath: "org/repo", ForgeOwner: "org", ForgeRepo: "repo", BaseBranch: "main"}}
		s.repoCIStatus = make(map[string]repoCIState)
		s.githubApp = &stubAppClient{forgeClient: &stubForge{headSHA: "abc123", checkRuns: successRuns}}

		s.handleCheckSuiteEvent(context.Background(), &github.CheckSuiteEvent{
			Action: "completed",
			CheckSuite: struct {
				HeadSHA    string `json:"head_sha"`
				HeadBranch string `json:"head_branch"`
				Conclusion string `json:"conclusion"`
			}{HeadSHA: "abc123", HeadBranch: "main"},
			Repository:   github.WebhookRepo{FullName: "org/repo"},
			Installation: github.WebhookInstallation{ID: 1},
		})

		s.mu.Lock()
		got := s.repoCIStatus["org/repo"].Status
		s.mu.Unlock()
		if got != cicache.StatusSuccess {
			t.Errorf("repoCIStatus = %q, want %q", got, cicache.StatusSuccess)
		}
	})

	t.Run("ignores out-of-order delivery when SHA is not HEAD", func(t *testing.T) {
		s := minimalServer(t)
		s.repos = []repoInfo{{RelPath: "org/repo", ForgeOwner: "org", ForgeRepo: "repo", BaseBranch: "main"}}
		s.repoCIStatus = make(map[string]repoCIState)
		// HEAD is now "newsha"; the webhook carries "oldsha".
		s.githubApp = &stubAppClient{forgeClient: &stubForge{headSHA: "newsha", checkRuns: failureRuns}}

		s.handleCheckSuiteEvent(context.Background(), &github.CheckSuiteEvent{
			Action: "completed",
			CheckSuite: struct {
				HeadSHA    string `json:"head_sha"`
				HeadBranch string `json:"head_branch"`
				Conclusion string `json:"conclusion"`
			}{HeadSHA: "oldsha", HeadBranch: "main"},
			Repository:   github.WebhookRepo{FullName: "org/repo"},
			Installation: github.WebhookInstallation{ID: 1},
		})

		s.mu.Lock()
		got := s.repoCIStatus["org/repo"].Status
		s.mu.Unlock()
		if got != "" {
			t.Errorf("repoCIStatus = %q, want empty (stale event should be ignored)", got)
		}
	})
}

func TestHandleGitHubWebhook(t *testing.T) {
	secret := []byte("test-secret-abc123")

	t.Run("ping event returns 200", func(t *testing.T) {
		s := newTestServer(t)
		s.githubWebhookSecret = secret

		body := []byte(`{}`)
		req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "ping")
		req.Header.Set("X-Hub-Signature-256", signGitHub(body, secret))

		w := httptest.NewRecorder()
		s.handleGitHubWebhook(w, req)

		if w.Code != http.StatusNoContent {
			t.Errorf("status = %d, want %d", w.Code, http.StatusNoContent)
		}
	})

	t.Run("bad signature returns 401", func(t *testing.T) {
		s := newTestServer(t)
		s.githubWebhookSecret = secret

		body := []byte(`{}`)
		req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "ping")
		req.Header.Set("X-Hub-Signature-256", signGitHub(body, []byte("wrong-secret")))

		w := httptest.NewRecorder()
		s.handleGitHubWebhook(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
		}
	})

	t.Run("missing signature returns 401", func(t *testing.T) {
		s := newTestServer(t)
		s.githubWebhookSecret = secret

		body := []byte(`{}`)
		req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "ping")
		// No X-Hub-Signature-256

		w := httptest.NewRecorder()
		s.handleGitHubWebhook(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
		}
	})

	t.Run("completed check_run returns 204", func(t *testing.T) {
		s := newTestServer(t)
		s.githubWebhookSecret = secret

		ev := githubCheckRunEvent{}
		ev.CheckRun.Status = "completed"
		ev.CheckRun.Conclusion = "success"
		ev.CheckRun.HeadSHA = "abc123def456"
		ev.Repository.FullName = "owner/repo"
		body, _ := json.Marshal(ev)

		req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "check_run")
		req.Header.Set("X-Hub-Signature-256", signGitHub(body, secret))

		w := httptest.NewRecorder()
		s.handleGitHubWebhook(w, req)

		if w.Code != http.StatusNoContent {
			t.Errorf("status = %d, want %d", w.Code, http.StatusNoContent)
		}
	})
}

func TestHandleGitLabWebhook(t *testing.T) {
	secret := []byte("gitlab-secret-xyz")

	t.Run("valid pipeline event returns 204", func(t *testing.T) {
		s := newTestServer(t)
		s.gitlabWebhookSecret = secret

		ev := gitlabPipelineEvent{}
		ev.ObjectAttributes.SHA = "deadbeef1234"
		ev.ObjectAttributes.Status = "success"
		ev.Project.PathWithNamespace = "group/repo"
		body, _ := json.Marshal(ev)

		req := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab", bytes.NewReader(body))
		req.Header.Set("X-Gitlab-Event", "Pipeline Hook")
		req.Header.Set("X-Gitlab-Token", string(secret))

		w := httptest.NewRecorder()
		s.handleGitLabWebhook(w, req)

		if w.Code != http.StatusNoContent {
			t.Errorf("status = %d, want %d", w.Code, http.StatusNoContent)
		}
	})

	t.Run("bad token returns 401", func(t *testing.T) {
		s := newTestServer(t)
		s.gitlabWebhookSecret = secret

		body := []byte(`{}`)
		req := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab", bytes.NewReader(body))
		req.Header.Set("X-Gitlab-Event", "Pipeline Hook")
		req.Header.Set("X-Gitlab-Token", "wrong-token")

		w := httptest.NewRecorder()
		s.handleGitLabWebhook(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
		}
	})

	t.Run("non-terminal status returns 204 without dispatch", func(t *testing.T) {
		s := newTestServer(t)
		s.gitlabWebhookSecret = secret

		ev := gitlabPipelineEvent{}
		ev.ObjectAttributes.SHA = "deadbeef1234"
		ev.ObjectAttributes.Status = "running"
		ev.Project.PathWithNamespace = "group/repo"
		body, _ := json.Marshal(ev)

		req := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab", bytes.NewReader(body))
		req.Header.Set("X-Gitlab-Event", "Pipeline Hook")
		req.Header.Set("X-Gitlab-Token", string(secret))

		w := httptest.NewRecorder()
		s.handleGitLabWebhook(w, req)

		if w.Code != http.StatusNoContent {
			t.Errorf("status = %d, want %d", w.Code, http.StatusNoContent)
		}
	})

	t.Run("oversized body returns 413", func(t *testing.T) {
		s := newTestServer(t)
		s.gitlabWebhookSecret = secret

		body := make([]byte, maxWebhookBodyBytes+1)
		req := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab", bytes.NewReader(body))
		req.Header.Set("X-Gitlab-Event", "Pipeline Hook")
		req.Header.Set("X-Gitlab-Token", string(secret))

		w := httptest.NewRecorder()
		s.handleGitLabWebhook(w, req)

		if w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want %d", w.Code, http.StatusRequestEntityTooLarge)
		}
	})
}

func TestBuildHandlerWebhookRoutes(t *testing.T) {
	t.Run("gitlab webhook registered when secret set", func(t *testing.T) {
		s := newTestServer(t)
		s.gitlabWebhookSecret = []byte("secret")

		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}

		ev := gitlabPipelineEvent{}
		ev.ObjectAttributes.SHA = "abc123"
		ev.ObjectAttributes.Status = "success"
		ev.Project.PathWithNamespace = "group/repo"
		body, _ := json.Marshal(ev)

		req := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab", bytes.NewReader(body))
		req.Header.Set("X-Gitlab-Event", "Pipeline Hook")
		req.Header.Set("X-Gitlab-Token", "secret")

		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		// Valid token + pipeline event → 204
		if w.Code != http.StatusNoContent {
			t.Errorf("status = %d, want %d", w.Code, http.StatusNoContent)
		}
	})

	t.Run("gitlab webhook not registered when secret unset", func(t *testing.T) {
		s := newTestServer(t)
		// gitlabWebhookSecret is nil (not configured).

		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}

		body := []byte(`{}`)
		req := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab", bytes.NewReader(body))
		req.Header.Set("X-Gitlab-Event", "Pipeline Hook")
		req.Header.Set("X-Gitlab-Token", "any-token")

		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		// Route not registered → falls through; should not return 204 as if handled.
		if w.Code == http.StatusNoContent {
			t.Errorf("status = %d; gitlab webhook endpoint should not be active when secret is unset", w.Code)
		}
	})
}

// minimalServer returns a Server with just enough state for webhook handler tests.
func minimalServer(t *testing.T) *Server {
	t.Helper()
	cache, err := cicache.Open(t.TempDir() + "/cicache.json")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	return &Server{
		ctx:                 ctx,
		ciCache:             cache,
		tasks:               make(map[string]*taskEntry),
		repoCIStatus:        make(map[string]repoCIState),
		changed:             make(chan struct{}, 1),
		githubInstallations: make(map[string]int64),
	}
}

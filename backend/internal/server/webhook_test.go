// Tests for GitHub webhook event handlers.
package server

import (
	"context"
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

// Tests for adoption-time CI monitoring helpers.

package server

import (
	"testing"

	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/tasks"
)

func TestAdoptedHeadSHA(t *testing.T) {
	t.Parallel()
	t.Run("valid_uses_pr_head_sha", func(t *testing.T) {
		t.Parallel()
		f := &stubForge{
			headSHA: "branch-sha",
			findPR:  forge.PR{Number: 8, HeadSHA: "pr-head-sha"},
		}
		sha, err := adoptedHeadSHA(t.Context(), f, &tasks.AdoptedTask{
			ForgeOwner: "caic-xyz",
			ForgeRepo:  "caic",
			Branch:     "caic-7",
		})
		if err != nil {
			t.Fatalf("adoptedHeadSHA: %v", err)
		}
		if sha != "pr-head-sha" {
			t.Errorf("sha = %q, want pr-head-sha", sha)
		}
		if f.defaultBranchCalls != 0 {
			t.Errorf("default branch calls = %d, want 0", f.defaultBranchCalls)
		}
		if f.findPRCalls != 1 {
			t.Errorf("find PR calls = %d, want 1", f.findPRCalls)
		}
	})
	t.Run("valid_falls_back_to_branch_sha", func(t *testing.T) {
		t.Parallel()
		f := &stubForge{headSHA: "branch-sha"}
		sha, err := adoptedHeadSHA(t.Context(), f, &tasks.AdoptedTask{
			ForgeOwner: "caic-xyz",
			ForgeRepo:  "caic",
			Branch:     "caic-7",
		})
		if err != nil {
			t.Fatalf("adoptedHeadSHA: %v", err)
		}
		if sha != "branch-sha" {
			t.Errorf("sha = %q, want branch-sha", sha)
		}
		if f.defaultBranchCalls != 1 {
			t.Errorf("default branch calls = %d, want 1", f.defaultBranchCalls)
		}
	})
}

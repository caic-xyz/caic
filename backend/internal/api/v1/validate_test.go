// Tests for API v1 request validation logic.

package v1

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	api "github.com/caic-xyz/caic/backend/internal/api"
)

func TestValidate(t *testing.T) {
	t.Parallel()
	t.Run("EmptyReq", func(t *testing.T) {
		t.Parallel()
		var r EmptyReq
		if err := r.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("InputReq", func(t *testing.T) {
		t.Parallel()
		t.Run("MissingPromptAndImages", func(t *testing.T) {
			t.Parallel()
			assertBadRequest(t, (&InputReq{}).Validate(), "prompt or images required")
		})
		t.Run("Valid", func(t *testing.T) {
			t.Parallel()
			if err := (&InputReq{Prompt: Prompt{Text: "hello"}}).Validate(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
		t.Run("ImagesOnly", func(t *testing.T) {
			t.Parallel()
			r := &InputReq{Prompt: Prompt{Images: []ImageData{{MediaType: "image/png", Data: "YQ=="}}}}
			if err := r.Validate(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
		t.Run("InvalidImageMediaType", func(t *testing.T) {
			t.Parallel()
			r := &InputReq{Prompt: Prompt{Text: "x", Images: []ImageData{{MediaType: "image/bmp", Data: "YQ=="}}}}
			assertBadRequest(t, r.Validate(), "unsupported image mediaType: image/bmp")
		})
		t.Run("MissingImageData", func(t *testing.T) {
			t.Parallel()
			r := &InputReq{Prompt: Prompt{Text: "x", Images: []ImageData{{MediaType: "image/png"}}}}
			assertBadRequest(t, r.Validate(), "image data is required")
		})
		t.Run("MissingImageMediaType", func(t *testing.T) {
			t.Parallel()
			r := &InputReq{Prompt: Prompt{Text: "x", Images: []ImageData{{Data: "YQ=="}}}}
			assertBadRequest(t, r.Validate(), "image mediaType is required")
		})
		t.Run("ImageTooLarge", func(t *testing.T) {
			t.Parallel()
			data := strings.Repeat("A", base64.StdEncoding.EncodedLen(maxImageBytes+1))
			r := &InputReq{Prompt: Prompt{Text: "x", Images: []ImageData{{MediaType: "image/png", Data: data}}}}
			assertBadRequest(t, r.Validate(), "image data too large")
		})
	})

	t.Run("RestartReq", func(t *testing.T) {
		t.Parallel()
		t.Run("Empty", func(t *testing.T) {
			t.Parallel()
			if err := (&RestartReq{}).Validate(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
		t.Run("WithPrompt", func(t *testing.T) {
			t.Parallel()
			if err := (&RestartReq{Prompt: Prompt{Text: "continue"}}).Validate(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	})

	t.Run("SyncReq", func(t *testing.T) {
		t.Parallel()
		t.Run("Empty", func(t *testing.T) {
			t.Parallel()
			if err := (SyncReq{}).Validate(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
		t.Run("Branch", func(t *testing.T) {
			t.Parallel()
			if err := (SyncReq{Target: SyncTargetBranch}).Validate(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
		t.Run("Default", func(t *testing.T) {
			t.Parallel()
			if err := (SyncReq{Target: SyncTargetDefault}).Validate(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
		t.Run("Invalid", func(t *testing.T) {
			t.Parallel()
			assertBadRequest(t, (SyncReq{Target: "bogus"}).Validate(), "invalid sync target: bogus")
		})
	})

	t.Run("CloneRepoReq", func(t *testing.T) {
		t.Parallel()
		t.Run("Valid_URLOnly", func(t *testing.T) {
			t.Parallel()
			r := &CloneRepoReq{URL: "https://github.com/org/repo.git"}
			if err := r.Validate(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
		t.Run("Valid_URLAndPath", func(t *testing.T) {
			t.Parallel()
			r := &CloneRepoReq{URL: "https://github.com/org/repo.git", Path: "github.com/org/repo"}
			if err := r.Validate(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
		t.Run("MissingURL", func(t *testing.T) {
			t.Parallel()
			assertBadRequest(t, (&CloneRepoReq{}).Validate(), "url is required")
		})
		t.Run("NegativeDepth", func(t *testing.T) {
			t.Parallel()
			r := &CloneRepoReq{URL: "https://example.com/repo.git", Depth: -1}
			assertBadRequest(t, r.Validate(), "depth must be non-negative")
		})
		t.Run("PathWithDotDot", func(t *testing.T) {
			t.Parallel()
			r := &CloneRepoReq{URL: "https://example.com/repo.git", Path: "foo/../bar"}
			assertBadRequest(t, r.Validate(), "path must be clean (use filepath.Clean form)")
		})
		t.Run("AbsolutePath", func(t *testing.T) {
			t.Parallel()
			r := &CloneRepoReq{URL: "https://example.com/repo.git", Path: "/etc/repo"}
			assertBadRequest(t, r.Validate(), "path must be relative")
		})
		t.Run("TooDeepPath", func(t *testing.T) {
			t.Parallel()
			r := &CloneRepoReq{URL: "https://example.com/repo.git", Path: "a/b/c/d"}
			assertBadRequest(t, r.Validate(), "path too deep (max 3 segments)")
		})
		t.Run("InvalidCharsInSegment", func(t *testing.T) {
			t.Parallel()
			r := &CloneRepoReq{URL: "https://example.com/repo.git", Path: "foo/b@r"}
			assertBadRequest(t, r.Validate(), "path segment contains invalid characters: b@r")
		})
		t.Run("TooLongPath", func(t *testing.T) {
			t.Parallel()
			long := strings.Repeat("a", 256)
			r := &CloneRepoReq{URL: "https://example.com/repo.git", Path: long}
			assertBadRequest(t, r.Validate(), "path too long (max 255 characters)")
		})
		t.Run("PathStartsWithDot", func(t *testing.T) {
			t.Parallel()
			r := &CloneRepoReq{URL: "https://example.com/repo.git", Path: ".hidden"}
			assertBadRequest(t, r.Validate(), "path segment contains invalid characters: .hidden")
		})
	})

	t.Run("CreateTaskReq", func(t *testing.T) {
		t.Parallel()
		valid := CreateTaskReq{
			InitialPrompt: Prompt{Text: "do stuff"},
			Repos:         []RepoSpec{{Name: "org/repo"}},
			Harness:       HarnessClaude,
		}

		t.Run("Valid", func(t *testing.T) {
			t.Parallel()
			r := valid
			if err := r.Validate(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
		t.Run("ValidNoRepos", func(t *testing.T) {
			t.Parallel()
			r := CreateTaskReq{InitialPrompt: Prompt{Text: "do stuff"}, Harness: HarnessClaude}
			if err := r.Validate(); err != nil {
				t.Errorf("no repos should be allowed, got: %v", err)
			}
		})
		t.Run("MissingPrompt", func(t *testing.T) {
			t.Parallel()
			r := valid
			r.InitialPrompt = Prompt{}
			assertBadRequest(t, r.Validate(), "prompt or images required")
		})
		t.Run("EmptyRepoName", func(t *testing.T) {
			t.Parallel()
			r := CreateTaskReq{
				InitialPrompt: Prompt{Text: "do stuff"},
				Repos:         []RepoSpec{{Name: ""}},
				Harness:       HarnessClaude,
			}
			assertBadRequest(t, r.Validate(), "repos contains entry with empty name")
		})
		t.Run("DuplicateRepoName", func(t *testing.T) {
			t.Parallel()
			r := CreateTaskReq{
				InitialPrompt: Prompt{Text: "do stuff"},
				Repos:         []RepoSpec{{Name: "org/repo"}, {Name: "org/repo"}},
				Harness:       HarnessClaude,
			}
			assertBadRequest(t, r.Validate(), "repos contains duplicate name: org/repo")
		})
		t.Run("MissingHarness", func(t *testing.T) {
			t.Parallel()
			r := valid
			r.Harness = ""
			assertBadRequest(t, r.Validate(), "harness is required")
		})
	})

	t.Run("UpdatePreferencesReq", func(t *testing.T) {
		t.Parallel()
		t.Run("Valid", func(t *testing.T) {
			t.Parallel()
			var r UpdatePreferencesReq
			if err := json.Unmarshal([]byte(`{"settings":{"autoFixOnCIFailure":false,"autoFixOnPROpen":false,"useDefaultCaches":true}}`), &r); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if err := r.Validate(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
		t.Run("MissingSettings", func(t *testing.T) {
			t.Parallel()
			var r UpdatePreferencesReq
			if err := json.Unmarshal([]byte(`{}`), &r); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			assertBadRequest(t, r.Validate(), "settings is required")
		})
		t.Run("UnknownCache", func(t *testing.T) {
			t.Parallel()
			var r UpdatePreferencesReq
			if err := json.Unmarshal([]byte(`{"settings":{"autoFixOnCIFailure":false,"autoFixOnPROpen":false,"useDefaultCaches":true,"wellKnownCaches":{"bogus":true}}}`), &r); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			assertBadRequest(t, r.Validate(), "unknown cache: bogus")
		})
		t.Run("UnknownTopLevelField", func(t *testing.T) {
			t.Parallel()
			var r UpdatePreferencesReq
			if err := json.Unmarshal([]byte(`{"settings":{"autoFixOnCIFailure":false,"autoFixOnPROpen":false,"useDefaultCaches":true},"bogus":true}`), &r); err == nil {
				t.Fatal("expected unknown field error")
			}
		})
	})
}

// assertBadRequest checks that err is an *api.Error with 400 status and the expected message.
func assertBadRequest(t *testing.T, err error, wantMsg string) {
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *api.Error, got %T", err)
	}
	if apiErr.StatusCode() != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", apiErr.StatusCode(), http.StatusBadRequest)
	}
	if apiErr.Code() != api.CodeBadRequest {
		t.Errorf("code = %q, want %q", apiErr.Code(), api.CodeBadRequest)
	}
	if apiErr.Error() != wantMsg {
		t.Errorf("message = %q, want %q", apiErr.Error(), wantMsg)
	}
}

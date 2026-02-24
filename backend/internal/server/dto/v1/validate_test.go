package v1

import (
	"errors"
	"net/http"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/server/dto"
)

func TestValidate(t *testing.T) {
	t.Run("EmptyReq", func(t *testing.T) {
		var r EmptyReq
		if err := r.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("InputReq", func(t *testing.T) {
		t.Run("MissingPromptAndImages", func(t *testing.T) {
			assertBadRequest(t, (&InputReq{}).Validate(), "prompt or images required")
		})
		t.Run("Valid", func(t *testing.T) {
			if err := (&InputReq{Prompt: Prompt{Text: "hello"}}).Validate(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
		t.Run("ImagesOnly", func(t *testing.T) {
			r := &InputReq{Prompt: Prompt{Images: []ImageData{{MediaType: "image/png", Data: "abc"}}}}
			if err := r.Validate(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
		t.Run("InvalidImageMediaType", func(t *testing.T) {
			r := &InputReq{Prompt: Prompt{Text: "x", Images: []ImageData{{MediaType: "image/bmp", Data: "abc"}}}}
			assertBadRequest(t, r.Validate(), "unsupported image mediaType: image/bmp")
		})
		t.Run("MissingImageData", func(t *testing.T) {
			r := &InputReq{Prompt: Prompt{Text: "x", Images: []ImageData{{MediaType: "image/png"}}}}
			assertBadRequest(t, r.Validate(), "image data is required")
		})
		t.Run("MissingImageMediaType", func(t *testing.T) {
			r := &InputReq{Prompt: Prompt{Text: "x", Images: []ImageData{{Data: "abc"}}}}
			assertBadRequest(t, r.Validate(), "image mediaType is required")
		})
	})

	t.Run("RestartReq", func(t *testing.T) {
		t.Run("Empty", func(t *testing.T) {
			if err := (&RestartReq{}).Validate(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
		t.Run("WithPrompt", func(t *testing.T) {
			if err := (&RestartReq{Prompt: Prompt{Text: "continue"}}).Validate(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	})

	t.Run("SyncReq", func(t *testing.T) {
		t.Run("Empty", func(t *testing.T) {
			if err := (SyncReq{}).Validate(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
		t.Run("Branch", func(t *testing.T) {
			if err := (SyncReq{Target: SyncTargetBranch}).Validate(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
		t.Run("Default", func(t *testing.T) {
			if err := (SyncReq{Target: SyncTargetDefault}).Validate(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
		t.Run("Invalid", func(t *testing.T) {
			assertBadRequest(t, (SyncReq{Target: "bogus"}).Validate(), "invalid sync target: bogus")
		})
	})

	t.Run("CreateTaskReq", func(t *testing.T) {
		valid := CreateTaskReq{InitialPrompt: Prompt{Text: "do stuff"}, Repo: "/repo", Harness: HarnessClaude}

		t.Run("Valid", func(t *testing.T) {
			r := valid
			if err := r.Validate(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
		t.Run("MissingPrompt", func(t *testing.T) {
			r := valid
			r.InitialPrompt = Prompt{}
			assertBadRequest(t, r.Validate(), "prompt or images required")
		})
		t.Run("MissingRepo", func(t *testing.T) {
			r := valid
			r.Repo = ""
			assertBadRequest(t, r.Validate(), "repo is required")
		})
		t.Run("MissingHarness", func(t *testing.T) {
			r := valid
			r.Harness = ""
			assertBadRequest(t, r.Validate(), "harness is required")
		})
	})
}

// assertBadRequest checks that err is an *dto.APIError with 400 status and the expected message.
func assertBadRequest(t *testing.T, err error, wantMsg string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var apiErr *dto.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *dto.APIError, got %T", err)
	}
	if apiErr.StatusCode() != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", apiErr.StatusCode(), http.StatusBadRequest)
	}
	if apiErr.Code() != dto.CodeBadRequest {
		t.Errorf("code = %q, want %q", apiErr.Code(), dto.CodeBadRequest)
	}
	if apiErr.Error() != wantMsg {
		t.Errorf("message = %q, want %q", apiErr.Error(), wantMsg)
	}
}

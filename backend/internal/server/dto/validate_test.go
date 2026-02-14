package dto

import (
	"net/http"
	"testing"
)

func TestValidate(t *testing.T) {
	t.Run("EmptyReq", func(t *testing.T) {
		var r EmptyReq
		if err := r.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("InputReq", func(t *testing.T) {
		t.Run("MissingPrompt", func(t *testing.T) {
			assertBadRequest(t, (&InputReq{}).Validate(), "prompt is required")
		})
		t.Run("Valid", func(t *testing.T) {
			if err := (&InputReq{Prompt: "hello"}).Validate(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	})

	t.Run("RestartReq", func(t *testing.T) {
		t.Run("Empty", func(t *testing.T) {
			if err := (&RestartReq{}).Validate(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
		t.Run("WithPrompt", func(t *testing.T) {
			if err := (&RestartReq{Prompt: "continue"}).Validate(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	})

	t.Run("SyncReq", func(t *testing.T) {
		if err := (SyncReq{}).Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("CreateTaskReq", func(t *testing.T) {
		valid := CreateTaskReq{Prompt: "do stuff", Repo: "/repo", Harness: HarnessClaude}

		t.Run("Valid", func(t *testing.T) {
			r := valid
			if err := r.Validate(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
		t.Run("MissingPrompt", func(t *testing.T) {
			r := valid
			r.Prompt = ""
			assertBadRequest(t, r.Validate(), "prompt is required")
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

// assertBadRequest checks that err is an *APIError with 400 status and the expected message.
func assertBadRequest(t *testing.T, err error, wantMsg string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.StatusCode() != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", apiErr.StatusCode(), http.StatusBadRequest)
	}
	if apiErr.Code() != CodeBadRequest {
		t.Errorf("code = %q, want %q", apiErr.Code(), CodeBadRequest)
	}
	if apiErr.Error() != wantMsg {
		t.Errorf("message = %q, want %q", apiErr.Error(), wantMsg)
	}
}

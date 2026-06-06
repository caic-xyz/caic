// Tests for generic HTTP handler helpers, including error mapping.

package server

import (
	"errors"
	"net/http"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/server/api"
	"github.com/caic-xyz/caic/backend/internal/tasks"
)

func TestToDTO(t *testing.T) {
	t.Parallel()

	t.Run("nil", func(t *testing.T) {
		t.Parallel()
		if got := toDTO(nil); got != nil {
			t.Errorf("toDTO(nil) = %v, want nil", got)
		}
	})

	t.Run("kinds", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name       string
			kind       tasks.ErrorKind
			wantStatus int
			wantCode   api.ErrorCode
		}{
			{"not_found", tasks.KindNotFound, http.StatusNotFound, api.CodeNotFound},
			{"conflict", tasks.KindConflict, http.StatusConflict, api.CodeConflict},
			{"bad_request", tasks.KindBadRequest, http.StatusBadRequest, api.CodeBadRequest},
			{"internal", tasks.KindInternal, http.StatusInternalServerError, api.CodeInternalError},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				got := toDTO(&tasks.Error{Kind: tc.kind, Msg: "boom"})
				var ews api.ErrorWithStatus
				if !errors.As(got, &ews) {
					t.Fatalf("toDTO returned %T, want api.ErrorWithStatus", got)
				}
				if ews.StatusCode() != tc.wantStatus {
					t.Errorf("StatusCode() = %d, want %d", ews.StatusCode(), tc.wantStatus)
				}
				if ews.Code() != tc.wantCode {
					t.Errorf("Code() = %q, want %q", ews.Code(), tc.wantCode)
				}
			})
		}
	})

	t.Run("not_found_trims_suffix", func(t *testing.T) {
		t.Parallel()
		got := toDTO(&tasks.Error{Kind: tasks.KindNotFound, Msg: "task 42 not found"})
		if got.Error() != "task 42 not found" {
			t.Errorf("Error() = %q, want %q", got.Error(), "task 42 not found")
		}
	})

	t.Run("internal_preserves_wrapped", func(t *testing.T) {
		t.Parallel()
		inner := errors.New("connection refused")
		got := toDTO(&tasks.Error{Kind: tasks.KindInternal, Msg: "sync to default", Err: inner})
		if got.Error() != "sync to default: connection refused" {
			t.Errorf("Error() = %q, want it to include the wrapped error", got.Error())
		}
	})

	t.Run("already_api", func(t *testing.T) {
		t.Parallel()
		orig := api.BadRequest("invalid input")
		got := toDTO(orig)
		if !errors.Is(got, orig) {
			t.Errorf("toDTO should return the API error unchanged, got %v", got)
		}
	})

	t.Run("fallback_plain_error", func(t *testing.T) {
		t.Parallel()
		got := toDTO(errors.New("random failure"))
		var ews api.ErrorWithStatus
		if !errors.As(got, &ews) {
			t.Fatalf("toDTO returned %T, want api.ErrorWithStatus", got)
		}
		if ews.StatusCode() != http.StatusInternalServerError {
			t.Errorf("StatusCode() = %d, want 500", ews.StatusCode())
		}
		if got.Error() != "random failure" {
			t.Errorf("Error() = %q, want %q", got.Error(), "random failure")
		}
	})
}

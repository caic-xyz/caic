// Tests for generic HTTP handler wrappers, including error mapping.

package server

import (
	"errors"
	"net/http"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/server/api"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
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
			kind       taskmgr.ErrorKind
			wantStatus int
			wantCode   api.ErrorCode
		}{
			{"not_found", taskmgr.KindNotFound, http.StatusNotFound, api.CodeNotFound},
			{"conflict", taskmgr.KindConflict, http.StatusConflict, api.CodeConflict},
			{"bad_request", taskmgr.KindBadRequest, http.StatusBadRequest, api.CodeBadRequest},
			{"internal", taskmgr.KindInternal, http.StatusInternalServerError, api.CodeInternalError},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				got := toDTO(&taskmgr.Error{Kind: tc.kind, Msg: "boom"})
				ews, ok := errors.AsType[api.ErrorWithStatus](got)
				if !ok {
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
		got := toDTO(&taskmgr.Error{Kind: taskmgr.KindNotFound, Msg: "task 42 not found"})
		if got.Error() != "task 42 not found" {
			t.Errorf("Error() = %q, want %q", got.Error(), "task 42 not found")
		}
	})

	t.Run("internal_preserves_wrapped", func(t *testing.T) {
		t.Parallel()
		inner := errors.New("connection refused")
		got := toDTO(&taskmgr.Error{Kind: taskmgr.KindInternal, Msg: "sync to default", Err: inner})
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
		ews, ok := errors.AsType[api.ErrorWithStatus](got)
		if !ok {
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

func TestComputeTaskPatch(t *testing.T) {
	t.Parallel()
	t.Run("ChangedFields", func(t *testing.T) {
		t.Parallel()
		old := `{"id":"abc","state":"running","costUSD":0.0}`
		new_ := `{"id":"abc","state":"waiting","costUSD":1.5}`
		patch, err := computeTaskPatch([]byte(old), []byte(new_))
		if err != nil {
			t.Fatal(err)
		}
		if string(patch["id"]) != `"abc"` {
			t.Errorf("id = %s, want \"abc\"", patch["id"])
		}
		if string(patch["state"]) != `"waiting"` {
			t.Errorf("state = %s, want \"waiting\"", patch["state"])
		}
		if string(patch["costUSD"]) != `1.5` {
			t.Errorf("costUSD = %s, want 1.5", patch["costUSD"])
		}
		// Unchanged field should not be in patch
		if _, ok := patch["costUSD"]; !ok {
			t.Error("costUSD should be in patch (changed from 0.0 to 1.5)")
		}
	})
	t.Run("UnchangedFieldsOmitted", func(t *testing.T) {
		t.Parallel()
		old := `{"id":"abc","state":"running","repo":"myrepo"}`
		new_ := `{"id":"abc","state":"waiting","repo":"myrepo"}`
		patch, err := computeTaskPatch([]byte(old), []byte(new_))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := patch["repo"]; ok {
			t.Error("repo should not be in patch (unchanged)")
		}
		if _, ok := patch["state"]; !ok {
			t.Error("state should be in patch (changed)")
		}
	})
	t.Run("RemovedFieldSetToNull", func(t *testing.T) {
		t.Parallel()
		old := `{"id":"abc","error":"boom"}`
		new_ := `{"id":"abc"}`
		patch, err := computeTaskPatch([]byte(old), []byte(new_))
		if err != nil {
			t.Fatal(err)
		}
		if string(patch["error"]) != "null" {
			t.Errorf("removed field error = %s, want null", patch["error"])
		}
	})
	t.Run("AlwaysIncludesID", func(t *testing.T) {
		t.Parallel()
		old := `{"id":"xyz","state":"running"}`
		new_ := `{"id":"xyz","state":"purged"}`
		patch, err := computeTaskPatch([]byte(old), []byte(new_))
		if err != nil {
			t.Fatal(err)
		}
		if string(patch["id"]) != `"xyz"` {
			t.Errorf("id = %s, want \"xyz\"", patch["id"])
		}
	})
}

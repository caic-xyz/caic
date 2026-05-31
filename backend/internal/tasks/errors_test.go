// Tests for the tasks package typed errors.

package tasks

import (
	"errors"
	"testing"
)

func TestErrorKind(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		cases := map[ErrorKind]string{
			KindNotFound:   "NotFound",
			KindConflict:   "Conflict",
			KindBadRequest: "BadRequest",
			KindInternal:   "Internal",
		}
		for k, want := range cases {
			if got := k.String(); got != want {
				t.Errorf("%d.String() = %q, want %q", int(k), got, want)
			}
		}
	})
	t.Run("unknown", func(t *testing.T) {
		t.Parallel()
		if got := ErrorKind(99).String(); got != "ErrorKind(99)" {
			t.Errorf("String() = %q, want ErrorKind(99)", got)
		}
	})
}

func TestError(t *testing.T) {
	t.Parallel()
	t.Run("message", func(t *testing.T) {
		t.Parallel()
		e := &Error{Kind: KindConflict, Msg: "task is not stopped"}
		if e.Error() != "task is not stopped" {
			t.Errorf("Error() = %q, want %q", e.Error(), "task is not stopped")
		}
	})
	t.Run("wrapped_message", func(t *testing.T) {
		t.Parallel()
		inner := errors.New("connection refused")
		e := internalErr(inner, "sync to default")
		if got, want := e.Error(), "sync to default: connection refused"; got != want {
			t.Errorf("Error() = %q, want %q", got, want)
		}
	})
	t.Run("unwrap", func(t *testing.T) {
		t.Parallel()
		inner := errors.New("boom")
		e := internalErr(inner, "start task")
		if !errors.Is(e, inner) {
			t.Error("errors.Is did not find wrapped error")
		}
		var te *Error
		if !errors.As(error(e), &te) || te.Kind != KindInternal {
			t.Error("errors.As did not extract *Error with KindInternal")
		}
	})
	t.Run("unwrap_nil", func(t *testing.T) {
		t.Parallel()
		e := conflict("nope")
		if e.Unwrap() != nil {
			t.Error("Unwrap() should be nil when no wrapped error")
		}
	})
}

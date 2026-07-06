// Tests for the taskmgr package typed errors.

package taskmgr

import (
	"errors"
	"testing"
)

func TestNoSessionError(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		inner := errors.New("no active session for task: waiting")
		e := &NoSessionError{Err: inner}
		if got := e.Error(); got != inner.Error() {
			t.Errorf("Error() = %q, want %q", got, inner.Error())
		}
		if !errors.Is(e, ErrNoSession) {
			t.Error("errors.Is did not match ErrNoSession")
		}
	})
	t.Run("unwrap", func(t *testing.T) {
		t.Parallel()
		inner := errors.New("no session")
		e := &NoSessionError{Err: inner}
		if !errors.Is(e, inner) {
			t.Error("errors.Is did not find wrapped error")
		}
		var ns *NoSessionError
		if !errors.As(e, &ns) || !errors.Is(ns.Err, inner) {
			t.Error("errors.As did not extract *NoSessionError")
		}
	})
}

func TestConflictErr(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		inner := errors.New("timeout")
		e := conflictErr(inner, "no active session to compact")
		if got, want := e.Error(), "no active session to compact: timeout"; got != want {
			t.Errorf("Error() = %q, want %q", got, want)
		}
		if e.Kind != KindConflict {
			t.Errorf("Kind = %v, want KindConflict", e.Kind)
		}
		if !errors.Is(e, inner) {
			t.Error("errors.Is did not find wrapped error")
		}
	})
}

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

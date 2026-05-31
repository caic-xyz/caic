// Typed errors for the tasks package, mapped to HTTP status codes by the server.

package tasks

import (
	"errors"
	"fmt"
)

// ErrNoSession is a sentinel reported by Manager.SendInput when the task has no
// active agent session to forward input to. The underlying error from
// task.SendInput (which carries the state/session diagnostics) is wrapped, so
// both errors.Is(err, ErrNoSession) and the original message remain reachable.
//
// Manager.SendInput returns a *NoSessionError, whose Error() is exactly the
// underlying task.SendInput message (no sentinel prefix) while still satisfying
// errors.Is(err, ErrNoSession). This lets the HTTP layer detect the condition
// and surface the original diagnostic text byte-for-byte.
var ErrNoSession = errors.New("no active session")

// NoSessionError wraps the underlying task.SendInput error when there is no
// active agent session. It reports the underlying message verbatim and
// satisfies errors.Is(err, ErrNoSession).
type NoSessionError struct {
	Err error // underlying task.SendInput diagnostic error
}

// Error returns the underlying diagnostic message verbatim.
func (e *NoSessionError) Error() string {
	return e.Err.Error()
}

// Unwrap returns the underlying error for errors.Is/errors.As traversal.
func (e *NoSessionError) Unwrap() error {
	return e.Err
}

// Is reports a match against the ErrNoSession sentinel.
func (e *NoSessionError) Is(target error) bool {
	return target == ErrNoSession
}

// ErrorKind classifies a tasks.Error so the HTTP layer can map it to a status
// code without inspecting the message string.
type ErrorKind int

const (
	// KindInternal is an unexpected or infrastructure failure (HTTP 500).
	KindInternal ErrorKind = iota
	// KindNotFound is a missing task or resource (HTTP 404).
	KindNotFound
	// KindConflict is a state/precondition violation (HTTP 409).
	KindConflict
	// KindBadRequest is invalid input (HTTP 400).
	KindBadRequest
)

// String returns the kind name.
func (k ErrorKind) String() string {
	switch k {
	case KindNotFound:
		return "NotFound"
	case KindConflict:
		return "Conflict"
	case KindBadRequest:
		return "BadRequest"
	case KindInternal:
		return "Internal"
	default:
		return fmt.Sprintf("ErrorKind(%d)", int(k))
	}
}

// Error is a typed error returned by Manager lifecycle methods. Kind classifies
// the failure; the HTTP layer maps it to a status code. An optional wrapped
// error is preserved for errors.Is/errors.As.
type Error struct {
	Kind ErrorKind
	Msg  string
	Err  error // wrapped underlying error, if any
}

// Error implements error.
func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Msg, e.Err)
	}
	return e.Msg
}

// Unwrap returns the wrapped error so errors.Is/errors.As traverse the chain.
func (e *Error) Unwrap() error {
	return e.Err
}

// notFoundf builds a KindNotFound error with a formatted message.
func notFoundf(format string, args ...any) *Error {
	return &Error{Kind: KindNotFound, Msg: fmt.Sprintf(format, args...)}
}

// conflict builds a KindConflict error.
func conflict(msg string) *Error {
	return &Error{Kind: KindConflict, Msg: msg}
}

// conflictErr builds a KindConflict error wrapping err. The wrapped error
// remains reachable via errors.Is/As; toDTO surfaces it in the response.
func conflictErr(err error, msg string) *Error {
	return &Error{Kind: KindConflict, Msg: msg, Err: err}
}

// badRequestf builds a KindBadRequest error with a formatted message.
func badRequestf(format string, args ...any) *Error {
	return &Error{Kind: KindBadRequest, Msg: fmt.Sprintf(format, args...)}
}

// internalErr builds a KindInternal error wrapping err. The wrapped error
// remains reachable via errors.Is/As. It mirrors conflictErr; the trailing
// "Err" marks that a cause is required (use a dedicated no-cause constructor
// if one is ever needed, rather than passing nil here).
func internalErr(err error, msg string) *Error {
	return &Error{Kind: KindInternal, Msg: msg, Err: err}
}

// Runtime system test double.

package runtimetest

import "github.com/caic-xyz/caic/backend/internal/runtime"

// FakeSystem is an in-memory runtime.System test double.
type FakeSystem struct {
	FakeBackend
	FakeInfo
}

var _ runtime.System = (*FakeSystem)(nil)

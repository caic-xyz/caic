// Package runtimetest provides shared test doubles for the runtime package's
// interfaces, reusable by any package that depends on a runtime seam.
package runtimetest

import (
	"context"
	"sync"

	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// InstanceStatus is the lifecycle state FakeBackend tracks per instance.
type InstanceStatus int

const (
	// StatusAbsent is the zero value: never launched, or referenced only by a
	// read. It is also what Status reports for an unknown instance.
	StatusAbsent InstanceStatus = iota
	// StatusRunning is set by Launch, Fork, and Revive.
	StatusRunning
	// StatusStopped is set by Stop.
	StatusStopped
	// StatusPurged is set by Purge.
	StatusPurged
)

// String returns the lowercase status name for readable test diagnostics.
func (s InstanceStatus) String() string {
	switch s {
	case StatusRunning:
		return "running"
	case StatusStopped:
		return "stopped"
	case StatusPurged:
		return "purged"
	default:
		return "absent"
	}
}

// SignalDelivery is the most recent signal a FakeBackend delivered to an instance.
type SignalDelivery struct {
	PID    int
	Signal string
}

// FakeBackend is an in-memory runtime.Backend. It models each instance's
// lifecycle so tests assert on the resulting state (Status, LastSignal) rather
// than on which methods were called. The zero value is usable and every method
// is safe for concurrent use.
//
// Configure returned data through the exported fields. To inject latency or a
// failure, embed it and override the method, calling the embedded method when
// the fake's state should still advance:
//
//	type stopFails struct{ *runtimetest.FakeBackend }
//
//	func (f stopFails) Stop(ctx context.Context, id runtime.InstanceID) error {
//		return errBoom // deliberately does not advance to StatusStopped
//	}
type FakeBackend struct {
	// DiffOutput is returned verbatim by Diff.
	DiffOutput string
	// LaunchErr, when set, is returned by Launch.
	LaunchErr error
	// FetchErr, when set, is returned by Fetch.
	FetchErr error

	mu      sync.Mutex
	status  map[runtime.InstanceID]InstanceStatus
	signals map[runtime.InstanceID]SignalDelivery
}

// Ensure the fake satisfies the interface at compile time.
var _ runtime.Backend = (*FakeBackend)(nil)

// Status reports the tracked lifecycle state of id, StatusAbsent if unknown.
func (f *FakeBackend) Status(id runtime.InstanceID) InstanceStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status[id]
}

// LastSignal reports the most recent signal delivered to id.
func (f *FakeBackend) LastSignal(id runtime.InstanceID) (SignalDelivery, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.signals[id]
	return d, ok
}

// Launch implements runtime.Backend.
func (f *FakeBackend) Launch(ctx context.Context, repos []runtime.Repo, opts *runtime.StartOptions) (runtime.InstanceID, error) {
	if f.LaunchErr != nil {
		return "", f.LaunchErr
	}
	f.set("fake-container", StatusRunning)
	return "fake-container", nil
}

// Connect implements runtime.Backend.
func (f *FakeBackend) Connect(ctx context.Context, id runtime.InstanceID, opts *runtime.StartOptions) (runtime.ConnectionInfo, error) {
	return runtime.ConnectionInfo{AgentTarget: runtime.ConnectionTarget{SSHHost: string(id)}}, nil
}

// Diff implements runtime.Backend.
func (f *FakeBackend) Diff(ctx context.Context, id runtime.InstanceID, repoIdx int, args ...string) (string, error) {
	return f.DiffOutput, nil
}

// Fetch implements runtime.Backend.
func (f *FakeBackend) Fetch(ctx context.Context, id runtime.InstanceID) error { return f.FetchErr }

// Stop implements runtime.Backend.
func (f *FakeBackend) Stop(ctx context.Context, id runtime.InstanceID) error {
	f.set(id, StatusStopped)
	return nil
}

// Purge implements runtime.Backend.
func (f *FakeBackend) Purge(ctx context.Context, id runtime.InstanceID) error {
	f.set(id, StatusPurged)
	return nil
}

// Revive implements runtime.Backend.
func (f *FakeBackend) Revive(ctx context.Context, id runtime.InstanceID) error {
	f.set(id, StatusRunning)
	return nil
}

// Fork implements runtime.Backend.
func (f *FakeBackend) Fork(ctx context.Context, id runtime.InstanceID, repos []runtime.Repo, opts *runtime.ForkOptions) (runtime.InstanceID, runtime.ConnectionInfo, []runtime.Repo, error) {
	f.set("fake-fork", StatusRunning)
	return "fake-fork", runtime.ConnectionInfo{AgentTarget: runtime.ConnectionTarget{SSHHost: "fake-fork"}}, nil, nil
}

// VNCPort implements runtime.Backend.
func (f *FakeBackend) VNCPort(ctx context.Context, id runtime.InstanceID) int { return 0 }

// Processes implements runtime.Backend.
func (f *FakeBackend) Processes(ctx context.Context, id runtime.InstanceID) ([]runtime.ProcessInfo, error) {
	return nil, nil
}

// Signal implements runtime.Backend.
func (f *FakeBackend) Signal(ctx context.Context, id runtime.InstanceID, pid int, sig string) error {
	f.mu.Lock()
	if f.signals == nil {
		f.signals = map[runtime.InstanceID]SignalDelivery{}
	}
	f.signals[id] = SignalDelivery{PID: pid, Signal: sig}
	f.mu.Unlock()
	return nil
}

func (f *FakeBackend) set(id runtime.InstanceID, s InstanceStatus) {
	f.mu.Lock()
	if f.status == nil {
		f.status = map[runtime.InstanceID]InstanceStatus{}
	}
	f.status[id] = s
	f.mu.Unlock()
}

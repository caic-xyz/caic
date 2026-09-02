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

// FakeBackend is an in-memory runtime lifecycle and repository backend. It
// models each instance's lifecycle so tests assert on the resulting state
// (Status, LastSignal) rather than on which methods were called. The zero value
// is usable and every method is safe for concurrent use.
//
// Configure returned data through the exported fields. To inject latency or a
// failure, embed it and override the method, calling the embedded method when
// the fake's state should still advance:
//
//	type stopFails struct{ *runtimetest.FakeBackend }
//
//	func (f stopFails) Stop(ctx context.Context, id runtime.ID) error {
//		return errBoom // deliberately does not advance to StatusStopped
//	}
type FakeBackend struct {
	// RuntimeName is returned by Name. Empty defaults to "test-runtime".
	RuntimeName runtime.Name
	// DiffOutput is returned verbatim by Diff.
	DiffOutput string
	// RepositoryStatusValue is returned by RepositoryStatus.
	RepositoryStatusValue runtime.RepositoryStatus
	// LaunchErr, when set, is returned by Launch.
	LaunchErr error
	// FetchErr, when set, is returned by Fetch.
	FetchErr error

	mu      sync.Mutex
	status  map[runtime.ID]InstanceStatus
	signals map[runtime.ID]SignalDelivery
}

// Ensure the fake satisfies the interface at compile time.
var _ runtime.Lifecycle = (*FakeBackend)(nil)
var _ runtime.Repository = (*FakeBackend)(nil)

// Name returns the runtime backend name.
func (f *FakeBackend) Name() runtime.Name {
	if f.RuntimeName == "" {
		return "test-runtime"
	}
	return f.RuntimeName
}

// Status reports the tracked lifecycle state of id, StatusAbsent if unknown.
func (f *FakeBackend) Status(id runtime.ID) InstanceStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status[f.normalizeID(id)]
}

// LastSignal reports the most recent signal delivered to id.
func (f *FakeBackend) LastSignal(id runtime.ID) (SignalDelivery, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.signals[f.normalizeID(id)]
	return d, ok
}

// Launch implements runtime.Lifecycle.
func (f *FakeBackend) Launch(ctx context.Context, repos []runtime.Repo, opts *runtime.StartOptions) (runtime.ID, error) {
	if f.LaunchErr != nil {
		return "", f.LaunchErr
	}
	id := runtime.NewID(f.Name(), "fake-container")
	f.set(id, StatusRunning)
	return id, nil
}

// Connect implements runtime.Lifecycle.
func (f *FakeBackend) Connect(ctx context.Context, id runtime.ID, opts *runtime.StartOptions) (runtime.ConnectionInfo, error) {
	return runtime.ConnectionInfo{AgentTarget: runtime.ConnectionTarget{SSHHost: string(id.InstanceID())}}, nil
}

// Diff implements runtime.Repository.
func (f *FakeBackend) Diff(ctx context.Context, id runtime.ID, repoIdx int, args ...string) (string, error) {
	return f.DiffOutput, nil
}

// RepositoryStatus implements runtime.Repository.
func (f *FakeBackend) RepositoryStatus(context.Context, runtime.ID, int) (runtime.RepositoryStatus, error) {
	return f.RepositoryStatusValue, nil
}

// Fetch implements runtime.Repository.
func (f *FakeBackend) Fetch(ctx context.Context, id runtime.ID) error { return f.FetchErr }

// Stop implements runtime.Lifecycle.
func (f *FakeBackend) Stop(ctx context.Context, id runtime.ID) error {
	f.set(id, StatusStopped)
	return nil
}

// Purge implements runtime.Lifecycle.
func (f *FakeBackend) Purge(ctx context.Context, id runtime.ID) error {
	f.set(id, StatusPurged)
	return nil
}

// Revive implements runtime.Lifecycle.
func (f *FakeBackend) Revive(ctx context.Context, id runtime.ID) error {
	f.set(id, StatusRunning)
	return nil
}

// Fork implements runtime.Lifecycle.
func (f *FakeBackend) Fork(ctx context.Context, id runtime.ID, opts *runtime.ForkOptions) (runtime.ID, runtime.ConnectionInfo, error) {
	forkID := runtime.NewID(f.Name(), "fake-fork")
	f.set(forkID, StatusRunning)
	return forkID, runtime.ConnectionInfo{AgentTarget: runtime.ConnectionTarget{SSHHost: "fake-fork"}}, nil
}

// VNCPort implements runtime.Lifecycle.
func (f *FakeBackend) VNCPort(ctx context.Context, id runtime.ID) int { return 0 }

// Processes implements runtime.Lifecycle.
func (f *FakeBackend) Processes(ctx context.Context, id runtime.ID) ([]runtime.ProcessInfo, error) {
	return nil, nil
}

// Signal implements runtime.Lifecycle.
func (f *FakeBackend) Signal(ctx context.Context, id runtime.ID, pid int, sig string) error {
	f.mu.Lock()
	if f.signals == nil {
		f.signals = map[runtime.ID]SignalDelivery{}
	}
	f.signals[f.normalizeID(id)] = SignalDelivery{PID: pid, Signal: sig}
	f.mu.Unlock()
	return nil
}

func (f *FakeBackend) set(id runtime.ID, s InstanceStatus) {
	f.mu.Lock()
	if f.status == nil {
		f.status = map[runtime.ID]InstanceStatus{}
	}
	f.status[f.normalizeID(id)] = s
	f.mu.Unlock()
}

func (f *FakeBackend) normalizeID(id runtime.ID) runtime.ID {
	if id.RuntimeName() == "" {
		return runtime.NewID(f.Name(), id.InstanceID())
	}
	return id
}

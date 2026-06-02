// Package tasktest provides shared test doubles for the task package's
// infrastructure seams, reusable across internal/task and internal/tasks.
package tasktest

import (
	"context"
	"sync"

	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/task"
)

// Call records a single task.ContainerBackend invocation. Only the fields
// relevant to the invoked Method are populated; the rest are zero.
type Call struct {
	Method  string         // Method name, e.g. "Stop", "Purge", "Launch".
	Name    string         // Container name argument, when the method takes one.
	Repos   []runtime.Repo // Repos argument, when present.
	Labels  []string       // Labels argument (Launch).
	RepoIdx int            // Repository index, when the method operates on one repo.
	Args    []string       // Variadic git args (Diff).
	PID     int            // Process id (Signal).
	Sig     string         // Signal name (Signal).
}

// FakeContainerBackend is a programmable task.ContainerBackend test double. The
// zero value is usable: every method records its call and returns a benign
// default. Override any method via its matching *Func field. All methods and
// accessors are safe for concurrent use.
type FakeContainerBackend struct {
	mu    sync.Mutex
	calls []Call

	LaunchFunc    func(ctx context.Context, repos []runtime.Repo, labels []string, opts *task.StartOptions) (runtime.InstanceID, error)
	ConnectFunc   func(ctx context.Context, id runtime.InstanceID, repos []runtime.Repo, opts *task.StartOptions) (runtime.ConnectionInfo, error)
	DiffFunc      func(ctx context.Context, repos []runtime.Repo, repoIdx int, args ...string) (string, error)
	FetchFunc     func(ctx context.Context, repos []runtime.Repo) error
	StopFunc      func(ctx context.Context, id runtime.InstanceID) error
	PurgeFunc     func(ctx context.Context, id runtime.InstanceID, repos []runtime.Repo) error
	ReviveFunc    func(ctx context.Context, id runtime.InstanceID, repos []runtime.Repo) error
	ForkFunc      func(ctx context.Context, id runtime.InstanceID, repos []runtime.Repo, opts *task.ForkOptions) (runtime.InstanceID, []runtime.Repo, error)
	VNCPortFunc   func(ctx context.Context, id runtime.InstanceID) int
	ProcessesFunc func(ctx context.Context, id runtime.InstanceID) ([]runtime.ProcessInfo, error)
	SignalFunc    func(ctx context.Context, id runtime.InstanceID, pid int, sig string) error
}

// Ensure the fake satisfies the interface at compile time.
var _ task.ContainerBackend = (*FakeContainerBackend)(nil)

// Calls returns a copy of the recorded calls in invocation order.
func (f *FakeContainerBackend) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// Count returns how many times method was invoked.
func (f *FakeContainerBackend) Count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for i := range f.calls {
		if f.calls[i].Method == method {
			n++
		}
	}
	return n
}

// Called reports whether method was invoked at least once.
func (f *FakeContainerBackend) Called(method string) bool { return f.Count(method) > 0 }

// Launch implements task.ContainerBackend.
func (f *FakeContainerBackend) Launch(ctx context.Context, repos []runtime.Repo, labels []string, opts *task.StartOptions) (runtime.InstanceID, error) {
	f.record(&Call{Method: "Launch", Repos: repos, Labels: labels})
	if f.LaunchFunc != nil {
		return f.LaunchFunc(ctx, repos, labels, opts)
	}
	return "fake-container", nil
}

// Connect implements task.ContainerBackend.
func (f *FakeContainerBackend) Connect(ctx context.Context, id runtime.InstanceID, repos []runtime.Repo, opts *task.StartOptions) (runtime.ConnectionInfo, error) {
	f.record(&Call{Method: "Connect", Name: string(id), Repos: repos})
	if f.ConnectFunc != nil {
		return f.ConnectFunc(ctx, id, repos, opts)
	}
	return runtime.ConnectionInfo{}, nil
}

// Diff implements task.ContainerBackend.
func (f *FakeContainerBackend) Diff(ctx context.Context, repos []runtime.Repo, repoIdx int, args ...string) (string, error) {
	f.record(&Call{Method: "Diff", Repos: repos, RepoIdx: repoIdx, Args: args})
	if f.DiffFunc != nil {
		return f.DiffFunc(ctx, repos, repoIdx, args...)
	}
	return "", nil
}

// Fetch implements task.ContainerBackend.
func (f *FakeContainerBackend) Fetch(ctx context.Context, repos []runtime.Repo) error {
	f.record(&Call{Method: "Fetch", Repos: repos})
	if f.FetchFunc != nil {
		return f.FetchFunc(ctx, repos)
	}
	return nil
}

// Stop implements task.ContainerBackend.
func (f *FakeContainerBackend) Stop(ctx context.Context, id runtime.InstanceID) error {
	f.record(&Call{Method: "Stop", Name: string(id)})
	if f.StopFunc != nil {
		return f.StopFunc(ctx, id)
	}
	return nil
}

// Purge implements task.ContainerBackend.
func (f *FakeContainerBackend) Purge(ctx context.Context, id runtime.InstanceID, repos []runtime.Repo) error {
	f.record(&Call{Method: "Purge", Name: string(id), Repos: repos})
	if f.PurgeFunc != nil {
		return f.PurgeFunc(ctx, id, repos)
	}
	return nil
}

// Revive implements task.ContainerBackend.
func (f *FakeContainerBackend) Revive(ctx context.Context, id runtime.InstanceID, repos []runtime.Repo) error {
	f.record(&Call{Method: "Revive", Name: string(id), Repos: repos})
	if f.ReviveFunc != nil {
		return f.ReviveFunc(ctx, id, repos)
	}
	return nil
}

// Fork implements task.ContainerBackend.
func (f *FakeContainerBackend) Fork(ctx context.Context, id runtime.InstanceID, repos []runtime.Repo, opts *task.ForkOptions) (runtime.InstanceID, []runtime.Repo, error) {
	f.record(&Call{Method: "Fork", Name: string(id), Repos: repos})
	if f.ForkFunc != nil {
		return f.ForkFunc(ctx, id, repos, opts)
	}
	return "fake-fork", nil, nil
}

// VNCPort implements task.ContainerBackend.
func (f *FakeContainerBackend) VNCPort(ctx context.Context, id runtime.InstanceID) int {
	f.record(&Call{Method: "VNCPort", Name: string(id)})
	if f.VNCPortFunc != nil {
		return f.VNCPortFunc(ctx, id)
	}
	return 0
}

// Processes implements task.ContainerBackend.
func (f *FakeContainerBackend) Processes(ctx context.Context, id runtime.InstanceID) ([]runtime.ProcessInfo, error) {
	f.record(&Call{Method: "Processes", Name: string(id)})
	if f.ProcessesFunc != nil {
		return f.ProcessesFunc(ctx, id)
	}
	return nil, nil
}

// Signal implements task.ContainerBackend.
func (f *FakeContainerBackend) Signal(ctx context.Context, id runtime.InstanceID, pid int, sig string) error {
	f.record(&Call{Method: "Signal", Name: string(id), PID: pid, Sig: sig})
	if f.SignalFunc != nil {
		return f.SignalFunc(ctx, id, pid, sig)
	}
	return nil
}

func (f *FakeContainerBackend) record(c *Call) {
	f.mu.Lock()
	f.calls = append(f.calls, *c)
	f.mu.Unlock()
}

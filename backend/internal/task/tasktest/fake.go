// Package tasktest provides shared test doubles for the task package's
// infrastructure seams, reusable across internal/task and internal/tasks.
package tasktest

import (
	"context"
	"sync"

	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// Call records a single runtime.Backend invocation. Only the fields
// relevant to the invoked Method are populated; the rest are zero.
type Call struct {
	Method   string           // Method name, e.g. "Stop", "Purge", "Launch".
	Name     string           // Instance name argument, when the method takes one.
	Repos    []runtime.Repo   // Repos argument, when present.
	Metadata runtime.Metadata // Runtime metadata argument (Launch).
	RepoIdx  int              // Repository index, when the method operates on one repo.
	Args     []string         // Variadic git args (Diff).
	PID      int              // Process id (Signal).
	Sig      string           // Signal name (Signal).
}

// FakeRuntimeBackend is a programmable runtime.Backend test double. The
// zero value is usable: every method records its call and returns a benign
// default. Override any method via its matching *Func field. All methods and
// accessors are safe for concurrent use.
type FakeRuntimeBackend struct {
	mu    sync.Mutex
	calls []Call

	LaunchFunc    func(ctx context.Context, repos []runtime.Repo, opts *runtime.StartOptions) (runtime.InstanceID, error)
	ConnectFunc   func(ctx context.Context, id runtime.InstanceID, opts *runtime.StartOptions) (runtime.ConnectionInfo, error)
	DiffFunc      func(ctx context.Context, id runtime.InstanceID, repoIdx int, args ...string) (string, error)
	FetchFunc     func(ctx context.Context, id runtime.InstanceID) error
	StopFunc      func(ctx context.Context, id runtime.InstanceID) error
	PurgeFunc     func(ctx context.Context, id runtime.InstanceID) error
	ReviveFunc    func(ctx context.Context, id runtime.InstanceID) error
	ForkFunc      func(ctx context.Context, id runtime.InstanceID, repos []runtime.Repo, opts *runtime.ForkOptions) (runtime.InstanceID, []runtime.Repo, error)
	VNCPortFunc   func(ctx context.Context, id runtime.InstanceID) int
	ProcessesFunc func(ctx context.Context, id runtime.InstanceID) ([]runtime.ProcessInfo, error)
	SignalFunc    func(ctx context.Context, id runtime.InstanceID, pid int, sig string) error
}

// Ensure the fake satisfies the interface at compile time.
var _ runtime.Backend = (*FakeRuntimeBackend)(nil)

// Calls returns a copy of the recorded calls in invocation order.
func (f *FakeRuntimeBackend) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// Count returns how many times method was invoked.
func (f *FakeRuntimeBackend) Count(method string) int {
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
func (f *FakeRuntimeBackend) Called(method string) bool { return f.Count(method) > 0 }

// Launch implements runtime.Backend.
func (f *FakeRuntimeBackend) Launch(ctx context.Context, repos []runtime.Repo, opts *runtime.StartOptions) (runtime.InstanceID, error) {
	var metadata runtime.Metadata
	if opts != nil {
		metadata = opts.Metadata
	}
	f.record(&Call{Method: "Launch", Repos: repos, Metadata: metadata})
	if f.LaunchFunc != nil {
		return f.LaunchFunc(ctx, repos, opts)
	}
	return "fake-container", nil
}

// Connect implements runtime.Backend.
func (f *FakeRuntimeBackend) Connect(ctx context.Context, id runtime.InstanceID, opts *runtime.StartOptions) (runtime.ConnectionInfo, error) {
	f.record(&Call{Method: "Connect", Name: string(id)})
	if f.ConnectFunc != nil {
		return f.ConnectFunc(ctx, id, opts)
	}
	return runtime.ConnectionInfo{}, nil
}

// Diff implements runtime.Backend.
func (f *FakeRuntimeBackend) Diff(ctx context.Context, id runtime.InstanceID, repoIdx int, args ...string) (string, error) {
	f.record(&Call{Method: "Diff", Name: string(id), RepoIdx: repoIdx, Args: args})
	if f.DiffFunc != nil {
		return f.DiffFunc(ctx, id, repoIdx, args...)
	}
	return "", nil
}

// Fetch implements runtime.Backend.
func (f *FakeRuntimeBackend) Fetch(ctx context.Context, id runtime.InstanceID) error {
	f.record(&Call{Method: "Fetch", Name: string(id)})
	if f.FetchFunc != nil {
		return f.FetchFunc(ctx, id)
	}
	return nil
}

// Stop implements runtime.Backend.
func (f *FakeRuntimeBackend) Stop(ctx context.Context, id runtime.InstanceID) error {
	f.record(&Call{Method: "Stop", Name: string(id)})
	if f.StopFunc != nil {
		return f.StopFunc(ctx, id)
	}
	return nil
}

// Purge implements runtime.Backend.
func (f *FakeRuntimeBackend) Purge(ctx context.Context, id runtime.InstanceID) error {
	f.record(&Call{Method: "Purge", Name: string(id)})
	if f.PurgeFunc != nil {
		return f.PurgeFunc(ctx, id)
	}
	return nil
}

// Revive implements runtime.Backend.
func (f *FakeRuntimeBackend) Revive(ctx context.Context, id runtime.InstanceID) error {
	f.record(&Call{Method: "Revive", Name: string(id)})
	if f.ReviveFunc != nil {
		return f.ReviveFunc(ctx, id)
	}
	return nil
}

// Fork implements runtime.Backend.
func (f *FakeRuntimeBackend) Fork(ctx context.Context, id runtime.InstanceID, repos []runtime.Repo, opts *runtime.ForkOptions) (runtime.InstanceID, []runtime.Repo, error) {
	f.record(&Call{Method: "Fork", Name: string(id), Repos: repos})
	if f.ForkFunc != nil {
		return f.ForkFunc(ctx, id, repos, opts)
	}
	return "fake-fork", nil, nil
}

// VNCPort implements runtime.Backend.
func (f *FakeRuntimeBackend) VNCPort(ctx context.Context, id runtime.InstanceID) int {
	f.record(&Call{Method: "VNCPort", Name: string(id)})
	if f.VNCPortFunc != nil {
		return f.VNCPortFunc(ctx, id)
	}
	return 0
}

// Processes implements runtime.Backend.
func (f *FakeRuntimeBackend) Processes(ctx context.Context, id runtime.InstanceID) ([]runtime.ProcessInfo, error) {
	f.record(&Call{Method: "Processes", Name: string(id)})
	if f.ProcessesFunc != nil {
		return f.ProcessesFunc(ctx, id)
	}
	return nil, nil
}

// Signal implements runtime.Backend.
func (f *FakeRuntimeBackend) Signal(ctx context.Context, id runtime.InstanceID, pid int, sig string) error {
	f.record(&Call{Method: "Signal", Name: string(id), PID: pid, Sig: sig})
	if f.SignalFunc != nil {
		return f.SignalFunc(ctx, id, pid, sig)
	}
	return nil
}

func (f *FakeRuntimeBackend) record(c *Call) {
	f.mu.Lock()
	f.calls = append(f.calls, *c)
	f.mu.Unlock()
}

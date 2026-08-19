// Synchronized task-log path storage.

package taskslog

import "sync"

// Path owns the current physical log path for one task lifecycle.
type Path struct {
	mu   sync.RWMutex
	path string
}

// Get returns the current path, which is empty before a local log opens or if
// adoption cannot establish an authoritative local log.
func (p *Path) Get() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.path
}

// Set records the current physical task-log path.
func (p *Path) Set(path string) {
	p.mu.Lock()
	p.path = path
	p.mu.Unlock()
}

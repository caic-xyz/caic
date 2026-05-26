// Process and signal types used by ContainerBackend.

package task

// ProcessInfo describes a single process running inside a container.
type ProcessInfo struct {
	PID     int
	PPID    int
	User    string
	State   string // Single-character: R, S, D, Z, T, etc.
	CPU     float64
	Mem     float64
	Time    string // Cumulative CPU time.
	Command string // Full command line.
}

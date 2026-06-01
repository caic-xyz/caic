// Runs ps inside a container via SSH and returns the process list.

package container

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/task"
)

// Processes runs ps -eo pid,ppid,user,stat,%cpu,%mem,time,args --no-headers
// inside the named container via SSH and returns the parsed process list.
func (b *Backend) Processes(ctx context.Context, containerName string) ([]task.ProcessInfo, error) {
	ct, err := b.client.Get(ctx, containerName)
	if err != nil {
		return nil, fmt.Errorf("get container %s: %w", containerName, err)
	}
	cmd := "ps -eo pid,ppid,user,stat,%cpu,%mem,time,args --no-headers"
	sshArgs := ct.SSHCommand(nil, cmd)

	c := exec.CommandContext(ctx, sshArgs[0], sshArgs[1:]...) //nolint:gosec // containerName is internally-assigned; cmd is a constant literal
	out, err := c.Output()
	if err != nil {
		return nil, fmt.Errorf("ps in container %s: %w", containerName, err)
	}
	return parsePSOutput(string(out))
}

// Signal sends a signal (e.g. SIGTERM, SIGKILL) to a process inside the
// named container via SSH using kill.
func (b *Backend) Signal(ctx context.Context, containerName string, pid int, sig string) error {
	ct, err := b.client.Get(ctx, containerName)
	if err != nil {
		return fmt.Errorf("get container %s: %w", containerName, err)
	}
	cmd := fmt.Sprintf("kill -s %s %d", sig, pid)
	sshArgs := ct.SSHCommand(nil, cmd)
	c := exec.CommandContext(ctx, sshArgs[0], sshArgs[1:]...) //nolint:gosec // containerName is internally-assigned; cmd uses fmt.Sprintf
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("signal %s pid %d in container %s: %w (output: %s)", sig, pid, containerName, err, string(out))
	}
	return nil
}

// parsePSOutput parses the output of ps with the columns above. The last
// column (args) may contain spaces; we split by whitespace for the first 7
// fields and treat the remainder as the command.
func parsePSOutput(out string) ([]task.ProcessInfo, error) {
	var procs []task.ProcessInfo
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 8)
		var clean []string
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				clean = append(clean, p)
			}
		}
		if len(clean) < 8 {
			clean = strings.Fields(line)
			if len(clean) < 8 {
				continue
			}
			cmd := strings.Join(clean[7:], " ")
			clean = append(clean[:7], cmd)
		}
		pid, err := strconv.Atoi(clean[0])
		if err != nil {
			continue
		}
		ppid, _ := strconv.Atoi(clean[1])
		cpu, _ := strconv.ParseFloat(clean[4], 64)
		mem, _ := strconv.ParseFloat(clean[5], 64)
		procs = append(procs, task.ProcessInfo{
			PID:     pid,
			PPID:    ppid,
			User:    clean[2],
			State:   clean[3],
			CPU:     cpu,
			Mem:     mem,
			Time:    clean[6],
			Command: clean[7],
		})
	}
	// Filter out the ps process itself.
	procs = slices.DeleteFunc(procs, func(p task.ProcessInfo) bool {
		return strings.HasPrefix(p.Command, "ps ")
	})
	return procs, nil
}

// Validates bounded NUL-delimited shell environment snapshots without logging secrets.

//go:build !windows

package main

import (
	"bytes"
	"errors"
	"strings"
)

const shellEnvLimit = 1 << 20

func parseShellEnvironment(out []byte) (map[string]string, error) {
	if len(out) == 0 {
		return nil, errors.New("shell returned no environment snapshot")
	}
	if out[len(out)-1] != 0 {
		return nil, errors.New("shell returned an incomplete environment snapshot")
	}
	env := make(map[string]string)
	for entry := range strings.SplitSeq(string(out[:len(out)-1]), "\x00") {
		k, v, ok := strings.Cut(entry, "=")
		if !ok || k == "" {
			return nil, errors.New("shell returned an invalid environment entry")
		}
		switch k {
		case "HOME", "PWD", "OLDPWD", "SHLVL", "_", "XDG_RUNTIME_DIR", "NOTIFY_SOCKET", "LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES":
			// Keep caic's own process and session bookkeeping, not the probe's.
			continue
		}
		env[k] = v
	}
	return env, nil
}

// accountShellOutput bounds account lookup output to 1 MiB.
// Overflow errors omit captured account data.
type accountShellOutput struct {
	buf bytes.Buffer
}

func (o *accountShellOutput) Write(p []byte) (int, error) {
	if len(p) > shellEnvLimit-o.buf.Len() {
		return 0, errors.New("account lookup output exceeds 1 MiB")
	}
	return o.buf.Write(p)
}

// Standalone utility and conversion functions used across server handlers.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/auth"
	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/md/gitutil"
)

// relayStatus describes the state of the in-container relay daemon, probed
// over SSH when SendInput fails. Combined with the task state and session
// status (from task.SendInput's error), the three values pinpoint why input
// delivery failed:
//
//   - state=waiting session=none  relay=dead → relay died, reconnect failed.
//   - state=waiting session=exited relay=alive → SSH attach exited but relay
//     is still running; reconnect should recover.
//   - state=running session=none  relay=alive → state-machine bug: state says
//     running but no Go-side session object exists.
//   - state=pending session=none  relay=no-container → task never started.
type relayStatus string

const (
	relayAlive       relayStatus = "alive"        // Relay socket exists; daemon is running.
	relayDead        relayStatus = "dead"         // No socket; daemon exited or was never started.
	relayCheckFailed relayStatus = "check-failed" // SSH probe failed (container unreachable).
	relayNoContainer relayStatus = "no-container" // Task has no container yet.
)

// CreateAuthTokenConfig is the request for POST /v1alpha/auth_tokens.
// See https://github.com/googleapis/go-genai/blob/main/types.go
type CreateAuthTokenConfig struct {
	Uses                 int    `json:"uses"`
	ExpireTime           string `json:"expireTime"`
	NewSessionExpireTime string `json:"newSessionExpireTime"`
}

// AuthToken is the response from POST /v1alpha/auth_tokens.
// See https://github.com/googleapis/go-genai/blob/main/types.go
type AuthToken struct {
	Name string `json:"name"`
}

// responseWriter wraps http.ResponseWriter to capture status code and response size.
type responseWriter struct {
	http.ResponseWriter
	status int
	size   int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	n, err := rw.ResponseWriter.Write(b)
	rw.size += n
	return n, err
}

// Flush implements http.Flusher so SSE handlers can flush through the wrapper.
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the underlying ResponseWriter so http.NewResponseController
// can discover interfaces like http.Flusher.
func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

// hexDecode decodes a hex string to bytes, returning an error if invalid.
func hexDecode(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, errors.New("odd length hex string")
	}
	b := make([]byte, len(s)/2)
	for i := range b {
		hi, lo := hexNibble(s[2*i]), hexNibble(s[2*i+1])
		if hi < 0 || lo < 0 {
			return nil, fmt.Errorf("invalid hex character at position %d", 2*i)
		}
		b[i] = byte(hi<<4 | lo) //nolint:gosec // G115: hi and lo are 0-15 from hexNibble, no overflow
	}
	return b, nil
}

func hexNibble(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// parseAllowedUsers splits a comma-separated username list into a set.
// Returns nil for an empty input.
func parseAllowedUsers(csv string) map[string]struct{} {
	if csv == "" {
		return nil
	}
	m := make(map[string]struct{})
	for _, u := range strings.Split(csv, ",") {
		if u = strings.TrimSpace(u); u != "" {
			m[strings.ToLower(u)] = struct{}{}
		}
	}
	return m
}

// userIDFromCtx returns the authenticated user's ID, or "default" in no-auth mode.
func userIDFromCtx(ctx context.Context) string {
	if u, ok := auth.UserFromContext(ctx); ok {
		return u.ID
	}
	return "default"
}

// computeTaskPatch returns a sparse map containing only the fields that differ
// between oldJSON and newJSON, always including "id". Fields present in oldJSON
// but absent in newJSON are set to null so clients can clear them.
func computeTaskPatch(oldJSON, newJSON []byte) (map[string]json.RawMessage, error) {
	var oldMap, newMap map[string]json.RawMessage
	if err := json.Unmarshal(oldJSON, &oldMap); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(newJSON, &newMap); err != nil {
		return nil, err
	}
	patch := map[string]json.RawMessage{"id": newMap["id"]}
	for k, newVal := range newMap {
		if oldVal, ok := oldMap[k]; !ok || !bytes.Equal(oldVal, newVal) {
			patch[k] = newVal
		}
	}
	for k := range oldMap {
		if _, ok := newMap[k]; !ok {
			patch[k] = json.RawMessage("null")
		}
	}
	return patch, nil
}

// emitTaskListEvent marshals ev and writes it as an SSE message event.
func emitTaskListEvent(w http.ResponseWriter, flusher http.Flusher, ev v1.TaskListEvent) error { //nolint:gocritic // struct size grew with Repos field; refactor not worth it
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
	flusher.Flush()
	return nil
}

// serverWarning is a timestamped warning message stored in the Server's ring buffer.
type serverWarning struct {
	seq uint64
	msg string
	ts  time.Time
}

const (
	// maxWarnings caps the warning ring buffer.
	maxWarnings = 100
	// warningDedup suppresses duplicate messages within this window.
	warningDedup = 5 * time.Minute
)

// emitWarning appends a user-visible warning to the ring buffer and wakes SSE
// clients. Duplicate messages within warningDedup are suppressed. Must NOT be
// called with s.mu held.
func (s *Server) emitWarning(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	// Deduplicate: skip if the same message was emitted recently.
	for i := len(s.warnings) - 1; i >= 0; i-- {
		w := s.warnings[i]
		if now.Sub(w.ts) > warningDedup {
			break
		}
		if w.msg == msg {
			return
		}
	}
	s.warningSeq++
	s.warnings = append(s.warnings, serverWarning{seq: s.warningSeq, msg: msg, ts: now})
	// Trim to cap.
	if len(s.warnings) > maxWarnings {
		s.warnings = s.warnings[len(s.warnings)-maxWarnings:]
	}
	s.taskChanged()
}

// warningsSince returns all warnings with seq > after.
func (s *Server) warningsSince(after uint64) []serverWarning {
	var out []serverWarning
	for _, w := range s.warnings {
		if w.seq > after {
			out = append(out, w)
		}
	}
	return out
}

// tailscaleURL returns the Tailscale URL for the task, or "true" if enabled
// but FQDN not yet known, or "" if disabled.
func tailscaleURL(t *task.Task) string {
	if t.TailscaleFQDN != "" {
		return "https://" + t.TailscaleFQDN
	}
	if t.Tailscale {
		return "true"
	}
	return ""
}

// roundDuration rounds d to 3 significant digits with minimum 1us precision.
func roundDuration(d time.Duration) time.Duration {
	for t := 100 * time.Second; t >= 100*time.Microsecond; t /= 10 {
		if d >= t {
			return d.Round(t / 100)
		}
	}
	return d.Round(time.Microsecond)
}

// needsTitleRegen reports whether the adopted task needs an LLM title
// regeneration. It returns true when no usable title exists or when the
// relay captured more completed turns (ResultMessages) than the log file,
// indicating a turn finished while the server was down.
func needsTitleRegen(t *task.Task, lt *task.LoadedTask) bool {
	if lt == nil || lt.Title == "" {
		return true // no saved title — must generate
	}
	// Load log messages to count completed turns the title was based on.
	logResults := 0
	if err := lt.LoadMessages(); err == nil {
		logResults = countResultMessages(lt.Msgs)
	}
	restoredResults := countResultMessages(t.Messages())
	return restoredResults > logResults
}

// countResultMessages counts the number of ResultMessages in msgs.
func countResultMessages(msgs []agent.Message) int {
	n := 0
	for _, m := range msgs {
		if _, ok := m.(*agent.ResultMessage); ok {
			n++
		}
	}
	return n
}

// authEnabled reports whether OAuth authentication is configured.
func (s *Server) authEnabled() bool {
	return s.authStore != nil
}

// authProviders returns the list of configured OAuth provider names.
func (s *Server) authProviders() []string {
	var ps []string
	if s.githubOAuth != nil {
		ps = append(ps, "github")
	}
	if s.gitlabOAuth != nil {
		ps = append(ps, "gitlab")
	}
	return ps
}

func (s *Server) repoURL(rel string) string {
	for _, r := range s.repos {
		if r.RelPath == rel {
			return gitutil.RemoteToHTTPS(r.Remote)
		}
	}
	return ""
}

func (s *Server) repoForge(rel string) v1.Forge {
	for _, r := range s.repos {
		if r.RelPath == rel {
			return v1.Forge(r.ForgeKind)
		}
	}
	return ""
}

func (s *Server) repoAbsPath(rel string) (string, bool) {
	for _, r := range s.repos {
		if r.RelPath == rel {
			return r.AbsPath, true
		}
	}
	return "", false
}

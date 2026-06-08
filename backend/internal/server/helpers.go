// Standalone utility and conversion functions used across server handlers.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/caic-xyz/md/gitutil"

	"github.com/caic-xyz/caic/backend/internal/auth"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

// relayStatus describes the state of the runtime-instance relay daemon, probed
// over SSH when SendInput fails. Combined with the task state and session
// status (from task.SendInput's error), the three values pinpoint why input
// delivery failed:
//
//   - state=waiting session=none  relay=dead → relay died, reconnect failed.
//   - state=waiting session=exited relay=alive → SSH attach exited but relay
//     is still running; reconnect should recover.
//   - state=running session=none  relay=alive → state-machine bug: state says
//     running but no Go-side session object exists.
//   - state=pending session=none  relay=no-instance → task never started.
type relayStatus string

const (
	relayAlive       relayStatus = "alive"        // Relay socket exists; daemon is running.
	relayDead        relayStatus = "dead"         // No socket; daemon exited or was never started.
	relayCheckFailed relayStatus = "check-failed" // SSH probe failed (runtime instance unreachable).
	relayNoInstance  relayStatus = "no-instance"  // Task has no runtime instance yet.
)

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

// roundDuration rounds d to 3 significant digits with minimum 1us precision.
func roundDuration(d time.Duration) time.Duration {
	for t := 100 * time.Second; t >= 100*time.Microsecond; t /= 10 {
		if d >= t {
			return d.Round(t / 100)
		}
	}
	return d.Round(time.Microsecond)
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
	s.initConcernAdapters()
	if info, ok := s.repos.InfoFor(rel); ok {
		return gitutil.RemoteToHTTPS(info.Remote)
	}
	return ""
}

func (s *Server) repoForge(rel string) v1.Forge {
	s.initConcernAdapters()
	if info, ok := s.repos.InfoFor(rel); ok {
		return v1.Forge(info.ForgeKind)
	}
	return ""
}

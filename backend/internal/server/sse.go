// SSE streaming handlers for task list events and usage events.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	api "github.com/caic-xyz/caic/backend/internal/api"
	v1 "github.com/caic-xyz/caic/backend/internal/api/v1"
	"github.com/caic-xyz/caic/backend/internal/api/v1conv"
	"github.com/caic-xyz/caic/backend/internal/tasks"
	"github.com/caic-xyz/caic/backend/internal/usage"
)

// handleTaskListEvents streams patch events for the task list as SSE. On first
// iteration it sends a full snapshot; thereafter it sends only upsert/delete
// events for changed or removed tasks. It pushes immediately when a
// server-handled mutation fires the changed channel, and falls back to a
// 2-second ticker to catch runner-internal state transitions.
func (s *Server) handleTaskListEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, api.InternalError("streaming not supported"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	// With GitHub App configured, CI updates arrive via check_suite webhooks;
	// use a nil channel so the ticker case is never selected.
	var ciTickerC <-chan time.Time
	if s.forge.githubApp == nil {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		ciTickerC = t.C
	}

	// Seed CI status immediately on connect (once); subsequent updates come from
	// webhooks (App) or the ciTicker (polling).
	ctx := r.Context()
	go s.ciService.PollCIForActiveRepos(context.WithoutCancel(ctx))

	// prevByID tracks the last marshalled JSON for each task ID.
	prevByID := map[string][]byte{}
	var prevReposJSON []byte
	var lastWarnTime time.Time
	first := true

	for {
		var out []v1.Task
		s.taskMgr.Range(func(_ string, e *tasks.Entry) bool {
			out = append(out, v1conv.Task(ctx, e, s.taskResolvers()))
			return true
		})
		ch := s.taskMgr.Changed()
		repos := s.repoList()
		s.mu.Lock()
		newWarnings := s.warningsSince(lastWarnTime)
		s.mu.Unlock()

		reposJSON, err := json.Marshal(repos)
		if err != nil {
			slog.Warn("marshal repos", "err", err)
			return
		}

		if first {
			if err := emitTaskListEvent(w, flusher, v1.TaskListEvent{Kind: "snapshot", Snapshot: out}); err != nil {
				slog.Warn("marshal task list snapshot", "err", err)
				return
			}
			if err := emitTaskListEvent(w, flusher, v1.TaskListEvent{Kind: "repos", Repos: *repos}); err != nil {
				slog.Warn("marshal repos snapshot", "err", err)
				return
			}
			for i := range out {
				data, err := json.Marshal(&out[i])
				if err != nil {
					slog.Warn("marshal task entry", "err", err)
					continue
				}
				prevByID[out[i].ID.String()] = data
			}
			prevReposJSON = reposJSON
			first = false
		} else {
			// Emit upserts/patches for new or changed tasks.
			currentIDs := make(map[string]struct{}, len(out))
			for i := range out {
				id := out[i].ID.String()
				currentIDs[id] = struct{}{}
				data, err := json.Marshal(&out[i])
				if err != nil {
					slog.Warn("marshal task", "id", id, "err", err)
					continue
				}
				if !bytes.Equal(data, prevByID[id]) {
					prev := prevByID[id]
					prevByID[id] = data
					if prev == nil {
						// New task: emit full object.
						if err := emitTaskListEvent(w, flusher, v1.TaskListEvent{Kind: "upsert", Upsert: &out[i]}); err != nil {
							slog.Warn("marshal task upsert", "id", id, "err", err)
							return
						}
					} else {
						// Existing task changed: emit only the diff.
						patch, err := computeTaskPatch(prev, data)
						if err != nil {
							slog.Warn("compute task patch", "id", id, "err", err)
							continue
						}
						if err := emitTaskListEvent(w, flusher, v1.TaskListEvent{Kind: "patch", Patch: patch}); err != nil {
							slog.Warn("marshal task patch", "id", id, "err", err)
							return
						}
					}
				}
			}
			// Emit deletes for removed tasks.
			for id := range prevByID {
				if _, ok := currentIDs[id]; !ok {
					if err := emitTaskListEvent(w, flusher, v1.TaskListEvent{Kind: "delete", Delete: id}); err != nil {
						slog.Warn("marshal task delete", "id", id, "err", err)
						return
					}
					delete(prevByID, id)
				}
			}
			// Emit any new warnings.
			for _, warn := range newWarnings {
				if err := emitTaskListEvent(w, flusher, v1.TaskListEvent{Kind: "warning", Warning: warn.msg}); err != nil {
					slog.Warn("marshal warning", "err", err)
					return
				}
				lastWarnTime = warn.ts
			}

			// Emit repos update when default-branch CI status has changed.
			if !bytes.Equal(reposJSON, prevReposJSON) {
				prevReposJSON = reposJSON
				if err := emitTaskListEvent(w, flusher, v1.TaskListEvent{Kind: "repos", Repos: *repos}); err != nil {
					slog.Warn("marshal repos update", "err", err)
					return
				}
			}
		}

		select {
		case <-r.Context().Done():
			return
		case <-ch:
		case <-ticker.C:
		case <-ciTickerC:
			go s.ciService.PollCIForActiveRepos(context.WithoutCancel(r.Context()))
		}
	}
}

// handleUsageEvents streams usage snapshots as SSE. It reacts to task changes
// immediately and ticks every CacheTTL for provider cache refreshes. Each
// message is a single UsageResp JSON object.
func (s *Server) handleUsageEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, api.InternalError("streaming not supported"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	ticker := time.NewTicker(usage.CacheTTL)
	defer ticker.Stop()

	var prev []byte

	for {
		resp := s.buildUsageResp(r.Context())

		data, err := json.Marshal(resp)
		if err == nil && !bytes.Equal(data, prev) {
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
			flusher.Flush()
			prev = data
		}

		ch := s.taskMgr.Changed()
		select {
		case <-r.Context().Done():
			return
		case <-ch:
		case <-ticker.C:
		}
	}
}

// handleGetUsage returns a one-shot usage snapshot as JSON.
func (s *Server) handleGetUsage(w http.ResponseWriter, r *http.Request) {
	resp := s.buildUsageResp(r.Context())
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.WarnContext(r.Context(), "encode usage response", "err", err)
	}
}

// buildUsageResp assembles the full usage response: local task cost
// aggregation plus per-provider quota data from each registered fetcher.
func (s *Server) buildUsageResp(ctx context.Context) v1.UsageResp {
	local := v1conv.LocalUsage(s.taskMgr, time.Now())

	resp := v1.UsageResp{Local: local}
	detached := context.WithoutCancel(ctx)
	for _, f := range s.usageFetchers {
		if q := f.Get(detached); q != nil {
			out := v1conv.ProviderQuota(q)
			out.LogoURL = "/logos/" + out.Provider + ".svg"
			out.UsageURL = f.UsageURL()
			resp.Providers = append(resp.Providers, out)
		}
	}
	return resp
}

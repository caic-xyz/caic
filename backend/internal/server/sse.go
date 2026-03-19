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

	"github.com/caic-xyz/caic/backend/internal/server/dto"
	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
)

// handleTaskListEvents streams patch events for the task list as SSE. On first
// iteration it sends a full snapshot; thereafter it sends only upsert/delete
// events for changed or removed tasks. It pushes immediately when a
// server-handled mutation fires the changed channel, and falls back to a
// 2-second ticker to catch runner-internal state transitions.
func (s *Server) handleTaskListEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, dto.InternalError("streaming not supported"))
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
	go s.pollCIForActiveRepos(context.WithoutCancel(r.Context()))

	// prevByID tracks the last marshalled JSON for each task ID.
	prevByID := map[string][]byte{}
	var prevReposJSON []byte
	first := true

	for {
		s.mu.Lock()
		out := make([]v1.Task, 0, len(s.tasks))
		for _, e := range s.tasks {
			out = append(out, s.toJSON(e))
		}
		repos := s.reposLocked()
		ch := s.changed
		s.mu.Unlock()

		reposJSON, _ := json.Marshal(repos)

		if first {
			if err := emitTaskListEvent(w, flusher, v1.TaskListEvent{Kind: "snapshot", Tasks: out}); err != nil {
				slog.Warn("marshal task list snapshot", "err", err)
				return
			}
			if err := emitTaskListEvent(w, flusher, v1.TaskListEvent{Kind: "repos", Repos: *repos}); err != nil {
				slog.Warn("marshal repos snapshot", "err", err)
				return
			}
			for i := range out {
				data, _ := json.Marshal(&out[i])
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
						if err := emitTaskListEvent(w, flusher, v1.TaskListEvent{Kind: "upsert", Task: &out[i]}); err != nil {
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
					if err := emitTaskListEvent(w, flusher, v1.TaskListEvent{Kind: "delete", ID: id}); err != nil {
						slog.Warn("marshal task delete", "id", id, "err", err)
						return
					}
					delete(prevByID, id)
				}
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
			go s.pollCIForActiveRepos(context.WithoutCancel(r.Context()))
		}
	}
}

// handleUsageEvents streams usage snapshots as SSE. It reacts to task changes
// immediately and ticks every 5 minutes for window rollovers and OAuth cache
// refreshes. Each message is a single UsageResp JSON object.
func (s *Server) handleUsageEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, dto.InternalError("streaming not supported"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	ticker := time.NewTicker(usageCacheTTL)
	defer ticker.Stop()

	var prev []byte

	for {
		s.mu.Lock()
		resp := computeUsage(s.tasks, time.Now())
		ch := s.changed
		s.mu.Unlock()

		if s.usage != nil {
			if oauth := s.usage.get(); oauth != nil {
				resp.FiveHour.Utilization = oauth.FiveHour.Utilization
				resp.FiveHour.ResetsAt = oauth.FiveHour.ResetsAt
				resp.SevenDay.Utilization = oauth.SevenDay.Utilization
				resp.SevenDay.ResetsAt = oauth.SevenDay.ResetsAt
				resp.ExtraUsage = oauth.ExtraUsage
			}
		}

		data, err := json.Marshal(resp)
		if err == nil && !bytes.Equal(data, prev) {
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
			flusher.Flush()
			prev = data
		}

		select {
		case <-r.Context().Done():
			return
		case <-ch:
		case <-ticker.C:
		}
	}
}

// handleGetUsage returns a one-shot usage snapshot as JSON.
func (s *Server) handleGetUsage(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	resp := computeUsage(s.tasks, time.Now())
	s.mu.Unlock()

	if s.usage != nil {
		if oauth := s.usage.get(); oauth != nil {
			resp.FiveHour.Utilization = oauth.FiveHour.Utilization
			resp.FiveHour.ResetsAt = oauth.FiveHour.ResetsAt
			resp.SevenDay.Utilization = oauth.SevenDay.Utilization
			resp.SevenDay.ResetsAt = oauth.SevenDay.ResetsAt
			resp.ExtraUsage = oauth.ExtraUsage
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

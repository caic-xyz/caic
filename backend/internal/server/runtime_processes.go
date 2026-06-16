// HTTP handlers for runtime process inspection and signaling.

package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/api/v1conv"
	"github.com/caic-xyz/caic/backend/internal/tasks"
)

type runtimeProcessBackend interface {
	Processes(ctx context.Context, id runtime.InstanceID) ([]runtime.ProcessInfo, error)
	Signal(ctx context.Context, id runtime.InstanceID, pid int, sig string) error
}

// runtimeProcessHandlers handles task runtime process routes.
type runtimeProcessHandlers struct {
	taskMgr      *tasks.Manager
	backend      runtimeProcessBackend
	authEnabled  func() bool
	notifyChange func()
}

// HandleGetProcesses returns the list of running processes inside a task runtime instance.
func (h *runtimeProcessHandlers) HandleGetProcesses(w http.ResponseWriter, r *http.Request) {
	entry, err := h.getTask(r)
	if err != nil {
		writeError(w, err)
		return
	}
	t := entry.Task()
	instanceID := t.RuntimeInstanceID()
	if instanceID == "" {
		writeError(w, api.Conflict("task has no instance"))
		return
	}
	if h.backend == nil {
		writeError(w, api.InternalError("runtime backend not configured"))
		return
	}
	procs, err := h.backend.Processes(r.Context(), instanceID)
	if err != nil {
		writeError(w, api.InternalError(err.Error()))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v1.ProcessListResp{Processes: v1conv.ProcessInfos(procs)}); err != nil {
		slog.WarnContext(r.Context(), "encode process list response", "err", err)
	}
}

// HandleSignalProcess sends a signal to a process inside a task runtime instance.
func (h *runtimeProcessHandlers) HandleSignalProcess(w http.ResponseWriter, r *http.Request) {
	entry, err := h.getTask(r)
	if err != nil {
		writeError(w, err)
		return
	}
	req := &v1.SignalProcessReq{}
	if !readAndDecodeBody(w, r, req) {
		return
	}
	populatePathParams(r, req)
	if err := req.Validate(); err != nil {
		writeError(w, err)
		return
	}
	resp, err := h.signalProcess(r.Context(), entry, req)
	if err == nil && h.notifyChange != nil {
		h.notifyChange()
	}
	writeJSONResponse(w, resp, err)
}

func (h *runtimeProcessHandlers) signalProcess(ctx context.Context, entry *tasks.Entry, req *v1.SignalProcessReq) (*v1.StatusResp, error) {
	t := entry.Task()
	instanceID := t.RuntimeInstanceID()
	if instanceID == "" {
		return nil, api.Conflict("task has no instance")
	}
	if h.backend == nil {
		return nil, api.InternalError("runtime backend not configured")
	}
	if err := h.backend.Signal(ctx, instanceID, req.PID, req.Signal); err != nil {
		return nil, api.InternalError(err.Error())
	}
	slog.InfoContext(ctx, "signal sent", "task", t.ID, "instance", instanceID, "pid", req.PID, "signal", req.Signal)
	return &v1.StatusResp{Status: "signalled"}, nil
}

func (h *runtimeProcessHandlers) getTask(r *http.Request) (*tasks.Entry, error) {
	id := r.PathValue("id")
	entry, ok := h.taskMgr.GetEntry(id)
	if !ok {
		return nil, api.NotFound("task")
	}
	if h.authEnabled != nil && h.authEnabled() {
		if u, ok := auth.UserFromContext(r.Context()); ok {
			if owner := entry.Task().OwnerID; owner != "" && owner != u.ID {
				return nil, api.Forbidden("task")
			}
		}
	}
	return entry, nil
}

// routes returns the handler for task runtime process inspection and signaling.
// Patterns are relative to the /api/caic/v1 version prefix, stripped at mount
// time; {id} is the task ID.
func (h *runtimeProcessHandlers) routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /processes/{id}", h.HandleGetProcesses)
	m.HandleFunc("POST /processes/{id}/{pid}/signal", h.HandleSignalProcess)
	return m
}

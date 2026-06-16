// HTTP handlers for usage snapshot and SSE event stream.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/api/v1conv"
	"github.com/caic-xyz/caic/backend/internal/tasks"
	"github.com/caic-xyz/caic/backend/internal/usage"
)

type usageHandlers struct {
	taskMgr  *tasks.Manager
	fetchers []usage.ProviderFetcher
}

// handleEvents streams usage snapshots as SSE. It reacts to task changes
// immediately and ticks every CacheTTL for provider cache refreshes. Each
// message is a single UsageResp JSON object.
func (h *usageHandlers) handleEvents(w http.ResponseWriter, r *http.Request) {
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
		resp := h.buildResp(r.Context())

		data, err := json.Marshal(resp)
		if err == nil && !bytes.Equal(data, prev) {
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
			flusher.Flush()
			prev = data
		}

		ch := h.taskMgr.Changed()
		select {
		case <-r.Context().Done():
			return
		case <-ch:
		case <-ticker.C:
		}
	}
}

// handleGetUsage returns a one-shot usage snapshot as JSON.
func (h *usageHandlers) handleGetUsage(w http.ResponseWriter, r *http.Request) {
	resp := h.buildResp(r.Context())
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.WarnContext(r.Context(), "encode usage response", "err", err)
	}
}

// buildResp assembles the full usage response: local task cost
// aggregation plus per-provider quota data from each registered fetcher.
func (h *usageHandlers) buildResp(ctx context.Context) v1.UsageResp {
	local := v1conv.LocalUsage(h.taskMgr, time.Now())

	resp := v1.UsageResp{Local: local}
	detached := context.WithoutCancel(ctx)
	for _, f := range h.fetchers {
		if q := f.Get(detached); q != nil {
			out := v1conv.ProviderQuota(q)
			out.LogoURL = "/logos/" + out.Provider + ".svg"
			out.UsageURL = f.UsageURL()
			resp.Providers = append(resp.Providers, out)
		}
	}
	return resp
}

// routes returns the handler for usage quota snapshots and the usage SSE
// stream. Patterns are relative to the /api/caic/v1 version prefix, stripped at
// mount time.
func (h *usageHandlers) routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /usage", h.handleGetUsage)
	m.HandleFunc("GET /usage/events", h.handleEvents)
	return m
}

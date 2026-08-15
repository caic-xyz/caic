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

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/apiconv"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
	"github.com/caic-xyz/caic/backend/internal/usage"
)

type usageHandlers struct {
	log          *slog.Logger
	taskMgr      *taskmgr.Manager
	fetchers     []usage.ProviderFetcher
	quotaTracker *usage.Tracker
}

// handleEvents streams usage snapshots as SSE. It reacts to task changes
// immediately and ticks every CacheTTL for provider cache refreshes. Each
// message is a single UsageResp JSON object.
func (h *usageHandlers) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(r.Context(), w, api.InternalError("streaming not supported"))
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
		h.log.WarnContext(r.Context(), "encode usage response", "err", err)
	}
}

// buildResp assembles the full usage response: local task cost
// aggregation plus per-provider quota data from each registered fetcher.
func (h *usageHandlers) buildResp(ctx context.Context) v1.UsageResp {
	now := time.Now()
	local := localUsage(h.taskMgr, now)

	resp := v1.UsageResp{Local: local}
	detached := context.WithoutCancel(ctx)
	providerQuotas := make([]usage.ProviderQuota, 0, len(h.fetchers))
	usageURLs := make(map[agent.QuotaProvider]string, len(h.fetchers))
	for _, f := range h.fetchers {
		if q := f.Get(detached); q != nil {
			providerQuotas = append(providerQuotas, *q)
		}
		usageURLs[f.Provider()] = f.UsageURL()
	}
	mergedQuotas := h.quotaTracker.Merge(providerQuotas, now)
	for i := range mergedQuotas {
		out, err := apiconv.ProviderQuota(&mergedQuotas[i])
		if err != nil {
			h.log.ErrorContext(ctx, "convert provider quota", "provider", mergedQuotas[i].Provider, "err", err)
			continue
		}
		out.LogoURL = "/logos/" + string(out.Provider) + ".svg"
		out.UsageURL = usageURLs[agent.QuotaProvider(out.Provider)]
		resp.Providers = append(resp.Providers, out)
	}
	return resp
}

// localUsage resolves manager-backed task usage before converting it to an API DTO.
func localUsage(mgr *taskmgr.Manager, now time.Time) v1.LocalUsage {
	inputs := make([]apiconv.LocalUsageInput, 0)
	mgr.Range(func(_ string, e *taskmgr.Entry) bool {
		t := e.Task()
		input := apiconv.LocalUsageInput{StartedAt: t.StartedAt}
		if result := e.Result(); result != nil {
			input.CostUSD = result.CostUSD
			input.Usage = result.Usage
		} else {
			input.CostUSD, _, _, input.Usage, _ = t.LiveStats()
		}
		inputs = append(inputs, input)
		return true
	})
	return apiconv.LocalUsage(inputs, now)
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

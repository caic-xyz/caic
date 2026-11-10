// HTTP handlers for usage snapshot and SSE event stream.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/frontend"
	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/apiconv"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
	"github.com/caic-xyz/caic/backend/internal/usage"
	"github.com/caic-xyz/caic/backend/internal/usagedb"
	"github.com/maruel/gomode/sse"
	"github.com/maruel/ksid"
)

type usageHandlers struct {
	log          *slog.Logger
	taskMgr      *taskmgr.Manager
	rollup       *usagedb.Store
	fetchers     []usage.ProviderFetcher
	quotaTracker *usage.Tracker
}

// handleEvents streams usage snapshots as SSE. It reacts to task changes
// immediately and ticks every CacheTTL for provider cache refreshes. Each
// message is a single UsageResp JSON object.
func (h *usageHandlers) handleEvents(w http.ResponseWriter, r *http.Request) {
	stream := sse.New(w)
	if err := stream.Flush(); err != nil {
		writeError(r.Context(), w, &api.Error{Status: http.StatusInternalServerError, Code: api.CodeInternalError, Message: "streaming not supported"})
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	var streamMu sync.Mutex
	write := func(event string, data []byte) error {
		streamMu.Lock()
		defer streamMu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := stream.Writef("event: %s\ndata: %s\n\n", event, data); err != nil {
			return err
		}
		return stream.Flush()
	}
	// Provider refreshes can block; transport liveness must remain observable
	// independently of them. Serialize complete frames with the snapshot writer.
	workers.Go(func() {
		heartbeat := time.NewTicker(15 * time.Second)
		defer heartbeat.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-heartbeat.C:
				data, err := json.Marshal(v1.StreamHeartbeat{Time: time.Now().UTC()})
				if err != nil {
					h.log.WarnContext(ctx, "encode usage heartbeat", "err", err)
					cancel()
					return
				}
				if err := write("heartbeat", data); err != nil {
					h.log.WarnContext(ctx, "write usage heartbeat", "err", err)
					cancel()
					return
				}
			}
		}
	})
	ticker := time.NewTicker(usage.CacheTTL)
	defer ticker.Stop()

	var prev []byte

	for {
		// Subscribe before building and writing the snapshot so a quota update
		// during either operation closes this channel and triggers another pass.
		ch := h.taskMgr.Changed()
		resp := h.buildResp(r.Context())

		data, err := json.Marshal(resp)
		if err == nil && !bytes.Equal(data, prev) {
			if err := write("message", data); err != nil {
				h.log.WarnContext(r.Context(), "write usage SSE event", "err", err)
				return
			}
			prev = data
		}

		select {
		case <-ctx.Done():
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

// handleGetDashboard returns daily usage from the in-memory rollup snapshot.
// It intentionally never reads task logs: the rollup is the one cross-task
// aggregation surface.
func (h *usageHandlers) handleGetDashboard(w http.ResponseWriter, r *http.Request) {
	resp := apiconv.UsageDashboard(h.rollup.Days())
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.log.WarnContext(r.Context(), "encode usage dashboard response", "err", err)
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
		out, err := apiconv.ProviderQuota(&mergedQuotas[i], now)
		if err != nil {
			h.log.ErrorContext(ctx, "convert provider quota", "provider", mergedQuotas[i].Provider, "err", err)
			continue
		}
		if logo := frontend.LogoFile(string(out.Provider)); logo != "" {
			out.LogoURL = "/logos/" + logo
		}
		out.UsageURL = usageURLs[agent.QuotaProvider(out.Provider)]
		resp.Providers = append(resp.Providers, out)
	}
	return resp
}

// localUsage resolves manager-backed task usage before converting it to an API DTO.
func localUsage(mgr *taskmgr.Manager, now time.Time) v1.LocalUsage {
	inputs := make([]apiconv.LocalUsageInput, 0)
	mgr.Range(func(_ ksid.ID, e *taskmgr.Entry) bool {
		t := e.Task()
		input := apiconv.LocalUsageInput{StartedAt: t.StartedAt}
		if result := e.Result(); result != nil {
			input.CostUSD = result.CostUSD
			input.CostComplete = result.CostComplete
			input.Usage = result.Usage
		} else {
			snapshot := t.Snapshot()
			input.CostUSD = snapshot.CostUSD
			input.CostComplete = snapshot.CostComplete
			input.Usage = snapshot.Usage
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
	m.HandleFunc("GET /usage/dashboard", h.handleGetDashboard)
	m.HandleFunc("GET /usage/events", h.handleEvents)
	return m
}

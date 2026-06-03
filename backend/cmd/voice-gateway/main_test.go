// Tests for the standalone voice gateway command handlers.

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/voicegateway"
)

func TestGatewayHandler(t *testing.T) {
	t.Parallel()
	t.Run("health", func(t *testing.T) {
		t.Parallel()
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/health", http.NoBody)
		cfg := voicegateway.DefaultConfig()
		gatewayHandler(&cfg, nil).ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		var resp map[string]string
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp["status"] != "ok" {
			t.Fatalf("status field = %q, want ok", resp["status"])
		}
	})

	t.Run("compat", func(t *testing.T) {
		t.Parallel()
		cfg := voicegateway.DefaultConfig()
		cfg.Services = []voicegateway.ServiceConfig{
			{ID: "home-caic", Kind: "caic", BaseURL: "https://caic.example.com"},
		}
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/compat", http.NoBody)
		gatewayHandler(&cfg, nil).ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		var resp voicegateway.Compatibility
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp.Service != "voice-gateway" {
			t.Fatalf("Service = %q, want voice-gateway", resp.Service)
		}
		if len(resp.ServiceKinds) != 1 || resp.ServiceKinds[0] != "caic" {
			t.Fatalf("ServiceKinds = %v, want [caic]", resp.ServiceKinds)
		}
	})
}

func TestMainImpl(t *testing.T) {
	t.Parallel()
	t.Run("rejects disabled webrtc port", func(t *testing.T) {
		t.Parallel()
		err := mainImpl([]string{
			"-config", filepath.Join(t.TempDir(), "missing.toml"),
			"-udp-port", "-1",
		})
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

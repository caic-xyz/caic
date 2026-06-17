// Tests for Go Mode service compatibility settings handlers.

package gomode

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewHandler(t *testing.T) {
	t.Parallel()
	t.Run("nil settings", func(t *testing.T) {
		t.Parallel()
		if _, err := NewHandler(nil); err == nil {
			t.Fatal("NewHandler(nil) error = nil, want error")
		}
	})

	t.Run("settings", func(t *testing.T) {
		t.Parallel()
		settings := Settings{
			Service:        "caic",
			ServiceVersion: "v1.2.3",
			APIVersion:     1,
			WebShell: WebShellSettings{
				BridgeVersion: 1,
				ToolGroups: []ToolGroup{{
					Name:            "tasks",
					Endpoint:        "/api/caic/v1/mcp",
					ProtocolVersion: "2026-07-28",
					AuthRequired:    true,
				}},
				VoiceGateway: VoiceGatewaySettings{
					Required:     false,
					URL:          "https://voice.example.com",
					AuthRequired: true,
				},
			},
		}
		handler, err := NewHandler(&settings)
		if err != nil {
			t.Fatal(err)
		}
		settings.WebShell.VoiceGateway.URL = "https://mutated.example.com"

		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/.well-known/gomode.json", http.NoBody)
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		if got := w.Header().Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
		if got := w.Header().Get("Cache-Control"); got != "public, max-age=300" {
			t.Fatalf("Cache-Control = %q, want public, max-age=300", got)
		}
		if w.Header().Get("ETag") == "" {
			t.Fatal("ETag header missing")
		}
		var resp Settings
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp.Service != "caic" || resp.ServiceVersion != "v1.2.3" || resp.APIVersion != 1 {
			t.Fatalf("resp identity = %+v", resp)
		}
		if resp.WebShell.BridgeVersion != 1 {
			t.Fatalf("webShell = %+v", resp.WebShell)
		}
		if len(resp.WebShell.ToolGroups) != 1 {
			t.Fatalf("toolGroups = %+v, want 1", resp.WebShell.ToolGroups)
		}
		if grp := resp.WebShell.ToolGroups[0]; grp.Name != "tasks" || grp.Endpoint != "/api/caic/v1/mcp" || grp.ProtocolVersion != "2026-07-28" || !grp.AuthRequired {
			t.Fatalf("toolGroup = %+v", resp.WebShell.ToolGroups[0])
		}
		if resp.WebShell.VoiceGateway.URL != "https://voice.example.com" || !resp.WebShell.VoiceGateway.AuthRequired {
			t.Fatalf("voiceGateway = %+v", resp.WebShell.VoiceGateway)
		}
	})

	t.Run("not modified", func(t *testing.T) {
		t.Parallel()
		handler, err := NewHandler(&Settings{Service: "caic", APIVersion: 1})
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/.well-known/gomode.json", http.NoBody)
		handler.ServeHTTP(w, req)
		etag := w.Header().Get("ETag")
		if etag == "" {
			t.Fatal("ETag header missing")
		}

		w = httptest.NewRecorder()
		req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/.well-known/gomode.json", http.NoBody)
		req.Header.Set("If-None-Match", etag)
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusNotModified {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusNotModified)
		}
		if w.Body.Len() != 0 {
			t.Fatalf("body = %q, want empty", w.Body.String())
		}
	})

	t.Run("method not allowed", func(t *testing.T) {
		t.Parallel()
		settings := Settings{}
		handler, err := NewHandler(&settings)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/.well-known/gomode.json", http.NoBody)
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
		}
	})
}

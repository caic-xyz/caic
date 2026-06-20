// Tests for Go Mode discovery validation and settings handlers.

package gomode

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

	t.Run("invalid settings", func(t *testing.T) {
		t.Parallel()
		settings := validSettings()
		settings.Service = ""
		_, err := NewHandler(&settings)
		if err == nil || !strings.Contains(err.Error(), "service is required") {
			t.Fatalf("NewHandler() error = %v, want service validation error", err)
		}
	})

	t.Run("settings", func(t *testing.T) {
		t.Parallel()
		settings := validSettings()
		settings.WebShell.VoiceGateway = VoiceGatewaySettings{
			Required:     false,
			URL:          "https://voice.example.com",
			AuthRequired: true,
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
		settings := validSettings()
		handler, err := NewHandler(&settings)
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
		settings := validSettings()
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

func TestSettings(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		settings := validSettings()
		if err := settings.Validate(); err != nil {
			t.Fatalf("Validate() error = %v, want nil", err)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name   string
			mutate func(*Settings)
			want   string
		}{
			{
				name:   "missing service",
				mutate: func(s *Settings) { s.Service = " " },
				want:   "service is required",
			},
			{
				name:   "unsupported api version shape",
				mutate: func(s *Settings) { s.APIVersion = 0 },
				want:   "apiVersion must be positive",
			},
			{
				name:   "missing bridge version",
				mutate: func(s *Settings) { s.WebShell.BridgeVersion = 0 },
				want:   "webShell.bridgeVersion must be positive",
			},
			{
				name:   "missing tool groups field",
				mutate: func(s *Settings) { s.WebShell.ToolGroups = nil },
				want:   "webShell.toolGroups is required",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				settings := validSettings()
				tc.mutate(&settings)
				err := settings.Validate()
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("Validate() error = %v, want containing %q", err, tc.want)
				}
			})
		}
	})
}

func TestToolGroup(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		group := validToolGroup()
		group.SkillURL = "/.well-known/gomode/skills/tasks/SKILL.md"
		if err := group.Validate(); err != nil {
			t.Fatalf("Validate() error = %v, want nil", err)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name   string
			mutate func(*ToolGroup)
			want   string
		}{
			{
				name:   "missing name",
				mutate: func(g *ToolGroup) { g.Name = "" },
				want:   "toolGroup.name is required",
			},
			{
				name:   "missing endpoint",
				mutate: func(g *ToolGroup) { g.Endpoint = "" },
				want:   "toolGroup.endpoint is required",
			},
			{
				name:   "relative endpoint",
				mutate: func(g *ToolGroup) { g.Endpoint = "api/mcp" },
				want:   "toolGroup.endpoint must be an absolute URL or absolute path",
			},
			{
				name:   "missing protocol version",
				mutate: func(g *ToolGroup) { g.ProtocolVersion = "" },
				want:   "toolGroup.protocolVersion is required",
			},
			{
				name:   "bad skill url",
				mutate: func(g *ToolGroup) { g.SkillURL = "skills/tasks/SKILL.md" },
				want:   "toolGroup.skillUrl must be an absolute URL or absolute path",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				group := validToolGroup()
				tc.mutate(&group)
				err := group.Validate()
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("Validate() error = %v, want containing %q", err, tc.want)
				}
			})
		}
	})
}

func TestSkillFrontmatter(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		frontmatter := validSkillFrontmatter()
		if err := frontmatter.Validate(); err != nil {
			t.Fatalf("Validate() error = %v, want nil", err)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name   string
			mutate func(*SkillFrontmatter)
			want   string
		}{
			{
				name:   "missing name",
				mutate: func(f *SkillFrontmatter) { f.Name = "" },
				want:   "name is required",
			},
			{
				name: "bad physical position",
				mutate: func(f *SkillFrontmatter) {
					f.GoMode.Activation.Locations = []LocationActivation{{
						PhysicalPosition: PhysicalPosition{Name: "office", Latitude: 100, RadiusMeters: 50},
					}}
				},
				want: "gomode.activation.locations[0].physicalPosition.latitude must be between -90 and 90",
			},
			{
				name: "missing physical position name",
				mutate: func(f *SkillFrontmatter) {
					f.GoMode.Activation.Locations = []LocationActivation{{
						PhysicalPosition: PhysicalPosition{
							Latitude:     45.5017,
							Longitude:    -73.5673,
							RadiusMeters: 100,
						},
					}}
				},
				want: "gomode.activation.locations[0].physicalPosition.name is required",
			},
			{
				name:   "empty location activation",
				mutate: func(f *SkillFrontmatter) { f.GoMode.Activation.Locations = []LocationActivation{{}} },
				want:   "gomode.activation.locations[0] requires wifi.ssids or physicalPosition",
			},
			{
				name:   "missing mcp server",
				mutate: func(f *SkillFrontmatter) { f.GoMode.MCPServers = nil },
				want:   "gomode.mcpServers is required",
			},
			{
				name:   "missing tool allowlist",
				mutate: func(f *SkillFrontmatter) { f.GoMode.MCPServers[0].Tools = nil },
				want:   "gomode.mcpServers[0].tools is required",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				frontmatter := validSkillFrontmatter()
				tc.mutate(&frontmatter)
				err := frontmatter.Validate()
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("Validate() error = %v, want containing %q", err, tc.want)
				}
			})
		}
	})
}

func TestVoiceGatewaySettings(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		cases := []VoiceGatewaySettings{
			{},
			{URL: "/"},
			{URL: "https://voice.example.com", AuthRequired: true, TokenEndpoint: "/api/voice-token"},
			{Required: true, URL: "/"},
		}
		for _, settings := range cases {
			if err := settings.Validate(); err != nil {
				t.Fatalf("Validate() error = %v, want nil for %+v", err, settings)
			}
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name     string
			settings VoiceGatewaySettings
			want     string
		}{
			{
				name:     "required without url",
				settings: VoiceGatewaySettings{Required: true},
				want:     "voiceGateway.url is required",
			},
			{
				name:     "auth without url",
				settings: VoiceGatewaySettings{AuthRequired: true},
				want:     "voiceGateway.url is required when voiceGateway.authRequired is true",
			},
			{
				name:     "bad url",
				settings: VoiceGatewaySettings{URL: "voice.example.com"},
				want:     "voiceGateway.url must be an absolute URL or absolute path",
			},
			{
				name:     "bad token endpoint",
				settings: VoiceGatewaySettings{URL: "/", TokenEndpoint: "voice-token"},
				want:     "voiceGateway.tokenEndpoint must be an absolute URL or absolute path",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				err := tc.settings.Validate()
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("Validate() error = %v, want containing %q", err, tc.want)
				}
			})
		}
	})
}

func validSettings() Settings {
	return Settings{
		Service:        "caic",
		ServiceVersion: "v1.2.3",
		APIVersion:     1,
		WebShell: WebShellSettings{
			BridgeVersion: 1,
			ToolGroups:    []ToolGroup{validToolGroup()},
			VoiceGateway:  VoiceGatewaySettings{URL: "/"},
		},
	}
}

func validToolGroup() ToolGroup {
	return ToolGroup{
		Name:            "tasks",
		Description:     "Task management",
		Endpoint:        "/api/caic/v1/mcp",
		ProtocolVersion: "2026-07-28",
		AuthRequired:    true,
	}
}

func validSkillFrontmatter() SkillFrontmatter {
	return SkillFrontmatter{
		Name:        "tasks",
		Description: "Task management",
		GoMode: SkillGoMode{
			Activation: SkillActivation{Locations: []LocationActivation{
				{WiFi: LocationWiFi{SSIDs: []string{"Office-WiFi"}}},
				{PhysicalPosition: PhysicalPosition{
					Name:         "office",
					Latitude:     45.5017,
					Longitude:    -73.5673,
					RadiusMeters: 100,
				}},
			}},
			MCPServers: []SkillMCPServer{{
				Name:            "caic",
				Endpoint:        "/api/caic/v1/mcp",
				ProtocolVersion: "2026-07-28",
				AuthRequired:    true,
				Tools:           []string{"tasks_list"},
			}},
		},
	}
}

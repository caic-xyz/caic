// Tests account-role schemas, browser/OAuth authority, coordinated suspension, and OAuth grant invalidation.

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/gomodedev/gomode/mcp"
	"github.com/gomodedev/gomode/oauth"
	"github.com/gomodedev/gomode/oauth/oauthserver"
	"github.com/maruel/ksid"
)

func TestAccountService(t *testing.T) {
	t.Run("account roles for browser sessions and remote OAuth clients", func(t *testing.T) {
		t.Parallel()
		store, err := auth.Open(filepath.Join(t.TempDir(), "users.json"), "github:root")
		if err != nil {
			t.Fatal(err)
		}
		root, err := store.UpsertUser(&auth.User{Provider: auth.ProviderGitHub, ProviderID: "root", AccessToken: "never-expose-secret"})
		if err != nil {
			t.Fatal(err)
		}
		member, err := store.UpsertUser(&auth.User{Provider: auth.ProviderGitHub, ProviderID: "member"})
		if err != nil {
			t.Fatal(err)
		}
		s := newTestOAuthRouter(t, store)
		s.taskMgr.CompleteRestoration(nil)
		service := accountService{store: store, tasks: s.taskMgr}
		rootCtx := auth.NewContext(t.Context(), &root)
		post := func(t *testing.T, ctx context.Context, name, args string) bool {
			body := mcpRequestJSON("tools/call", fmt.Sprintf(`"name":%q,"arguments":%s`, name, args))
			request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(body))
			request.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
			request.Header.Set("Mcp-Method", "tools/call")
			request.Header.Set("Mcp-Name", name)
			response := httptest.NewRecorder()
			s.mcpHandlers.handleMCP(response, request)
			var result struct {
				Error  json.RawMessage `json:"error"`
				Result struct {
					IsError           bool            `json:"isError"`
					StructuredContent json.RawMessage `json:"structuredContent"`
				} `json:"result"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatalf("MCP response %d: %s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "never-expose-secret") {
				t.Fatal("account listing exposed a token")
			}
			return response.Code == http.StatusOK && len(result.Error) == 0 && !result.Result.IsError
		}
		t.Run("published role schemas", func(t *testing.T) {
			t.Parallel()
			registry, ok := s.mcpHandlers.protocol.Registry.(*mcpRegistry)
			if !ok {
				t.Fatalf("registry type %T", s.mcpHandlers.protocol.Registry)
			}
			specs := registry.specs()
			assignment := slices.IndexFunc(specs, func(spec mcp.ToolSpec) bool { return spec.Name == "account_set_role" })
			listing := slices.IndexFunc(specs, func(spec mcp.ToolSpec) bool { return spec.Name == "accounts_list" })
			if assignment < 0 || listing < 0 {
				t.Fatal("account tools missing")
			}
			input, ok := specs[assignment].InputSchema.Properties.Get("role")
			if !ok || !reflect.DeepEqual(input.Enum, []any{"admin", "member"}) {
				t.Fatalf("assignment input role: %+v", input)
			}
			output, ok := specs[assignment].OutputSchema.Properties.Get("role")
			if !ok || !reflect.DeepEqual(output.Enum, []any{"admin", "member"}) {
				t.Fatalf("assignment output role: %+v", output)
			}
			accounts, ok := specs[listing].OutputSchema.Properties.Get("accounts")
			if !ok || accounts.Items == nil {
				t.Fatal("account list schema missing items")
			}
			items := accounts.Items
			if name, referenced := strings.CutPrefix(items.Ref, "#/$defs/"); referenced {
				items = specs[listing].OutputSchema.Definitions[name]
			}
			if items == nil || items.Properties == nil {
				t.Fatal("account list schema missing item properties")
			}
			role, ok := items.Properties.Get("role")
			if !ok || !reflect.DeepEqual(role.Enum, []any{"admin", "member", "super_admin"}) {
				t.Fatalf("listed role: %+v", role)
			}
			provider, ok := items.Properties.Get("provider")
			if !ok || !reflect.DeepEqual(provider.Enum, []any{"github", "gitlab", "google"}) {
				t.Fatalf("listed provider: %+v", provider)
			}
		})

		rootCtx = newMCPPrincipalContext(rootCtx, &mcpPrincipal{Remote: true, Scopes: []string{mcpScopeServerAdmin}})
		if !post(t, rootCtx, "accounts_list", `{}`) {
			t.Fatal("super administrator cannot list accounts")
		}
		if !post(t, rootCtx, "account_set_role", fmt.Sprintf(`{"user_id":%q,"role":"admin"}`, member.ID)) {
			t.Fatal("super administrator cannot appoint administrator")
		}
		admin, _ := store.FindByID(member.ID)
		if admin.Role != auth.RoleAdmin {
			t.Fatal("MCP assignment was not persisted")
		}
		adminCtx := auth.NewContext(t.Context(), &admin)
		if !post(t, adminCtx, "accounts_list", `{}`) {
			t.Fatal("ordinary administrator cannot list accounts")
		}
		if post(t, adminCtx, "account_set_role", fmt.Sprintf(`{"user_id":%q,"role":"member"}`, root.ID)) {
			t.Fatal("ordinary administrator changed a role")
		}
		if _, err := service.setEnabled(adminCtx, root.ID, false); err == nil {
			t.Fatal("ordinary administrator disabled super administrator")
		}
		if post(t, rootCtx, "account_set_role", fmt.Sprintf(`{"user_id":%q,"role":"super_admin"}`, member.ID)) {
			t.Fatal("MCP assigned configuration role")
		}
		t.Run("remote OAuth client without server-admin scope is denied", func(t *testing.T) {
			t.Parallel()
			for _, scopes := range [][]string{nil, {mcpScopeRead}} {
				remote := newMCPPrincipalContext(rootCtx, &mcpPrincipal{Remote: true, Scopes: scopes})
				for _, call := range []struct{ name, args string }{
					{"account_set_enabled", fmt.Sprintf(`{"user_id":%q,"enabled":true}`, member.ID)},
					{"account_set_role", fmt.Sprintf(`{"user_id":%q,"role":"admin"}`, member.ID)},
					{"accounts_list", `{}`},
				} {
					if post(t, remote, call.name, call.args) {
						t.Fatalf("remote client scopes %v admitted %s", scopes, call.name)
					}
				}
			}
		})
		t.Run("authenticated browser session uses human authority without OAuth delegation", func(t *testing.T) { //nolint:paralleltest // Shared role assignments must finish before the later demotion.
			handler, err := s.buildHandler()
			if err != nil {
				t.Fatal(err)
			}
			for _, call := range []struct {
				user       *auth.User
				name, args string
			}{
				{&root, "account_set_role", fmt.Sprintf(`{"user_id":%q,"role":"admin"}`, member.ID)},
				{&admin, "accounts_list", `{}`},
			} {
				token, _, err := store.IssueToken(call.user, s.sessionSecret, time.Hour, auth.SessionInfo{})
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://caic.example.com/api/caic/v1/mcp", strings.NewReader(mcpRequestJSON("tools/call", fmt.Sprintf(`"name":%q,"arguments":%s`, call.name, call.args))))
				request.Header.Set("Origin", "https://caic.example.com")
				request.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
				request.Header.Set("Mcp-Method", "tools/call")
				request.Header.Set("Mcp-Name", call.name)
				request.AddCookie(&http.Cookie{Name: "caic_session", Value: token, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				var result struct {
					Error  json.RawMessage `json:"error"`
					Result struct {
						IsError bool `json:"isError"`
					} `json:"result"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
					t.Fatalf("browser response %d: %s", response.Code, response.Body.String())
				}
				if response.Code != http.StatusOK || len(result.Error) != 0 || result.Result.IsError {
					t.Fatalf("browser session denied %s: %d %s", call.name, response.Code, response.Body.String())
				}
			}
		})

		taskCtx := newMCPPrincipalContext(rootCtx, &mcpPrincipal{TaskID: ksid.NewID()})
		for _, name := range []string{"account_set_enabled", "account_set_role", "accounts_list"} {
			if _, allowed := authorizeToolScope(taskCtx, name); allowed {
				t.Fatalf("task inherited human authority for %s", name)
			}
		}
		if _, err := service.setRole(taskCtx, member.ID, "member"); err == nil {
			t.Fatal("task principal changed roles")
		}
		if _, err := service.setRole(rootCtx, member.ID, "member"); err != nil {
			t.Fatal(err)
		}
		if _, err := service.list(adminCtx); err == nil {
			t.Fatal("demoted administrator retained cached authority")
		}
		if post(t, adminCtx, "accounts_list", `{}`) {
			t.Fatal("demoted administrator retained MCP listing")
		}
	})

	t.Run("pending authorization cannot survive re-enable", func(t *testing.T) {
		t.Parallel()
		s, h, user, client := newMCPOAuthLifecycleRouter(t)
		form := authorizeMCPClientCode(t, s.authStore, h, &user, &client, "https://claude.ai/api/mcp/auth_callback", []string{mcpScopeRead})
		admin, err := s.authStore.UpsertUser(&auth.User{Provider: auth.ProviderGitHub, ProviderID: "admin"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.authStore.SetRole(admin.ID, auth.RoleAdmin); err != nil {
			t.Fatal(err)
		}
		s.taskMgr.CompleteRestoration(nil)
		service := accountService{store: s.authStore, tasks: s.taskMgr, oauth: s.oauthServer}
		ctx := auth.NewContext(t.Context(), &admin)
		if _, err := service.setEnabled(ctx, user.ID, false); err != nil {
			t.Fatal(err)
		}
		if _, err := service.setEnabled(ctx, user.ID, true); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
		req.Host = "caic.example.com"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		h.ServeHTTP(response, req)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("old authorization code minted authority: %d %s", response.Code, response.Body.String())
		}
		adapter := caicSessionManager{store: s.authStore}
		fresh, _ := s.authStore.ActiveUser(user.ID)
		attached := adapter.AttachUser(auth.NewContext(t.Context(), &fresh), oauth.User{ID: user.ID})
		if _, ok := auth.UserFromContext(attached); ok {
			t.Error("retired bearer authority fell through to cookie identity")
		}
		published := false
		if err := adapter.WithUserAuthority(oauth.User{ID: user.ID}, func() error { published = true; return nil }); !errors.Is(err, oauthserver.ErrUserAuthority) || published {
			t.Fatalf("late old-generation OAuth publication = %v, published=%v", err, published)
		}
	})

	t.Run("Streams", func(t *testing.T) {
		t.Parallel()
		for _, version := range []string{mcp.ProtocolVersion, "2025-11-25"} {
			t.Run(version, func(t *testing.T) {
				t.Parallel()
				s, handler, user, client := newMCPOAuthLifecycleRouter(t)
				token := authorizeMCPClient(t, s.authStore, handler, &user, &client)
				server := httptest.NewServer(handler)
				t.Cleanup(server.Close)
				var session string
				request := func(method, body string) *http.Request {
					req, err := http.NewRequestWithContext(t.Context(), method, server.URL+"/api/caic/v1/mcp", strings.NewReader(body))
					if err != nil {
						t.Fatal(err)
					}
					req.Host = "caic.example.com"
					req.Header.Set("Authorization", "Bearer "+token.AccessToken)
					req.Header.Set("Mcp-Protocol-Version", version)
					if session != "" {
						req.Header.Set("Mcp-Session-Id", session)
					}
					return req
				}
				var req *http.Request
				if version == mcp.ProtocolVersion {
					req = request(http.MethodPost, mcpRequestJSON("subscriptions/listen", `"notifications":{}`))
					req.Header.Set("Mcp-Method", "subscriptions/listen")
				} else {
					init := request(http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+version+`","clientInfo":{"name":"disable-test","version":"1"}}}`)
					response, err := server.Client().Do(init)
					if err != nil {
						t.Fatal(err)
					}
					session = response.Header.Get("Mcp-Session-Id")
					if err := response.Body.Close(); err != nil {
						t.Fatal(err)
					}
					if session == "" {
						t.Fatal("initialize omitted session")
					}
					req = request(http.MethodGet, "")
				}
				req.Header.Set("Accept", "text/event-stream")
				response, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = response.Body.Close() })
				if response.StatusCode != http.StatusOK {
					t.Fatal(response.StatusCode)
				}
				done := make(chan struct{})
				var workers sync.WaitGroup
				workers.Go(func() { _, _ = io.Copy(io.Discard, response.Body); close(done) })
				t.Cleanup(func() { _ = response.Body.Close(); workers.Wait() })
				if err := s.authStore.SetDisabled(user.ID, true); err != nil {
					t.Fatal(err)
				}
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("account stream survived disablement")
				}
			})
		}
	})
	t.Parallel()
	t.Run("suspension failure and retry", func(t *testing.T) {
		t.Parallel()
		store, err := auth.Open(filepath.Join(t.TempDir(), "users.json"), "")
		if err != nil {
			t.Fatal(err)
		}
		admin, err := store.UpsertUser(&auth.User{Provider: auth.ProviderGitHub, ProviderID: "admin"})
		if err != nil {
			t.Fatal(err)
		}
		target, err := store.UpsertUser(&auth.User{Provider: auth.ProviderGitHub, ProviderID: "target"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SetRole(admin.ID, auth.RoleAdmin); err != nil {
			t.Fatal(err)
		}
		manager := newTestRouterWithAuthHost(t, store, "", nil).taskMgr
		svc := accountService{store: store, tasks: manager}
		ctx := auth.NewContext(t.Context(), &admin)
		manager.CompleteRestoration(errors.New("runtime inventory unavailable"))
		if _, err := svc.setEnabled(ctx, target.ID, false); err == nil {
			t.Fatal("unconfirmed suspension reported success")
		}
		current, _ := store.FindByID(target.ID)
		if !current.Disabled {
			t.Fatal("failure removed durable fence")
		}
		if _, err := svc.setEnabled(ctx, target.ID, true); err == nil {
			t.Fatal("enabled before suspension confirmed")
		}
		manager.CompleteRestoration(nil)
		out, err := svc.setEnabled(ctx, target.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		if out.Enabled || !out.Suspended {
			t.Fatalf("retry output: %+v", out)
		}
		out, err = svc.setEnabled(ctx, target.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		if !out.Enabled || !out.Suspended {
			t.Fatalf("enable output: %+v", out)
		}
		if _, err := svc.setEnabled(auth.NewContext(t.Context(), &target), admin.ID, false); err == nil {
			t.Fatal("member administered accounts")
		}
		if err := store.SetRole(admin.ID, auth.RoleMember); err == nil {
			t.Fatal("demoted last administrator")
		}
		second, err := store.UpsertUser(&auth.User{Provider: auth.ProviderGitHub, ProviderID: "second"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SetRole(second.ID, auth.RoleAdmin); err != nil {
			t.Fatal(err)
		}
		if err := store.SetRole(admin.ID, auth.RoleMember); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.setEnabled(ctx, target.ID, false); err == nil {
			t.Fatal("stale context retained administrator role")
		}
	})
	t.Run("OAuth grants stay revoked after re-enable", func(t *testing.T) {
		t.Parallel()
		s, h, target, client := newMCPOAuthLifecycleRouter(t)
		token := authorizeMCPClient(t, s.authStore, h, &target, &client)
		admin, err := s.authStore.UpsertUser(&auth.User{Provider: auth.ProviderGitHub, ProviderID: "admin"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.authStore.SetRole(admin.ID, auth.RoleAdmin); err != nil {
			t.Fatal(err)
		}
		s.taskMgr.CompleteRestoration(nil)
		svc := accountService{store: s.authStore, tasks: s.taskMgr, oauth: s.oauthServer}
		ctx := auth.NewContext(t.Context(), &admin)
		// A durable grant write failure cannot make a revoked account usable.
		path := s.oauthRefreshTokenPath
		if err := os.Rename(path, path+".saved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.setEnabled(ctx, target.ID, false); err == nil {
			t.Fatal("grant save failure reported suspension complete")
		}
		if _, err := svc.setEnabled(ctx, target.ID, true); err == nil {
			t.Fatal("grant save failure allowed re-enable")
		}
		current, _ := s.authStore.FindByID(target.ID)
		if !current.Disabled {
			t.Fatal("grant failure removed account fence")
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path+".saved", path); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.setEnabled(ctx, target.ID, false); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.setEnabled(ctx, target.ID, true); err != nil {
			t.Fatal(err)
		}
		refreshMCPToken(t, h, client.ClientID, token.RefreshToken, http.StatusBadRequest)
		stale := auth.NewContext(t.Context(), &target)
		session := caicSessionManager{store: s.authStore}
		if _, ok := session.CurrentUser(stale); ok {
			t.Fatal("stale authorize context regained authority")
		}
	})
}

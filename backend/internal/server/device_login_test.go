// Tests host policy, middleware, audit persistence, CSRF, and normal-session device pairing.

package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/server/ipgeo"
	"github.com/maruel/gomode/devicelogin"
	"github.com/maruel/gomode/oauth/oauthclient"
)

//nolint:paralleltest // Virtual polling must isolate KSID's process-wide clock.
func TestDeviceLogin(t *testing.T) {
	// KSID's process-wide generator must not interleave real and frozen clocks.
	// Keep host fixtures and background workers outside each bubble, then align
	// virtual time with the existing approver session before advancing the poll.
	afterPollInterval := func(t *testing.T, f func(*testing.T)) {
		now := time.Now()
		synctest.Test(t, func(t *testing.T) {
			time.Sleep(time.Until(now) + 5*time.Second)
			f(t)
		})
	}
	setup := func(t *testing.T) (*testRouter, http.Handler, auth.User, string, string) {
		dir := t.TempDir()
		store, err := auth.Open(filepath.Join(dir, "users.json"))
		if err != nil {
			t.Fatal(err)
		}
		user, err := store.UpsertUser(&auth.User{Provider: auth.Provider("github"), ProviderID: "42", Username: "Approver"})
		if err != nil {
			t.Fatal(err)
		}
		s := newTestOAuthRouter(t, store)
		s.authHandlers.sessionSecret = s.sessionSecret
		s.authHandlers.githubOAuth = oauthclient.NewGitHubConfig("id", "secret", func(*http.Request) string { return "https://caic.example.com/auth/github/callback" })
		s.authHandlers.githubAllowedUsers = []string{"approver"}
		path := filepath.Join(dir, "audit.jsonl")
		s.deviceLogin = devicelogin.New(&deviceLoginHost{auth: s.authHandlers, audit: &auditStore{log: testLogger(), path: path}})
		s.voiceGateway.Mode = VoiceGatewayModeExternal
		s.voiceGateway.Issuer = "https://caic.example.com"
		s.voiceGateway.InstanceID = "caic-test"
		s.voiceGateway.SigningKey = ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
		h, err := s.buildHandler()
		if err != nil {
			t.Fatal(err)
		}
		credential, _, err := store.IssueToken(&user, s.sessionSecret, time.Hour, auth.SessionInfo{Source: "test"})
		if err != nil {
			t.Fatal(err)
		}
		return s, h, user, path, credential
	}
	post := func(t *testing.T, h http.Handler, path, body string, credential, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://caic.example.com"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.RemoteAddr = "192.0.2.4:1234"
		if credential != "" {
			r.Header.Set("Authorization", "Bearer "+credential)
		}
		if token != "" {
			r.Header.Set("X-Device-Login-Csrf", token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	start := func(t *testing.T, h http.Handler) devicelogin.StartResponse {
		w := post(t, h, "/auth/device/start", "{}", "", "")
		if w.Code != 200 {
			t.Fatalf("start %d: %s", w.Code, w.Body.String())
		}
		var out devicelogin.StartResponse
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	lookup := func(t *testing.T, h http.Handler, credential, code string) devicelogin.LookupResponse {
		w := post(t, h, "/auth/device/lookup", `{"user_code":"`+code+`"}`, credential, "")
		if w.Code != 200 {
			t.Fatalf("lookup %d: %s", w.Code, w.Body.String())
		}
		var out devicelogin.LookupResponse
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	t.Run("completion persistence", func(t *testing.T) {
		for _, failure := range []string{"none", "before creation", "after creation", "created audit", "response audit", "write", "concurrent", "cancel"} {
			t.Run(failure, func(t *testing.T) {
				s, _, user, path, credential := setup(t)
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				host := &deviceCompletionHost{Host: &deviceLoginHost{auth: s.authHandlers, audit: &auditStore{log: testLogger(), path: path}}, failure: failure, cancel: cancel, ip: netip.MustParseAddr("192.0.2.4")}
				core := devicelogin.New(host)
				s.deviceLogin = core
				mux, err := s.buildHandler()
				if err != nil {
					t.Fatal(err)
				}
				code := start(t, mux)
				detail := lookup(t, mux, credential, code.UserCode)
				if w := post(t, mux, "/auth/device/approve", `{"user_code":"`+code.UserCode+`","approve":true}`, credential, detail.CSRFToken); w.Code != 200 {
					t.Fatal(w.Body.String())
				}
				afterPollInterval(t, func(t *testing.T) {
					poll := func() {
						r := httptest.NewRequestWithContext(ctx, http.MethodPost, "https://caic.example.com/auth/device/poll", strings.NewReader(`{"device_code":"`+code.DeviceCode+`"}`))
						r.Header.Set("Content-Type", "application/json")
						r.Header.Set("Accept-Encoding", "gzip")
						r.RemoteAddr = "192.0.2.4:1234"
						w := http.ResponseWriter(httptest.NewRecorder())
						if failure == "write" {
							w = &deviceFailedWriter{w}
						}
						mux.ServeHTTP(w, r)
					}
					if failure == "concurrent" {
						var wg sync.WaitGroup
						for range 12 {
							wg.Go(poll)
						}
						wg.Wait()
					} else {
						poll()
					}
					restarted, err := auth.Open(filepath.Join(filepath.Dir(path), "users.json"))
					if err != nil {
						t.Fatal(err)
					}
					active := slices.DeleteFunc(restarted.Sessions(user.ID), func(p auth.Session) bool { return p.Source != "device" })
					if failure == "none" || failure == "concurrent" {
						if len(active) != 1 || host.created.Load() != 1 {
							t.Fatalf("active %d created %d", len(active), host.created.Load())
						}
					} else if len(active) != 0 {
						t.Fatalf("failed completion left %d active sessions", len(active))
					}
					if failure == "before creation" {
						if host.created.Load() != 0 {
							t.Fatal("creation attempted after prerequisite failed")
						}
					} else if host.created.Load() != 1 {
						t.Fatalf("created %d sessions", host.created.Load())
					}
					before := host.created.Load()
					poll()
					if host.created.Load() != before {
						t.Fatal("retry duplicated persistent session")
					}
					events := readAuditEvents(t, path)
					for _, event := range events {
						if !strings.Contains(event.Args, code.RequestID) {
							t.Fatal("missing correlation")
						}
						if strings.Contains(event.Args, code.DeviceCode) || strings.Contains(event.Args, code.UserCode) {
							t.Fatal("secret in audit")
						}
					}
				})
			})
		}
	})
	t.Run("normal cookie and single use", func(t *testing.T) {
		s, h, user, path, credential := setup(t)
		c := start(t, h)
		d := lookup(t, h, credential, c.UserCode)
		decision := `{"user_code":"` + c.UserCode + `","approve":true}`
		if w := post(t, h, "/auth/device/approve", decision, credential, ""); w.Code != 403 {
			t.Fatalf("missing CSRF: %d", w.Code)
		}
		if w := post(t, h, "/auth/device/approve", decision, credential, d.CSRFToken); w.Code != 200 {
			t.Fatalf("approve: %d %s", w.Code, w.Body.String())
		}
		poll := `{"device_code":"` + c.DeviceCode + `"}`
		afterPollInterval(t, func(t *testing.T) {
			w := post(t, h, "/auth/device/poll", poll, "", "")
			if w.Code != 200 {
				t.Fatalf("poll: %d %s", w.Code, w.Body.String())
			}
			cookies := w.Result().Cookies()
			if len(cookies) != 1 {
				t.Fatalf("cookies: %v", cookies)
			}
			cookie := cookies[0]
			if cookie.Name != "caic_session" || cookie.MaxAge < 2591995 || cookie.MaxAge > 2592000 || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
				t.Fatalf("cookie: %v", cookie)
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://caic.example.com/auth/me", nil)
			req.AddCookie(cookie)
			me := httptest.NewRecorder()
			h.ServeHTTP(me, req)
			if me.Code != 200 || !strings.Contains(me.Body.String(), user.ID) {
				t.Fatalf("normal identity %d %s", me.Code, me.Body.String())
			}
			records := slices.DeleteFunc(s.authStore.Sessions(user.ID), func(s auth.Session) bool { return s.Source != "device" })
			if len(records) != 1 || time.Until(records[0].ExpiresAt) > sessionTTL || time.Until(records[0].ExpiresAt) < sessionTTL-time.Minute {
				t.Fatalf("normal session lifetime: %+v", records)
			}
			if !cookie.Expires.Equal(records[0].ExpiresAt) {
				t.Fatalf("cookie expires at %v, persisted session at %v", cookie.Expires, records[0].ExpiresAt)
			}
			for _, bearer := range []bool{false, true} {
				for _, tc := range []struct {
					method, path, body string
					status             int
				}{
					{http.MethodPost, "/api/caic/v1/tasks", "{}", http.StatusBadRequest},
					{http.MethodGet, "/api/caic/v1/voice/token", "", http.StatusOK},
					{http.MethodGet, "/oauth/authorize", "", http.StatusBadRequest},
				} {
					req := httptest.NewRequestWithContext(t.Context(), tc.method, "https://caic.example.com"+tc.path, strings.NewReader(tc.body))
					req.Header.Set("Content-Type", "application/json")
					if bearer {
						req.Header.Set("Authorization", "Bearer "+cookie.Value)
					} else {
						req.AddCookie(cookie)
					}
					response := httptest.NewRecorder()
					h.ServeHTTP(response, req)
					if response.Code != tc.status {
						t.Errorf("bearer=%v %s: %d %s", bearer, tc.path, response.Code, response.Body.String())
					}
				}
			}
			// The normal cookie may authorize an OAuth client; MCP grant scopes remain separate.
			registered := post(t, h, "/oauth/register", `{"client_name":"Device parity test","redirect_uris":["https://client.example/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code"]}`, "", "")
			if registered.Code != http.StatusCreated {
				t.Fatalf("register OAuth client: %d %s", registered.Code, registered.Body.String())
			}
			var client struct {
				ClientID string `json:"client_id"`
			}
			if err := json.Unmarshal(registered.Body.Bytes(), &client); err != nil {
				t.Fatal(err)
			}
			query := url.Values{"response_type": {"code"}, "client_id": {client.ClientID}, "redirect_uri": {"https://client.example/callback"}, "code_challenge": {strings.Repeat("A", 43)}, "code_challenge_method": {"S256"}, "resource": {"https://caic.example.com/api/caic/v1/mcp"}, "scope": {mcpAuthDefaultScope}}
			consentRequest := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://caic.example.com/oauth/authorize?"+query.Encode(), http.NoBody)
			consentRequest.AddCookie(cookie)
			consentPage := httptest.NewRecorder()
			h.ServeHTTP(consentPage, consentRequest)
			_, suffix, found := strings.Cut(consentPage.Body.String(), `name="consent_token" value="`)
			consent, _, closed := strings.Cut(suffix, `"`)
			if consentPage.Code != 200 || !found || !closed || consent == "" {
				t.Fatalf("device OAuth consent: %d %s", consentPage.Code, consentPage.Body.String())
			}
			consentForm := url.Values{"consent_token": {consent}, "scope_form": {"1"}, "scope": strings.Fields(mcpAuthDefaultScope)}
			decisionRequest := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://caic.example.com/oauth/authorize", strings.NewReader(consentForm.Encode()))
			decisionRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			decisionRequest.AddCookie(cookie)
			oauthDecision := httptest.NewRecorder()
			h.ServeHTTP(oauthDecision, decisionRequest)
			location, err := url.Parse(oauthDecision.Header().Get("Location"))
			if err != nil || oauthDecision.Code != http.StatusSeeOther || location.Query().Get("code") == "" {
				t.Fatalf("device OAuth approval: %d %s", oauthDecision.Code, oauthDecision.Body.String())
			}
			delegated := start(t, h)
			proof := lookup(t, h, cookie.Value, delegated.UserCode)
			if response := post(t, h, "/auth/device/approve", `{"user_code":"`+delegated.UserCode+`","approve":true}`, cookie.Value, proof.CSRFToken); response.Code != 200 {
				t.Fatalf("delegate approval: %d %s", response.Code, response.Body.String())
			}
			if w := post(t, h, "/auth/device/poll", poll, "", ""); w.Code == 200 {
				t.Fatal("approval reused")
			}
			events := readAuditEvents(t, path)
			if len(events) != 5 || events[0].UserID != user.ID || events[0].Decision != "allow" {
				t.Fatalf("audit: %+v", events)
			}
		})
	})
	t.Run("same-account session cannot reuse decision proof", func(t *testing.T) {
		s, h, user, _, credential := setup(t)
		c := start(t, h)
		d := lookup(t, h, credential, c.UserCode)
		replacement, _, err := s.authHandlers.store.IssueToken(&user, s.sessionSecret, time.Hour, auth.SessionInfo{Source: "test"})
		if err != nil {
			t.Fatal(err)
		}
		body := `{"user_code":"` + c.UserCode + `","approve":true}`
		if w := post(t, h, "/auth/device/approve", body, replacement, d.CSRFToken); w.Code != 403 {
			t.Fatalf("stale decision proof: %d %s", w.Code, w.Body.String())
		}
		fresh := lookup(t, h, replacement, c.UserCode)
		if w := post(t, h, "/auth/device/approve", body, replacement, fresh.CSRFToken); w.Code != 200 {
			t.Fatalf("fresh decision proof: %d %s", w.Code, w.Body.String())
		}
	})
	t.Run("denial audited", func(t *testing.T) {
		_, h, _, path, credential := setup(t)
		c := start(t, h)
		d := lookup(t, h, credential, c.UserCode)
		w := post(t, h, "/auth/device/approve", `{"user_code":"`+c.UserCode+`","approve":false}`, credential, d.CSRFToken)
		if w.Code != 200 {
			t.Fatalf("deny %d", w.Code)
		}
		afterPollInterval(t, func(t *testing.T) {
			w = post(t, h, "/auth/device/poll", `{"device_code":"`+c.DeviceCode+`"}`, "", "")
			if !strings.Contains(w.Body.String(), "access_denied") || len(w.Result().Cookies()) != 0 {
				t.Fatalf("denied poll %d %s", w.Code, w.Body.String())
			}
			events := readAuditEvents(t, path)
			if len(events) != 1 || events[0].Decision != "deny" {
				t.Fatalf("audit %+v", events)
			}
		})
	})
	t.Run("policy rechecked before issuance", func(t *testing.T) {
		s, h, _, _, credential := setup(t)
		c := start(t, h)
		d := lookup(t, h, credential, c.UserCode)
		w := post(t, h, "/auth/device/approve", `{"user_code":"`+c.UserCode+`","approve":true}`, credential, d.CSRFToken)
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		s.authHandlers.githubAllowedUsers = []string{"other"}
		afterPollInterval(t, func(t *testing.T) {
			w = post(t, h, "/auth/device/poll", `{"device_code":"`+c.DeviceCode+`"}`, "", "")
			if w.Code != 403 || len(w.Result().Cookies()) != 0 {
				t.Fatalf("policy %d %s", w.Code, w.Body.String())
			}
		})
	})
	t.Run("eligibility and authentication", func(t *testing.T) {
		s, h, user, _, credential := setup(t)
		c := start(t, h)
		body := `{"user_code":"` + c.UserCode + `"}`
		if w := post(t, h, "/auth/device/lookup", body, "", ""); w.Code != 401 {
			t.Fatalf("anonymous lookup %d", w.Code)
		}
		s.authHandlers.githubAllowedUsers = []string{"other"}
		if w := post(t, h, "/auth/device/lookup", body, credential, ""); w.Code != 403 {
			t.Fatalf("ineligible lookup %d", w.Code)
		}
		s.authHandlers.githubAllowedUsers = nil
		s.authHandlers.githubOAuth = nil
		if err := (&deviceLoginHost{auth: s.authHandlers}).Eligible(t.Context(), user.ID+"\x00session"); err == nil {
			t.Fatal("removed provider eligible")
		}
	})
	t.Run("session creation rechecks eligibility", func(t *testing.T) {
		s, _, user, _, _ := setup(t)
		host := &deviceLoginHost{auth: s.authHandlers}
		subject := user.ID + "\x00session"
		if err := host.Eligible(t.Context(), subject); err != nil {
			t.Fatal(err)
		}
		s.authHandlers.githubAllowedUsers = []string{"other"}
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://caic.example.com/auth/device/poll", nil)
		if session, err := host.CreateSession(r, subject); err == nil || session != nil {
			t.Fatalf("session created = %v, error = %v", session != nil, err)
		}
		if got := len(s.authStore.Sessions(user.ID)); got != 1 {
			t.Fatalf("session count = %d, want original session only", got)
		}
	})
	t.Run("audit failure fails closed", func(t *testing.T) {
		_, h, _, path, credential := setup(t)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		c := start(t, h)
		d := lookup(t, h, credential, c.UserCode)
		w := post(t, h, "/auth/device/approve", `{"user_code":"`+c.UserCode+`","approve":true}`, credential, d.CSRFToken)
		if w.Code != 500 {
			t.Fatalf("audit failure %d %s", w.Code, w.Body.String())
		}
		afterPollInterval(t, func(t *testing.T) {
			w = post(t, h, "/auth/device/poll", `{"device_code":"`+c.DeviceCode+`"}`, "", "")
			if !strings.Contains(w.Body.String(), "authorization_pending") {
				t.Fatalf("audit issued session: %s", w.Body.String())
			}
		})
	})
	t.Run("off without provider", func(t *testing.T) {
		s, h, _, _, _ := setup(t)
		s.authHandlers.githubOAuth = nil
		if w := post(t, h, "/auth/device/start", "{}", "", ""); w.Code != 404 {
			t.Fatalf("off %d", w.Code)
		}
	})
	t.Run("host protection retained", func(t *testing.T) {
		_, h, _, _, _ := setup(t)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://evil.example/auth/device/start", strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 403 {
			t.Fatalf("host protection %d %s", w.Code, w.Body.String())
		}
	})
	t.Run("off without authentication", func(t *testing.T) {
		s := newTestRouter(t, nil)
		h, err := s.buildHandler()
		if err != nil {
			t.Fatal(err)
		}
		if w := post(t, h, "/auth/device/start", "{}", "", ""); w.Code != 404 {
			t.Fatalf("no-auth pairing %d", w.Code)
		}
	})
	t.Run("IP origin protection retained", func(t *testing.T) {
		s, h, _, _, _ := setup(t)
		checker, err := ipgeo.NewChecker(t.Context(), testLogger(), "127.0.0.0/8", "", "")
		if err != nil {
			t.Fatal(err)
		}
		s.ipgeoChecker = checker
		if w := post(t, h, "/auth/device/start", "{}", "", ""); w.Code != 403 {
			t.Fatalf("IP protection %d", w.Code)
		}
	})
	t.Run("persistent audit path required", func(t *testing.T) {
		a := &auditStore{log: testLogger()}
		if err := a.recordDeviceLogin(t.Context(), &devicelogin.AuditEvent{Subject: "user", Approved: true}); err == nil {
			t.Fatal("memory-only audit accepted")
		}
		if len(a.snapshot()) != 0 {
			t.Fatal("unpersisted event recorded as success")
		}
	})
}

type deviceFailedWriter struct{ http.ResponseWriter }

func (w *deviceFailedWriter) Write([]byte) (int, error) { return 0, errors.New("connection lost") }

// deviceCompletionHost exercises failures around the real host's durable issuer.
type deviceCompletionHost struct {
	devicelogin.Host

	failure string
	cancel  context.CancelFunc
	ip      netip.Addr
	created atomic.Int32
}

func (h *deviceCompletionHost) ClientIP(*http.Request) netip.Addr { return h.ip }
func (h *deviceCompletionHost) CreateSession(r *http.Request, subject string) (devicelogin.Session, error) {
	if h.failure == "before creation" {
		return nil, errors.New("creation unavailable")
	}
	session, err := h.Host.CreateSession(r, subject)
	if session != nil {
		h.created.Add(1)
	}
	if err != nil {
		return session, err
	}
	if h.failure == "after creation" {
		return session, errors.New("post-creation failure")
	}
	if h.failure == "cancel" {
		h.cancel()
	}
	return session, nil
}
func (h *deviceCompletionHost) Audit(ctx context.Context, e *devicelogin.AuditEvent) error {
	if h.failure == "created audit" && e.Kind == devicelogin.AuditSessionCreated || h.failure == "response audit" && e.Kind == devicelogin.AuditResponseWritten {
		return errors.New("audit unavailable")
	}
	return h.Host.Audit(ctx, e)
}

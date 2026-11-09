// Tests MCP HTTP routing, protocol discovery, tool dispatch, and subscription delivery.

package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/server/mcpschema"

	"github.com/maruel/ksid"

	"github.com/maruel/gomode/mcp"
	"github.com/maruel/gomode/oauth"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/metrics"
)

type notificationReadRegistry struct {
	*mcpRegistry

	reads atomic.Int64
}

func (r *notificationReadRegistry) ReadResource(ctx context.Context, p *mcp.ResourcesReadParams) (mcp.ResourcesReadResult, error) {
	if p.URI == "gomode://notifications" {
		r.reads.Add(1)
	}
	return r.mcpRegistry.ReadResource(ctx, p)
}

func TestMCPDetailOnlyInvalidation(t *testing.T) {
	t.Parallel()
	s := newTestRouter(t, nil)
	tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "Detail update"}, harness.Claude)
	insertTestTask(s, tk.ID, tk)
	base, ok := s.mcpHandlers.protocol.Registry.(*mcpRegistry)
	if !ok {
		t.Fatalf("registry type=%T", s.mcpHandlers.protocol.Registry)
	}
	registry := &notificationReadRegistry{mcpRegistry: base}
	s.mcpHandlers.protocol.Registry = registry
	handler, err := s.buildHandler()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	uri := "caic://tasks/" + tk.ID.String()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	body := mcpRequestJSON("subscriptions/listen", `"notifications":{"resourceSubscriptions":["`+uri+`","gomode://notifications"]}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/api/caic/v1/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
	req.Header.Set("Mcp-Method", "subscriptions/listen")
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = response.Body.Close() })
	if response.StatusCode != http.StatusOK {
		t.Fatal(response.StatusCode)
	}
	scanner := bufio.NewScanner(response.Body)
	readUpdate := func() string {
		for scanner.Scan() {
			data, ok := strings.CutPrefix(scanner.Text(), "data: ")
			if !ok {
				continue
			}
			var event struct {
				Method string `json:"method"`
				Params struct {
					URI string `json:"uri"`
				} `json:"params"`
			}
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				t.Fatal(err)
			}
			if event.Method == "notifications/resources/updated" {
				return event.Params.URI
			}
		}
		t.Fatalf("stream ended: %v", scanner.Err())
		return ""
	}
	if got := readUpdate(); got != uri {
		t.Fatalf("initial task=%s", got)
	}
	if got := readUpdate(); got != "gomode://notifications" {
		t.Fatalf("initial notifications=%s", got)
	}
	tk.SetCIStatus(forge.CIStatusPending, nil)
	s.taskMgr.NotifyTaskUpdate(tk.ID)
	if got := readUpdate(); got != uri {
		t.Fatalf("detail-only change invalidated unrelated resource=%s", got)
	}
	if got := registry.reads.Load(); got != 1 {
		t.Fatalf("detail-only change probed alerts: reads=%d", got)
	}
}

func TestMCPReleasedNotifications(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"2025-06-18", "2025-11-25"} {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			s, handler, owner, registered := newMCPOAuthLifecycleRouter(t)
			ownerToken := authorizeMCPClient(t, s.authStore, handler, &owner, &registered)
			other, err := s.authStore.UpsertUser(&auth.User{Provider: auth.ProviderGitHub, ProviderID: "2", Username: "bob"})
			if err != nil {
				t.Fatal(err)
			}
			otherToken := authorizeMCPClient(t, s.authStore, handler, &other, &registered)
			id := ksid.NewID()
			tk := mustNewTask(t, id, agent.Prompt{Text: "Owned task"}, harness.Claude)
			tk.OwnerID = owner.ID
			tk.SetState(task.StateRunning)
			insertTestTask(s, id, tk)
			foreignID := ksid.NewID()
			foreign := mustNewTask(t, foreignID, agent.Prompt{Text: "Private work"}, harness.Claude)
			foreign.OwnerID = other.ID
			insertTestTask(s, foreignID, foreign)
			server := httptest.NewServer(handler)
			t.Cleanup(server.Close)
			client := server.Client()
			request := func(method, token, session, body string) *http.Request {
				req, err := http.NewRequestWithContext(t.Context(), method, server.URL+"/api/caic/v1/mcp", strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Host = "caic.example.com"
				if token != "" {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				req.Header.Set("Mcp-Protocol-Version", version)
				if session != "" {
					req.Header.Set("Mcp-Session-Id", session)
				}
				return req
			}
			post := func(token, session, method, params string) (http.Header, mcp.JSONRPCResponse, int) {
				body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":` + params + `}`
				req := request(http.MethodPost, token, session, body)
				req.Header.Set("Content-Type", "application/json")
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := resp.Body.Close(); err != nil {
						t.Error(err)
					}
				}()
				var out mcp.JSONRPCResponse
				if resp.StatusCode == http.StatusOK {
					if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
						t.Fatal(err)
					}
				}
				return resp.Header, out, resp.StatusCode
			}
			_, _, status := post("", "", "initialize", `{"protocolVersion":"`+version+`"}`)
			if status != http.StatusUnauthorized {
				t.Fatalf("unauthenticated initialize: HTTP %d", status)
			}
			headers, initialized, status := post(ownerToken.AccessToken, "", "initialize", `{"protocolVersion":"`+version+`","clientInfo":{"name":"shipped-client-fixture","version":"1"}}`)
			if status != http.StatusOK || initialized.Error != nil {
				t.Fatalf("initialize: HTTP %d, %#v", status, initialized)
			}
			session := headers.Get("Mcp-Session-Id")
			if session == "" {
				t.Fatal("initialize omitted the notification session")
			}
			emptyReq := request(http.MethodGet, ownerToken.AccessToken, session, "")
			emptyReq.Header.Set("Accept", "text/event-stream")
			emptyStream, err := client.Do(emptyReq)
			if err != nil {
				t.Fatal(err)
			}
			if emptyStream.StatusCode != http.StatusOK {
				t.Fatalf("pre-subscribe stream HTTP %d", emptyStream.StatusCode)
			}
			if err := emptyStream.Body.Close(); err != nil {
				t.Fatal(err)
			}
			for _, uri := range []string{"gomode://items", "gomode://notifications"} {
				_, out, code := post(ownerToken.AccessToken, session, "resources/subscribe", `{"uri":"`+uri+`"}`)
				if code != http.StatusOK || out.Error != nil {
					t.Fatalf("subscribe %s: HTTP %d, %#v", uri, code, out)
				}
			}
			_, denied, _ := post(ownerToken.AccessToken, session, "resources/subscribe", `{"uri":"caic://tasks/`+foreignID.String()+`"}`)
			if denied.Error == nil {
				t.Fatal("subscribed to another user's task")
			}
			_, _, code := post(otherToken.AccessToken, session, "ping", `{}`)
			if code != http.StatusNotFound {
				t.Fatalf("another user reused the session: HTTP %d", code)
			}
			for _, method := range []string{http.MethodGet, http.MethodDelete} {
				for _, denied := range []struct {
					token  string
					status int
				}{{"", http.StatusUnauthorized}, {otherToken.AccessToken, http.StatusNotFound}} {
					req := request(method, denied.token, session, "")
					req.Header.Set("Accept", "text/event-stream")
					resp, err := client.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					if err := resp.Body.Close(); err != nil {
						t.Fatal(err)
					}
					if resp.StatusCode != denied.status {
						t.Fatalf("unauthorized %s: HTTP %d, want %d", method, resp.StatusCode, denied.status)
					}
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			req := request(http.MethodGet, ownerToken.AccessToken, session, "").WithContext(ctx)
			req.Header.Set("Accept", "text/event-stream")
			stream, err := client.Do(req)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cancel()
				if err := stream.Body.Close(); err != nil {
					t.Error(err)
				}
			})
			if stream.StatusCode != http.StatusOK {
				t.Fatalf("notification stream HTTP %d", stream.StatusCode)
			}
			events := make(chan string, 8)
			ended := make(chan error, 1)
			go func() {
				scanner := bufio.NewScanner(stream.Body)
				for scanner.Scan() {
					data, ok := strings.CutPrefix(scanner.Text(), "data: ")
					if !ok {
						continue
					}
					var event struct {
						Method string `json:"method"`
						Params struct {
							URI string `json:"uri"`
						} `json:"params"`
					}
					if err := json.Unmarshal([]byte(data), &event); err != nil {
						ended <- err
						return
					}
					if event.Method == "notifications/resources/updated" {
						select {
						case events <- event.Params.URI:
						case <-ctx.Done():
							return
						}
					}
				}
				ended <- scanner.Err()
			}()
			readUpdates := func() {
				var got []string
				for range 2 {
					select {
					case uri := <-events:
						got = append(got, uri)
					case err := <-ended:
						t.Fatalf("notification stream ended: %v", err)
					case <-time.After(3 * time.Second):
						t.Fatal("task update did not reach the MCP client")
					}
				}
				slices.Sort(got)
				if !slices.Equal(got, []string{"gomode://items", "gomode://notifications"}) {
					t.Fatalf("resource notifications = %q", got)
				}
			}
			readUpdates()
			_, baseline, _ := post(ownerToken.AccessToken, session, "resources/read", `{"uri":"gomode://notifications"}`)
			if baseline.Error != nil {
				t.Fatalf("baseline notifications: %#v", baseline)
			}
			alertStarted := time.Now()
			tk.SetState(task.StateWaiting)
			s.taskMgr.NotifyTaskUpdate(tk.ID)
			readUpdates()
			_, updated, _ := post(ownerToken.AccessToken, session, "resources/read", `{"uri":"gomode://notifications"}`)
			data, err := json.Marshal(updated.Result)
			if err != nil {
				t.Fatal(err)
			}
			var resource struct {
				Contents []struct {
					Text string `json:"text"`
				} `json:"contents"`
			}
			if err := json.Unmarshal(data, &resource); err != nil || len(resource.Contents) != 1 {
				t.Fatalf("notification resource = %s, err %v", data, err)
			}
			var alerts []serviceNotification
			if err := json.Unmarshal([]byte(resource.Contents[0].Text), &alerts); err != nil || len(alerts) != 1 || alerts[0].ItemID != id.String() || alerts[0].Title != "Task #1 is waiting" {
				t.Fatalf("delivered alerts = %#v, err %v", alerts, err)
			}
			t.Logf("mutation-to-alert resource ready: %s elapsed=%s", version, time.Since(alertStarted))
			firstOccurrence := alerts[0].ID
			// Both transitions happen between resource reads. The final enum is
			// unchanged, but its atomic task-state identity denotes a new alert.
			tk.SetState(task.StateRunning)
			s.taskMgr.NotifyTaskUpdate(tk.ID)
			tk.SetState(task.StateWaiting)
			s.taskMgr.NotifyTaskUpdate(tk.ID)
			readUpdates()
			readUpdates()
			_, roundTrip, _ := post(ownerToken.AccessToken, session, "resources/read", `{"uri":"gomode://notifications"}`)
			data, err = json.Marshal(roundTrip.Result)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &resource); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(resource.Contents[0].Text), &alerts); err != nil {
				t.Fatal(err)
			}
			if len(alerts) != 2 || alerts[0].ID == alerts[1].ID || (alerts[0].ID != firstOccurrence && alerts[1].ID != firstOccurrence) {
				t.Fatalf("repeated-state occurrences=%+v", alerts)
			}
			occurrences := []string{alerts[0].ID, alerts[1].ID}
			tk.SetTitle("Content-only update")
			s.taskMgr.NotifyTaskUpdate(tk.ID)
			readUpdates()
			_, contentOnly, _ := post(ownerToken.AccessToken, session, "resources/read", `{"uri":"gomode://notifications"}`)
			data, err = json.Marshal(contentOnly.Result)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &resource); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(resource.Contents[0].Text), &alerts); err != nil {
				t.Fatal(err)
			}
			if len(alerts) != 2 || !slices.Equal([]string{alerts[0].ID, alerts[1].ID}, occurrences) {
				t.Fatalf("content activity manufactured occurrence=%+v", alerts)
			}
			// Replacement restarts the task-local sequence. Its existing timeline
			// incarnation identifies a quiet baseline, not a new occurrence ID.
			restored := mustNewTask(t, id, agent.Prompt{Text: "Restored task"}, harness.Claude)
			restored.OwnerID = owner.ID
			restored.SetState(task.StateWaiting)
			insertTestTask(s, id, restored)
			readUpdates()
			_, replaced, _ := post(ownerToken.AccessToken, session, "resources/read", `{"uri":"gomode://notifications"}`)
			data, err = json.Marshal(replaced.Result)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &resource); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(resource.Contents[0].Text), &alerts); err != nil {
				t.Fatal(err)
			}
			if len(alerts) != 2 || !slices.Equal([]string{alerts[0].ID, alerts[1].ID}, occurrences) {
				t.Fatalf("restored baseline manufactured occurrence=%+v", alerts)
			}
			deleteReq := request(http.MethodDelete, ownerToken.AccessToken, session, "")
			deleted, err := client.Do(deleteReq)
			if err != nil {
				t.Fatal(err)
			}
			if err := deleted.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if deleted.StatusCode != http.StatusOK {
				t.Fatalf("session DELETE HTTP %d", deleted.StatusCode)
			}
			select {
			case err := <-ended:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("session deletion left the stream running")
			}
		})
	}
}

var mcpSkillNameRE = regexp.MustCompile("`([a-z][a-z0-9_]*_[a-z0-9_]+)`")

func TestMCPSessionOwner(t *testing.T) {
	t.Parallel()
	t.Run("credential-independent user identity and task isolation", func(t *testing.T) {
		t.Parallel()
		userCtx := auth.NewContext(t.Context(), &auth.User{ID: "owner"})
		cookieOwner, err := mcpSessionOwner(userCtx)
		if err != nil {
			t.Fatal(err)
		}
		bearerCtx := newMCPPrincipalContext(userCtx, &mcpPrincipal{Remote: true, Subject: "owner"})
		bearerOwner, err := mcpSessionOwner(bearerCtx)
		if err != nil || cookieOwner != bearerOwner || cookieOwner == "" {
			t.Fatalf("cookie owner %q, bearer owner %q, err %v", cookieOwner, bearerOwner, err)
		}
		first, err := mcpSessionOwner(newMCPPrincipalContext(userCtx, &mcpPrincipal{TaskID: ksid.NewID(), Remote: true}))
		if err != nil {
			t.Fatal(err)
		}
		second, err := mcpSessionOwner(newMCPPrincipalContext(userCtx, &mcpPrincipal{TaskID: ksid.NewID(), Remote: true}))
		if err != nil || first == second || first == cookieOwner || second == cookieOwner {
			t.Fatalf("task owners %q, %q; user owner %q; err %v", first, second, cookieOwner, err)
		}
	})
	t.Run("missing remote identity cannot become local", func(t *testing.T) {
		t.Parallel()
		ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{Remote: true})
		if owner, err := mcpSessionOwner(ctx); err == nil || owner != "" {
			t.Fatalf("missing remote identity produced owner %q, err %v", owner, err)
		}
	})
}

func mcpRequestJSON(method, paramsFields string) string {
	if paramsFields == "{}" || paramsFields == "" {
		paramsFields = ""
	} else {
		paramsFields += ","
	}
	return `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":{` + paramsFields + `"_meta":{"io.modelcontextprotocol/protocolVersion":"` + mcp.ProtocolVersion + `","io.modelcontextprotocol/clientInfo":{"name":"caic-test","version":"1.0.0"},"io.modelcontextprotocol/clientCapabilities":{}}}}`
}

func TestMCPEndpointTaskAccessContract(t *testing.T) {
	t.Parallel()
	s := newTestRouter(t, nil)
	owner := &auth.User{ID: "owner"}
	ownedID := ksid.NewID()
	owned := mustNewTask(t, ownedID, agent.Prompt{Text: "owned task"}, harness.Claude)
	owned.OwnerID = owner.ID
	insertTestTask(s, ownedID, owned)
	delegatingID := ksid.NewID()
	delegating := mustNewTask(t, delegatingID, agent.Prompt{Text: "delegating task"}, harness.Claude)
	insertTestTask(s, delegatingID, delegating)
	childID := ksid.NewID()
	child := mustNewTask(t, childID, agent.Prompt{Text: "child task"}, harness.Claude)
	child.ParentTaskID = delegatingID
	insertTestTask(s, childID, child)
	foreignID := ksid.NewID()
	foreign := mustNewTask(t, foreignID, agent.Prompt{Text: "foreign task"}, harness.Claude)
	foreign.OwnerID = "other"
	insertTestTask(s, foreignID, foreign)

	post := func(t *testing.T, ctx context.Context, method, name, params string) string {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(mcpRequestJSON(method, params)))
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", method)
		if name != "" {
			req.Header.Set("Mcp-Name", name)
		}
		w := &mcpNotificationWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel, until: mcp.NotificationMethodResourcesUpdated}
		s.mcpHandlers.handleMCP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want %d: %s", method, w.Code, http.StatusOK, w.Body.String())
		}
		return w.Body.String()
	}

	for _, tc := range []struct {
		name        string
		ctx         context.Context
		visibleID   ksid.ID
		hiddenID    ksid.ID
		createShown bool
	}{
		{
			name:        "user",
			ctx:         newMCPPrincipalContext(auth.NewContext(t.Context(), owner), &mcpPrincipal{Remote: true, Scopes: []string{mcpScopeTasksRead}}),
			visibleID:   ownedID,
			hiddenID:    foreignID,
			createShown: false,
		},
		{
			name:        "task principal",
			ctx:         newMCPPrincipalContext(t.Context(), &mcpPrincipal{TaskID: delegatingID, Remote: true}),
			visibleID:   childID,
			hiddenID:    foreignID,
			createShown: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			visibleURI := "caic://tasks/" + tc.visibleID.String()
			hiddenURI := "caic://tasks/" + tc.hiddenID.String()

			tools := post(t, tc.ctx, "tools/list", "", "{}")
			if !strings.Contains(tools, `"name":"tasks_list"`) {
				t.Fatalf("tools/list does not advertise tasks_list: %s", tools)
			}
			if got := strings.Contains(tools, `"name":"task_create"`); got != tc.createShown {
				t.Fatalf("tools/list task_create visibility = %t, want %t: %s", got, tc.createShown, tools)
			}

			resources := post(t, tc.ctx, "resources/list", "", "{}")
			if !strings.Contains(resources, visibleURI) {
				t.Fatalf("resources/list does not contain visible task resource %s: %s", visibleURI, resources)
			}
			if strings.Contains(resources, hiddenURI) {
				t.Fatalf("resources/list contains hidden task resource %s: %s", hiddenURI, resources)
			}

			read := post(t, tc.ctx, "resources/read", visibleURI, `"uri":"`+visibleURI+`"`)
			if !strings.Contains(read, tc.visibleID.String()) {
				t.Fatalf("resources/read does not contain visible task %s: %s", tc.visibleID, read)
			}
			hiddenRead := post(t, tc.ctx, "resources/read", hiddenURI, `"uri":"`+hiddenURI+`"`)
			var hiddenReadResp mcp.JSONRPCResponse
			if err := json.Unmarshal([]byte(hiddenRead), &hiddenReadResp); err != nil {
				t.Fatalf("decode hidden resources/read response: %v", err)
			}
			if hiddenReadResp.Error == nil {
				t.Fatalf("resources/read of hidden task succeeded: %s", hiddenRead)
			}

			list := post(t, tc.ctx, "tools/call", "tasks_list", `"name":"tasks_list","arguments":{}`)
			if !strings.Contains(list, tc.visibleID.String()) {
				t.Fatalf("tasks_list does not contain visible task %s: %s", tc.visibleID, list)
			}
			if strings.Contains(list, tc.hiddenID.String()) {
				t.Fatalf("tasks_list contains hidden task %s: %s", tc.hiddenID, list)
			}

			subscription := post(t, tc.ctx, "subscriptions/listen", "", `"notifications":{"resourceSubscriptions":["`+visibleURI+`"]}`)
			if !strings.Contains(subscription, tc.visibleID.String()) {
				t.Fatalf("subscription initial state does not contain visible task %s: %s", tc.visibleID, subscription)
			}
			hiddenSubscription := post(t, tc.ctx, "subscriptions/listen", "", `"notifications":{"resourceSubscriptions":["`+hiddenURI+`"]}`)
			var hiddenSubscriptionResp mcp.JSONRPCResponse
			if err := json.Unmarshal([]byte(hiddenSubscription), &hiddenSubscriptionResp); err != nil {
				t.Fatalf("decode hidden subscription response: %v", err)
			}
			if hiddenSubscriptionResp.Error == nil {
				t.Fatalf("subscription to hidden task succeeded: %s", hiddenSubscription)
			}
		})
	}
}

func TestMCPHandlers(t *testing.T) {
	t.Parallel()

	postMCP := func(t *testing.T, h *mcpHandlers, method, name, body string) (*httptest.ResponseRecorder, mcp.JSONRPCResponse) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(body))
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", method)
		if name != "" {
			req.Header.Set("Mcp-Name", name)
		}
		w := httptest.NewRecorder()
		h.handleMCP(w, req)
		var resp mcp.JSONRPCResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		return w, resp
	}

	t.Run("repositoryChangeNotification", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		registry := &mcpResources{serverConfig: s.serverHandlers, taskSvc: s.taskHandlers.taskSvc}
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		updates, err := registry.SubscribeResourceUpdates(ctx, &mcp.SubscriptionsListenParams{Notifications: mcp.SubscriptionFilter{ResourceSubscriptions: []string{"caic://repos/repo"}}})
		if err != nil {
			t.Fatal(err)
		}
		updateC := make(chan mcp.ResourceUpdate, 1)
		var workers sync.WaitGroup
		t.Cleanup(func() { cancel(); workers.Wait() })
		workers.Go(func() {
			for update, iterErr := range updates {
				if iterErr == nil {
					updateC <- update
				}
				return
			}
		})
		registerRouterCheckout(t, s.checkouts, "repo", newRouterTestCheckout(t.TempDir()))
		select {
		case update := <-updateC:
			if !slices.Equal(update.ResourceURIs, []string{"caic://repos/repo"}) {
				t.Errorf("ResourceURIs = %v, want [caic://repos/repo]", update.ResourceURIs)
			}
		case <-time.After(time.Second):
			t.Fatal("repository registration did not notify MCP subscribers")
		}
	})

	t.Run("disabledLeavesEndpointUnregistered", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		s.mcpDisabled = true
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler: %v", err)
		}
		for _, method := range []string{http.MethodPost, http.MethodGet} {
			req := httptest.NewRequestWithContext(t.Context(), method, "/api/caic/v1/mcp", strings.NewReader(mcpRequestJSON("tools/list", `{}`)))
			req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
			req.Header.Set("Mcp-Method", "tools/list")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusNotFound {
				t.Fatalf("%s status = %d, want %d", method, w.Code, http.StatusNotFound)
			}
		}
	})

	t.Run("enabledServesWithoutAuth", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler: %v", err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(mcpRequestJSON("tools/list", `{}`)))
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", "tools/list")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
	})

	t.Run("serverDiscover", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		w, resp := postMCP(t, s.mcpHandlers, "server/discover", "", mcpRequestJSON("server/discover", `{}`))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		if resp.Error != nil {
			t.Fatalf("error = %#v", resp.Error)
		}
		result, ok := resp.Result.(map[string]any)
		if !ok {
			t.Fatalf("result type = %T", resp.Result)
		}
		if result["resultType"] != "complete" {
			t.Errorf("resultType = %v, want complete", result["resultType"])
		}
		versions, ok := result["supportedVersions"].([]any)
		if !ok || len(versions) != 1 || versions[0] != mcp.ProtocolVersion {
			t.Fatalf("supportedVersions = %#v, want [%q]", result["supportedVersions"], mcp.ProtocolVersion)
		}
		caps, ok := result["capabilities"].(map[string]any)
		if !ok {
			t.Fatalf("capabilities type = %T", result["capabilities"])
		}
		toolsCapability, ok := caps["tools"].(map[string]any)
		if !ok {
			t.Error("tools capability missing")
		}
		if _, ok := toolsCapability["listChanged"]; ok {
			t.Fatalf("tools capability = %#v, want no listChanged support", toolsCapability)
		}
		extensions, ok := caps["extensions"].(map[string]any)
		if !ok {
			t.Fatalf("extensions type = %T", caps["extensions"])
		}
		if skillExtension, ok := extensions[mcp.SkillsExtension].(map[string]any); !ok || len(skillExtension) != 0 {
			t.Fatalf("skills extension = %#v, want empty object", extensions[mcp.SkillsExtension])
		}
		instructions, ok := result["instructions"].(string)
		if !ok {
			t.Fatalf("instructions type = %T", result["instructions"])
		}
		if instructions == "" {
			t.Fatal("discovery instructions are empty")
		}
	})

	t.Run("skills", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		registry, ok := s.mcpHandlers.protocol.Registry.(*mcpRegistry)
		if !ok {
			t.Fatalf("registry type = %T", s.mcpHandlers.protocol.Registry)
		}
		want, err := registry.ListSkills(t.Context(), "")
		if err != nil {
			t.Fatal(err)
		}

		_, resp := postMCP(t, s.mcpHandlers, "skills/list", "", mcpRequestJSON("skills/list", `{}`))
		if resp.Error != nil {
			t.Fatalf("skills/list error = %#v", resp.Error)
		}
		data, err := json.Marshal(resp.Result)
		if err != nil {
			t.Fatal(err)
		}
		var listed mcp.SkillsListResult
		if err := json.Unmarshal(data, &listed); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(listed, want) {
			t.Fatalf("skills/list = %#v, want %#v", listed, want)
		}

		_, resp = postMCP(t, s.mcpHandlers, "skills/get", "", mcpRequestJSON("skills/get", `"uri":"`+mcpTasksSkillURI+`"`))
		if resp.Error != nil {
			t.Fatalf("skills/get error = %#v", resp.Error)
		}
		data, err = json.Marshal(resp.Result)
		if err != nil {
			t.Fatal(err)
		}
		var got mcp.SkillsGetResult
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Skill, want.Skills[0]) {
			t.Fatalf("skills/get = %#v, want %#v", got.Skill, want.Skills[0])
		}

		_, resp = postMCP(t, s.mcpHandlers, "resources/read", mcpTasksSkillURI, mcpRequestJSON("resources/read", `"uri":"`+mcpTasksSkillURI+`"`))
		if resp.Error != nil {
			t.Fatalf("resources/read error = %#v", resp.Error)
		}
		data, err = json.Marshal(resp.Result)
		if err != nil {
			t.Fatal(err)
		}
		var resource mcp.ResourcesReadResult
		if err := json.Unmarshal(data, &resource); err != nil {
			t.Fatal(err)
		}
		if len(resource.Contents) != 1 || resource.Contents[0].Text != mcpTasksSkillMarkdown {
			t.Fatalf("resources/read = %#v, want embedded skill", resource.Contents)
		}
		manifest := listed.Skills[0].Resources
		if len(manifest) != 1 || manifest[0].Size != int64(len(resource.Contents[0].Text)) {
			t.Fatalf("skill manifest = %#v, want one resource with size %d", manifest, len(resource.Contents[0].Text))
		}
		digest := sha256.Sum256([]byte(resource.Contents[0].Text))
		if manifest[0].Digest != "sha256:"+hex.EncodeToString(digest[:]) {
			t.Errorf("skill digest = %q, want sha256 of resources/read content", manifest[0].Digest)
		}

		tools, err := registry.Tools(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		availableTools := make(map[string]struct{}, len(tools))
		// Inline code can name tools or arguments. Validate both against the catalog.
		for _, tool := range tools {
			availableTools[tool.Name] = struct{}{}
			data, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			var sch mcpToolInputSchema
			if err := json.Unmarshal(data, &sch); err != nil {
				t.Fatal(err)
			}
			for name := range sch.Properties {
				availableTools[name] = struct{}{}
			}
		}
		referencedTools := mcpSkillNameRE.FindAllStringSubmatch(mcpTasksSkillMarkdown, -1)
		if len(referencedTools) == 0 {
			t.Fatal("MCP task skill does not reference any tools")
		}
		for _, match := range referencedTools {
			if _, ok := availableTools[match[1]]; !ok {
				t.Errorf("MCP task skill references unavailable tool or argument %q", match[1])
			}
		}

		_, resp = postMCP(t, s.mcpHandlers, "skills/get", "", mcpRequestJSON("skills/get", `"uri":"skill://missing/SKILL.md"`))
		if resp.Error == nil || resp.Error.Code != mcp.InvalidParamsCode {
			t.Fatalf("unknown skills/get error = %#v, want invalid params", resp.Error)
		}
	})

	t.Run("toolsList", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		w, resp := postMCP(t, s.mcpHandlers, "tools/list", "", mcpRequestJSON("tools/list", `{}`))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		if resp.Error != nil {
			t.Fatalf("error = %#v", resp.Error)
		}
		result, ok := resp.Result.(map[string]any)
		if !ok {
			t.Fatalf("result type = %T", resp.Result)
		}
		if result["resultType"] != "complete" {
			t.Errorf("resultType = %v, want complete", result["resultType"])
		}
		if result["cacheScope"] != string(mcp.CacheScopePrivate) {
			t.Errorf("cacheScope = %v, want %q", result["cacheScope"], mcp.CacheScopePrivate)
		}
		tools, ok := result["tools"].([]any)
		if !ok {
			t.Fatalf("tools type = %T", result["tools"])
		}
		if len(tools) == 0 {
			t.Fatal("tools is empty")
		}
		var foundRepos, foundTasks bool
		for _, item := range tools {
			tool, ok := item.(map[string]any)
			if !ok {
				t.Fatalf("tool type = %T", item)
			}
			switch tool["name"] {
			case "repos_list":
				foundRepos = true
				inputSchema, ok := tool["inputSchema"].(map[string]any)
				if !ok {
					t.Fatalf("inputSchema type = %T", tool["inputSchema"])
				}
				properties, ok := inputSchema["properties"].(map[string]any)
				if !ok || properties["cursor"] == nil || properties["limit"] == nil {
					t.Fatalf("repos_list input properties = %#v, want cursor and limit", inputSchema["properties"])
				}
				limit, ok := properties["limit"].(map[string]any)
				if !ok || limit["minimum"] != float64(1) || limit["maximum"] != float64(mcpRepoPageSizeMax) {
					t.Fatalf("repos_list limit schema = %#v", properties["limit"])
				}
			case "tasks_list":
				foundTasks = true
				inputSchema, ok := tool["inputSchema"].(map[string]any)
				if !ok {
					t.Fatalf("inputSchema type = %T", tool["inputSchema"])
				}
				properties, ok := inputSchema["properties"].(map[string]any)
				if !ok || properties["cursor"] == nil || properties["limit"] == nil {
					t.Fatalf("tasks_list input properties = %#v, want cursor and limit", inputSchema["properties"])
				}
				limit, ok := properties["limit"].(map[string]any)
				if !ok || limit["minimum"] != float64(1) || limit["maximum"] != float64(mcpTaskPageSizeMax) {
					t.Fatalf("tasks_list limit schema = %#v", properties["limit"])
				}
				outputSchema, ok := tool["outputSchema"].(map[string]any)
				if !ok {
					t.Fatalf("outputSchema type = %T", tool["outputSchema"])
				}
				if outputSchema["type"] != "object" {
					t.Fatalf("outputSchema.type = %v, want object", outputSchema["type"])
				}
			}
		}
		if !foundRepos || !foundTasks {
			t.Fatalf("list tools found repos=%t tasks=%t, want both", foundRepos, foundTasks)
		}
	})

	t.Run("resourceTemplatesList", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		w, resp := postMCP(t, s.mcpHandlers, "resources/templates/list", "", mcpRequestJSON("resources/templates/list", `{}`))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		if resp.Error != nil {
			t.Fatalf("error = %#v", resp.Error)
		}
		result, ok := resp.Result.(map[string]any)
		if !ok {
			t.Fatalf("result type = %T", resp.Result)
		}
		if result["resultType"] != "complete" {
			t.Errorf("resultType = %v, want complete", result["resultType"])
		}
		templates, ok := result["resourceTemplates"].([]any)
		if !ok {
			t.Fatalf("resourceTemplates type = %T", result["resourceTemplates"])
		}
		if len(templates) == 0 {
			t.Fatal("resourceTemplates is empty")
		}
	})

	t.Run("subscriptionsListenAcknowledges", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		ctx, cancel := context.WithCancel(t.Context())
		body := mcpRequestJSON("subscriptions/listen", `"notifications":{"resourcesListChanged":true,"resourceSubscriptions":["gomode://items"]}`)
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(body))
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", "subscriptions/listen")
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		w := &mcpNotificationWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel, until: mcp.NotificationMethodResourcesUpdated}
		s.mcpHandlers.handleMCP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		data := w.Body.String()
		if !strings.Contains(data, `"method":"notifications/subscriptions/acknowledged"`) {
			t.Fatalf("subscription response = %s, want acknowledgment", data)
		}
		if !strings.Contains(data, `"resourcesListChanged":true`) {
			t.Fatalf("subscription response = %s, want supported resources list changes acknowledged", data)
		}
		if !strings.Contains(data, `"resourceSubscriptions":["gomode://items"]`) {
			t.Fatalf("subscription response = %s, want supported task resource acknowledged", data)
		}
		// Cancel after the initial resource update, so the body contains
		// the acknowledgment, list change, and initial resource snapshot.
		if !strings.Contains(data, `"method":"notifications/resources/list_changed"`) {
			t.Fatalf("subscription response = %s, want initial resources list_changed", data)
		}
		// The legacy re-read burst follows the delivered initial state so
		// clients that only react to resources/updated still re-read; one
		// notification per subscribed URI.
		if n := strings.Count(data, `"method":"notifications/resources/updated"`); n != 1 {
			t.Fatalf("resources/updated count = %d in %s, want 1", n, data)
		}
		initialIndex := strings.Index(data, `"method":"notifications/subscriptions/initial_state"`)
		burstIndex := strings.Index(data, `"method":"notifications/resources/updated"`)
		if initialIndex < 0 || initialIndex > burstIndex {
			t.Fatalf("subscription response = %s, want initial state before resources/updated", data)
		}
		found, msg := sseInitialStateNotification(data)
		if !found {
			t.Fatalf("subscription response = %s, want initial state", data)
		}
		var initial mcp.SubscriptionsInitialStateParams
		if b, err := json.Marshal(msg.Params); err == nil {
			if err := json.Unmarshal(b, &initial); err != nil {
				t.Fatalf("decode initial state: %v", err)
			}
		} else {
			t.Fatalf("encode initial state params: %v", err)
		}
		if initial.URI != "gomode://items" {
			t.Fatalf("initial state uri = %q, want gomode://items", initial.URI)
		}
		if len(initial.Contents) == 0 {
			t.Fatalf("initial state contents = empty, want items read")
		}

		// The delivered initial state must equal what resources/read returns
		// for the same target right after subscribing.
		_, readResp := postMCP(t, s.mcpHandlers, "resources/read", "gomode://items", mcpRequestJSON("resources/read", `"uri":"gomode://items"`))
		if readResp.Error != nil {
			t.Fatalf("resources/read failed: %v", readResp.Error)
		}
		var read mcp.ResourcesReadResult
		if b, err := json.Marshal(readResp.Result); err == nil {
			if err := json.Unmarshal(b, &read); err != nil {
				t.Fatalf("decode read result: %v", err)
			}
		} else {
			t.Fatalf("encode read result: %v", err)
		}
		if !reflect.DeepEqual(initial.Contents, read.Contents) {
			t.Fatalf("initial state contents = %+v, want resources/read contents %+v", initial.Contents, read.Contents)
		}
	})

	t.Run("subscriptionsListenAcceptsGoModeNotifications", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		ctx, cancel := context.WithCancel(t.Context())
		body := mcpRequestJSON("subscriptions/listen", `"notifications":{"resourceSubscriptions":["gomode://notifications"]}`)
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(body))
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", "subscriptions/listen")
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		w := &mcpNotificationWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel, until: mcp.NotificationMethodResourcesUpdated}
		s.mcpHandlers.handleMCP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		if n := strings.Count(w.Body.String(), `"method":"notifications/resources/updated"`); n != 1 {
			t.Fatalf("resources/updated count = %d in %s, want 1", n, w.Body.String())
		}
		initialIndex := strings.Index(w.Body.String(), `"method":"notifications/subscriptions/initial_state"`)
		burstIndex := strings.Index(w.Body.String(), `"method":"notifications/resources/updated"`)
		if initialIndex < 0 || initialIndex > burstIndex {
			t.Fatalf("subscription response = %s, want initial state before resources/updated", w.Body.String())
		}
		found, msg := sseInitialStateNotification(w.Body.String())
		if !found {
			t.Fatalf("subscription response = %s, want initial state", w.Body.String())
		}
		var initial mcp.SubscriptionsInitialStateParams
		if b, err := json.Marshal(msg.Params); err == nil {
			if err := json.Unmarshal(b, &initial); err != nil {
				t.Fatalf("decode initial state: %v", err)
			}
		} else {
			t.Fatalf("encode initial state params: %v", err)
		}
		if initial.URI != "gomode://notifications" {
			t.Fatalf("initial state uri = %q, want gomode://notifications", initial.URI)
		}
		if len(initial.Contents) == 0 {
			t.Fatalf("initial state contents = empty, want notifications read")
		}
	})

	// A remote principal without the task read scope cannot subscribe to
	// task resources; the request is rejected as invalid params and nothing
	// is written to the stream, initial state included.
	t.Run("subscriptionsListenRejectsMissingScope", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		ctx = newMCPPrincipalContext(ctx, &mcpPrincipal{Scopes: []string{mcpScopeRead}, Remote: true})
		ctx = auth.NewContext(ctx, &auth.User{ID: "user-1"})
		body := mcpRequestJSON("subscriptions/listen", `"notifications":{"resourceSubscriptions":["gomode://items"]}`)
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(body))
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", "subscriptions/listen")
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		w := httptest.NewRecorder()
		s.mcpHandlers.handleMCP(w, req)
		var resp mcp.JSONRPCResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if resp.Error == nil || resp.Error.Code != mcp.InvalidParamsCode {
			t.Fatalf("response error = %+v, want invalid params scope denial: %s", resp.Error, w.Body.String())
		}
		if !strings.Contains(resp.Error.Message, "missing required MCP scope: ") {
			t.Fatalf("error message = %q, want missing required MCP scope", resp.Error.Message)
		}
		for _, method := range []string{
			"\"method\":\"notifications/subscriptions/acknowledged\"",
			"\"method\":\"notifications/subscriptions/initial_state\"",
			"\"method\":\"notifications/resources/updated\"",
		} {
			if strings.Contains(w.Body.String(), method) {
				t.Fatalf("subscription response = %s, want no %s delivered for the denied scope", w.Body.String(), method)
			}
		}
	})

	// The delivered initial state applies the mcpServiceItemMaxCount
	// bounding: the payload carries only the cap and the omitted count, and
	// the raw value of the one omitted item must not reach the wire.
	t.Run("subscriptionsListenDeliversBoundedInitialState", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		count := mcpServiceItemMaxCount + 1
		seeded := make([]string, 0, count)
		for range count {
			id := ksid.NewID()
			insertTestTask(s, id, mustNewTask(t, id, agent.Prompt{Text: "subscription initial state item"}, harness.Claude))
			seeded = append(seeded, id.String())
		}
		ctx, cancel := context.WithCancel(t.Context())
		body := mcpRequestJSON("subscriptions/listen", `"notifications":{"resourceSubscriptions":["gomode://items"]}`)
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(body))
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", "subscriptions/listen")
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		w := &mcpNotificationWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel, until: mcp.NotificationMethodResourcesUpdated}
		s.mcpHandlers.handleMCP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		data := w.Body.String()
		found, msg := sseInitialStateNotification(data)
		if !found {
			t.Fatalf("subscription response = %s, want initial state", data)
		}
		var initial mcp.SubscriptionsInitialStateParams
		b, err := json.Marshal(msg.Params)
		if err != nil {
			t.Fatalf("marshal initial state params: %v", err)
		}
		if err := json.Unmarshal(b, &initial); err != nil {
			t.Fatalf("decode initial state: %v", err)
		}
		if len(initial.Contents) != 1 {
			t.Fatalf("initial state contents = %d, want 1", len(initial.Contents))
		}
		var streamItems mcpschema.ServiceItemsOutput
		if err := json.Unmarshal([]byte(initial.Contents[0].Text), &streamItems); err != nil {
			t.Fatalf("decode delivered items: %v", err)
		}
		if len(streamItems.Items) != mcpServiceItemMaxCount {
			t.Fatalf("delivered item count = %d, want %d", len(streamItems.Items), mcpServiceItemMaxCount)
		}
		if streamItems.OmittedCount != 1 {
			t.Fatalf("delivered omitted count = %d, want 1", streamItems.OmittedCount)
		}
		// The delivered payload must be the same authorized read output the
		// client gets from resources/read right after subscribing.
		_, readResp := postMCP(t, s.mcpHandlers, "resources/read", "gomode://items", mcpRequestJSON("resources/read", `"uri":"gomode://items"`))
		if readResp.Error != nil {
			t.Fatalf("resources/read failed: %v", readResp.Error)
		}
		var read mcp.ResourcesReadResult
		b, err = json.Marshal(readResp.Result)
		if err != nil {
			t.Fatalf("marshal read result: %v", err)
		}
		if err := json.Unmarshal(b, &read); err != nil {
			t.Fatalf("decode read result: %v", err)
		}
		if !reflect.DeepEqual(initial.Contents, read.Contents) {
			t.Fatalf("initial state contents = %+v, want resources/read contents %+v", initial.Contents, read.Contents)
		}
		// The one item the bounded read omits keeps its raw value off the
		// stream.
		delivered := make(map[string]bool, len(streamItems.Items))
		for _, item := range streamItems.Items {
			if delivered[item.ID] {
				t.Fatalf("delivered item id %s appears twice", item.ID)
			}
			delivered[item.ID] = true
		}
		var omitted []string
		for _, id := range seeded {
			if !delivered[id] {
				omitted = append(omitted, id)
			}
			if len(omitted) == 1 {
				break
			}
		}
		if len(omitted) != 1 {
			t.Fatalf("omitted item ids = %v, want exactly one", omitted)
		}
		if n := strings.Count(data, omitted[0]); n != 0 {
			t.Fatalf("stream contains %d occurrences of omitted item id %s, want 0", n, omitted[0])
		}
	})

	// The pre-ack read that supplies the delivered initial state must run
	// with the subscriber's context, so the task list snapshot is owner
	// filtered: a task belonging to another owner never reaches the wire,
	// in the delivered payload or in any later re-read burst.
	t.Run("subscriptionsListenDeliversOwnerFilteredInitialState", func(t *testing.T) {
		t.Parallel()
		usersPath := filepath.Join(t.TempDir(), "users.json")
		store, err := auth.Open(usersPath)
		if err != nil {
			t.Fatalf("open auth store: %v", err)
		}
		user, err := store.UpsertUser(&auth.User{
			Provider:    auth.ProviderGitHub,
			ProviderID:  "1",
			Username:    "alice",
			AccessToken: "forge-token",
			AvatarURL:   "https://github.com/avatar/alice",
		})
		if err != nil {
			t.Fatalf("upsert user: %v", err)
		}
		s := newTestRouterWithAuthHost(t, store, "", newTestHostPolicy(t, "https://caic.example.com"))
		mineID := ksid.NewID()
		mine := mustNewTask(t, mineID, agent.Prompt{Text: "subscription initial state item owned"}, harness.Claude)
		mine.OwnerID = user.ID
		insertTestTask(s, mineID, mine)
		otherID := ksid.NewID()
		other := mustNewTask(t, otherID, agent.Prompt{Text: "subscription initial state item foreign"}, harness.Claude)
		other.OwnerID = "other-owner"
		insertTestTask(s, otherID, other)
		ctx, cancel := context.WithCancel(t.Context())
		ctx = auth.NewContext(ctx, &user)
		body := mcpRequestJSON("subscriptions/listen", `"notifications":{"resourceSubscriptions":["gomode://items"]}`)
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(body))
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", "subscriptions/listen")
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		w := &mcpNotificationWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel, until: mcp.NotificationMethodResourcesUpdated}
		s.mcpHandlers.handleMCP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		data := w.Body.String()
		found, msg := sseInitialStateNotification(data)
		if !found {
			t.Fatalf("subscription response = %s, want initial state", data)
		}
		var initial mcp.SubscriptionsInitialStateParams
		b, err := json.Marshal(msg.Params)
		if err != nil {
			t.Fatalf("marshal initial state params: %v", err)
		}
		if err := json.Unmarshal(b, &initial); err != nil {
			t.Fatalf("decode initial state: %v", err)
		}
		if initial.URI != "gomode://items" {
			t.Fatalf("initial state uri = %q, want gomode://items", initial.URI)
		}
		if len(initial.Contents) != 1 {
			t.Fatalf("initial state contents = %d, want 1", len(initial.Contents))
		}
		var streamItems mcpschema.ServiceItemsOutput
		if err := json.Unmarshal([]byte(initial.Contents[0].Text), &streamItems); err != nil {
			t.Fatalf("decode delivered items: %v", err)
		}
		if len(streamItems.Items) != 1 {
			t.Fatalf("delivered item count = %d, want 1 (owner filtered): %s", len(streamItems.Items), initial.Contents[0].Text)
		}
		if streamItems.Items[0].ID != mineID.String() {
			t.Fatalf("delivered item id = %q, want the subscriber's task %s", streamItems.Items[0].ID, mineID)
		}
		if n := strings.Count(data, otherID.String()); n != 0 {
			t.Fatalf("stream contains %d occurrences of another owner's task id %s, want 0", n, otherID)
		}
	})

	t.Run("subscriptionsListenEmptyFilter", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		// Nil production dependencies make any accidental content read fail.
		sources, err := (&mcpResources{}).subscriptionSources(t.Context(), mcp.SubscriptionFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(sources, subscriptionSources{}) {
			t.Fatalf("empty filter armed content sources: %+v", sources)
		}
		ctx, cancel := context.WithCancel(t.Context())
		body := mcpRequestJSON("subscriptions/listen", `"notifications":{}`)
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Mcp-Method", "subscriptions/listen")
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		w := &mcpNotificationWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel, until: mcp.NotificationMethodSubscriptionsAcknowledged}
		s.mcpHandlers.protocol.HandleMCP(w, req)
		if !strings.Contains(w.Body.String(), "subscriptions/acknowledged") {
			t.Fatalf("empty native filter was not acknowledged: %s", w.Body.String())
		}
	})

	t.Run("subscriptionChangesSignalTaskAndNotificationResources", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		registry, ok := s.mcpHandlers.protocol.Registry.(*mcpRegistry)
		if !ok {
			t.Fatalf("registry type = %T", s.mcpHandlers.protocol.Registry)
		}
		ctx, cancel := context.WithCancel(t.Context())
		changes, err := registry.SubscribeResourceUpdates(ctx, &mcp.SubscriptionsListenParams{Notifications: mcp.SubscriptionFilter{ResourceSubscriptions: []string{"gomode://items", "gomode://notifications"}}})
		if err != nil {
			t.Fatal(err)
		}
		got := make(chan mcp.ResourceUpdate, 1)
		done := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() {
			defer close(done)
			for update, err := range changes {
				if err != nil {
					t.Error(err)
					return
				}
				got <- update
				return
			}
		})
		t.Cleanup(func() {
			cancel()
			wg.Wait()
		})
		s.taskMgr.NotifyTaskChange()
		var update mcp.ResourceUpdate
		select {
		case update = <-got:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for subscription change")
		}
		if !slices.Equal(update.ResourceURIs, []string{"gomode://items", "gomode://notifications"}) {
			t.Fatalf("update resource uris = %#v, want task and notification resources", update.ResourceURIs)
		}
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for subscription iterator to stop")
		}
	})

	t.Run("subscriptionChangesSignalFocus", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		id := ksid.NewID()
		insertTestTask(s, id, mustNewTask(t, id, agent.Prompt{Text: "focus subscription"}, harness.Claude))
		registry, ok := s.mcpHandlers.protocol.Registry.(*mcpRegistry)
		if !ok {
			t.Fatalf("registry type = %T", s.mcpHandlers.protocol.Registry)
		}
		ctx, cancel := context.WithCancel(t.Context())
		changes, err := registry.SubscribeResourceUpdates(ctx, &mcp.SubscriptionsListenParams{Notifications: mcp.SubscriptionFilter{ResourceSubscriptions: []string{"gomode://items", "gomode://notifications"}}})
		if err != nil {
			t.Fatal(err)
		}
		got := make(chan mcp.ResourceUpdate, 1)
		var wg sync.WaitGroup
		wg.Go(func() {
			for update, err := range changes {
				if err != nil {
					t.Error(err)
					return
				}
				got <- update
				return
			}
		})
		t.Cleanup(func() {
			cancel()
			wg.Wait()
		})
		testTaskHandlers(s).taskSvc.focusTask(t.Context(), id)
		select {
		case update := <-got:
			if !slices.Equal(update.ResourceURIs, []string{"gomode://items", "gomode://notifications"}) {
				t.Fatalf("focus update resource uris = %#v", update.ResourceURIs)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for focus subscription change")
		}
	})

	t.Run("subscriptionDoesNotMissTaskChangeWhileDeliveringPreviousChange", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		id := ksid.NewID()
		tk := mustNewTask(t, id, agent.Prompt{Text: "subscription state changes"}, harness.Claude)
		insertTestTask(s, id, tk)
		registry, ok := s.mcpHandlers.protocol.Registry.(*mcpRegistry)
		if !ok {
			t.Fatalf("registry type = %T", s.mcpHandlers.protocol.Registry)
		}
		ctx, cancel := context.WithCancel(t.Context())
		changes, err := registry.SubscribeResourceUpdates(ctx, &mcp.SubscriptionsListenParams{Notifications: mcp.SubscriptionFilter{ResourceSubscriptions: []string{"gomode://items"}}})
		if err != nil {
			t.Fatal(err)
		}
		updates := make(chan mcp.ResourceUpdate, 2)
		done := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() {
			defer close(done)
			count := 0
			for update, err := range changes {
				if err != nil {
					t.Error(err)
					return
				}
				updates <- update
				count++
				if count == 1 {
					tk.SetState(task.StateWaiting)
					s.taskMgr.NotifyTaskUpdate(tk.ID)
				}
				if count == 2 {
					return
				}
			}
		})
		t.Cleanup(func() {
			cancel()
			wg.Wait()
		})

		tk.SetState(task.StateRunning)
		s.taskMgr.NotifyTaskUpdate(tk.ID)
		for range 2 {
			select {
			case update := <-updates:
				if !slices.Equal(update.ResourceURIs, []string{"gomode://items"}) {
					t.Fatalf("update resource uris = %#v, want gomode://items", update.ResourceURIs)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for task state update")
			}
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for subscription iterator to stop")
		}
	})

	t.Run("systemStatsMatchesHTTP", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		s.serverHandlers.systemStats = &systemStats{samples: []v1.SystemSample{{Time: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC), CPUs: []v1.CPUUsage{{ID: "cpu0", UsagePerc: 25}}, MemUsed: 512, MemTotal: 1024, MemPerc: 50, NetRx: 200, NetTx: 100, DiskUsed: 2048, DiskTotal: 4096}}, err: "memory unavailable"}
		w := httptest.NewRecorder()
		s.serverHandlers.routes().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/server/system-stats", http.NoBody))
		if w.Code != http.StatusOK {
			t.Fatalf("REST status: %d", w.Code)
		}
		var rest v1.SystemStatsResp
		if err := json.Unmarshal(w.Body.Bytes(), &rest); err != nil {
			t.Fatal(err)
		}
		body := mcpRequestJSON("tools/call", `"name":"get_system_stats","arguments":{}`)
		w, resp := postMCP(t, s.mcpHandlers, "tools/call", "get_system_stats", body)
		if w.Code != http.StatusOK || resp.Error != nil {
			t.Fatalf("MCP response: %d, %+v", w.Code, resp.Error)
		}
		data, err := json.Marshal(resp.Result)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			StructuredContent v1.SystemStatsResp `json:"structuredContent"`
			IsError           bool               `json:"isError"`
		}
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		if result.IsError || !reflect.DeepEqual(result.StructuredContent, rest) {
			t.Fatalf("MCP = %+v, REST = %+v", result, rest)
		}
	})

	t.Run("toolsCall", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		id := ksid.NewID()
		tk := mustNewTask(t, id, agent.Prompt{Text: "test"}, harness.Claude)
		tk.SetTitle("Fix tests")
		tk.SetState(task.StateWaiting)
		insertTestTask(s, id, tk)

		body := mcpRequestJSON("tools/call", `"name":"tasks_list","arguments":{},"inputResponses":{},"requestState":"retry-state"`)
		body = strings.Replace(body, `"io.modelcontextprotocol/clientCapabilities":{}`, `"io.modelcontextprotocol/clientCapabilities":{},"example.com/clientTrace":"trace-1"`, 1)
		w, resp := postMCP(t, s.mcpHandlers, "tools/call", "tasks_list", body)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		if resp.Error != nil {
			t.Fatalf("error = %#v", resp.Error)
		}
		data, err := json.Marshal(resp.Result)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if !bytes.Contains(data, []byte(`"resultType":"complete"`)) {
			t.Fatalf("result = %s, want complete resultType", data)
		}
		if !bytes.Contains(data, []byte("Fix tests")) {
			t.Fatalf("result = %s, want task title", data)
		}
		if !bytes.Contains(data, []byte("structuredContent")) {
			t.Fatalf("result = %s, want structuredContent", data)
		}

		var recorded bool
		for _, metric := range s.serverHandlers.metrics.Snapshot() {
			if metric.Name == "mcp.tool.tasks_list" && metric.Outcome == metrics.OutcomeOK && metric.Count >= 1 {
				recorded = true
			}
		}
		if !recorded {
			t.Fatalf("metrics = %+v, want a recorded tasks_list ok call", s.serverHandlers.metrics.Snapshot())
		}
	})

	t.Run("headerMismatch", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(mcpRequestJSON("tools/list", `{}`)))
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", "tools/call")
		w := httptest.NewRecorder()
		s.mcpHandlers.handleMCP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		var resp mcp.JSONRPCResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if resp.Error == nil || resp.Error.Code != mcp.InvalidRequestCode {
			t.Fatalf("error = %#v, want invalid request", resp.Error)
		}
	})

	t.Run("toolParamHeaderMismatch", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		body := mcpRequestJSON("tools/call", `"name":"task_fork","arguments":{"task_number":1,"prompt":"fork"}`)
		w, resp := postMCP(t, s.mcpHandlers, "tools/call", "task_fork", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		if resp.Error == nil || resp.Error.Code != mcp.InvalidRequestCode {
			t.Fatalf("error = %#v, want invalid request", resp.Error)
		}
	})

	t.Run("toolExecutionErrorIncludesDomainCode", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		body := mcpRequestJSON("tools/call", `"name":"task_get_detail","arguments":{"task":"`+ksid.NewID().String()+`"}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(body))
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", "tools/call")
		req.Header.Set("Mcp-Name", "task_get_detail")
		w := httptest.NewRecorder()
		s.mcpHandlers.handleMCP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		var rpc map[string]any
		if err := json.NewDecoder(w.Body).Decode(&rpc); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		result, ok := rpc["result"].(map[string]any)
		if !ok {
			t.Fatalf("result type = %T", rpc["result"])
		}
		if result["isError"] != true {
			t.Fatalf("isError = %v, want true", result["isError"])
		}
		meta, ok := result["_meta"].(map[string]any)
		if !ok || meta[mcp.ToolErrorCodeMetaKey] != string(api.CodeNotFound) {
			t.Fatalf("tool error metadata = %#v, want caic not-found code", result["_meta"])
		}
	})

	t.Run("unsupportedProtocolVersion", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		body := strings.ReplaceAll(mcpRequestJSON("tools/list", `{}`), mcp.ProtocolVersion, "2099-01-01")
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(body))
		req.Header.Set("Mcp-Protocol-Version", "2099-01-01")
		req.Header.Set("Mcp-Method", "tools/list")
		w := httptest.NewRecorder()
		s.mcpHandlers.handleMCP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		var resp mcp.JSONRPCResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if resp.Error == nil || resp.Error.Code != mcp.UnsupportedProtocolVersionCode {
			t.Fatalf("error = %#v, want unsupported protocol version", resp.Error)
		}
	})

	// The draft Streamable HTTP transport maps an unknown RPC method to HTTP 404
	// with a -32601 JSON-RPC error body.
	t.Run("initializeRemoved", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		w, resp := postMCP(t, s.mcpHandlers, "initialize", "", mcpRequestJSON("initialize", `{}`))
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
		}
		if resp.Error == nil || resp.Error.Code != -32601 {
			t.Fatalf("error = %#v, want method not found", resp.Error)
		}
	})

	// Bad client input (unknown tool) stays an invalid-params error.
	t.Run("toolCallInvalidParams", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		body := mcpRequestJSON("tools/call", `"name":"nope","arguments":{}`)
		_, resp := postMCP(t, s.mcpHandlers, "tools/call", "nope", body)
		if resp.Error == nil || resp.Error.Code != mcp.InvalidParamsCode {
			t.Fatalf("error = %#v, want invalid params", resp.Error)
		}
	})

	// The caic registry rejects unknown resource names and invalid paging cursors.
	t.Run("resourceErrors", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name     string
			method   string
			resource string
			params   string
		}{
			{name: "unknown resource", method: "resources/read", resource: "caic://nope", params: `"uri":"caic://nope"`},
			{name: "missing repository", method: "resources/read", resource: "caic://repos/ghost", params: `"uri":"caic://repos/ghost"`},
			{name: "invalid cursor", method: "resources/list", params: `"cursor":"bad"`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				s := newTestRouter(t, nil)
				_, resp := postMCP(t, s.mcpHandlers, tc.method, tc.resource, mcpRequestJSON(tc.method, tc.params))
				if resp.Error == nil || resp.Error.Code != mcp.InvalidParamsCode {
					t.Fatalf("error = %#v, want invalid params", resp.Error)
				}
			})
		}
	})
}

// sseInitialStateNotification returns the first SSE data-line notification in
// the body that delivers the subscribed initial state.
func sseInitialStateNotification(data string) (bool, mcp.JSONRPCNotification) {
	for line := range strings.SplitSeq(data, "\n") {
		payload, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
		if !ok {
			continue
		}
		var msg mcp.JSONRPCNotification
		if err := json.Unmarshal([]byte(payload), &msg); err != nil || msg.Method != mcp.NotificationMethodSubscriptionsInitialState {
			continue
		}
		return true, msg
	}
	return false, mcp.JSONRPCNotification{}
}

func newAuthEnabledRouter(t *testing.T) (*Router, auth.User) {
	usersPath := filepath.Join(t.TempDir(), "users.json")
	store, err := auth.Open(usersPath)
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}
	user, err := store.UpsertUser(&auth.User{
		Provider:    auth.ProviderGitHub,
		ProviderID:  "1",
		Username:    "alice",
		AccessToken: "forge-token",
		AvatarURL:   "https://github.com/avatar/alice",
	})
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	s := newTestOAuthRouter(t, store)
	return s.Router, user
}

func registerTestClient(t *testing.T, h http.Handler, clientName string, redirectURIs []string) oauth.RegisterResponse {
	body := strings.NewReader(`{"client_name":"` + clientName + `","redirect_uris":["` + strings.Join(redirectURIs, `","`) + `"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"]}`)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/register", body)
	req.Header.Set("Content-Type", "application/json")
	req.Host = "caic.example.com"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("register status = %d, want %d: %s", w.Code, http.StatusCreated, w.Body.String())
	}
	if contentType := w.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}
	var resp oauth.RegisterResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	return resp
}

// mcpNotificationWriter cancels a finite subscription only after its expected notification is delivered.
type mcpNotificationWriter struct {
	*httptest.ResponseRecorder

	cancel context.CancelFunc
	until  mcp.NotificationMethod
}

func (w *mcpNotificationWriter) Write(raw []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(raw)
	if err != nil {
		return n, err
	}
	data, ok := bytes.CutPrefix(raw, []byte("data: "))
	if !ok {
		return n, nil
	}
	var notification mcp.JSONRPCNotification
	if err := json.Unmarshal(bytes.TrimSpace(data), &notification); err != nil {
		w.cancel()
		return n, err
	}
	if notification.Method == w.until {
		w.cancel()
	}
	return n, nil
}

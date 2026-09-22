// Tests for the MCP JSON-RPC endpoint.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/mcp"
	"github.com/caic-xyz/caic/backend/internal/mcp/mcptest"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	"github.com/caic-xyz/caic/metrics"
	"github.com/caic-xyz/caic/oauth"
)

func mcpRequestJSON(method, paramsFields string) string {
	if paramsFields == "{}" || paramsFields == "" {
		paramsFields = ""
	} else {
		paramsFields += ","
	}
	return `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":{` + paramsFields + `"_meta":{"io.modelcontextprotocol/protocolVersion":"` + mcp.ProtocolVersion + `","io.modelcontextprotocol/clientInfo":{"name":"caic-test","version":"1.0.0"},"io.modelcontextprotocol/clientCapabilities":{}}}}`
}

func TestTaskListSnapshotOrdersActiveTasksFirst(t *testing.T) {
	t.Parallel()

	s := newTestRouter(t, nil)
	inactiveID := ksid.NewID()
	inactive := mustNewTask(t, inactiveID, agent.Prompt{Text: "inactive"}, harness.Claude)
	inactive.SetState(taskslog.StateStopped)
	insertTestTask(s, inactiveID.String(), inactive)

	activeID := ksid.NewID()
	active := mustNewTask(t, activeID, agent.Prompt{Text: "active"}, harness.Claude)
	insertTestTask(s, activeID.String(), active)

	tasks := testTaskHandlers(s).taskSvc.taskListSnapshot(t.Context())
	if len(tasks) != 2 {
		t.Fatalf("task count = %d, want 2", len(tasks))
	}
	if got := tasks[0].InitialPrompt; got != "active" {
		t.Errorf("first task = %q, want active", got)
	}
	if got := tasks[1].InitialPrompt; got != "inactive" {
		t.Errorf("second task = %q, want inactive", got)
	}
}

func TestMCPEndpointTaskAccessContract(t *testing.T) {
	t.Parallel()
	s := newTestRouter(t, nil)
	owner := &auth.User{ID: "owner"}
	ownedID := ksid.NewID()
	owned := mustNewTask(t, ownedID, agent.Prompt{Text: "owned task"}, harness.Claude)
	owned.OwnerID = owner.ID
	insertTestTask(s, ownedID.String(), owned)
	delegatingID := ksid.NewID()
	delegating := mustNewTask(t, delegatingID, agent.Prompt{Text: "delegating task"}, harness.Claude)
	insertTestTask(s, delegatingID.String(), delegating)
	childID := ksid.NewID()
	child := mustNewTask(t, childID, agent.Prompt{Text: "child task"}, harness.Claude)
	child.ParentTaskID = delegatingID
	insertTestTask(s, childID.String(), child)
	foreignID := ksid.NewID()
	foreign := mustNewTask(t, foreignID, agent.Prompt{Text: "foreign task"}, harness.Claude)
	foreign.OwnerID = "other"
	insertTestTask(s, foreignID.String(), foreign)

	post := func(t *testing.T, ctx context.Context, method, name, params string) string {
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(mcpRequestJSON(method, params)))
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", method)
		if name != "" {
			req.Header.Set("Mcp-Name", name)
		}
		w := httptest.NewRecorder()
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

			streamCtx, cancel := context.WithCancel(tc.ctx)
			cancel()
			subscription := post(t, streamCtx, "subscriptions/listen", "", `"notifications":{"resourceSubscriptions":["`+visibleURI+`"]}`)
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

	postMCP := func(t *testing.T, h *mcp.Handler, method, name, body string) (*httptest.ResponseRecorder, mcp.JSONRPCResponse) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(body))
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", method)
		if name != "" {
			req.Header.Set("Mcp-Name", name)
		}
		w := httptest.NewRecorder()
		h.HandleMCP(w, req)
		var resp mcp.JSONRPCResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		return w, resp
	}

	t.Run("repositoryChangeNotification", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		registry := &mcpRegistry{serverConfig: s.serverHandlers, taskSvc: s.taskHandlers.taskSvc}
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		updates, err := registry.SubscribeResourceUpdates(ctx, mcp.SubscriptionFilter{ResourceSubscriptions: []string{"caic://repos/repo"}})
		if err != nil {
			t.Fatal(err)
		}
		updateC := make(chan mcp.ResourceUpdate, 1)
		go func() {
			for update, iterErr := range updates {
				if iterErr == nil {
					updateC <- update
				}
				return
			}
		}()
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
		w, resp := postMCP(t, s.mcpHandlers.protocol, "server/discover", "", mcpRequestJSON("server/discover", `{}`))
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
		instructions, ok := result["instructions"].(string)
		if !ok {
			t.Fatalf("instructions type = %T", result["instructions"])
		}
		if strings.Contains(instructions, "[Current tasks at session start]") {
			t.Fatalf("instructions duplicate the client-owned task snapshot: %q", instructions)
		}
	})

	t.Run("serverDiscoverInstructionsDoNotIncludeTaskSnapshot", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		id := ksid.NewID()
		tk := mustNewTask(t, id, agent.Prompt{Text: "ship voice prompt"}, harness.Claude)
		insertTestTask(s, id.String(), tk)
		_, resp := postMCP(t, s.mcpHandlers.protocol, "server/discover", "", mcpRequestJSON("server/discover", `{}`))
		if resp.Error != nil {
			t.Fatalf("error = %#v", resp.Error)
		}
		result, ok := resp.Result.(map[string]any)
		if !ok {
			t.Fatalf("result type = %T", resp.Result)
		}
		instructions, ok := result["instructions"].(string)
		if !ok {
			t.Fatalf("instructions type = %T", result["instructions"])
		}
		for _, unwanted := range []string{"[Current tasks at session start]", "Task #1", "ship voice prompt"} {
			if strings.Contains(instructions, unwanted) {
				t.Fatalf("instructions include dynamic task data %q: %q", unwanted, instructions)
			}
		}
		if !strings.Contains(instructions, "follow nextCursor until it is absent") {
			t.Fatalf("instructions lack pagination guidance: %q", instructions)
		}
	})

	t.Run("serverDiscoverInstructionsKeepVoiceRepliesBriefAndReactive", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		registry, ok := s.mcpHandlers.protocol.Registry.(*mcpRegistry)
		if !ok {
			t.Fatalf("registry type = %T", s.mcpHandlers.protocol.Registry)
		}
		instructions, err := registry.Instructions(t.Context())
		if err != nil {
			t.Fatalf("Instructions() error: %v", err)
		}
		for _, want := range []string{
			"one or two short sentences",
			"Never ask a follow-up or confirmation",
			"Do not volunteer ideas, next steps, related actions, or offers",
			"Notify the user only when an agent enters the waiting, asking, failed, or crashed state",
			"Do not notify on any other state transition",
			"When StartupFailure is present, state its harness, phase, and cause exactly",
			"otherwise state Error exactly",
			"Never call it an unknown error or invent a cause",
		} {
			if !strings.Contains(instructions, want) {
				t.Errorf("instructions missing %q: %q", want, instructions)
			}
		}
	})

	t.Run("serverDiscoverInstructionsHideTaskSnapshotWithoutScope", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		id := ksid.NewID()
		tk := mustNewTask(t, id, agent.Prompt{Text: "private task prompt"}, harness.Claude)
		insertTestTask(s, id.String(), tk)
		registry, ok := s.mcpHandlers.protocol.Registry.(*mcpRegistry)
		if !ok {
			t.Fatalf("registry type = %T", s.mcpHandlers.protocol.Registry)
		}
		ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{Scopes: []string{mcpScopeRead}, Remote: true})
		ctx = auth.NewContext(ctx, &auth.User{ID: "user-1"})
		instructions, err := registry.Instructions(ctx)
		if err != nil {
			t.Fatalf("Instructions() error: %v", err)
		}
		if strings.Contains(instructions, "private task prompt") || strings.Contains(instructions, "[Current tasks at session start]") {
			t.Fatalf("instructions disclose task snapshot: %q", instructions)
		}
	})

	t.Run("toolsList", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		w, resp := postMCP(t, s.mcpHandlers.protocol, "tools/list", "", mcpRequestJSON("tools/list", `{}`))
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
		w, resp := postMCP(t, s.mcpHandlers.protocol, "resources/templates/list", "", mcpRequestJSON("resources/templates/list", `{}`))
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
		cancel()
		body := mcpRequestJSON("subscriptions/listen", `"notifications":{"resourcesListChanged":true,"resourceSubscriptions":["gomode://items"]}`)
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(body))
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", "subscriptions/listen")
		w := httptest.NewRecorder()
		s.mcpHandlers.protocol.HandleMCP(w, req)
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
		// The context is pre-cancelled, so the change loop adds nothing and
		// the body is the ack, the announced list change, and the delivered
		// initial state.
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
		_, readResp := postMCP(t, s.mcpHandlers.protocol, "resources/read", "gomode://items", mcpRequestJSON("resources/read", `"uri":"gomode://items"`))
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
		cancel()
		body := mcpRequestJSON("subscriptions/listen", `"notifications":{"resourceSubscriptions":["gomode://notifications"]}`)
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(body))
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", "subscriptions/listen")
		w := httptest.NewRecorder()
		s.mcpHandlers.protocol.HandleMCP(w, req)
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
		w := httptest.NewRecorder()
		s.mcpHandlers.protocol.HandleMCP(w, req)
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

	// A subscribed URI whose scope check passes but whose initial read
	// fails sends no initial state for it and falls back to the legacy
	// re-read burst.
	t.Run("subscriptionsListenFallsBackWhenInitialStateReadFails", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		body := mcpRequestJSON("subscriptions/listen", `"notifications":{"resourceSubscriptions":["caic://repos/ghost"]}`)
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(body))
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", "subscriptions/listen")
		w := httptest.NewRecorder()
		s.mcpHandlers.protocol.HandleMCP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		data := w.Body.String()
		ackMethod := `"method":"notifications/subscriptions/acknowledged"`
		if !strings.Contains(data, ackMethod) {
			t.Fatalf("subscription response = %s, want acknowledgment", data)
		}
		if strings.Contains(data, `"method":"notifications/subscriptions/initial_state"`) {
			t.Fatalf("subscription response = %s, want no initial state for the unreadable resource", data)
		}
		ackIndex := strings.Index(data, ackMethod)
		burstIndex := strings.Index(data, `"method":"notifications/resources/updated"`)
		if burstIndex < 0 {
			t.Fatalf("subscription response = %s, want resources/updated fallback for the unreadable resource", data)
		}
		if ackIndex < 0 || ackIndex > burstIndex {
			t.Fatalf("subscription response = %s, want acknowledgment before the resources/updated fallback", data)
		}
		if !strings.Contains(data, `"uri":"caic://repos/ghost"`) {
			t.Fatalf("subscription response = %s, want the fallback to target caic://repos/ghost", data)
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
			insertTestTask(s, id.String(), mustNewTask(t, id, agent.Prompt{Text: "subscription initial state item"}, harness.Claude))
			seeded = append(seeded, id.String())
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		body := mcpRequestJSON("subscriptions/listen", `"notifications":{"resourceSubscriptions":["gomode://items"]}`)
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(body))
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", "subscriptions/listen")
		w := httptest.NewRecorder()
		s.mcpHandlers.protocol.HandleMCP(w, req)
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
		var streamItems serviceItemsOutput
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
		_, readResp := postMCP(t, s.mcpHandlers.protocol, "resources/read", "gomode://items", mcpRequestJSON("resources/read", `"uri":"gomode://items"`))
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
		s := newTestRouterWithAuthHost(t, store, "", auth.NewHostState("https://caic.example.com", nil))
		mineID := ksid.NewID()
		mine := mustNewTask(t, mineID, agent.Prompt{Text: "subscription initial state item owned"}, harness.Claude)
		mine.OwnerID = user.ID
		insertTestTask(s, mineID.String(), mine)
		otherID := ksid.NewID()
		other := mustNewTask(t, otherID, agent.Prompt{Text: "subscription initial state item foreign"}, harness.Claude)
		other.OwnerID = "other-owner"
		insertTestTask(s, otherID.String(), other)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		ctx = auth.NewContext(ctx, &user)
		body := mcpRequestJSON("subscriptions/listen", `"notifications":{"resourceSubscriptions":["gomode://items"]}`)
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(body))
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", "subscriptions/listen")
		w := httptest.NewRecorder()
		s.mcpHandlers.protocol.HandleMCP(w, req)
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
		var streamItems serviceItemsOutput
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

	t.Run("subscriptionsListenRejectsEmptyFilter", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		body := mcpRequestJSON("subscriptions/listen", `"notifications":{}`)
		_, resp := postMCP(t, s.mcpHandlers.protocol, "subscriptions/listen", "", body)
		if resp.Error == nil {
			t.Fatal("error is nil, want invalid params")
		}
		if resp.Error.Code != mcp.InvalidParamsCode {
			t.Fatalf("error code = %d, want %d", resp.Error.Code, mcp.InvalidParamsCode)
		}
		if !strings.Contains(resp.Error.Message, "empty") {
			t.Fatalf("error message = %q, want empty filter", resp.Error.Message)
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
		changes, err := registry.SubscribeResourceUpdates(ctx, mcp.SubscriptionFilter{ResourceSubscriptions: []string{"gomode://items", "gomode://notifications"}})
		if err != nil {
			t.Fatal(err)
		}
		got := make(chan mcp.ResourceUpdate, 1)
		done := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() {
			defer close(done)
			for update := range changes {
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

	t.Run("subscriptionDoesNotMissTaskChangeWhileDeliveringPreviousChange", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		id := ksid.NewID()
		tk := mustNewTask(t, id, agent.Prompt{Text: "subscription state changes"}, harness.Claude)
		insertTestTask(s, id.String(), tk)
		registry, ok := s.mcpHandlers.protocol.Registry.(*mcpRegistry)
		if !ok {
			t.Fatalf("registry type = %T", s.mcpHandlers.protocol.Registry)
		}
		ctx, cancel := context.WithCancel(t.Context())
		changes, err := registry.SubscribeResourceUpdates(ctx, mcp.SubscriptionFilter{ResourceSubscriptions: []string{"gomode://items"}})
		if err != nil {
			t.Fatal(err)
		}
		updates := make(chan mcp.ResourceUpdate, 2)
		done := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() {
			defer close(done)
			count := 0
			for update := range changes {
				updates <- update
				count++
				if count == 1 {
					tk.SetState(taskslog.StateWaiting)
					s.taskMgr.NotifyTaskChange()
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

		tk.SetState(taskslog.StateRunning)
		s.taskMgr.NotifyTaskChange()
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

	t.Run("toolsCall", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		id := ksid.NewID()
		tk := mustNewTask(t, id, agent.Prompt{Text: "test"}, harness.Claude)
		tk.SetTitle("Fix tests")
		tk.SetState(taskslog.StateWaiting)
		insertTestTask(s, id.String(), tk)

		body := mcpRequestJSON("tools/call", `"name":"tasks_list","arguments":{},"inputResponses":{},"requestState":"retry-state"`)
		body = strings.Replace(body, `"io.modelcontextprotocol/clientCapabilities":{}`, `"io.modelcontextprotocol/clientCapabilities":{},"example.com/clientTrace":"trace-1"`, 1)
		w, resp := postMCP(t, s.mcpHandlers.protocol, "tools/call", "tasks_list", body)
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
		s.mcpHandlers.protocol.HandleMCP(w, req)
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
		w, resp := postMCP(t, s.mcpHandlers.protocol, "tools/call", "task_fork", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		if resp.Error == nil || resp.Error.Code != mcp.InvalidRequestCode {
			t.Fatalf("error = %#v, want invalid request", resp.Error)
		}
	})

	t.Run("toolExecutionErrorOmitsStructuredContent", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		body := mcpRequestJSON("tools/call", `"name":"task_get_detail","arguments":{"task":1}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(body))
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", "tools/call")
		req.Header.Set("Mcp-Name", "task_get_detail")
		w := httptest.NewRecorder()
		s.mcpHandlers.protocol.HandleMCP(w, req)
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
		if _, ok := result["structuredContent"]; ok {
			t.Fatalf("structuredContent present on tool execution error: %#v", result["structuredContent"])
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
		s.mcpHandlers.protocol.HandleMCP(w, req)
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
		w, resp := postMCP(t, s.mcpHandlers.protocol, "initialize", "", mcpRequestJSON("initialize", `{}`))
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
		}
		if resp.Error == nil || resp.Error.Code != -32601 {
			t.Fatalf("error = %#v, want method not found", resp.Error)
		}
	})

	// A registry fault (e.g. a backend lookup failing while building the catalog)
	// must surface as an internal error, not as invalid params.
	t.Run("toolCallBackendError", func(t *testing.T) {
		t.Parallel()
		h := &mcp.Handler{Registry: mcptest.FakeRegistry{CallErr: errors.New("backend unavailable")}, ServerInfo: mcp.Implementation{Name: "caic"}}
		body := mcpRequestJSON("tools/call", `"name":"tasks_list","arguments":{}`)
		w, resp := postMCP(t, h, "tools/call", "tasks_list", body)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		if resp.Error == nil || resp.Error.Code != mcp.InternalErrorCode {
			t.Fatalf("error = %#v, want internal error", resp.Error)
		}
	})

	// Bad client input (unknown tool) stays an invalid-params error.
	t.Run("toolCallInvalidParams", func(t *testing.T) {
		t.Parallel()
		h := &mcp.Handler{Registry: mcptest.FakeRegistry{CallErr: mcp.ErrInvalidParams("unknown tool: nope")}, ServerInfo: mcp.Implementation{Name: "caic"}}
		body := mcpRequestJSON("tools/call", `"name":"nope","arguments":{}`)
		_, resp := postMCP(t, h, "tools/call", "nope", body)
		if resp.Error == nil || resp.Error.Code != mcp.InvalidParamsCode {
			t.Fatalf("error = %#v, want invalid params", resp.Error)
		}
	})

	// Client-caused resource errors are invalid params; catalog/backend faults are internal.
	t.Run("resourceErrors", func(t *testing.T) {
		t.Parallel()

		cases := []struct {
			name     string
			registry mcptest.FakeRegistry
			method   string
			resource string
			params   string
			want     mcp.ErrorCode
		}{
			{name: "read backend", registry: mcptest.FakeRegistry{ReadErr: errors.New("snapshot failed")}, method: "resources/read", resource: "caic://tasks", params: `"uri":"caic://tasks"`, want: mcp.InternalErrorCode},
			{name: "read unknown", registry: mcptest.FakeRegistry{ReadErr: mcp.ErrInvalidParams("unknown resource: caic://nope")}, method: "resources/read", resource: "caic://nope", params: `"uri":"caic://nope"`, want: mcp.InvalidParamsCode},
			{name: "list catalog", registry: mcptest.FakeRegistry{ListErr: errors.New("invalid server catalog")}, method: "resources/list", want: mcp.InternalErrorCode},
			{name: "list cursor", registry: mcptest.FakeRegistry{ListErr: mcp.ErrInvalidParams("invalid cursor")}, method: "resources/list", params: `"cursor":"bad"`, want: mcp.InvalidParamsCode},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				h := &mcp.Handler{Registry: tc.registry, ServerInfo: mcp.Implementation{Name: "caic"}}
				_, resp := postMCP(t, h, tc.method, tc.resource, mcpRequestJSON(tc.method, tc.params))
				if resp.Error == nil || resp.Error.Code != tc.want {
					t.Fatalf("error = %#v, want code %d", resp.Error, tc.want)
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

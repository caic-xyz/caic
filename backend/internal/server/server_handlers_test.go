// Tests server configuration and preferences HTTP handlers.

package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/server/api"
)

func TestServerHandlersListHarnesses(t *testing.T) {
	t.Parallel()

	s := newTestRouter(t)
	s.taskMgr.RegisterBackends(map[harness.Name]agent.Backend{
		harness.Codex: &agenttest.FakeBackend{
			Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "gpt-5", EffortOptions: []string{"low", "ultra"}}}},
		},
	})

	got, err := s.serverHandlers.listHarnesses(t.Context(), &api.EmptyReq{})
	if err != nil {
		t.Fatalf("listHarnesses: %v", err)
	}
	if len(*got) != 1 {
		t.Fatalf("len(harnesses) = %d, want 1", len(*got))
	}
	models := (*got)[0].Models
	if len(models) != 1 || models[0].ID != "gpt-5" || len(models[0].EffortOptions) != 2 {
		t.Fatalf("models = %#v, want gpt-5 with [low ultra]", models)
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal harnesses: %v", err)
	}
	if strings.Contains(string(data), "null") {
		t.Fatalf("harnesses JSON = %s, want arrays instead of null", data)
	}
	if !strings.Contains(string(data), `"models":[{"id":"gpt-5"`) {
		t.Fatalf("harnesses JSON = %s, want gpt-5 in models", data)
	}
	if strings.Contains(string(data), `"modelCapabilities":`) || strings.Contains(string(data), `"modes":`) {
		t.Fatalf("harnesses JSON = %s, want models as the only model configuration", data)
	}
}

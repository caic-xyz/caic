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
			ModelList: []string{"gpt-5"},
			Efforts:   []string{"low", "ultra"},
		},
	})

	got, err := s.serverHandlers.listHarnesses(t.Context(), &api.EmptyReq{})
	if err != nil {
		t.Fatalf("listHarnesses: %v", err)
	}
	if len(*got) != 1 {
		t.Fatalf("len(harnesses) = %d, want 1", len(*got))
	}
	if efforts := (*got)[0].EffortOptions; len(efforts) != 2 || efforts[0] != "low" || efforts[1] != "ultra" {
		t.Fatalf("effortOptions = %v, want [low ultra]", efforts)
	}
	capabilities := (*got)[0].ModelCapabilities
	if len(capabilities) != 1 || capabilities[0].Model != "gpt-5" || len(capabilities[0].EffortOptions) != 2 {
		t.Fatalf("modelCapabilities = %#v, want gpt-5 with [low ultra]", capabilities)
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal harnesses: %v", err)
	}
	if strings.Contains(string(data), "null") {
		t.Fatalf("harnesses JSON = %s, want arrays instead of null", data)
	}
}

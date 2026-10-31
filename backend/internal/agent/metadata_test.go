// Tests ordered root metadata updates and restoration precedence.

package agent

import "testing"

func TestSessionMetadata(t *testing.T) {
	t.Parallel()
	t.Run("Update", func(t *testing.T) {
		t.Parallel()
		m := SessionMetadata{SessionID: "first", ReportedModel: "model", ReportedEffort: "high", AgentVersion: "1", AutoCompactTokenLimit: 100}
		m.Update(SessionMetadata{})
		if m.SessionID != "first" || m.ReportedModel != "model" || m.ReportedEffort != "high" || m.AgentVersion != "1" || m.AutoCompactTokenLimit != 100 {
			t.Fatalf("empty report erased known metadata: %+v", m)
		}
		m.Update(SessionMetadata{SessionID: "next", ReportedModel: "new-model", ReportedEffort: "low", AgentVersion: "2", AutoCompactTokenLimit: 200})
		if m.SessionID != "next" || m.ReportedModel != "new-model" || m.ReportedEffort != "low" || m.AgentVersion != "2" || m.AutoCompactTokenLimit != 200 {
			t.Fatalf("new report did not replace known metadata: %+v", m)
		}
	})
	t.Run("Apply", func(t *testing.T) {
		t.Parallel()
		m := SessionMetadata{SessionID: "root", ReportedModel: "first", ReportedEffort: "high", AgentVersion: "1", AutoCompactTokenLimit: 100}
		child := MessageProvenance{Source: MessageSource{DelegationToolUseID: "child"}}
		for _, msg := range []Message{
			&SystemMessage{MessageProvenance: child, Subtype: SystemSubtypeModelRerouted, ReportedModel: "child-model"},
			&SystemMessage{MessageProvenance: child, Subtype: messageSubtypeContextCleared},
		} {
			m.Apply(msg)
			if m.SessionID != "root" || m.ReportedModel != "first" {
				t.Fatalf("child event changed root metadata: %+v", m)
			}
		}
		m.Apply(&SystemMessage{Subtype: SystemSubtypeModelRerouted, ReportedModel: "next"})
		m.Apply(&SystemMessage{Subtype: SystemSubtypeModelRerouted})
		m.Apply(ContextCleared())
		want := SessionMetadata{ReportedModel: "next", ReportedEffort: "high", AgentVersion: "1", AutoCompactTokenLimit: 100}
		if m != want {
			t.Fatalf("root reroute and boundary = %+v, want %+v", m, want)
		}
	})
	t.Run("authoritative partial reports and restoration", func(t *testing.T) {
		t.Parallel()
		m := SessionMetadata{SessionID: "root", ReportedModel: "old", ReportedEffort: "low", AgentVersion: "1"}
		m.Apply(&SessionMetadataMessage{ReportedModel: "new", ReportedEffort: "high"})
		m.Apply(&SessionMetadataMessage{Source: MessageSource{DelegationToolUseID: "child"}, ReportedModel: "child", AgentVersion: "child"})
		m.Apply(&MetaSessionMessage{SessionID: "root", ReportedModel: "stale", ReportedEffort: "low"})
		want := SessionMetadata{SessionID: "root", ReportedModel: "new", ReportedEffort: "high", AgentVersion: "1"}
		if m != want {
			t.Fatalf("partial metadata = %+v, want %+v", m, want)
		}
	})
	t.Run("Restore", func(t *testing.T) {
		t.Parallel()
		m := SessionMetadata{ReportedModel: "native-model", ReportedEffort: "high"}
		m.Restore(SessionMetadata{SessionID: "restored", ReportedModel: "old-model", ReportedEffort: "low", AgentVersion: "1", AutoCompactTokenLimit: 100})
		if m.SessionID != "restored" || m.ReportedModel != "native-model" || m.ReportedEffort != "high" || m.AgentVersion != "1" || m.AutoCompactTokenLimit != 100 {
			t.Fatalf("restoration replaced native model or effort: %+v", m)
		}
		m.Restore(SessionMetadata{SessionID: "next", AgentVersion: "2", AutoCompactTokenLimit: 200})
		if m.SessionID != "next" || m.AgentVersion != "2" || m.AutoCompactTokenLimit != 200 {
			t.Fatalf("restoration did not update session settings: %+v", m)
		}
	})
}

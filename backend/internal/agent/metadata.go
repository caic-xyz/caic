// Runtime session metadata and ordered root-event policies shared by task loading and execution.

package agent

// SessionMetadata describes the active harness session independently of wire and disk schemas.
type SessionMetadata struct {
	SessionID             string // Harness-native session or thread ID.
	ReportedModel         string
	ReportedEffort        string
	AgentVersion          string
	AutoCompactTokenLimit int // Configured session threshold; zero means unknown.
}

// Apply observes one ordered root-session event. Native reports replace known
// values; session controls restore model and effort only while they are unknown.
// A context clear releases the session identity, retaining configured settings.
func (m *SessionMetadata) Apply(msg Message) {
	if msg.Provenance().IsChild() {
		return
	}
	switch src := msg.(type) {
	case *InitMessage:
		m.Update(SessionMetadata{
			SessionID: src.SessionID, ReportedModel: src.ReportedModel,
			ReportedEffort: src.ReportedEffort, AgentVersion: src.Version,
			AutoCompactTokenLimit: src.AutoCompactTokenLimit,
		})
	case *MetaSessionMessage:
		m.Restore(SessionMetadata{
			SessionID: src.SessionID, ReportedModel: src.ReportedModel,
			ReportedEffort: src.ReportedEffort, AgentVersion: src.AgentVersion,
			AutoCompactTokenLimit: src.AutoCompactTokenLimit,
		})
	case *SystemMessage:
		switch src.Subtype {
		case SystemSubtypeModelRerouted:
			if src.ReportedModel != "" {
				m.ReportedModel = src.ReportedModel
			}
		case messageSubtypeContextCleared:
			m.SessionID = ""
		}
	case *UsageMessage:
		if src.ReportedModel != "" {
			m.ReportedModel = src.ReportedModel
		}
	}
}

// Restore updates persisted session identity and settings without replacing an
// already observed model or effort with restoration defaults.
func (m *SessionMetadata) Restore(src SessionMetadata) {
	if m.ReportedModel != "" {
		src.ReportedModel = ""
	}
	if m.ReportedEffort != "" {
		src.ReportedEffort = ""
	}
	m.Update(src)
}

// Update applies known incoming values, preserving fields omitted by a partial report.
func (m *SessionMetadata) Update(src SessionMetadata) {
	if src.SessionID != "" {
		m.SessionID = src.SessionID
	}
	if src.ReportedModel != "" {
		m.ReportedModel = src.ReportedModel
	}
	if src.ReportedEffort != "" {
		m.ReportedEffort = src.ReportedEffort
	}
	if src.AgentVersion != "" {
		m.AgentVersion = src.AgentVersion
	}
	if src.AutoCompactTokenLimit > 0 {
		m.AutoCompactTokenLimit = src.AutoCompactTokenLimit
	}
}

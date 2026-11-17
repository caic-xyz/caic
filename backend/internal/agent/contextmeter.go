// Root context meter shared by the live task and its history projection, so both fold records into one context size.

package agent

// ContextMeter folds root records into the context size: the latest native
// occupancy when the harness reports one, else the input of the latest API
// call. The zero value knows neither.
type ContextMeter struct {
	// Used is the latest native occupancy; nil means unknown.
	Used *int
	// API is the latest API call's usage; its input classes fill the context.
	API Usage
}

// Observe folds one record. priorSessionID is the root session before msg, as
// SessionMetadata tracks it. A new session, a compaction, or a context clear
// discards native occupancy; a boundary that reports its resulting size
// becomes the API snapshot. A usage record replaces the API snapshot unless it
// reports only native occupancy: no token class and not one API call.
func (c *ContextMeter) Observe(msg Message, priorSessionID string) ContextChange {
	if msg.Provenance().IsChild() {
		return ContextUnchanged
	}
	switch m := msg.(type) {
	case *InitMessage:
		if m.SessionID == priorSessionID {
			return ContextUnchanged
		}
		c.Used = nil
		return ContextObserved
	case *UsageMessage:
		change := ContextObserved
		if m.ContextUsed == nil || m.PerCall || m.Usage.HasTokens() {
			c.API = m.Usage
			change = ContextAPICall
		}
		if m.ContextUsed != nil {
			c.Used = new(*m.ContextUsed)
		}
		return change
	case *SystemMessage:
		if m.Subtype != SystemSubtypeCompactBoundary && m.Subtype != SystemSubtypeContextCleared {
			return ContextUnchanged
		}
		c.Used = nil
		if m.ContextTokensAfter > 0 {
			c.API = Usage{InputTokens: int(m.ContextTokensAfter)}
		}
		return ContextObserved
	default:
		return ContextUnchanged
	}
}

// Tokens is the context size; zero is unknown.
func (c *ContextMeter) Tokens() int {
	if c.Used != nil {
		return *c.Used
	}
	return c.API.InputTokens + c.API.CacheCreationInputTokens + c.API.CacheReadInputTokens
}

// ContextChange is what one record did to a ContextMeter.
type ContextChange uint8

const (
	// ContextUnchanged is a record that is not a root context observation.
	ContextUnchanged ContextChange = iota
	// ContextObserved is a session start, boundary, or occupancy report.
	ContextObserved
	// ContextAPICall is a usage record that replaced the API-call snapshot.
	ContextAPICall
)

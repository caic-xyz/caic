// Frames projected events and resolves namespace-aware activity ownership for immutable resources.

package history

import (
	"bufio"
	"encoding/json"
	"io"
	"iter"

	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

func activityOwner(src *v1.EventMessageSource, routes []activityRoute, pending map[string]int) string {
	if src.AgentID != "" {
		return src.AgentID
	}
	if src.DelegationToolUseID == "" {
		return ""
	}
	namespace := src.RootThreadID
	if namespace == "" {
		namespace = src.ThreadID
	}
	owner := ""
	for _, a := range routes {
		if a.tool != src.DelegationToolUseID {
			continue
		}
		if namespace != "" && a.namespace != "" && a.namespace != namespace {
			continue
		}
		if a.namespace == "" && pending[src.DelegationToolUseID] > 1 {
			continue
		}
		if owner != "" && owner != a.id {
			return ""
		}
		owner = a.id
	}
	return owner
}

func writeHistoryEvents(w io.Writer, events iter.Seq2[Event, error]) error {
	b := bufio.NewWriterSize(w, 32<<10)
	if _, err := b.WriteString("[\n"); err != nil {
		return err
	}
	encoder := json.NewEncoder(b)
	comma := false
	for e, err := range events {
		if err != nil {
			return err
		}
		if comma {
			if err := b.WriteByte(','); err != nil {
				return err
			}
		}
		if err := encoder.Encode(e); err != nil {
			return err
		}
		comma = true
	}
	if _, err := b.WriteString("]\n"); err != nil {
		return err
	}
	return b.Flush()
}

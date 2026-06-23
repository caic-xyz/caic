// Tool display normalization for API event conversion.

package v1conv

import (
	"encoding/json"
	"path"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/agent"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

// ToolUseDisplay returns backend-normalized display data for a tool call.
func ToolUseDisplay(name string, input json.RawMessage, view *agent.ToolInputView) (detail string, inputView v1.EventToolInputView) {
	if view != nil && view.Kind != "" {
		return "", EventToolInputView(view)
	}
	lower := strings.ToLower(name)
	switch lower {
	case "read", "write", "listdirectory":
		return basename(pathField(input)), v1.EventToolInputView{}
	case "edit":
		return basename(pathField(input)), v1.EventToolInputView{}
	case "bash":
		return strings.TrimLeft(stringField(input, "command"), " \t\r\n"), v1.EventToolInputView{}
	case "grep", "glob":
		return stringField(input, "pattern"), v1.EventToolInputView{}
	case "task", "agent", "subagent":
		return stringField(input, "description"), v1.EventToolInputView{}
	case "webfetch":
		return stringField(input, "url"), v1.EventToolInputView{}
	case "websearch":
		return stringField(input, "query"), v1.EventToolInputView{}
	case "notebookedit":
		return basename(stringField(input, "notebook_path")), v1.EventToolInputView{}
	default:
		return "", v1.EventToolInputView{}
	}
}

// EventToolInputView converts the backend-domain tool input view to an API DTO.
func EventToolInputView(view *agent.ToolInputView) v1.EventToolInputView {
	if view == nil {
		return v1.EventToolInputView{}
	}
	out := v1.EventToolInputView{Kind: v1.EventToolInputKind(view.Kind)}
	if len(view.Files) > 0 {
		out.Files = make([]v1.EventFileChange, len(view.Files))
		for i, file := range view.Files {
			out.Files[i] = v1.EventFileChange{Path: file.Path, Patch: file.Patch}
		}
	}
	if len(view.Subagents) > 0 {
		out.Subagents = make([]v1.EventSubagentSpawn, len(view.Subagents))
		for i, s := range view.Subagents {
			out.Subagents[i] = v1.EventSubagentSpawn{
				Agent: s.Agent,
				Task:  s.Task,
				Label: s.Label,
				Phase: s.Phase,
			}
		}
	}
	return out
}

func pathField(input json.RawMessage) string {
	fields := objectFields(input)
	if fields == nil {
		return ""
	}
	return pathFromFields(fields)
}

func pathFromFields(fields map[string]json.RawMessage) string {
	if p, ok := stringFieldFromMap(fields, "path"); ok {
		return p
	}
	if p, ok := stringFieldFromMap(fields, "file_path"); ok {
		return p
	}
	return ""
}

func stringField(input json.RawMessage, key string) string {
	fields := objectFields(input)
	if fields == nil {
		return ""
	}
	value, _ := stringFieldFromMap(fields, key)
	return value
}

func stringFieldFromMap(fields map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := fields[key]
	if !ok {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func objectFields(input json.RawMessage) map[string]json.RawMessage {
	if len(input) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(input, &fields); err != nil {
		return nil
	}
	return fields
}

func basename(p string) string {
	if p == "" {
		return ""
	}
	return path.Base(p)
}

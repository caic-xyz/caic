// Agent event conversions from backend messages to API v1 SSE DTOs.

package apiconv

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

// InputTruncateThreshold is the maximum byte length of a tool input JSON before it
// is omitted from the SSE stream. Clients fetch the full input on demand via
// GET /api/caic/v1/tasks/{id}/tool/{toolUseID}.
const InputTruncateThreshold = 4096

// ToolOutputFormatter analyzes a tool output string and returns its content
// type along with an optional formatted version.
type ToolOutputFormatter func(raw string) (contentType v1.ToolOutputContentType, formatted string)

// ToolTimingTracker computes per-tool-call duration while converting agent
// messages to API events.
type ToolTimingTracker struct {
	harness          harness.Name
	formatToolOutput ToolOutputFormatter
	pending          map[string]time.Time
}

// NewToolTimingTracker creates a tracker for converting agent messages from a
// harness into API events.
func NewToolTimingTracker(h harness.Name, f ToolOutputFormatter) *ToolTimingTracker {
	return &ToolTimingTracker{
		harness:          h,
		formatToolOutput: f,
		pending:          make(map[string]time.Time),
	}
}

// ConvertMessage converts an agent.Message into zero or more EventMessages.
func (tt *ToolTimingTracker) ConvertMessage(msg agent.Message, now time.Time) []v1.EventMessage {
	ts := now.UnixMilli()
	switch m := msg.(type) {
	case *agent.InitMessage:
		return []v1.EventMessage{{
			Kind: v1.EventKindInit,
			Ts:   ts,
			Init: &v1.EventInit{
				Model:        m.Model,
				Effort:       m.Effort,
				AgentVersion: m.Version,
				SessionID:    m.SessionID,
				Tools:        m.Tools,
				Cwd:          m.Cwd,
				Harness:      string(tt.harness),
			},
		}}
	case *agent.SystemMessage:
		return []v1.EventMessage{{
			Kind:   v1.EventKindSystem,
			Ts:     ts,
			System: &v1.EventSystem{Subtype: m.Subtype, Detail: m.Detail},
		}}
	case *agent.TextMessage:
		if m.Text != "" {
			// TODO: propagate m.Phase to EventText once EventText has a Phase field.
			return []v1.EventMessage{{
				Kind: v1.EventKindText,
				Ts:   ts,
				Text: &v1.EventText{Text: m.Text},
			}}
		}
		return nil
	case *agent.ToolUseMessage:
		tt.pending[m.ToolUseID] = now
		detail, inputView := ToolUseDisplay(m.Name, m.Input, &m.InputView)
		if m.Detail != "" {
			detail = m.Detail
		}
		input := m.Input
		truncated := false
		if len(input) > InputTruncateThreshold {
			input = nil
			truncated = true
		}
		var bg bool
		if len(m.Input) > 0 {
			var probe toolInputProbe
			if json.Unmarshal(m.Input, &probe) == nil {
				bg = probe.RunInBackground
			}
		}
		return []v1.EventMessage{{
			Kind: v1.EventKindToolUse,
			Ts:   ts,
			ToolUse: &v1.EventToolUse{
				ToolUseID:      m.ToolUseID,
				Name:           m.Name,
				Input:          input,
				Detail:         detail,
				InputView:      inputView,
				PlanContent:    m.PlanContent,
				InputTruncated: truncated,
				Background:     bg,
			},
		}}
	case *agent.AskMessage:
		tt.pending[m.ToolUseID] = now
		return []v1.EventMessage{{
			Kind: v1.EventKindAsk,
			Ts:   ts,
			Ask: &v1.EventAsk{
				ToolUseID: m.ToolUseID,
				Questions: AskQuestions(m.Questions),
			},
		}}
	case *agent.TodoMessage:
		tt.pending[m.ToolUseID] = now
		if todos := TodoItems(m.Todos); len(todos) > 0 {
			return []v1.EventMessage{{
				Kind: v1.EventKindTodo,
				Ts:   ts,
				Todo: &v1.EventTodo{ToolUseID: m.ToolUseID, Todos: todos},
			}}
		}
		return nil
	case *agent.UserInputMessage:
		if m.Text == "" && len(m.Images) == 0 {
			return nil
		}
		var images []v1.ImageData
		for _, img := range m.Images {
			images = append(images, v1.ImageData{MediaType: img.MediaType, Data: img.Data})
		}
		return []v1.EventMessage{{
			Kind:      v1.EventKindUserInput,
			Ts:        ts,
			UserInput: &v1.EventUserInput{Text: m.Text, Images: images},
		}}
	case *agent.ToolResultMessage:
		var duration float64
		if started, ok := tt.pending[m.ToolUseID]; ok {
			duration = now.Sub(started).Seconds()
			delete(tt.pending, m.ToolUseID)
		}
		return []v1.EventMessage{{
			Kind: v1.EventKindToolResult,
			Ts:   ts,
			ToolResult: &v1.EventToolResult{
				ToolUseID: m.ToolUseID,
				Duration:  duration,
				Error:     m.Error,
			},
		}}
	case *agent.UsageMessage:
		return []v1.EventMessage{{
			Kind: v1.EventKindUsage,
			Ts:   ts,
			Usage: &v1.EventUsage{
				InputTokens:              m.Usage.InputTokens,
				OutputTokens:             m.Usage.OutputTokens,
				CacheCreationInputTokens: m.Usage.CacheCreationInputTokens,
				CacheReadInputTokens:     m.Usage.CacheReadInputTokens,
				ReasoningOutputTokens:    m.Usage.ReasoningOutputTokens,
				Model:                    m.Model,
			},
		}}
	case *agent.ResultMessage:
		return []v1.EventMessage{{
			Kind: v1.EventKindResult,
			Ts:   ts,
			Result: &v1.EventResult{
				Subtype:      m.Subtype,
				IsError:      m.IsError,
				Result:       m.Result,
				DiffStat:     DiffStat(m.DiffStat),
				TotalCostUSD: m.TotalCostUSD,
				Duration:     float64(m.DurationMs) / 1e3,
				DurationAPI:  float64(m.DurationAPIMs) / 1e3,
				NumTurns:     m.NumTurns,
				Usage: v1.EventUsage{
					InputTokens:              m.Usage.InputTokens,
					OutputTokens:             m.Usage.OutputTokens,
					CacheCreationInputTokens: m.Usage.CacheCreationInputTokens,
					CacheReadInputTokens:     m.Usage.CacheReadInputTokens,
					ReasoningOutputTokens:    m.Usage.ReasoningOutputTokens,
				},
			},
		}}
	case *agent.TextDeltaMessage:
		if m.Text != "" {
			return []v1.EventMessage{{
				Kind:      v1.EventKindTextDelta,
				Ts:        ts,
				TextDelta: &v1.EventTextDelta{Text: m.Text},
			}}
		}
		return nil
	case *agent.ThinkingMessage:
		if m.Text != "" {
			return []v1.EventMessage{{
				Kind:     v1.EventKindThinking,
				Ts:       ts,
				Thinking: &v1.EventThinking{Text: m.Text},
			}}
		}
		return nil
	case *agent.ThinkingDeltaMessage:
		if m.Text != "" {
			return []v1.EventMessage{{
				Kind:          v1.EventKindThinkingDelta,
				Ts:            ts,
				ThinkingDelta: &v1.EventThinkingDelta{Text: m.Text},
			}}
		}
		return nil
	case *agent.SubagentStartMessage:
		return []v1.EventMessage{{
			Kind: v1.EventKindSubagentStart,
			Ts:   ts,
			SubagentStart: &v1.EventSubagentStart{
				TaskID:      m.TaskID,
				Description: m.Description,
			},
		}}
	case *agent.SubagentEndMessage:
		return []v1.EventMessage{{
			Kind: v1.EventKindSubagentEnd,
			Ts:   ts,
			SubagentEnd: &v1.EventSubagentEnd{
				TaskID: m.TaskID,
				Status: m.Status,
			},
		}}
	case *agent.DiffStatMessage:
		return []v1.EventMessage{{
			Kind:     v1.EventKindDiffStat,
			Ts:       ts,
			DiffStat: &v1.EventDiffStat{DiffStat: DiffStat(m.DiffStat)},
		}}
	case *agent.ParseErrorMessage:
		return []v1.EventMessage{{
			Kind:  v1.EventKindError,
			Ts:    ts,
			Error: &v1.EventError{Err: m.Err, Line: m.Line},
		}}
	case *agent.ExitMessage:
		if m.ExitCode == 0 {
			return nil
		}
		err := m.Error
		if err == "" {
			err = fmt.Sprintf("agent subprocess exited with code %d", m.ExitCode)
		}
		return []v1.EventMessage{{
			Kind:  v1.EventKindError,
			Ts:    ts,
			Error: &v1.EventError{Err: err},
		}}
	case *agent.LogMessage:
		return []v1.EventMessage{{
			Kind: v1.EventKindLog,
			Ts:   ts,
			Log:  &v1.EventLog{Line: m.Line},
		}}
	case *agent.ToolOutputDeltaMessage:
		if m.Delta != "" {
			ev := &v1.EventToolOutputDelta{
				ToolUseID: m.ToolUseID,
				Delta:     m.Delta,
			}
			if tt.formatToolOutput != nil {
				contentType, formatted := tt.formatToolOutput(m.Delta)
				if contentType != "" {
					ev.ContentType = contentType
				}
				if formatted != "" {
					ev.Formatted = formatted
				}
			}
			return []v1.EventMessage{{
				Kind:            v1.EventKindToolOutputDelta,
				Ts:              ts,
				ToolOutputDelta: ev,
			}}
		}
		return nil
	case *agent.WidgetMessage:
		tt.pending[m.ToolUseID] = now
		return []v1.EventMessage{{
			Kind: v1.EventKindWidget,
			Ts:   ts,
			Widget: &v1.EventWidget{
				ToolUseID: m.ToolUseID,
				Title:     m.Title,
				HTML:      m.HTML,
			},
		}}
	case *agent.WidgetDeltaMessage:
		if m.Delta != "" {
			return []v1.EventMessage{{
				Kind: v1.EventKindWidgetDelta,
				Ts:   ts,
				WidgetDelta: &v1.EventWidgetDelta{
					ToolUseID: m.ToolUseID,
					Delta:     m.Delta,
				},
			}}
		}
		return nil
	case *agent.RateLimitMessage:
		return []v1.EventMessage{{
			Kind: v1.EventKindRateLimit,
			Ts:   ts,
			RateLimit: &v1.EventRateLimit{
				Status:          v1.EventRateLimitStatus(m.Status),
				ResetsAt:        m.ResetsAt,
				RateLimitType:   m.RateLimitType,
				Utilization:     m.Utilization,
				IsUsingOverage:  m.IsUsingOverage,
				OverageResetsAt: m.OverageResetsAt,
			},
		}}
	default:
		return nil
	}
}

// AskQuestions converts agent ask questions to API DTOs.
func AskQuestions(qs []agent.AskQuestion) []v1.AskQuestion {
	if len(qs) == 0 {
		return nil
	}
	out := make([]v1.AskQuestion, len(qs))
	for i, q := range qs {
		opts := make([]v1.AskOption, len(q.Options))
		for j, o := range q.Options {
			opts[j] = v1.AskOption{Label: o.Label, Description: o.Description}
		}
		out[i] = v1.AskQuestion{
			Question:    q.Question,
			Header:      q.Header,
			Options:     opts,
			MultiSelect: q.MultiSelect,
		}
	}
	return out
}

// TodoItems converts agent todo items to API DTOs.
func TodoItems(items []agent.TodoItem) []v1.TodoItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]v1.TodoItem, len(items))
	for i, t := range items {
		out[i] = v1.TodoItem{Content: t.Content, Status: t.Status, ActiveForm: t.ActiveForm}
	}
	return out
}

// MarshalEvent marshals an EventMessage.
func MarshalEvent(ev *v1.EventMessage) ([]byte, error) {
	return json.Marshal(ev)
}

// toolInputProbe extracts run_in_background from a tool's raw JSON input.
type toolInputProbe struct {
	RunInBackground bool `json:"run_in_background"`
}

// ValidateEventJSON accepts exactly one recognized EventMessage payload
// matching its kind. Cache readers must reject JSON values that Unmarshal can
// otherwise silently coerce into a zero-value EventMessage.
func ValidateEventJSON(line []byte) error {
	if err := validateReplayJSON(line, reflect.TypeFor[v1.EventMessage]()); err != nil {
		return fmt.Errorf("invalid EventMessage schema: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	var event v1.EventMessage
	if err := decoder.Decode(&event); err != nil {
		return err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return err
	}
	payloads := 0
	for _, payload := range []bool{
		event.Init != nil, event.Text != nil, event.TextDelta != nil,
		event.ToolUse != nil, event.ToolResult != nil, event.Ask != nil,
		event.Usage != nil, event.Result != nil, event.System != nil,
		event.UserInput != nil, event.Todo != nil, event.DiffStat != nil,
		event.Error != nil, event.Thinking != nil, event.ThinkingDelta != nil,
		event.SubagentStart != nil, event.SubagentEnd != nil, event.Log != nil,
		event.ToolOutputDelta != nil, event.Widget != nil, event.WidgetDelta != nil,
		event.RateLimit != nil, event.Stats != nil,
	} {
		if payload {
			payloads++
		}
	}
	if payloads != 1 {
		return errors.New("EventMessage must contain exactly one payload")
	}
	var validPayload bool
	switch event.Kind {
	case v1.EventKindInit:
		validPayload = event.Init != nil
	case v1.EventKindText:
		validPayload = event.Text != nil
	case v1.EventKindTextDelta:
		validPayload = event.TextDelta != nil
	case v1.EventKindToolUse:
		validPayload = event.ToolUse != nil
	case v1.EventKindToolResult:
		validPayload = event.ToolResult != nil
	case v1.EventKindAsk:
		validPayload = event.Ask != nil
	case v1.EventKindUsage:
		validPayload = event.Usage != nil
	case v1.EventKindResult:
		validPayload = event.Result != nil
	case v1.EventKindSystem:
		validPayload = event.System != nil
	case v1.EventKindUserInput:
		validPayload = event.UserInput != nil
	case v1.EventKindTodo:
		validPayload = event.Todo != nil
	case v1.EventKindDiffStat:
		validPayload = event.DiffStat != nil
	case v1.EventKindError:
		validPayload = event.Error != nil
	case v1.EventKindThinking:
		validPayload = event.Thinking != nil
	case v1.EventKindThinkingDelta:
		validPayload = event.ThinkingDelta != nil
	case v1.EventKindSubagentStart:
		validPayload = event.SubagentStart != nil
	case v1.EventKindSubagentEnd:
		validPayload = event.SubagentEnd != nil
	case v1.EventKindLog:
		validPayload = event.Log != nil
	case v1.EventKindToolOutputDelta:
		validPayload = event.ToolOutputDelta != nil
	case v1.EventKindWidget:
		validPayload = event.Widget != nil
	case v1.EventKindWidgetDelta:
		validPayload = event.WidgetDelta != nil
	case v1.EventKindRateLimit:
		validPayload = event.RateLimit != nil
	case v1.EventKindStats:
		validPayload = event.Stats != nil
	default:
		return fmt.Errorf("unknown EventMessage kind %q", event.Kind)
	}
	if !validPayload {
		return fmt.Errorf("EventMessage kind %q has mismatched payload", event.Kind)
	}
	return nil
}

// validateReplayJSON recursively enforces the emitted EventMessage schema.
// encoding/json accepts unknown and duplicate object keys, so cache validation
// must inspect every nested object before decoding it into the public DTO.
var (
	jsonRawMessageType  = reflect.TypeFor[json.RawMessage]()
	jsonUnmarshalerType = reflect.TypeFor[json.Unmarshaler]()
)

func validateReplayJSON(data []byte, typ reflect.Type) error {
	if !json.Valid(data) {
		return errors.New("invalid JSON")
	}
	typ = indirectJSONType(typ)
	if typ == jsonRawMessageType || reflect.PointerTo(typ).Implements(jsonUnmarshalerType) {
		return validateReplayJSONUnknown(data)
	}
	if isJSONScalarType(typ) {
		if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
			return errors.New("scalar field must not be JSON null")
		}
		if err := json.Unmarshal(data, reflect.New(typ).Interface()); err != nil {
			return err
		}
		return nil
	}
	if typ.Kind() == reflect.Struct {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(data, &object); err != nil {
			return errors.New("must be a JSON object")
		}
		if object == nil {
			return errors.New("must be a JSON object")
		}
		fields := jsonSchemaFields(typ)
		seen, err := jsonObjectKeys(data)
		if err != nil {
			return err
		}
		for _, key := range seen {
			field, ok := fields[key]
			if !ok {
				return fmt.Errorf("unknown field %q", key)
			}
			if err := validateReplayJSON(object[key], field.typ); err != nil {
				return fmt.Errorf("field %q: %w", key, err)
			}
		}
		for name, field := range fields {
			if field.required {
				if _, ok := object[name]; !ok {
					return fmt.Errorf("missing required field %q", name)
				}
			}
		}
		return nil
	}
	switch typ.Kind() { //nolint:exhaustive // primitives have no nested keys.
	case reflect.Slice, reflect.Array:
		var values []json.RawMessage
		if err := json.Unmarshal(data, &values); err != nil {
			return err
		}
		for _, value := range values {
			if err := validateReplayJSON(value, typ.Elem()); err != nil {
				return err
			}
		}
	case reflect.Map, reflect.Interface:
		return validateReplayJSONUnknown(data)
	}
	return nil
}

type replayJSONField struct {
	typ      reflect.Type
	required bool
}

func jsonSchemaFields(typ reflect.Type) map[string]replayJSONField {
	fields := make(map[string]replayJSONField)
	for field := range typ.Fields() {
		if field.Anonymous {
			maps.Copy(fields, jsonSchemaFields(indirectJSONType(field.Type)))
			continue
		}
		tag := field.Tag.Get("json")
		name, options, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		fields[name] = replayJSONField{
			typ:      field.Type,
			required: !strings.Contains(options, "omitempty") && !strings.Contains(options, "omitzero"),
		}
	}
	return fields
}

func indirectJSONType(typ reflect.Type) reflect.Type {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	return typ
}

func isJSONScalarType(typ reflect.Type) bool {
	switch typ.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.String:
		return true
	default:
		return false
	}
}

// jsonObjectKeys returns object keys while detecting duplicates. Values remain
// raw so their own nested schemas can be checked by validateReplayJSON.
func jsonObjectKeys(data []byte) ([]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, errors.New("must be a JSON object")
	}
	keys := make([]string, 0)
	present := make(map[string]struct{})
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("invalid JSON object key")
		}
		if _, duplicate := present[key]; duplicate {
			return nil, fmt.Errorf("duplicate field %q", key)
		}
		present[key] = struct{}{}
		keys = append(keys, key)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	return keys, requireJSONEOF(decoder)
}

// validateReplayJSONUnknown permits arbitrary raw input values while still
// rejecting duplicate keys at every nested object level.
func validateReplayJSONUnknown(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := validateReplayJSONValue(decoder); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func validateReplayJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		present := make(map[string]struct{})
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok {
				return errors.New("invalid JSON object key")
			}
			if _, duplicate := present[key]; duplicate {
				return fmt.Errorf("duplicate field %q", key)
			}
			present[key] = struct{}{}
			if err := validateReplayJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := validateReplayJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return errors.New("invalid JSON delimiter")
	}
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

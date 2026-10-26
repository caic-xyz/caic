// Selects recent task previews from semantic messages or ordered API projections with complete text and tool context.

package server

import (
	"slices"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/apiconv"
)

// taskPreviewEventLimit counts visible semantic messages, excluding metadata and
// superseded streaming fragments. The latest response and missing tool starts
// may extend the preview past this limit.
const taskPreviewEventLimit = 50

type previewContentKey struct {
	source agent.MessageSource
	kind   v1.EventKind
	toolID string
}

type previewMessage struct {
	timed     agent.TimedMessage
	projected *v1.EventMessage
	position  int
	parts     []string // Newest first; assembled once after backward collection.
	complete  bool
}

type taskPreview struct {
	messages        []*previewMessage
	content         map[previewContentKey]*previewMessage
	tools           map[agent.ToolKey]struct{}
	response        bool
	visible         int
	boundaryVisible int
	position        int
}

func newTaskPreview() *taskPreview {
	return &taskPreview{content: make(map[previewContentKey]*previewMessage), tools: make(map[agent.ToolKey]struct{})}
}

func (p *taskPreview) add(timed agent.TimedMessage) bool {
	r := previewRecord{entry: previewMessage{timed: timed}, source: *timed.Message.Provenance()}
	switch m := timed.Message.(type) {
	case *agent.TextMessage:
		r.kind = v1.EventKindText
		r.text = m.Text
	case *agent.TextDeltaMessage:
		r.kind = v1.EventKindTextDelta
		r.text = m.Text
	case *agent.ThinkingMessage:
		r.kind = v1.EventKindThinking
		r.text = m.Text
	case *agent.ThinkingDeltaMessage:
		r.kind = v1.EventKindThinkingDelta
		r.text = m.Text
	case *agent.WidgetMessage:
		r.kind = v1.EventKindWidget
		r.text = m.HTML
		r.toolID = m.ToolUseID
	case *agent.WidgetDeltaMessage:
		r.kind = v1.EventKindWidgetDelta
		r.text = m.Delta
		r.toolID = m.ToolUseID
	case *agent.ToolOutputDeltaMessage:
		r.kind = v1.EventKindToolOutputDelta
		r.text = m.Delta
		r.toolID = m.ToolUseID
	case *agent.ToolResultMessage:
		r.kind = v1.EventKindToolResult
		r.toolID = m.ToolUseID
	case *agent.ToolUseMessage:
		r.kind = v1.EventKindToolUse
		r.toolID = m.ToolUseID
	case *agent.AskMessage:
		if len(m.Questions) > 0 {
			r.kind = v1.EventKindAsk
			r.toolID = m.ToolUseID
		}
	case *agent.TodoMessage:
		r.kind = v1.EventKindTodo
		r.toolID = m.ToolUseID
	case *agent.ResultMessage:
		r.kind = v1.EventKindResult
		r.text = m.Result
	case *agent.UserInputMessage:
		if m.Text != "" || len(m.Images) > 0 {
			r.kind = v1.EventKindUserInput
		}
	case *agent.InitMessage:
		r.kind = v1.EventKindInit
	case *agent.SystemMessage:
		if m.Subtype == agent.SystemSubtypeCompactBoundary || m.Subtype == "context_cleared" {
			r.kind = v1.EventKindSystem
		}
	case *agent.ParseErrorMessage:
		r.kind = v1.EventKindError
	case *agent.NativeSubagentMessage:
		r.kind = v1.EventKindNativeSubagent
	case *agent.BackgroundCommandMessage:
		r.kind = v1.EventKindBackgroundCommand
	}
	return p.addRecord(&r)
}

func projectedPreviewRecord(event *v1.EventMessage) previewRecord {
	r := previewRecord{entry: previewMessage{projected: event}, source: agent.MessageSource(event.Source), kind: event.Kind}
	if event.Ts != 0 {
		r.entry.timed.ProducerTime = time.UnixMilli(event.Ts)
	}
	switch event.Kind {
	case v1.EventKindText:
		r.text = event.Text.Text
	case v1.EventKindTextDelta:
		r.text = event.TextDelta.Text
	case v1.EventKindThinking:
		r.text = event.Thinking.Text
	case v1.EventKindThinkingDelta:
		r.text = event.ThinkingDelta.Text
	case v1.EventKindToolUse:
		r.toolID = event.ToolUse.ToolUseID
	case v1.EventKindToolResult:
		r.toolID = event.ToolResult.ToolUseID
	case v1.EventKindToolOutputDelta:
		r.text = event.ToolOutputDelta.Delta
		r.toolID = event.ToolOutputDelta.ToolUseID
	case v1.EventKindWidget:
		r.text = event.Widget.HTML
		r.toolID = event.Widget.ToolUseID
	case v1.EventKindWidgetDelta:
		r.text = event.WidgetDelta.Delta
		r.toolID = event.WidgetDelta.ToolUseID
	case v1.EventKindAsk:
		if len(event.Ask.Questions) == 0 {
			r.kind = ""
		}
		r.toolID = event.Ask.ToolUseID
	case v1.EventKindTodo:
		r.toolID = event.Todo.ToolUseID
	case v1.EventKindResult:
		r.text = event.Result.Result
	case v1.EventKindUserInput:
		if event.UserInput.Text == "" && len(event.UserInput.Images) == 0 {
			r.kind = ""
		}
	case v1.EventKindSystem:
		if event.System.Subtype != agent.SystemSubtypeCompactBoundary && event.System.Subtype != "context_cleared" {
			r.kind = ""
		}
	case v1.EventKindInit, v1.EventKindError, v1.EventKindNativeSubagent, v1.EventKindBackgroundCommand:
	default:
		r.kind = ""
	}
	return r
}

// addRecord consumes newest-first root messages, keeping the window plus its
// latest response and tool context. Child transcripts load with canonical history.
func (p *taskPreview) addRecord(r *previewRecord) bool {
	p.position++
	if r.kind == "" || r.source.IsChild() {
		return true
	}
	keep := p.visible < taskPreviewEventLimit
	response := false
	key := previewContentKey{source: r.source, toolID: r.toolID}
	complete := true
	tool := agent.ToolKeyForSource(&r.source, r.toolID)
	switch r.kind {
	case v1.EventKindText, v1.EventKindThinking:
		if r.text == "" && !r.source.HasIdentity() {
			return true
		}
		key.kind = r.kind
		response = r.kind == v1.EventKindText && r.text != ""
	case v1.EventKindTextDelta, v1.EventKindThinkingDelta:
		if r.text == "" {
			return true
		}
		complete = false
		key.kind = v1.EventKindThinking
		if r.kind == v1.EventKindTextDelta {
			key.kind = v1.EventKindText
			response = true
		}
	case v1.EventKindWidget, v1.EventKindWidgetDelta, v1.EventKindToolOutputDelta:
		if r.kind != v1.EventKindWidget && r.text == "" {
			return true
		}
		key.source = previewToolSource(&r.source)
		key.kind = v1.EventKindWidget
		complete = r.kind == v1.EventKindWidget
		if r.kind == v1.EventKindToolOutputDelta {
			key.kind = r.kind
		}
		if complete {
			_, needed := p.tools[tool]
			keep = keep || needed
			delete(p.tools, tool)
		}
	case v1.EventKindToolResult:
		if keep {
			p.tools[tool] = struct{}{}
			p.content[previewContentKey{source: previewToolSource(&r.source), kind: v1.EventKindToolOutputDelta, toolID: r.toolID}] = &previewMessage{complete: true}
		}
	case v1.EventKindToolUse, v1.EventKindAsk, v1.EventKindTodo:
		_, needed := p.tools[tool]
		if r.kind == v1.EventKindTodo {
			keep = needed
		} else {
			keep = keep || needed
		}
		delete(p.tools, tool)
		p.closeToolContent(&tool)
		response = r.kind == v1.EventKindAsk
	case v1.EventKindResult:
		response = r.text != ""
		p.closeContent()
	case v1.EventKindUserInput, v1.EventKindSystem:
		p.closeContent()
	case v1.EventKindInit:
		p.closeContent()
		if keep && (len(p.messages) == 0 || p.visible > p.boundaryVisible) {
			p.append(&r.entry, true)
			p.visible--
			p.boundaryVisible = p.visible
		}
		return !p.done()
	default:
		// Other visible records do not need content or tool reconstruction.
	}
	keep = keep || (response && !p.response)
	// Root ownership can be confirmed after an item starts; it is not part of
	// the frontend's message or tool identity. Preserve it on the actual event.
	key.source.RootThreadID = ""
	if key.kind != "" {
		if previous := p.content[key]; previous != nil {
			if !complete {
				if !previous.complete {
					previous.parts = append(previous.parts, r.text)
					previous.position = p.position
					previous.timed.ProducerTime = r.entry.timed.ProducerTime
				}
				return !p.done()
			}
			if key.source.HasIdentity() || key.toolID != "" {
				return !p.done()
			}
			previous.complete = true
		}
		if keep {
			entry := p.append(&r.entry, complete)
			if !complete {
				entry.parts = []string{r.text}
			}
			p.content[key] = entry
			if key.kind == v1.EventKindToolOutputDelta {
				p.tools[tool] = struct{}{}
			}
		}
	} else if keep {
		p.append(&r.entry, true)
	}
	if keep && response {
		p.response = true
	}
	return !p.done()
}

func (p *taskPreview) append(entry *previewMessage, complete bool) *previewMessage {
	m := *entry
	m.position = p.position
	m.complete = complete
	p.messages = append(p.messages, &m)
	p.visible++
	return &m
}

func (p *taskPreview) closeContent() {
	for key, m := range p.content {
		if key.toolID == "" {
			m.complete = true
			delete(p.content, key)
		}
	}
}

func (p *taskPreview) closeToolContent(tool *agent.ToolKey) {
	for key, m := range p.content {
		if key.toolID != "" && agent.ToolKeyForSource(&key.source, key.toolID) == *tool {
			m.complete = true
			delete(p.content, key)
		}
	}
}

func (p *taskPreview) done() bool {
	if p.visible < taskPreviewEventLimit || !p.response || len(p.tools) > 0 {
		return false
	}
	for _, m := range p.content {
		if !m.complete {
			return false
		}
	}
	return true
}

func (p *taskPreview) events(tracker *apiconv.ToolTimingTracker) []v1.EventMessage {
	slices.SortFunc(p.messages, func(a, b *previewMessage) int { return b.position - a.position })
	events := make([]v1.EventMessage, 0, len(p.messages))
	for _, entry := range p.messages {
		timed := entry.timed
		var text string
		if len(entry.parts) > 0 {
			slices.Reverse(entry.parts)
			text = strings.Join(entry.parts, "")
		}
		if entry.projected != nil {
			ev := *entry.projected
			if len(entry.parts) > 0 {
				ev.Ts = 0
				if !timed.ProducerTime.IsZero() {
					ev.Ts = timed.ProducerTime.UnixMilli()
				}
				switch ev.Kind {
				case v1.EventKindTextDelta:
					ev.TextDelta = &v1.EventTextDelta{Text: text}
				case v1.EventKindThinkingDelta:
					ev.ThinkingDelta = &v1.EventThinkingDelta{Text: text}
				case v1.EventKindToolOutputDelta:
					output := *ev.ToolOutputDelta
					output.Delta = text
					output.ContentType, output.Formatted = apiconv.FormatToolOutput(text)
					ev.ToolOutputDelta = &output
				case v1.EventKindWidgetDelta:
					widget := *ev.WidgetDelta
					widget.Delta = text
					ev.WidgetDelta = &widget
				default:
					// Parts are populated only for streaming delta records.
				}
			}
			events = append(events, ev)
			continue
		}
		if len(entry.parts) > 0 {
			switch m := timed.Message.(type) {
			case *agent.TextDeltaMessage:
				timed.Message = &agent.TextDeltaMessage{MessageProvenance: m.MessageProvenance, Text: text}
			case *agent.ThinkingDeltaMessage:
				timed.Message = &agent.ThinkingDeltaMessage{MessageProvenance: m.MessageProvenance, Text: text}
			case *agent.ToolOutputDeltaMessage:
				timed.Message = &agent.ToolOutputDeltaMessage{MessageProvenance: m.MessageProvenance, ToolUseID: m.ToolUseID, Delta: text}
			case *agent.WidgetDeltaMessage:
				timed.Message = &agent.WidgetDeltaMessage{MessageProvenance: m.MessageProvenance, ToolUseID: m.ToolUseID, Delta: text}
			}
		}
		projected := tracker.ConvertMessage(timed.Message, timed.ProducerTime)
		if result, ok := timed.Message.(*agent.ResultMessage); ok && result.Result == "" {
			// The selected window can omit intervening turn boundaries. Its
			// text cannot be used to reconstruct an empty native result.
			for i := range projected {
				if projected[i].Result != nil {
					projected[i].Result.Result = ""
				}
			}
		}
		events = append(events, projected...)
	}
	return events
}

// previewRecord describes selection independently of its semantic or already
// projected payload. Disk projections have been parsed and converted in order.
type previewRecord struct {
	entry  previewMessage
	source agent.MessageSource
	kind   v1.EventKind
	text   string
	toolID string
}

func previewToolSource(source *agent.MessageSource) agent.MessageSource {
	result := *source
	result.ItemID = ""
	result.RootThreadID = ""
	return result
}

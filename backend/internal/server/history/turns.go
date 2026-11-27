// Builds one root turn's timeline, completed or still running, from projected tool and question events, indexed lifecycles, and journaled per-call usage, with inferred model gaps, per-tool time, and caps.

package history

import (
	"cmp"
	"errors"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/history/data"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/usage"
)

// maxTurnSpans and maxTurnCalls bound a turn timeline response;
// v1.HistoryTurnTimeline documents the selections.
const (
	maxTurnSpans = 500
	maxTurnCalls = 500
)

// ErrUnknownTurn reports a turn number that is neither a completed root turn
// nor the running one.
var ErrUnknownTurn = errors.New("unknown history turn")

// newTurnBuilder collects the lifecycles of subagents and commands that start in
// root turn number: a completed turn, or the running turn through the projected
// record through. The caller holds the source lock.
func newTurnBuilder(x *sectionIndex, through uint64, number int) (*turnBuilder, error) {
	b := &turnBuilder{open: map[toolKey]int{}, pending: make(map[string]int, len(x.pendingScopes)), latest: map[v1.EventMessageSource]int{}}
	if running := x.runningTurn(); running != nil && number == running.Number {
		// The axis end is unknown until the timeline observes the turn's records.
		b.turn = v1.HistoryTurn{Number: number, SectionID: running.SectionID, StartedAt: running.StartedAt, Model: x.model}
		b.first, b.last, b.running = x.startPos, v1.HistoryPosition{RecordID: through, Index: RecordEndIndex}, true
		b.start, b.end = running.StartedAt.UnixMilli(), math.MaxInt64
	} else if number >= 1 && number <= len(x.turns) {
		b.turn, b.first = x.turns[number-1], x.turnStarts[number-1]
		b.last = b.turn.HistoryPosition
		b.start, b.end = b.turn.StartedAt.UnixMilli(), b.turn.At.UnixMilli()
	} else {
		return nil, ErrUnknownTurn
	}
	for route := range x.routing {
		b.routes = append(b.routes, route)
	}
	for tool, scopes := range x.pendingScopes {
		b.pending[tool] = len(scopes)
	}
	for _, id := range slices.Sorted(maps.Keys(x.activities)) {
		a := x.activities[id]
		if a.StartedAt != nil && b.within(a.StartedAt.UnixMilli()) {
			b.spans = append(b.spans, v1.HistoryTurnSpan{Kind: v1.HistoryTurnSpanKindSubagent, StartMS: a.StartedAt.UnixMilli() - b.start, EndMS: b.offset(a.EndedAt), Label: a.Label, Status: a.Status, Source: a.Source, ActivityID: a.ID})
		}
	}
	for i := range x.backgroundCommands {
		c := &x.backgroundCommands[i]
		if c.StartedAt != nil && b.within(c.StartedAt.UnixMilli()) {
			b.spans = append(b.spans, v1.HistoryTurnSpan{Kind: v1.HistoryTurnSpanKindBackground, StartMS: c.StartedAt.UnixMilli() - b.start, EndMS: b.offset(c.EndedAt), Label: c.Label, Status: c.Status, Source: c.Source, ToolUseID: c.ToolUseID, ActivityID: activityOwner(&c.Source, b.routes, b.pending), CommandID: c.ID})
		}
	}
	return b, nil
}

// turnBuilder folds one turn's projected events and journaled usage into its
// timeline.
type turnBuilder struct {
	turn  v1.HistoryTurn
	first v1.HistoryPosition
	last  v1.HistoryPosition
	start int64
	end   int64
	// running marks the running turn; observed is its latest observation time.
	running  bool
	observed int64
	spans    []v1.HistoryTurnSpan
	open     map[toolKey]int
	routes   []activityRoute
	pending  map[string]int
	calls    []turnCall
	// latest indexes each source's most recent call in calls.
	latest map[v1.EventMessageSource]int
	// window is the latest root context window the turn reports.
	window int
	// unattributed sums root usage reports that cover no single call.
	unattributed agent.Usage
}

// contains reports whether p lies in the turn's record range, from its first
// output through its result, or through the projected prefix while it runs.
func (b *turnBuilder) contains(p v1.HistoryPosition) bool {
	return !positionBefore(p, b.first) && !positionBefore(b.last, p)
}

// observeFact folds one journaled usage or result fact into the turn's calls.
//
// A record the harness marks PerCall is one deduplicated call. A record with a
// model the harness reported, not derived, is a call too, but a harness can
// repeat it while the call runs: Claude Code sends the request's usage with
// each content block, then the final counts in an unmarked, model-derived
// message delta. A record without PerCall from the same source whose input
// classes equal its source's latest unmarked call reports that call again, so
// it replaces the call's counts instead of adding one. Any other unmarked
// record without a reported model covers no single call: Codex sends a
// cumulative delta that can span several requests after a missed event, and
// those requests have no call of their own. It adds no call and no per-call
// cost; a root record's tokens add to the turn's unattributed usage instead.
func (b *turnBuilder) observeFact(f *data.Fact) {
	if !b.contains(v1.HistoryPosition{RecordID: f.RecordID, Index: f.Index}) {
		return
	}
	b.observed = max(b.observed, f.At.UnixMilli())
	src := v1.EventMessageSource(f.Source)
	root := !historyChild(&src)
	if f.Kind == data.KindResult {
		if root && f.ContextWindow > 0 {
			b.window = f.ContextWindow
		}
		return
	}
	if f.Kind != data.KindUsage {
		return
	}
	if root && f.ContextWindow > 0 {
		b.window = f.ContextWindow
	}
	u := usageFromData(f.Usage)
	if u.Equal(agent.Usage{}) {
		return
	}
	c := turnCall{source: src, at: f.At, model: f.Model, usage: u, perCall: f.PerCall, window: f.ContextWindow, reportedUSD: f.ReportedCostUSD}
	if !f.PerCall {
		if i, ok := b.latest[src]; ok && !b.calls[i].perCall && sameInput(&b.calls[i].usage, &u) {
			prior := &b.calls[i]
			if !f.ModelDerived && f.Model != "" || prior.model == "" {
				prior.model = f.Model
			}
			prior.at, prior.usage = f.At, u
			if f.ContextWindow > 0 {
				prior.window = f.ContextWindow
			}
			if f.ReportedCostUSD != nil {
				prior.reportedUSD = f.ReportedCostUSD
			}
			return
		}
		if f.ModelDerived || f.Model == "" {
			if root {
				b.unattributed.Add(u)
			}
			return
		}
	}
	b.latest[src] = len(b.calls)
	b.calls = append(b.calls, c)
}

// tokensOf copies the token classes a timeline reports.
func tokensOf(u *agent.Usage) v1.HistoryTurnTokens {
	return v1.HistoryTurnTokens{InputTokens: u.InputTokens, CacheCreationInputTokens: u.CacheCreationInputTokens, CacheReadInputTokens: u.CacheReadInputTokens, OutputTokens: u.OutputTokens, ReasoningOutputTokens: u.ReasoningOutputTokens}
}

// sameInput reports whether two reports carry the same input context.
func sameInput(a, b *agent.Usage) bool {
	return a.InputTokens == b.InputTokens && a.CacheCreationInputTokens == b.CacheCreationInputTokens && a.CacheReadInputTokens == b.CacheReadInputTokens
}

// apiCalls prices the folded calls and bounds them.
func (b *turnBuilder) apiCalls(pricer usage.ModelPricer) ([]v1.HistoryTurnCall, v1.HistoryTurnCallOmission) {
	duration := b.end - b.start
	calls := make([]v1.HistoryTurnCall, 0, len(b.calls))
	for i := range b.calls {
		c := &b.calls[i]
		root := !historyChild(&c.source)
		model := c.model
		if model == "" && root {
			model = b.turn.Model
		}
		out := v1.HistoryTurnCall{
			HistoryTurnTokens: tokensOf(&c.usage),
			Number:            i + 1,
			AtMS:              min(max(c.at.UnixMilli()-b.start, 0), duration),
			Source:            c.source,
			Model:             model,
			ContextWindow:     c.window,
		}
		if root && out.ContextWindow == 0 {
			out.ContextWindow = b.window
		}
		if !root {
			out.ActivityID = activityOwner(&c.source, b.routes, b.pending)
		}
		switch {
		case c.reportedUSD != nil && *c.reportedUSD >= 0:
			out.Cost = v1.EventCost{USD: *c.reportedUSD, Source: v1.EventCostSource(task.CostReported)}
		case model != "" && pricer != nil:
			if price, ok := pricer.ModelPrice(model, c.at); ok {
				out.Cost = v1.EventCost{USD: price.RequestCost(c.usage), Source: v1.EventCostSource(task.CostEstimated)}
			}
		}
		calls = append(calls, out)
	}
	var omitted v1.HistoryTurnCallOmission
	if len(calls) <= maxTurnCalls {
		return calls, omitted
	}
	// Keep the calls with the most tokens; ties keep the earlier call.
	total := func(c *v1.HistoryTurnCall) int {
		return c.InputTokens + c.CacheCreationInputTokens + c.CacheReadInputTokens + c.OutputTokens
	}
	slices.SortStableFunc(calls, func(a, b v1.HistoryTurnCall) int { return cmp.Compare(total(&b), total(&a)) })
	for i := range calls[maxTurnCalls:] {
		c := &calls[maxTurnCalls+i]
		omitted.Count++
		omitted.InputTokens += c.InputTokens
		omitted.CacheCreationInputTokens += c.CacheCreationInputTokens
		omitted.CacheReadInputTokens += c.CacheReadInputTokens
		omitted.OutputTokens += c.OutputTokens
		omitted.ReasoningOutputTokens += c.ReasoningOutputTokens
	}
	calls = calls[:maxTurnCalls]
	slices.SortFunc(calls, func(a, b v1.HistoryTurnCall) int { return cmp.Compare(a.Number, b.Number) })
	return calls, omitted
}

// tools sums the tool spans by name; it runs before the span cap.
func (b *turnBuilder) tools() []v1.HistoryTurnTool {
	duration := b.end - b.start
	tools := []v1.HistoryTurnTool{}
	for i := range b.spans {
		span := &b.spans[i]
		if span.Kind != v1.HistoryTurnSpanKindTool {
			continue
		}
		end := duration
		if span.EndMS != nil {
			end = min(*span.EndMS, duration)
		}
		within := max(end-span.StartMS, 0)
		j := slices.IndexFunc(tools, func(t v1.HistoryTurnTool) bool { return t.Name == span.Label })
		if j < 0 {
			j = len(tools)
			tools = append(tools, v1.HistoryTurnTool{Name: span.Label})
		}
		tools[j].Calls++
		tools[j].DurationMS += within
		tools[j].LongestMS = max(tools[j].LongestMS, within)
	}
	slices.SortFunc(tools, func(a, b v1.HistoryTurnTool) int {
		return cmp.Or(cmp.Compare(b.DurationMS, a.DurationMS), cmp.Compare(b.Calls, a.Calls), cmp.Compare(a.Name, b.Name))
	})
	return tools
}

// observe folds one projected event from the turn's record range.
func (b *turnBuilder) observe(e *v1.HistoryEvent) {
	if !b.contains(eventPosition(e)) {
		return
	}
	m := &e.Message
	b.observed = max(b.observed, m.Ts)
	if use := m.ToolUse; use != nil && b.within(m.Ts) {
		b.open[scopedTool(&m.Source, use.ToolUseID)] = len(b.spans)
		span := v1.HistoryTurnSpan{Kind: v1.HistoryTurnSpanKindTool, StartMS: m.Ts - b.start, Label: use.Name, Detail: use.Detail, Source: m.Source, ToolUseID: use.ToolUseID}
		if historyChild(&m.Source) {
			span.ActivityID = activityOwner(&m.Source, b.routes, b.pending)
		}
		b.spans = append(b.spans, span)
	}
	// A root question waits until the tool result carrying the answer.
	if ask := m.Ask; ask != nil && !historyChild(&m.Source) && ask.ToolUseID != "" && b.within(m.Ts) {
		b.open[scopedTool(&m.Source, ask.ToolUseID)] = len(b.spans)
		b.spans = append(b.spans, v1.HistoryTurnSpan{Kind: v1.HistoryTurnSpanKindAsk, StartMS: m.Ts - b.start, Source: m.Source, ToolUseID: ask.ToolUseID})
	}
	if result := m.ToolResult; result != nil {
		key := scopedTool(&m.Source, result.ToolUseID)
		i, ok := b.open[key]
		if !ok {
			return
		}
		delete(b.open, key)
		span := &b.spans[i]
		end := max(m.Ts-b.start, span.StartMS)
		span.EndMS = &end
		switch {
		case result.Interrupted:
			span.Status = "interrupted"
		case result.Error != "":
			span.Status = "failed"
		default:
			span.Status = "completed"
		}
	}
}

func (b *turnBuilder) within(ts int64) bool {
	return ts > 0 && ts >= b.start && ts <= b.end
}

func (b *turnBuilder) offset(at *time.Time) *int64 {
	if at == nil {
		return nil
	}
	ms := at.UnixMilli() - b.start
	return &ms
}

// timeline adds the inferred model spans in the time no root tool call or
// question covers, prices the calls, and bounds the result. A running turn's axis ends at the later of now and its latest
// observation.
func (b *turnBuilder) timeline(now time.Time, pricer usage.ModelPricer) v1.HistoryTurnTimeline {
	if b.running {
		b.end = max(now.UnixMilli(), b.observed, b.start)
	}
	duration := b.end - b.start
	tools := b.tools()
	calls, omittedCalls := b.apiCalls(pricer)
	var busy [][2]int64
	for i := range b.spans {
		span := &b.spans[i]
		if span.Kind != v1.HistoryTurnSpanKindTool && span.Kind != v1.HistoryTurnSpanKindAsk || historyChild(&span.Source) {
			continue
		}
		end := duration
		if span.EndMS != nil {
			end = min(*span.EndMS, duration)
		}
		busy = append(busy, [2]int64{span.StartMS, end})
	}
	slices.SortFunc(busy, func(a, b [2]int64) int { return cmp.Compare(a[0], b[0]) })
	var model int64
	cursor := int64(0)
	gap := func(until int64, provisional bool) {
		if until > cursor {
			span := v1.HistoryTurnSpan{Kind: v1.HistoryTurnSpanKindModel, StartMS: cursor}
			if !provisional {
				span.EndMS = new(until)
			}
			b.spans = append(b.spans, span)
			model += until - cursor
		}
	}
	for _, interval := range busy {
		gap(interval[0], false)
		cursor = max(cursor, interval[1])
	}
	gap(duration, b.running)

	spans := b.spans
	if spans == nil {
		spans = []v1.HistoryTurnSpan{}
	}
	omitted := []v1.HistoryTurnOmission{}
	if len(spans) > maxTurnSpans {
		// Keep the longest spans; one without an end outranks every ended one.
		rank := func(s *v1.HistoryTurnSpan) int64 {
			if s.EndMS == nil {
				return math.MaxInt64
			}
			return *s.EndMS - s.StartMS
		}
		// An omitted span counts only its time within the turn, so an open or
		// outliving span adds at most the rest of the turn.
		within := func(s *v1.HistoryTurnSpan) int64 {
			end := duration
			if s.EndMS != nil {
				end = min(*s.EndMS, duration)
			}
			return max(end-s.StartMS, 0)
		}
		slices.SortStableFunc(spans, func(a, b v1.HistoryTurnSpan) int {
			return cmp.Or(cmp.Compare(rank(&b), rank(&a)), cmp.Compare(a.StartMS, b.StartMS))
		})
		for i := range spans[maxTurnSpans:] {
			span := &spans[maxTurnSpans+i]
			j := slices.IndexFunc(omitted, func(o v1.HistoryTurnOmission) bool { return o.Kind == span.Kind })
			if j < 0 {
				j = len(omitted)
				omitted = append(omitted, v1.HistoryTurnOmission{Kind: span.Kind})
			}
			omitted[j].Count++
			omitted[j].DurationMS += within(span)
		}
		spans = spans[:maxTurnSpans]
		slices.SortFunc(omitted, func(a, b v1.HistoryTurnOmission) int { return cmp.Compare(a.Kind, b.Kind) })
	}
	slices.SortStableFunc(spans, func(a, b v1.HistoryTurnSpan) int {
		return cmp.Or(cmp.Compare(a.StartMS, b.StartMS), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.ActivityID, b.ActivityID))
	})
	return v1.HistoryTurnTimeline{Number: b.turn.Number, StartedAt: b.turn.StartedAt, EndedAt: b.turn.At, DurationMS: duration, ModelMS: model, Spans: spans, Omitted: omitted, Calls: calls, OmittedCalls: omittedCalls, Unattributed: tokensOf(&b.unattributed), Tools: tools}
}

// turnCall is one API call before pricing.
type turnCall struct {
	source      v1.EventMessageSource
	at          time.Time
	model       string
	usage       agent.Usage
	perCall     bool
	window      int
	reportedUSD *float64
}

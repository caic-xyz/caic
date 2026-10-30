// Indexes context boundaries, compact invocation facts, and payload-free activity state during raw projection.

package history

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

type sectionIndex struct {
	revision           uint64
	closed             []v1.HistorySection
	current            v1.HistorySection
	last               Position
	lastResult         Position
	firstAfterResult   Position
	sessionID          string
	facts              []summaryFact
	closedFacts        [][]summaryFact
	lastResultFact     int
	routingDirty       bool
	activityHomesDirty bool
	routingRevision    uint64
	turns              []v1.HistoryTurn
	tools              map[toolKey]toolStart
	totals             map[string]v1.HistoryToolTiming
	activities         map[string]v1.HistoryActivity
	activityOrigins    map[string]Position
	transcripts        map[string]struct{}
	routing            map[activityRoute]uint64
	pendingScopes      map[string]map[string]uint64
	transcriptOrigins  map[transcriptOrigin]transcriptObservation
	runningPositions   map[string]transcriptObservation
	lifecycleAnchors   map[string]time.Time
	metadata           v1.HistoryMetadata
	waiting            int
	model              string
	initialDigest      [32]byte
	initialEligible    bool
	rootContent        bool
	actorRecords       map[string][]uint64
	delegationRecords  map[delegationScope][]uint64
	setupRecords       []uint64
}

func newSectionIndex() sectionIndex {
	return sectionIndex{
		current: v1.HistorySection{ID: "1", Skills: []v1.EventSkillLoad{}},
		tools:   map[toolKey]toolStart{}, totals: map[string]v1.HistoryToolTiming{},
		activities: map[string]v1.HistoryActivity{}, activityOrigins: map[string]Position{}, transcripts: map[string]struct{}{}, routing: map[activityRoute]uint64{}, pendingScopes: map[string]map[string]uint64{}, transcriptOrigins: map[transcriptOrigin]transcriptObservation{}, runningPositions: map[string]transcriptObservation{}, lifecycleAnchors: map[string]time.Time{}, waiting: -1,
		turns: []v1.HistoryTurn{}, lastResultFact: -1, actorRecords: map[string][]uint64{}, delegationRecords: map[delegationScope][]uint64{},
	}
}

func (s *sectionIndex) append(e *Event) {
	m := &e.Message
	if m.Init != nil || m.Result != nil || m.ToolResult != nil || m.NativeSubagent != nil || m.BackgroundCommand != nil || m.UserInput != nil || m.SkillLoad != nil || m.Todo != nil || m.Log != nil || m.CommitSnapshot != nil || m.System != nil {
		s.revision++
	}
	p := eventPosition(e)
	s.observeTool(m)
	s.observeActivity(e)
	rootBoundary := !historyChild(&m.Source) && (m.Init != nil || m.System != nil && (m.System.Subtype == "compact_boundary" || m.System.Subtype == "context_cleared"))
	if !rootBoundary && (m.Source.AgentID == "" || m.Kind == v1.EventKindRateLimit && m.RateLimit != nil && m.RateLimit.RateLimitType != "thread_goal") {
		s.addFact(m)
	}
	if s.current.Body == nil {
		s.current.Body = &v1.HistorySlice{First: p, Last: p}
	}
	s.current.Body.Last = p
	if s.lastResult.RecordID != 0 && s.firstAfterResult.RecordID == 0 {
		s.firstAfterResult = p
	}
	if rootEvent(m) {
		if (m.Init != nil || m.UserInput != nil) && m.Ts > 0 && (s.metadata.SetupEndedAt.IsZero() || m.Ts < s.metadata.SetupEndedAt.UnixMilli()) {
			s.metadata.SetupEndedAt = historyTime(m.Ts)
		}
		boundary := m.Kind == v1.EventKindSystem && m.System != nil && (m.System.Subtype == "compact_boundary" || m.System.Subtype == "context_cleared")
		changedInit := m.Init != nil && m.Init.SessionID != s.sessionID
		if boundary || changedInit && (s.current.Boundary == nil || s.current.Boundary.Message.System == nil || s.lastResult.RecordID != 0) {
			if s.current.Boundary != nil {
				s.close(e)
			}
			b := *e
			s.current.Boundary = &b
			if boundary {
				s.sessionID = ""
			}
		}
		if m.Init != nil {
			s.sessionID = m.Init.SessionID
			s.model = m.Init.ReportedModel
			s.current.ReportedModel = s.model
			if s.metadata.InitialInit == nil {
				init := *e
				s.metadata.InitialInit = &init
			}
		}
		if m.Usage != nil && m.Usage.ReportedModel != "" {
			s.model = m.Usage.ReportedModel
		}
		if m.UserInput != nil {
			if s.metadata.InitialInput == nil {
				s.metadata.InitialInput = &p
				s.initialDigest = sha256.Sum256([]byte(m.UserInput.Text))
				s.initialEligible = !s.rootContent
			}
			if s.waiting >= 0 {
				t := &s.turns[s.waiting]
				if !t.At.IsZero() && m.Ts > t.At.UnixMilli() {
					wait := m.Ts - t.At.UnixMilli()
					t.WaitMS = &wait
					t.WaitInput = &p
				}
				s.waiting = -1
			}
		}
		if m.Todo != nil {
			todo := *m.Todo
			todo.Todos = slices.Clone(m.Todo.Todos)
			s.metadata.Todo = &todo
		}
		if m.Log != nil {
			if s.metadata.Setup == nil {
				s.metadata.Setup = &v1.HistorySlice{First: p, Last: p}
			} else {
				s.metadata.Setup.Last = p
			}
		}
		if m.SkillLoad != nil {
			s.addSkill(*m.SkillLoad)
		}
		if m.Result != nil && !historyChild(&m.Source) {
			r := m.Result
			model := r.Usage.ReportedModel
			if model == "" {
				model = s.model
			}
			s.turns = append(s.turns, v1.HistoryTurn{HistoryPosition: p, SectionID: s.current.ID, At: historyTime(m.Ts), Model: model, Usage: r.Usage, DurationMS: int64(math.Round(r.Duration * 1000)), DurationAPIMS: int64(math.Round(r.DurationAPI * 1000)), Cost: r.Cost})
			s.waiting = len(s.turns) - 1
		}
		if m.CommitSnapshot != nil && !m.CommitSnapshot.Baseline && len(s.turns) > 0 {
			s.turns[len(s.turns)-1].ChangeStat = m.CommitSnapshot.ChangeStat
		}
		switch m.Kind {
		case v1.EventKindText, v1.EventKindTextDelta, v1.EventKindThinking, v1.EventKindThinkingDelta, v1.EventKindToolUse, v1.EventKindToolResult, v1.EventKindResult:
			s.rootContent = true
		default:
		}
		if m.Result != nil && !historyChild(&m.Source) {
			s.lastResult = p
			s.firstAfterResult = Position{}
			s.lastResultFact = len(s.facts) - 1
		}
	}
	s.last = p
	s.indexRecords(e)
}

func (s *sectionIndex) indexRecords(e *Event) {
	m := &e.Message
	if m.Source.AgentID != "" {
		key := string(v1.EventKindNativeSubagent) + ":" + m.Source.AgentID
		s.actorRecords[key] = appendRecord(s.actorRecords[key], e.RecordID)
	}
	if n := m.NativeSubagent; n != nil {
		key := string(v1.EventKindNativeSubagent) + ":" + n.ID
		s.actorRecords[key] = appendRecord(s.actorRecords[key], e.RecordID)
	}
	if b := m.BackgroundCommand; b != nil {
		key := string(v1.EventKindBackgroundCommand) + ":" + b.ID
		s.actorRecords[key] = appendRecord(s.actorRecords[key], e.RecordID)
	}
	if m.Source.AgentID == "" && m.Source.DelegationToolUseID != "" {
		namespace := m.Source.RootThreadID
		if namespace == "" {
			namespace = m.Source.ThreadID
		}
		key := delegationScope{tool: m.Source.DelegationToolUseID, namespace: namespace}
		s.delegationRecords[key] = appendRecord(s.delegationRecords[key], e.RecordID)
	}
	if !historyChild(&m.Source) && (m.Log != nil || m.Init != nil && s.metadata.InitialInit != nil && e.RecordID == s.metadata.InitialInit.RecordID) {
		s.setupRecords = appendRecord(s.setupRecords, e.RecordID)
	}
}

func (s *sectionIndex) close(e *Event) {
	end := s.last
	if s.lastResult.RecordID != 0 {
		end = s.lastResult
	}
	closed := s.current
	if closed.Body != nil {
		body := *closed.Body
		body.Last = end
		if end.RecordID < body.First.RecordID || end.RecordID == body.First.RecordID && end.Index < body.First.Index {
			closed.Body = nil
		} else {
			closed.Body = &body
		}
	}
	boundary := *e
	closed.EndBoundary = &boundary
	factCut := len(s.facts)
	if s.lastResultFact >= 0 {
		factCut = s.lastResultFact + 1
	}
	countSection(&closed, s.factGroups(s.facts[:factCut]))
	var deferred []summaryFact
	for i := range s.facts[:factCut] {
		f := &s.facts[i]
		if f.source.DelegationToolUseID != "" {
			deferred = slices.Clone(s.facts[:factCut])
			break
		}
	}
	s.closedFacts = append(s.closedFacts, deferred)
	s.closed = append(s.closed, closed)
	first := s.firstAfterResult
	if first.RecordID == 0 {
		first = eventPosition(e)
	}
	s.current = v1.HistorySection{ID: strconv.Itoa(len(s.closed) + 1), Body: &v1.HistorySlice{First: first, Last: eventPosition(e)}, ReportedModel: s.model, Skills: []v1.EventSkillLoad{}}
	s.lastResult = Position{}
	s.firstAfterResult = Position{}
	s.facts = slices.Clone(s.facts[factCut:])
	s.lastResultFact = -1
	s.activityHomesDirty = true
}

func (s *sectionIndex) addSkill(load v1.EventSkillLoad) {
	for _, seen := range s.current.Skills {
		if load.Path != "" && seen.Path == load.Path || load.Path == "" && seen.Path == "" && seen.Skill == load.Skill {
			return
		}
	}
	s.current.Skills = append(s.current.Skills, load)
}

func (s *sectionIndex) snapshot() ([]v1.HistorySection, v1.HistoryMetadata) {
	if s.routingDirty {
		for i, facts := range s.closedFacts {
			if len(facts) > 0 {
				countSection(&s.closed[i], s.factGroups(facts))
			}
		}
	}
	if s.routingDirty || s.activityHomesDirty {
		s.reanchorActivities()
		s.routingDirty = false
		s.activityHomesDirty = false
	}
	out := make([]v1.HistorySection, 0, len(s.closed)+1)
	for _, section := range s.closed {
		section.Skills = resolvedSkills(section.Skills)
		out = append(out, section)
	}
	current := s.current
	if current.Body != nil {
		body := *current.Body
		current.Body = &body
	}
	current.Skills = resolvedSkills(current.Skills)
	countSection(&current, s.factGroups(s.facts))
	out = append(out, current)
	metadata := s.metadata
	metadata.RoutingRevision = s.routingRevision
	metadata.Routing = []v1.HistoryRoute{}
	for route := range s.routing {
		metadata.Routing = append(metadata.Routing, v1.HistoryRoute{ToolUseID: route.tool, Namespace: route.namespace, AgentID: route.id})
	}
	for tool, scopes := range s.pendingScopes {
		for namespace := range scopes {
			metadata.Routing = append(metadata.Routing, v1.HistoryRoute{ToolUseID: tool, Namespace: namespace})
		}
	}
	slices.SortFunc(metadata.Routing, func(a, b v1.HistoryRoute) int {
		if n := strings.Compare(a.ToolUseID, b.ToolUseID); n != 0 {
			return n
		}
		if n := strings.Compare(a.Namespace, b.Namespace); n != 0 {
			return n
		}
		return strings.Compare(a.AgentID, b.AgentID)
	})

	if metadata.Setup != nil {
		setup := *metadata.Setup
		metadata.Setup = &setup
	}
	metadata.Turns = slices.Clone(s.turns)
	metadata.Tools = make([]v1.HistoryToolTiming, 0, len(s.totals))
	for _, t := range s.totals {
		metadata.Tools = append(metadata.Tools, t)
	}
	slices.SortFunc(metadata.Tools, func(a, b v1.HistoryToolTiming) int {
		if a.DurationMS > b.DurationMS {
			return -1
		}
		if a.DurationMS < b.DurationMS {
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	metadata.Activities = make([]v1.HistoryActivity, 0, len(s.activities))
	for key := range s.activities {
		a := s.activities[key]
		a.BodyThrough, a.OwnershipThrough = s.activityBounds(key, &a)
		metadata.Activities = append(metadata.Activities, a)
	}
	slices.SortFunc(metadata.Activities, func(a, b v1.HistoryActivity) int {
		if a.First.RecordID < b.First.RecordID {
			return -1
		}
		if a.First.RecordID > b.First.RecordID {
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, metadata
}

// activityBounds separates payload growth from related ownership observations.
// Both bounds are independent of unrelated root activity and snapshot timing.
func (s *sectionIndex) activityBounds(key string, a *v1.HistoryActivity) (body, ownership uint64) {
	body = a.First.RecordID
	if records := s.actorRecords[key]; len(records) > 0 {
		body = max(body, records[len(records)-1])
	}
	if a.Kind != v1.EventKindNativeSubagent {
		return body, 0
	}
	for route, at := range s.routing {
		if route.id != a.ID {
			continue
		}
		ownership = max(ownership, at)
		for other, observed := range s.routing {
			if other.tool == route.tool {
				ownership = max(ownership, observed)
			}
		}
		for _, observed := range s.pendingScopes[route.tool] {
			ownership = max(ownership, observed)
		}
		for scope, records := range s.delegationRecords {
			if scope.tool == route.tool && (route.namespace == "" || scope.namespace == "" || route.namespace == scope.namespace) && len(records) > 0 {
				body = max(body, records[len(records)-1])
			}
		}
	}
	return max(body, ownership), ownership
}

func (s *sectionIndex) observeTool(m *v1.EventMessage) {
	if use := m.ToolUse; use != nil {
		s.tools[scopedTool(&m.Source, use.ToolUseID)] = toolStart{use.Name, m.Ts}
	}
	if result := m.ToolResult; result != nil {
		key := scopedTool(&m.Source, result.ToolUseID)
		start, ok := s.tools[key]
		if !ok {
			return
		}
		delete(s.tools, key)
		duration := int64(math.Round(result.Duration * 1000))
		if duration <= 0 {
			if start.ts <= 0 || m.Ts <= start.ts {
				return
			}
			duration = m.Ts - start.ts
		}
		total := s.totals[start.name]
		total.Name = start.name
		total.Calls++
		total.DurationMS += duration
		s.totals[start.name] = total
	}
}

func (s *sectionIndex) observeActivity(e *Event) {
	m := &e.Message
	body := m.NativeSubagent == nil && m.BackgroundCommand == nil && (m.Kind != v1.EventKindRateLimit || m.RateLimit != nil && m.RateLimit.RateLimitType == "thread_goal")
	if body && (m.Source.AgentID != "" || m.Source.DelegationToolUseID != "") {
		namespace := m.Source.RootThreadID
		if namespace == "" {
			namespace = m.Source.ThreadID
		}
		origin := transcriptOrigin{agent: m.Source.AgentID}
		if origin.agent == "" {
			origin.tool = m.Source.DelegationToolUseID
			origin.namespace = namespace
		}
		if _, seen := s.transcriptOrigins[origin]; !seen {
			s.transcriptOrigins[origin] = transcriptObservation{position: eventPosition(e), at: historyTime(m.Ts)}
			s.routingDirty = true
			s.revision++
		}
	}
	if m.Source.DelegationToolUseID != "" {
		if m.Source.AgentID == "" {
			namespace := m.Source.RootThreadID
			if namespace == "" {
				namespace = m.Source.ThreadID
			}
			scopes := s.pendingScopes[m.Source.DelegationToolUseID]
			if scopes == nil {
				scopes = map[string]uint64{}
				s.pendingScopes[m.Source.DelegationToolUseID] = scopes
			}
			if _, seen := scopes[namespace]; !seen {
				scopes[namespace] = e.RecordID
				s.routingRevision++
				s.routingDirty = true
				s.revision++
			}
		}
	}
	kind, id := m.Kind, ""
	switch {
	case m.NativeSubagent != nil:
		id = m.NativeSubagent.ID
	case m.BackgroundCommand != nil:
		id = m.BackgroundCommand.ID
	case body && m.Source.AgentID != "":
		kind = v1.EventKindNativeSubagent
		id = m.Source.AgentID
	}
	if id == "" {
		return
	}
	key := string(kind) + ":" + id
	a, ok := s.activities[key]
	if !ok {
		s.activityOrigins[key] = eventPosition(e)
		s.activityHomesDirty = true
		a = v1.HistoryActivity{Kind: kind, ID: id, Status: "unknown", First: eventPosition(e), AnchorAt: historyTime(m.Ts), Source: m.Source}
		if body {
			a.Source = v1.EventMessageSource{}
		}
	}
	if m.NativeSubagent == nil && m.BackgroundCommand == nil {
		if _, seen := s.transcripts[id]; !seen {
			if a.StartedAt != nil {
				a.AnchorAt = *a.StartedAt
			} else {
				a.AnchorAt = historyTime(m.Ts)
			}
			s.transcripts[id] = struct{}{}
		}
	}
	previous := a.Status
	if n := m.NativeSubagent; n != nil {
		if a.Source.IsZero() {
			a.Source = m.Source
		}
		if n.ToolUseID != "" {
			namespace := m.Source.RootThreadID
			if namespace == "" {
				namespace = m.Source.ThreadID
			}
			route := activityRoute{id: n.ID, tool: n.ToolUseID, namespace: namespace}
			if _, known := s.routing[route]; !known {
				s.routing[route] = e.RecordID
				s.routingRevision++
				s.routingDirty = true
			}
			// First is a conservative read bound. Keep it independent of when
			// clients request snapshots, even if ownership later becomes ambiguous.
			for origin, observation := range s.transcriptOrigins {
				if origin.agent == "" && origin.tool == n.ToolUseID && (namespace == "" || origin.namespace == "" || origin.namespace == namespace) && positionBefore(observation.position, a.First) {
					a.First = observation.position
				}
			}
		}
		if a.ToolUseID == "" {
			a.ToolUseID = n.ToolUseID
		}
		if a.Scope == "" {
			a.Scope = n.Scope
		}
		if a.GroupID == "" {
			a.GroupID = n.GroupID
		}
		if a.Label == "" {
			a.Label = n.Label
		}
		a.Background = a.Background || n.Background
		if !terminal(a.Status) && n.Status != v1.EventNativeSubagentStatusUnknown {
			a.Status = string(n.Status)
		}
	} else if b := m.BackgroundCommand; b != nil {
		if a.ToolUseID == "" {
			a.ToolUseID = b.ToolUseID
		}
		if a.Label == "" {
			a.Label = b.Label
		}
		if !terminal(a.Status) && b.Status != "unknown" {
			a.Status = b.Status
		}
		a.ExitCode = b.ExitCode
		a.OutputRef = b.OutputRef
	}
	at := historyTime(m.Ts)
	if !at.IsZero() {
		if a.StartedAt == nil && (m.NativeSubagent != nil && m.NativeSubagent.Status == v1.EventNativeSubagentStatusRunning || m.BackgroundCommand != nil && m.BackgroundCommand.Status == "running") {
			a.StartedAt = &at
			if kind == v1.EventKindNativeSubagent {
				s.runningPositions[id] = transcriptObservation{position: eventPosition(e), at: at}
			}
		}
		if a.EndedAt == nil && (m.NativeSubagent != nil && terminal(string(m.NativeSubagent.Status)) || m.BackgroundCommand != nil && terminal(m.BackgroundCommand.Status)) {
			a.EndedAt = &at
		}
		_, hasTranscript := s.transcripts[id]
		if previous != a.Status && (kind == v1.EventKindBackgroundCommand || !hasTranscript) {
			a.AnchorAt = at
		}
	}
	if m.NativeSubagent != nil {
		if _, seen := s.lifecycleAnchors[id]; !seen || previous != a.Status && !at.IsZero() {
			s.lifecycleAnchors[id] = at
		}
	}
	s.activities[key] = a
}

func (s *sectionIndex) reanchorActivities() {
	routes := make([]activityRoute, 0, len(s.routing))
	for route := range s.routing {
		routes = append(routes, route)
	}
	pending := map[string]int{}
	for tool, scopes := range s.pendingScopes {
		pending[tool] = len(scopes)
	}
	first := map[string]transcriptObservation{}
	for origin, observation := range s.transcriptOrigins {
		source := v1.EventMessageSource{AgentID: origin.agent, DelegationToolUseID: origin.tool, ThreadID: origin.namespace}
		id := activityOwner(&source, routes, pending)
		if id == "" {
			continue
		}
		if previous, seen := first[id]; !seen || positionBefore(observation.position, previous.position) {
			first[id] = observation
		}
	}
	clear(s.transcripts)
	for key := range s.activities {
		a := s.activities[key]
		origin := s.activityOrigins[key]
		if a.Kind == v1.EventKindNativeSubagent {
			if observation, seen := first[a.ID]; seen {
				if positionBefore(observation.position, origin) {
					origin = observation.position
				}
				s.transcripts[a.ID] = struct{}{}
				a.AnchorAt = observation.at
				if running, seen := s.runningPositions[a.ID]; seen && positionBefore(running.position, observation.position) {
					a.AnchorAt = running.at
				}
			} else if anchor, seen := s.lifecycleAnchors[a.ID]; seen {
				a.AnchorAt = anchor
			}
		}
		a.SectionID = s.sectionAt(origin)
		s.activities[key] = a
	}
}

func (s *sectionIndex) sectionAt(position Position) string {
	if body := s.current.Body; body != nil && !positionBefore(position, body.First) && !positionBefore(body.Last, position) {
		return s.current.ID
	}
	for i := range s.closed {
		section := &s.closed[i]
		if body := section.Body; body != nil && !positionBefore(position, body.First) && !positionBefore(body.Last, position) {
			return section.ID
		}
	}
	return ""
}

func (s *sectionIndex) addFact(m *v1.EventMessage) {
	f := summaryFact{kind: m.Kind, source: m.Source, ts: m.Ts, end: m.Ts}
	if m.Text != nil {
		f.text = strings.TrimSpace(m.Text.Text) != ""
	}
	if m.TextDelta != nil {
		f.text = strings.TrimSpace(m.TextDelta.Text) != ""
	}
	if m.ToolUse != nil {
		f.tool = m.ToolUse.ToolUseID
	}
	if m.ToolResult != nil {
		f.tool = m.ToolResult.ToolUseID
	}
	if m.ToolOutputDelta != nil {
		f.tool = m.ToolOutputDelta.ToolUseID
	}
	if m.Result != nil {
		f.duration = m.Result.Duration
	}
	if m.System != nil {
		f.system = m.System.Subtype
	}
	f.globalQuota = m.RateLimit != nil && m.RateLimit.RateLimitType != "thread_goal"
	if m.RateLimit != nil {
		f.quotaStatus = m.RateLimit.Status
		f.quotaType = m.RateLimit.RateLimitType
		f.quotaUtilization = m.RateLimit.Utilization
	}
	if len(s.facts) > 0 && (f.kind == v1.EventKindTextDelta || f.kind == v1.EventKindThinkingDelta || f.kind == v1.EventKindToolOutputDelta) {
		last := &s.facts[len(s.facts)-1]
		if last.kind == f.kind && last.source == f.source && last.tool == f.tool {
			last.text = last.text || f.text
			last.end = max(last.end, f.end)
			return
		}
	}
	s.facts = append(s.facts, f)
}

func (s *sectionIndex) factGroups(facts []summaryFact) []summaryGroup {
	counter := summaryCounter{identified: map[string]int{}, legacy: map[string]legacySummaryTool{}, warnings: map[string]int{}, lastLegacy: -1}
	routes := make([]activityRoute, 0, len(s.routing))
	for route := range s.routing {
		routes = append(routes, route)
	}
	pending := map[string]int{}
	for tool, scopes := range s.pendingScopes {
		pending[tool] = len(scopes)
	}
	for i := range facts {
		f := &facts[i]
		if !f.globalQuota {
			if f.source.AgentID != "" {
				continue
			}
			if activityOwner(&f.source, routes, pending) != "" {
				continue
			}
		}
		m := v1.EventMessage{Kind: f.kind, Ts: f.ts, Source: f.source}
		text := ""
		if f.text {
			text = "text"
		}
		switch f.kind {
		case v1.EventKindText:
			m.Text = &v1.EventText{Text: text}
		case v1.EventKindTextDelta:
			m.TextDelta = &v1.EventTextDelta{Text: text}
		case v1.EventKindThinking:
			m.Thinking = &v1.EventThinking{}
		case v1.EventKindThinkingDelta:
			m.ThinkingDelta = &v1.EventThinkingDelta{}
		case v1.EventKindToolUse:
			m.ToolUse = &v1.EventToolUse{ToolUseID: f.tool}
		case v1.EventKindToolResult:
			m.ToolResult = &v1.EventToolResult{ToolUseID: f.tool}
		case v1.EventKindToolOutputDelta:
			m.ToolOutputDelta = &v1.EventToolOutputDelta{ToolUseID: f.tool}
		case v1.EventKindResult:
			if !historyChild(&f.source) {
				m.Result = &v1.EventResult{Duration: f.duration}
			}
		case v1.EventKindSystem:
			m.System = &v1.EventSystem{Subtype: f.system}
		case v1.EventKindRateLimit:
			m.RateLimit = &v1.EventRateLimit{Status: f.quotaStatus, RateLimitType: f.quotaType, Utilization: f.quotaUtilization}
		default:
		}
		counter.group(&m)
		if f.end > f.ts && len(counter.groups) > 0 {
			counter.groups[len(counter.groups)-1].end = max(counter.groups[len(counter.groups)-1].end, f.end)
		}
	}
	return counter.normalized()
}

type summaryCounter struct {
	groups         []summaryGroup
	identified     map[string]int
	legacy         map[string]legacySummaryTool
	warnings       map[string]int
	lastLegacy     int
	usageSinceTool bool
}

func (s *summaryCounter) group(m *v1.EventMessage) {
	// Retain only group identity, presence and timing: no transcript strings or
	// tool payloads enter the summary index, even for the current context.
	kind, key, text, delta, tools := "other", "", false, false, 0
	switch {
	case m.ToolUse != nil || m.ToolResult != nil || m.ToolOutputDelta != nil:
		id := ""
		if m.ToolUse != nil {
			id = m.ToolUse.ToolUseID
			tools = 1
		}
		if m.ToolResult != nil {
			id = m.ToolResult.ToolUseID
		}
		if m.ToolOutputDelta != nil {
			id = m.ToolOutputDelta.ToolUseID
		}
		kind = "action"
		toolKey := fmt.Sprintf("tool:%q", scopedTool(&m.Source, id))
		_, scoped := s.identified[toolKey]
		switch {
		case !m.Source.IsZero() || scoped:
			key = toolKey
		case m.ToolUse == nil:
			if legacy, ok := s.legacy[id]; ok && (m.ToolOutputDelta != nil || !legacy.completed) {
				s.groups[legacy.index].end = max(s.groups[legacy.index].end, m.Ts)
				if m.ToolResult != nil {
					legacy.completed = true
					s.legacy[id] = legacy
				}
				return
			}
			key = toolKey
		default:
			// Several source-free calls from one assistant message keep the
			// original action position, even with intervening commentary.
			// A usage observation marks the next assistant message.
			if !s.usageSinceTool && s.lastLegacy >= 0 {
				g := &s.groups[s.lastLegacy]
				g.tools++
				g.end = max(g.end, m.Ts)
				s.legacy[id] = legacySummaryTool{index: s.lastLegacy}
				return
			}
			s.usageSinceTool = false
		}
	case m.Text != nil || m.TextDelta != nil || m.Thinking != nil || m.ThinkingDelta != nil:
		kind = "action"
		if m.Text != nil {
			kind = "text"
			text = strings.TrimSpace(m.Text.Text) != ""
		}
		if m.TextDelta != nil {
			kind = "text"
			delta = true
			text = strings.TrimSpace(m.TextDelta.Text) != ""
		}
		if src := m.Source; src.ThreadID != "" && src.TurnID != "" && src.ItemID != "" {
			key = fmt.Sprintf("%q/%q/%q/%s", src.ThreadID, src.TurnID, src.ItemID, kind)
		}
	default:
		switch m.Kind {
		case v1.EventKindTodo, v1.EventKindSkillLoad, v1.EventKindLog, v1.EventKindDiffStat, v1.EventKindCommitSnapshot, v1.EventKindNativeSubagent, v1.EventKindBackgroundCommand, v1.EventKindSubagentStart, v1.EventKindSubagentEnd:
			return
		case v1.EventKindUsage:
			s.usageSinceTool = true
			if len(s.groups) > 0 {
				last := &s.groups[len(s.groups)-1]
				if last.key == "" && (last.kind == "text" || last.kind == "action") {
					last.end = max(last.end, m.Ts)
					return
				}
			}
		case v1.EventKindRateLimit:
			if m.RateLimit == nil {
				return
			}
			rl := m.RateLimit
			if rl.Status == v1.EventRateLimitStatusAllowedWarning {
				percent := int(math.Round(rl.Utilization * 100))
				if previous, ok := s.warnings[rl.RateLimitType]; ok && previous == percent {
					return
				}
				s.warnings[rl.RateLimitType] = percent
			} else {
				delete(s.warnings, rl.RateLimitType)
				if rl.Status != v1.EventRateLimitStatusRejected {
					return
				}
			}
		case v1.EventKindSystem:
			if m.System != nil && (m.System.Subtype == "active" || m.System.Subtype == "idle" || m.System.Subtype == "notLoaded" || m.System.Subtype == "system_error") {
				return
			}
		default:
		}
	}
	if key == "" && len(s.groups) > 0 {
		last := &s.groups[len(s.groups)-1]
		if last.key == "" && last.tools == 0 && (m.ThinkingDelta != nil && last.kind == "action" || m.Thinking != nil && last.thinkingDelta && (last.kind == "action" || last.kind == "text" && !last.finalText)) {
			last.thinking = true
			last.thinkingDelta = last.thinkingDelta || m.ThinkingDelta != nil
			last.end = max(last.end, m.Ts)
			return
		}
	}
	if key != "" {
		if i, ok := s.identified[key]; ok {
			g := &s.groups[i]
			if kind == "text" {
				g.everText = g.everText || text
				if delta {
					g.text = g.text || text
				} else {
					g.text = text
				}
			}
			g.tools = max(g.tools, tools)
			g.end = max(g.end, m.Ts)
			return
		}
	}
	if key == "" && kind == "text" && len(s.groups) > 0 {
		last := &s.groups[len(s.groups)-1]
		if last.key == "" && (last.kind == "text" && (delta || last.delta) || last.kind == "action" && last.tools == 0) {
			last.kind = "text"
			if delta {
				last.text = last.text || text
			} else {
				last.text = text
			}
			last.delta = last.delta || delta
			last.everText = last.everText || text
			last.finalText = last.finalText || m.Text != nil
			last.end = max(last.end, m.Ts)
			return
		}
	}
	g := summaryGroup{kind: kind, key: key, text: text, delta: delta, tools: tools, start: m.Ts, end: m.Ts}
	g.everText = text
	g.thinking = m.Thinking != nil || m.ThinkingDelta != nil
	g.thinkingDelta = m.ThinkingDelta != nil
	g.finalText = m.Text != nil
	if m.Result != nil {
		g.result = true
		g.duration = int64(math.Round(m.Result.Duration * 1000))
	}
	if key != "" {
		s.identified[key] = len(s.groups)
	} else if m.ToolUse != nil {
		s.legacy[m.ToolUse.ToolUseID] = legacySummaryTool{index: len(s.groups)}
		s.lastLegacy = len(s.groups)
	}
	s.groups = append(s.groups, g)
}

func (s *summaryCounter) normalized() []summaryGroup {
	// Thinking embedded in blank text remains one message when it is alone.
	// Several thinking blocks in a run collapse together and discard blank text.
	groups := s.groups
	for i := range groups {
		g := &groups[i]
		if i > 0 && g.key == "" && g.kind == "text" && g.thinking {
			previous := &groups[i-1]
			if previous.key == "" && previous.kind == "action" && previous.tools > 0 {
				previous.thinking = true
				g.thinking = false
			}
		}
	}
	var run []int
	flush := func() {
		if len(run) > 1 {
			for _, i := range run {
				if groups[i].kind == "text" && !groups[i].everText {
					groups[i].kind = "action"
				}
			}
		}
		run = run[:0]
	}
	for i := range groups {
		g := &groups[i]
		if g.key != "" || g.kind != "text" && g.kind != "action" {
			flush()
			continue
		}
		if g.thinking {
			run = append(run, i)
		}
		if g.kind == "text" && g.text {
			flush()
		}
	}
	flush()
	return groups
}

type summaryGroup struct {
	kind          string
	key           string
	text          bool
	everText      bool
	delta         bool
	tools         int
	start         int64
	end           int64
	result        bool
	duration      int64
	thinking      bool
	thinkingDelta bool
	finalText     bool
}

type legacySummaryTool struct {
	index     int
	completed bool
}

// summaryFact contains presence and identity only. Pending delegation ownership
// can change after a context closes; these compact facts permit correcting its
// header without retaining or replaying transcript payloads.
type summaryFact struct {
	kind             v1.EventKind
	source           v1.EventMessageSource
	ts               int64
	end              int64
	tool             string
	system           string
	text             bool
	duration         float64
	globalQuota      bool
	quotaStatus      v1.EventRateLimitStatus
	quotaType        string
	quotaUtilization float64
}

type toolKey struct {
	thread     string
	delegation string
	turn       string
	agent      string
	id         string
}

type toolStart struct {
	name string
	ts   int64
}

type activityRoute struct {
	id        string
	tool      string
	namespace string
}

// transcriptOrigin retains only correlation identity, never text or tool data.
type transcriptOrigin struct {
	agent     string
	tool      string
	namespace string
}

type transcriptObservation struct {
	position Position
	at       time.Time
}

func positionBefore(a, b Position) bool {
	return a.RecordID < b.RecordID || a.RecordID == b.RecordID && a.Index < b.Index
}

type delegationScope struct {
	tool      string
	namespace string
}

func appendRecord(records []uint64, id uint64) []uint64 {
	if len(records) == 0 || records[len(records)-1] != id {
		return append(records, id)
	}
	return records
}

func appendRecordPrefix(dst, records []uint64, through uint64) []uint64 {
	end, found := slices.BinarySearch(records, through)
	if found {
		end++
	}
	return append(dst, records[:end]...)
}

func eventPosition(e *Event) Position { return Position{RecordID: e.RecordID, Index: e.Index} }

func historyChild(src *v1.EventMessageSource) bool {
	return src.AgentID != "" || src.DelegationToolUseID != "" || src.RootThreadID != "" && src.ThreadID != "" && src.ThreadID != src.RootThreadID
}

func rootEvent(m *v1.EventMessage) bool {
	return !historyChild(&m.Source) || m.Kind == v1.EventKindRateLimit && m.RateLimit != nil && m.RateLimit.RateLimitType != "thread_goal"
}

func scopedTool(src *v1.EventMessageSource, id string) toolKey {
	agent := src.AgentID
	if src.ThreadID != "" || src.DelegationToolUseID != "" {
		agent = ""
	}
	return toolKey{src.ThreadID, src.DelegationToolUseID, src.TurnID, agent, id}
}

func resolvedSkills(loads []v1.EventSkillLoad) []v1.EventSkillLoad {
	out := []v1.EventSkillLoad{}
	for _, load := range loads {
		if load.Path == "" {
			var paths []v1.EventSkillLoad
			for _, other := range loads {
				if other.Skill == load.Skill && other.Path != "" {
					paths = append(paths, other)
				}
			}
			if len(paths) == 1 {
				load = paths[0]
			}
		}
		if !slices.Contains(out, load) {
			out = append(out, load)
		}
	}
	return out
}

func historyTime(ts int64) time.Time {
	if ts <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ts).UTC()
}

func terminal(status string) bool {
	return status == "completed" || status == "failed" || status == "interrupted"
}

func countSection(section *v1.HistorySection, groups []summaryGroup) {
	section.TurnCount = 0
	section.ToolCount = 0
	section.TextCount = 0
	section.DurationMS = 0
	start, end := int64(0), int64(0)
	pending := false
	for _, g := range groups {
		if g.kind == "text" && !g.everText && !g.thinking && g.key == "" {
			continue
		}
		pending = true
		section.ToolCount += g.tools
		if g.kind == "text" {
			section.TextCount++
		}
		if g.start > 0 && (start == 0 || g.start < start) {
			start = g.start
		}
		end = max(end, g.end)
		if g.result {
			section.TurnCount++
			if g.duration > 0 {
				section.DurationMS += g.duration
			} else if start > 0 {
				section.DurationMS += max(int64(0), end-start)
			}
			start = 0
			end = 0
			pending = false
		}
	}
	if pending {
		section.TurnCount++
		if start > 0 {
			section.DurationMS += max(int64(0), end-start)
		}
	}
}

func validSlice(body v1.HistorySlice, through uint64) error {
	if body.First.RecordID == 0 || body.Last.RecordID > through || body.Last.RecordID < body.First.RecordID || body.Last.RecordID == body.First.RecordID && body.Last.Index < body.First.Index {
		return errors.New("invalid normalized history slice")
	}
	return nil
}

// appendRecordRange selects an indexed finite interval before copying record IDs.
func appendRecordRange(dst, records []uint64, first, through uint64) []uint64 {
	start, _ := slices.BinarySearch(records, first)
	end, found := slices.BinarySearch(records, through)
	if found {
		end++
	}
	return append(dst, records[start:end]...)
}

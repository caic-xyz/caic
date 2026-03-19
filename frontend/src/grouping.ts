// Pure grouping and turn-splitting logic for agent event streams.
import type { EventMessage, EventToolUse, EventToolResult, EventAsk } from "@sdk/types.gen";
import { formatElapsed } from "./formatting";

export interface MessageGroup {
  kind: "text" | "action" | "ask" | "userInput" | "widget" | "other";
  events: EventMessage[];
  // For "action" groups: paired tool_use and tool_result events (empty for thinking-only).
  toolCalls: ToolCall[];
  // For "ask" groups: the ask payload.
  ask?: EventAsk;
  // For "ask" groups: the user's submitted answer (from the following userInput).
  answerText?: string;
  // For "widget" groups: accumulated HTML and metadata.
  widgetToolUseID?: string;
  widgetTitle?: string;
  widgetHTML?: string;
  widgetDone?: boolean;
}

// A tool_use event paired with its optional tool_result.
// done is true when the tool has completed — either via an explicit result
// event or implicitly because a later event arrived (the agent moved on).
export interface ToolCall {
  use: EventToolUse;
  result?: EventToolResult;
  done: boolean;
}

// A turn is a sequence of message groups between user interactions.
// Turns are separated by "result" messages (end of a Claude Code query).
export interface Turn {
  groups: MessageGroup[];
  toolCount: number;
  textCount: number;
  // Duration of the turn in milliseconds (last event ts minus first event ts).
  durationMs: number;
}

// A session is a segment of the event stream opened by an init or compact_boundary event.
// Session boundaries are init events (new Claude Code session) and compact_boundary system
// events (context compaction). The current (last) session is never elided.
export interface Session {
  // The event that opened this session (init or compact_boundary system event).
  // undefined for an implicit initial segment before the first session event.
  boundaryEvent?: EventMessage;
  turns: Turn[];
  toolCount: number;
  textCount: number;
}

// Tool names (case-insensitive) that are async and emit explicit toolResult
// events. All other Claude Code tools complete synchronously and are done
// as soon as their toolUse event is emitted.
const ASYNC_TOOLS = new Set(["bash", "task", "show_widget"]);

// Returns true if ev starts a new session.
export function isSessionBoundary(ev: EventMessage): boolean {
  return ev.kind === "init" || (ev.kind === "system" && ev.system?.subtype === "compact_boundary");
}

export function groupMessages(msgs: EventMessage[]): MessageGroup[] {
  const groups: MessageGroup[] = [];

  function lastGroup(): MessageGroup | undefined {
    return groups[groups.length - 1];
  }

  let usageSinceLastTool = false;

  for (const ev of msgs) {
    switch (ev.kind) {
      case "text": {
        // A final text event replaces any preceding textDelta group.
        const last = lastGroup();
        if (last && last.kind === "text" && last.events.some((e) => e.kind === "textDelta")) {
          last.events.push(ev);
        } else if (last && last.kind === "action" && last.toolCalls.length === 0) {
          // Text immediately after a thinking-only group: absorb thinking into this text group
          // so it renders as a collapsed block inside the text rather than a separate top-level item.
          const thinkingGroup = groups.pop();
          groups.push({ kind: "text", events: [...(thinkingGroup?.events ?? []), ev], toolCalls: [] });
        } else {
          groups.push({ kind: "text", events: [ev], toolCalls: [] });
        }
        break;
      }
      case "textDelta": {
        const last = lastGroup();
        if (last && last.kind === "text") {
          last.events.push(ev);
        } else if (last && last.kind === "action" && last.toolCalls.length === 0) {
          // Text immediately after a thinking-only group: absorb thinking into this text group.
          const thinkingGroup = groups.pop();
          groups.push({ kind: "text", events: [...(thinkingGroup?.events ?? []), ev], toolCalls: [] });
        } else {
          groups.push({ kind: "text", events: [ev], toolCalls: [] });
        }
        break;
      }
      case "toolUse": {
        if (ev.toolUse) {
          // Synchronous tools complete before the next event; only async tools
          // (Bash, Task) emit an explicit toolResult later.
          const call: ToolCall = { use: ev.toolUse, done: !ASYNC_TOOLS.has(ev.toolUse.name.toLowerCase()) };
          const last = lastGroup();
          if (last && last.kind === "action" && last.toolCalls.length > 0 && !usageSinceLastTool) {
            // Consecutive toolUse in the same AssistantMessage — merge.
            last.events.push(ev);
            last.toolCalls.push(call);
          } else if (!usageSinceLastTool) {
            // Same AssistantMessage but intervening text; find the most
            // recent action group with tool calls to coalesce into.
            let coalesced = false;
            for (let i = groups.length - 1; i >= 0; i--) {
              if (groups[i].kind === "action" && groups[i].toolCalls.length > 0) {
                groups[i].events.push(ev);
                groups[i].toolCalls.push(call);
                coalesced = true;
                break;
              }
            }
            if (!coalesced) {
              groups.push({ kind: "action", events: [ev], toolCalls: [call] });
            }
          } else {
            // New AssistantMessage — start a new action group.
            groups.push({ kind: "action", events: [ev], toolCalls: [call] });
            usageSinceLastTool = false;
          }
        }
        break;
      }
      case "toolResult": {
        if (ev.toolResult) {
          const tr = ev.toolResult;
          // Check widget groups first for matching toolResult.
          let matched = false;
          for (let i = groups.length - 1; i >= 0; i--) {
            const g = groups[i];
            if (g.kind === "widget" && g.widgetToolUseID === tr.toolUseID) {
              g.widgetDone = true;
              g.events.push(ev);
              matched = true;
              break;
            }
          }
          if (!matched) {
            // Search all tool groups for the matching toolUseID — results may
            // arrive after intervening text/other groups, not just the last group.
            for (let i = groups.length - 1; i >= 0; i--) {
              const g = groups[i];
              if (g.kind !== "action") continue;
              const tc = g.toolCalls.find((c) => c.use.toolUseID === tr.toolUseID && !c.result);
              if (tc) {
                tc.result = tr;
                tc.done = true;
                g.events.push(ev);
                matched = true;
                break;
              }
            }
          }
          if (!matched) {
            groups.push({ kind: "action", events: [ev], toolCalls: [] });
          }
        }
        break;
      }
      case "ask":
        if (ev.ask) {
          groups.push({ kind: "ask", events: [ev], toolCalls: [], ask: ev.ask });
        }
        break;
      case "userInput": {
        // Look backwards past result/other groups to find the most recent
        // unanswered ask group. The agent emits a result event after
        // AskUserQuestion, so the ask group is typically not the last group.
        let askGroup: MessageGroup | undefined;
        for (let i = groups.length - 1; i >= 0; i--) {
          const g = groups[i];
          if (g.kind === "ask" && !g.answerText) { askGroup = g; break; }
          if (g.kind !== "other") break; // stop at non-other boundaries
        }
        if (askGroup) {
          askGroup.answerText = ev.userInput?.text;
          askGroup.events.push(ev);
        } else {
          groups.push({ kind: "userInput", events: [ev], toolCalls: [] });
        }
        break;
      }
      case "usage":
        {
          usageSinceLastTool = true;
          const last = lastGroup();
          if (last && (last.kind === "text" || last.kind === "action")) {
            last.events.push(ev);
          } else {
            groups.push({ kind: "other", events: [ev], toolCalls: [] });
          }
        }
        break;
      case "todo":
        // Rendered by ProgressPanel from messages() directly; skip here to avoid
        // splitting consecutive tool groups.
        break;
      case "diffStat":
        // Metadata-only; live diff stat shown in the task list via Task.diffStat.
        break;
      case "thinking": {
        // A final thinking event replaces any preceding thinkingDelta in the same action group.
        const last = lastGroup();
        if (last && last.kind === "action" && last.toolCalls.length === 0 &&
            last.events.some((e) => e.kind === "thinkingDelta")) {
          last.events.push(ev);
        } else {
          groups.push({ kind: "action", events: [ev], toolCalls: [] });
        }
        break;
      }
      case "thinkingDelta": {
        const last = lastGroup();
        if (last && last.kind === "action" && last.toolCalls.length === 0) {
          last.events.push(ev);
        } else {
          groups.push({ kind: "action", events: [ev], toolCalls: [] });
        }
        break;
      }
      case "toolOutputDelta": {
        // Append to the most recent action group that owns this tool call so
        // the accumulated output can be displayed inside its ToolCallCard.
        const id = ev.toolOutputDelta?.toolUseID;
        if (id) {
          for (let i = groups.length - 1; i >= 0; i--) {
            const g = groups[i];
            if (g.kind !== "action") continue;
            if (g.toolCalls.some((c) => c.use.toolUseID === id)) {
              g.events.push(ev);
              break;
            }
          }
        }
        break;
      }
      case "system": {
        // compact_boundary is consumed by groupSessions() before reaching here.
        // Thread status changes (active, idle, etc.) duplicate information
        // already in the task state — skip them to avoid noisy "other" groups.
        // model_rerouted and other informational subtypes are rendered via MessageItem.
        const sub = ev.system?.subtype;
        if (sub === "active" || sub === "idle" || sub === "notLoaded" || sub === "system_error") break;
        groups.push({ kind: "other", events: [ev], toolCalls: [] });
        break;
      }
      case "widgetDelta": {
        const id = ev.widgetDelta?.toolUseID;
        if (id) {
          // Find or create a widget group for this toolUseID.
          let widgetGroup: MessageGroup | undefined;
          for (let i = groups.length - 1; i >= 0; i--) {
            if (groups[i].kind === "widget" && groups[i].widgetToolUseID === id) {
              widgetGroup = groups[i];
              break;
            }
          }
          if (widgetGroup) {
            widgetGroup.events.push(ev);
            widgetGroup.widgetHTML = (widgetGroup.widgetHTML ?? "") + (ev.widgetDelta?.delta ?? "");
          } else {
            groups.push({
              kind: "widget", events: [ev], toolCalls: [],
              widgetToolUseID: id,
              widgetHTML: ev.widgetDelta?.delta ?? "",
              widgetDone: false,
            });
          }
        }
        break;
      }
      case "widget": {
        if (ev.widget) {
          const id = ev.widget.toolUseID;
          // Find existing widget group or create a new one.
          let widgetGroup: MessageGroup | undefined;
          for (let i = groups.length - 1; i >= 0; i--) {
            if (groups[i].kind === "widget" && groups[i].widgetToolUseID === id) {
              widgetGroup = groups[i];
              break;
            }
          }
          if (widgetGroup) {
            widgetGroup.events.push(ev);
            widgetGroup.widgetTitle = ev.widget.title;
            widgetGroup.widgetHTML = ev.widget.html;
          } else {
            groups.push({
              kind: "widget", events: [ev], toolCalls: [],
              widgetToolUseID: id,
              widgetTitle: ev.widget.title,
              widgetHTML: ev.widget.html,
              widgetDone: false,
            });
          }
        }
        break;
      }
      case "rateLimit":
        // Only surface warning/rejected statuses; "allowed" is not interesting.
        if (ev.rateLimit && ev.rateLimit.status !== "allowed") {
          groups.push({ kind: "other", events: [ev], toolCalls: [] });
        }
        break;
      case "subagentStart":
      case "subagentEnd":
        // Skip: subagent lifecycle events are not rendered yet. Explicitly
        // listed to avoid creating OTHER groups that act as hard barriers.
        break;
      default:
        groups.push({ kind: "other", events: [ev], toolCalls: [] });
        break;
    }
  }

  // Merge action groups separated only by text groups.  The agent often emits
  // short commentary between tool turns ("Let me read...", "Now let me edit...").
  // Without merging, each turn shows as a separate 1-tool block.  ask, userInput,
  // and other groups act as hard boundaries that prevent merging.  Text groups
  // between action groups are kept for display; tool calls are consolidated into
  // the first action group of each run.  Thinking-only action groups adjacent to
  // tool-call action groups are absorbed into them.
  const merged: MessageGroup[] = [];
  for (const g of groups) {
    if (g.kind === "action" && g.toolCalls.length > 0) {
      // Find the nearest non-text, non-thinking-only anchor action group.
      let anchor: MessageGroup | undefined;
      for (let i = merged.length - 1; i >= 0; i--) {
        const m = merged[i];
        if (m.kind === "text" || (m.kind === "action" && m.toolCalls.length === 0)) continue;
        anchor = m;
        break;
      }
      // Absorb any trailing thinking-only action groups from the merged list.
      const thinkingEvents: EventMessage[] = [];
      while (merged.length > 0) {
        const last = merged[merged.length - 1];
        if (last.kind !== "action" || last.toolCalls.length > 0) break;
        thinkingEvents.unshift(...(merged.pop()?.events ?? []));
      }
      if (anchor && anchor.kind === "action") {
        // Merge tool calls into the existing anchor; prepend absorbed thinking events.
        anchor.events.push(...thinkingEvents, ...g.events);
        anchor.toolCalls.push(...g.toolCalls);
        continue;
      }
      if (thinkingEvents.length > 0) {
        // New action group with absorbed thinking events prepended.
        merged.push({ kind: "action", events: [...thinkingEvents, ...g.events], toolCalls: g.toolCalls });
        continue;
      }
    } else if (g.kind === "action" && g.toolCalls.length === 0) {
      // Thinking-only group immediately following a tool-call group: absorb into it.
      const last = merged[merged.length - 1];
      if (last && last.kind === "action" && last.toolCalls.length > 0) {
        last.events.push(...g.events);
        continue;
      }
    } else if (g.kind === "text") {
      // Text group with embedded thinking (from initial-pass absorption) following
      // a tool-call group: move thinking events into the tool group so they render
      // inside the collapsed tool block, not as a standalone ThinkingCard.
      const last = merged[merged.length - 1];
      if (last && last.kind === "action" && last.toolCalls.length > 0) {
        const thinkingEvs = g.events.filter((e) => e.kind === "thinking" || e.kind === "thinkingDelta");
        if (thinkingEvs.length > 0) {
          last.events.push(...thinkingEvs);
          g.events = g.events.filter((e) => e.kind !== "thinking" && e.kind !== "thinkingDelta");
        }
      }
    }
    merged.push(g);
  }

  // Mark tool calls as implicitly done when later events exist.
  // Claude Code doesn't emit explicit toolResult events for synchronous
  // tools (Read, Edit, Grep, etc.), so any tool call followed by a later
  // group is implicitly complete — only the very last tool group may have
  // genuinely pending calls.
  const lastActionGroupIdx = merged.findLastIndex((g) => g.kind === "action" && g.toolCalls.length > 0);
  for (let i = 0; i < merged.length; i++) {
    const g = merged[i];
    if (g.kind !== "action" || g.toolCalls.length === 0) continue;
    if (i < lastActionGroupIdx || i < merged.length - 1) {
      for (const tc of g.toolCalls) tc.done = true;
    }
  }
  return merged;
}

// Splits message groups into turns separated by "result" events.
export function groupTurns(groups: MessageGroup[]): Turn[] {
  const turns: Turn[] = [];
  let current: MessageGroup[] = [];
  let toolCount = 0;
  let textCount = 0;
  let firstTs = 0;
  let lastTs = 0;
  let hasTs = false;
  // Authoritative duration from the result event (seconds → ms). Undefined for
  // live incomplete turns, which fall back to ts-based computation.
  // ResultMessage.DurationMs is per-invocation (not cumulative), use directly.
  let resultDurationMs: number | undefined;
  // True when a result event has been seen for this turn (even if duration == 0).
  // Completed turns don't fall back to ts-based, which would inflate with idle time.
  let hasResultEvent = false;

  function flush() {
    if (current.length > 0) {
      const durationMs = hasResultEvent ? (resultDurationMs ?? 0) : Math.max(0, lastTs - firstTs);
      turns.push({ groups: current, toolCount, textCount, durationMs });
      current = [];
      toolCount = 0;
      textCount = 0;
      firstTs = 0;
      lastTs = 0;
      hasTs = false;
      resultDurationMs = undefined;
      hasResultEvent = false;
    }
  }

  for (const g of groups) {
    current.push(g);
    if (g.kind === "action") {
      toolCount += g.toolCalls.length;
    } else if (g.kind === "text") {
      textCount++;
    }
    for (const ev of g.events) {
      if (!hasTs) { firstTs = ev.ts; hasTs = true; }
      lastTs = ev.ts;
      if (ev.kind === "result" && ev.result !== undefined) {
        hasResultEvent = true;
        const durationMs = Math.round((ev.result.duration ?? 0) * 1000);
        if (durationMs > 0) {
          resultDurationMs = durationMs;
        }
      }
    }
    if (g.kind === "other" && g.events.some((ev) => ev.kind === "result")) {
      flush();
    }
  }
  flush();
  return turns;
}

// Splits messages into sessions at init (only when sessionID changes) and compact_boundary events.
// The boundary event starts the new session and is NOT passed to groupMessages.
// Re-invocations of Claude Code that share the same sessionID are NOT new sessions — they are
// just new turns within the same session.
export function groupSessions(msgs: EventMessage[]): Session[] {
  const sessions: Session[] = [];
  let segment: EventMessage[] = [];
  let boundaryEvent: EventMessage | undefined;
  let currentSessionID: string | undefined;

  function flushSession() {
    const groups = groupMessages(segment);
    const turns = groupTurns(groups);
    let toolCount = 0;
    let textCount = 0;
    for (const t of turns) {
      toolCount += t.toolCount;
      textCount += t.textCount;
    }
    sessions.push({ boundaryEvent, turns, toolCount, textCount });
    boundaryEvent = undefined;
    segment = [];
  }

  function flushAndCarry() {
    // Carry post-result events forward into the new session (e.g. a userInput sent just
    // before the new boundary). They belong to the session they triggered.
    const lastResultIdx = segment.findLastIndex((e) => e.kind === "result");
    const carry = lastResultIdx >= 0 ? segment.splice(lastResultIdx + 1) : [];
    flushSession();
    segment = carry;
  }

  for (const ev of msgs) {
    if (ev.kind === "init") {
      const newID = ev.init?.sessionID;
      if (newID !== currentSessionID) {
        // Genuinely new session: different sessionID.
        if (boundaryEvent !== undefined) flushAndCarry();
        boundaryEvent = ev;
        currentSessionID = newID;
      }
      // Same sessionID: re-invocation within the same session; skip the init event entirely.
    } else if (ev.kind === "system" && ev.system?.subtype === "compact_boundary") {
      if (boundaryEvent !== undefined) flushAndCarry();
      boundaryEvent = ev;
      currentSessionID = undefined; // Reset so next init is treated as a new boundary.
    } else {
      segment.push(ev);
    }
  }
  if (segment.length > 0 || boundaryEvent !== undefined) {
    flushSession();
  }
  return sessions;
}

// Flat display item for the message list.
export type MsgItem =
  | { kind: "sessionElided"; session: Session; sessionKey: string; key: string }
  | { kind: "sessionHeader"; session: Session; sessionKey: string; key: string }
  | { kind: "sessionBoundary"; event: EventMessage; key: string }
  | { kind: "elided"; turn: Turn; key: string; indent?: "session" }
  | { kind: "expandedHeader"; turn: Turn; turnKey: string; key: string; indent?: "session" }
  | { kind: "group"; group: MessageGroup; isLive: boolean; key: string; indent?: "turn" };

// Builds flat items for a list of turns within a session.
// All turns in this list are collapsible (caller handles the live turn separately).
// inPastSession: true when building turns for an expanded past session — items receive
// indent markers so the renderer can draw a visual left-border hierarchy.
export function buildTurnItems(turns: Turn[], expandedTurnKeys: ReadonlySet<string>, sessionKey: string, inPastSession = false): MsgItem[] {
  const items: MsgItem[] = [];
  for (let i = 0; i < turns.length; i++) {
    const turn = turns[i];
    const turnKey = `${sessionKey}:turn:${i}:${turn.groups[0]?.events[0]?.ts ?? ""}`;
    if (expandedTurnKeys.has(turnKey)) {
      items.push({ kind: "expandedHeader", turn, turnKey, key: `hdr:${turnKey}`, ...(inPastSession ? { indent: "session" as const } : {}) });
      for (let j = 0; j < turn.groups.length; j++) {
        items.push({ kind: "group", group: turn.groups[j], isLive: false, key: `${turnKey}-g${j}`, indent: "turn" });
      }
    } else {
      items.push({ kind: "elided", turn, key: turnKey, ...(inPastSession ? { indent: "session" as const } : {}) });
    }
  }
  return items;
}

// Builds flat items for past (non-current) sessions.
// Each past session is elided to a single row by default; click to expand.
export function buildPastSessionItems(sessions: Session[], expandedSessionKeys: ReadonlySet<string>, expandedTurnKeys: ReadonlySet<string>): MsgItem[] {
  const items: MsgItem[] = [];
  for (let i = 0; i < sessions.length; i++) {
    const session = sessions[i];
    const sessionKey = `session:${i}:${session.boundaryEvent?.ts ?? ""}`;
    if (expandedSessionKeys.has(sessionKey)) {
      items.push({ kind: "sessionHeader", session, sessionKey, key: `hdr:${sessionKey}` });
      items.push(...buildTurnItems(session.turns, expandedTurnKeys, sessionKey, true));
    } else {
      items.push({ kind: "sessionElided", session, sessionKey, key: sessionKey });
    }
  }
  return items;
}

export function toolCountSummary(calls: ToolCall[]): string {
  const counts = new Map<string, number>();
  for (const tc of calls) {
    const n = tc.use.name;
    counts.set(n, (counts.get(n) ?? 0) + 1);
  }
  return Array.from(counts.entries())
    .map(([name, c]) => (c > 1 ? `${name} \u00d7${c}` : name))
    .join(", ");
}

export function turnSummary(turn: Turn): string {
  const parts: string[] = [];
  if (turn.textCount > 0) {
    parts.push(turn.textCount === 1 ? "1 message" : `${turn.textCount} messages`);
  }
  if (turn.toolCount > 0) {
    parts.push(turn.toolCount === 1 ? "1 tool call" : `${turn.toolCount} tool calls`);
  }
  const summary = parts.length > 0 ? parts.join(", ") : "empty turn";
  return turn.durationMs > 0 ? `${summary} \u00b7 ${formatElapsed(turn.durationMs)}` : summary;
}

export function sessionSummary(session: Session): string {
  const parts: string[] = [];
  if (session.boundaryEvent?.kind === "init" && session.boundaryEvent.init) {
    const id = session.boundaryEvent.init.sessionID;
    parts.push(`Session ${id.slice(0, 8)}`);
  } else {
    parts.push("Compacted session");
  }
  if (session.textCount > 0) {
    parts.push(session.textCount === 1 ? "1 message" : `${session.textCount} messages`);
  }
  if (session.toolCount > 0) {
    parts.push(session.toolCount === 1 ? "1 tool call" : `${session.toolCount} tool calls`);
  }
  return parts.join(" \u00b7 ");
}

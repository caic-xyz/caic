// Source-scoped text and tool grouping with bounded stream snapshots and compact display aggregates.

import type {
  EventMessage,
  EventMessageSource,
  EventToolUse,
  EventToolResult,
  EventAsk,
  EventResult,
} from "@sdk/types.gen";

export interface MessageGroup {
  range?: { start: number; end: number } | null;
  tool?: ScopedTool;
  // Provider identity keeps concurrent messages separate and fixes their first position.
  messageKey?: string;
  // Identified items fold raw fragments into one immutable content snapshot.
  // events retains only first/latest provenance; range retains the full span.
  content?: {
    text: string;
    complete: boolean;
    range: { start: number; end: number } | null;
  };
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
  // For result groups whose payload canonically replaces duplicate text.
  resultReplacesText?: boolean;
  // Keep the identified message mounted while the result card shows metadata.
  resultDuplicatesText?: boolean;
  // Thinking event segments whose durations are summed when several Codex
  // thinking blocks collapse into one visual group.
  timingSegments?: EventMessage[][];
}

export interface ScopedTool {
  startedAt: number;
  finishedAt: number;
  ended: boolean;
  key: string;
  source: EventMessageSource;
  use?: EventToolUse;
  result?: EventToolResult;
  output: string;
  range: { start: number; end: number } | null;
}

export function isChildEvent(event: EventMessage): boolean {
  const s = event.source;
  return !!(s?.agentID || s?.delegationToolUseID || (s?.rootThreadID && s.threadID && s.threadID !== s.rootThreadID));
}

export function toolIdentity(source: EventMessageSource | undefined, id: string): string {
  const thread = source?.threadID ?? "";
  const delegation = source?.delegationToolUseID ?? "";
  return JSON.stringify([
    thread,
    delegation,
    source?.turnID ?? "",
    thread || delegation ? "" : (source?.agentID ?? ""),
    id,
  ]);
}

// Legacy coalescing may combine source-free calls; scoped groups own exactly one invocation.
function isLegacyToolGroup(group: MessageGroup): boolean {
  return !group.tool && !group.messageKey && group.kind === "action" && group.toolCalls.length > 0;
}

function matchesToolCall(call: ToolCall, source: EventMessageSource | undefined, id: string): boolean {
  return toolIdentity(call.source, call.use.toolUseID) === toolIdentity(source, id);
}

export function messageSource(event: EventMessage): EventMessageSource | undefined {
  return event.source;
}

export function messageIdentity(event: EventMessage): string | null {
  if (
    event.kind !== "text" &&
    event.kind !== "textDelta" &&
    event.kind !== "thinking" &&
    event.kind !== "thinkingDelta"
  )
    return null;
  const source = messageSource(event);
  if (!source?.threadID || !source.turnID || !source.itemID) return null;
  const channel = event.kind === "thinking" || event.kind === "thinkingDelta" ? "thinking" : "text";
  return JSON.stringify([source.threadID, source.turnID, source.itemID, channel]);
}

export function childAgentID(event: EventMessage): string | null {
  return event.source?.agentID || null;
}

function toolEventID(event: EventMessage): string | null {
  if (event.kind === "toolUse") return event.toolUse?.toolUseID ?? null;
  if (event.kind === "toolResult") return event.toolResult?.toolUseID ?? null;
  if (event.kind === "toolOutputDelta") return event.toolOutputDelta?.toolUseID ?? null;
  return null;
}

function appendScopedTool(group: MessageGroup, event: EventMessage, key: string, ended: boolean): void {
  const previous = group.tool;
  const range = previous?.range;
  group.tool = {
    key,
    startedAt: previous?.startedAt || (event.toolUse ? event.ts : 0),
    finishedAt: event.toolResult ? event.ts : (previous?.finishedAt ?? 0),
    ended: ended || previous?.ended === true,
    source: { ...previous?.source, ...event.source },
    use: event.toolUse ?? previous?.use,
    result: event.toolResult ?? previous?.result,
    output: (previous?.output ?? "") + (event.toolOutputDelta?.delta ?? ""),
    range:
      event.ts > 0
        ? { start: Math.min(range?.start ?? event.ts, event.ts), end: Math.max(range?.end ?? event.ts, event.ts) }
        : (range ?? null),
  };
  const first = group.events[0];
  group.events = first ? [first, event] : [event];
  const tool = group.tool;
  group.toolCalls = tool.use
    ? [
        {
          use: tool.use,
          result: tool.result,
          done: !!tool.result || (tool.ended && !tool.use.background),
          source: tool.source,
          key,
          output: tool.output,
          durationMs: tool.result ? toolResultDurationMs(tool.result, tool.startedAt, tool.finishedAt) : null,
        },
      ]
    : [];
}

// Display aggregates retain compact summaries while each invocation owns its bounded stream state.
export function aggregateToolGroups(groups: MessageGroup[]): MessageGroup[] {
  const result: MessageGroup[] = [];
  const aggregates = new Map<string, MessageGroup>();
  for (const group of groups) {
    if (!group.tool?.use) {
      result.push(group);
      continue;
    }
    const scope = toolIdentity(group.tool.source, "");
    let aggregate = aggregates.get(scope);
    if (!aggregate) {
      aggregate = { kind: "action", messageKey: `tools:${scope}`, events: [], toolCalls: [] };
      aggregates.set(scope, aggregate);
      result.push(aggregate);
    }
    aggregate.toolCalls.push(...group.toolCalls);
    if (group.tool.range)
      aggregate.range = {
        start: Math.min(aggregate.range?.start ?? group.tool.range.start, group.tool.range.start),
        end: Math.max(aggregate.range?.end ?? group.tool.range.end, group.tool.range.end),
      };
    if (aggregate.events.length === 0) aggregate.events = group.events;
  }
  return result;
}

function appendIdentifiedMessage(group: MessageGroup, event: EventMessage): void {
  const text = event.text?.text ?? event.textDelta?.text ?? event.thinking?.text ?? event.thinkingDelta?.text ?? "";
  const complete = event.kind === "text" || event.kind === "thinking";
  const previous = group.content;
  const range = previous?.range ?? null;
  group.content = {
    text: complete ? text : previous?.complete ? previous.text : (previous?.text ?? "") + text,
    complete: complete || previous?.complete === true,
    range:
      event.ts > 0
        ? { start: Math.min(range?.start ?? event.ts, event.ts), end: Math.max(range?.end ?? event.ts, event.ts) }
        : range,
  };
  const first = group.events[0];
  group.events = first ? [first, event] : [event];
}

// A tool_use event paired with its optional tool_result.
// done is true when the tool has completed — either via an explicit result
// event or implicitly because a later event arrived (the agent moved on).
export interface ToolCall {
  source?: EventMessageSource;
  key?: string;
  output?: string;
  durationMs?: number | null;
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
  // The result event payload for this turn, if the turn has completed.
  result?: EventResult;
}

// A session is a segment of the event stream opened by an init, compaction, or clear event.
// Session boundaries are init events (new Claude Code session) and compaction/clear system
// events. The current (last) session is never elided.
export interface Session {
  // The event that opened this session (init, compaction, or clear system event).
  // undefined for an implicit initial segment before the first session event.
  boundaryEvent?: EventMessage;
  turns: Turn[];
  toolCount: number;
  textCount: number;
  // Sum of turn runtimes, excluding time between a result and the next user input.
  durationMs: number;
}

// Returns true if ev starts a new session.
export function isSessionBoundary(ev: EventMessage): boolean {
  if (isChildEvent(ev)) return false;
  return (
    ev.kind === "init" ||
    (ev.kind === "system" && (ev.system?.subtype === "compact_boundary" || ev.system?.subtype === "context_cleared"))
  );
}

// rateLimitPercentage converts a utilization fraction to a percentage. A
// negative utilization means the harness reported only a status change, so
// callers that display it must check before formatting.
export function rateLimitPercentage(utilization: number): number {
  return Math.round(utilization * 100);
}

function rateLimitWarningPercentagesAfter(msgs: ReadonlyArray<EventMessage>): Map<string, number> {
  const percentages = new Map<string, number>();
  for (const ev of msgs) {
    const rl = ev.rateLimit;
    if (ev.kind !== "rateLimit" || !rl) continue;
    if (rl.status === "allowed_warning") {
      percentages.set(rl.rateLimitType, rateLimitPercentage(rl.utilization));
    } else {
      percentages.delete(rl.rateLimitType);
    }
  }
  return percentages;
}

// Groups consecutive events for cohesive rendering.
//
// IncrementalMessageGrouper uses this for cold starts and structural changes,
// then extends pure streaming-delta batches without regrouping the full turn.
export function groupMessages(msgs: EventMessage[]): MessageGroup[] {
  return groupMessagesAfter(msgs, new Map());
}

function groupMessagesAfter(
  msgs: EventMessage[],
  priorWarningPercentages: ReadonlyMap<string, number>,
): MessageGroup[] {
  const groups: MessageGroup[] = [];
  const byIdentity = new Map<string, MessageGroup>();
  const byTool = new Map<string, MessageGroup>();

  function lastGroup(): MessageGroup | undefined {
    const group = groups[groups.length - 1];
    return group?.messageKey ? undefined : group;
  }

  let usageSinceLastTool = false;
  const previousWarningPercentages = new Map(priorWarningPercentages);

  for (const ev of msgs) {
    const toolID = toolEventID(ev);
    if (ev.kind === "toolResult" && toolID) {
      const widget = groups.find(
        (group) =>
          group.kind === "widget" &&
          group.widgetToolUseID === toolID &&
          toolIdentity(group.events[0]?.source, toolID) === toolIdentity(ev.source, toolID),
      );
      if (widget) {
        widget.widgetDone = true;
        widget.events.push(ev);
        continue;
      }
    }
    if (ev.kind === "result" && ev.source) {
      const scope = toolIdentity(ev.source, "");
      for (const group of byTool.values()) {
        if (group.tool && toolIdentity(group.tool.source, "") === scope) {
          group.tool = { ...group.tool, ended: true };
          group.toolCalls = group.toolCalls.map((call) => (call.use.background ? call : { ...call, done: true }));
        }
      }
    }
    if (toolID !== null && (ev.source || byTool.has(toolIdentity(ev.source, toolID)))) {
      const key = toolIdentity(ev.source, toolID);
      let group = byTool.get(key);
      if (!group) {
        group = { kind: "action", messageKey: `tool:${key}`, events: [], toolCalls: [] };
        byTool.set(key, group);
        groups.push(group);
      }
      appendScopedTool(group, ev, key, false);
      continue;
    }
    const messageKey = messageIdentity(ev);
    if (messageKey !== null) {
      const existing = byIdentity.get(messageKey);
      if (existing) appendIdentifiedMessage(existing, ev);
      else {
        const group: MessageGroup = {
          kind: ev.kind === "thinking" || ev.kind === "thinkingDelta" ? "action" : "text",
          messageKey,
          events: [],
          toolCalls: [],
        };
        appendIdentifiedMessage(group, ev);
        groups.push(group);
        byIdentity.set(messageKey, group);
      }
      continue;
    }
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
          groups.push({
            kind: "text",
            events: [...(thinkingGroup?.events ?? []), ev],
            toolCalls: [],
          });
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
          groups.push({
            kind: "text",
            events: [...(thinkingGroup?.events ?? []), ev],
            toolCalls: [],
          });
        } else {
          groups.push({ kind: "text", events: [ev], toolCalls: [] });
        }
        break;
      }
      case "toolUse": {
        if (ev.toolUse) {
          // All tool calls start as pending; they become done when a toolResult
          // arrives or when later events prove the agent moved on (implicit done).
          const call: ToolCall = { use: ev.toolUse, done: false };
          const last = lastGroup();
          if (last && isLegacyToolGroup(last) && !usageSinceLastTool) {
            // Consecutive toolUse in the same AssistantMessage — merge.
            last.events.push(ev);
            last.toolCalls.push(call);
          } else if (!usageSinceLastTool) {
            // Same AssistantMessage but intervening text; find the most
            // recent action group with tool calls to coalesce into.
            let coalesced = false;
            for (let i = groups.length - 1; i >= 0; i--) {
              const group = groups[i];
              if (group && isLegacyToolGroup(group)) {
                group.events.push(ev);
                group.toolCalls.push(call);
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
            if (!g) throw new Error("Missing message group");
            if (
              g.kind === "widget" &&
              g.widgetToolUseID === tr.toolUseID &&
              toolIdentity(g.events[0]?.source, tr.toolUseID) === toolIdentity(ev.source, tr.toolUseID)
            ) {
              g.widgetDone = true;
              g.events.push(ev);
              matched = true;
              break;
            }
          }
          if (!matched) {
            // Search compatible legacy calls across intervening groups. Scoped
            // invocations are matched exclusively through the scoped lookup above.
            for (let i = groups.length - 1; i >= 0; i--) {
              const g = groups[i];
              if (!g) throw new Error("Missing message group");
              if (!isLegacyToolGroup(g)) continue;
              const tc = g.toolCalls.find((call) => matchesToolCall(call, ev.source, tr.toolUseID) && !call.result);
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
            const key = toolIdentity(ev.source, tr.toolUseID);
            const orphan: MessageGroup = { kind: "action", messageKey: `tool:${key}`, events: [], toolCalls: [] };
            appendScopedTool(orphan, ev, key, false);
            byTool.set(key, orphan);
            groups.push(orphan);
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
          if (!g) throw new Error("Missing message group");
          if (g.kind === "ask" && !g.answerText) {
            askGroup = g;
            break;
          }
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
        if (isChildEvent(ev)) groups.push({ kind: "other", events: [ev], toolCalls: [] });
        // Rendered by ProgressPanel from messages() directly; skip here to avoid
        // splitting consecutive tool groups.
        break;
      case "commitSnapshot":
      case "diffStat":
        // Metadata-only; repository summaries load the current diff state separately.
        break;
      case "stats":
        // Metadata-only; stats are handled by StatsIcon component.
        break;
      case "skillLoad":
        if (isChildEvent(ev)) groups.push({ kind: "other", events: [ev], toolCalls: [] });
        // Rendered by TaskDetail's loaded-skill links.
        break;
      case "thinking": {
        // A final thinking event replaces any preceding thinkingDelta: either
        // in its thinking-only action group, or in the text group that absorbed
        // it while the message was still streaming. The text group must not be
        // finalized yet (no final text event), so a non-streamed thinking block
        // from a later message starts its own group instead of bleeding into
        // the previous message. Pi streams thinking and text deltas, then
        // emits the consolidated thinking and text at message_end.
        const last = lastGroup();
        if (
          last &&
          last.toolCalls.length === 0 &&
          last.events.some((e) => e.kind === "thinkingDelta") &&
          (last.kind === "action" || (last.kind === "text" && !last.events.some((e) => e.kind === "text")))
        ) {
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
        let matched = false;
        if (id) {
          for (let i = groups.length - 1; i >= 0; i--) {
            const g = groups[i];
            if (!g) throw new Error("Missing message group");
            if (!isLegacyToolGroup(g)) continue;
            if (g.toolCalls.some((call) => matchesToolCall(call, ev.source, id))) {
              g.toolCalls = g.toolCalls.map((call) =>
                matchesToolCall(call, ev.source, id)
                  ? { ...call, output: (call.output ?? "") + (ev.toolOutputDelta?.delta ?? "") }
                  : call,
              );
              matched = true;
              break;
            }
          }
        }
        if (!matched && id) {
          const key = toolIdentity(ev.source, id);
          const orphan: MessageGroup = { kind: "action", messageKey: `tool:${key}`, events: [], toolCalls: [] };
          appendScopedTool(orphan, ev, key, false);
          byTool.set(key, orphan);
          groups.push(orphan);
        }
        break;
      }
      case "system": {
        // Compaction and clear are consumed by groupSessions() before reaching here.
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
            const group = groups[i];
            if (
              group?.kind === "widget" &&
              group.widgetToolUseID === id &&
              toolIdentity(group.events[0]?.source, id) === toolIdentity(ev.source, id)
            ) {
              widgetGroup = group;
              break;
            }
          }
          if (widgetGroup) {
            widgetGroup.events.push(ev);
            widgetGroup.widgetHTML = (widgetGroup.widgetHTML ?? "") + (ev.widgetDelta?.delta ?? "");
          } else {
            groups.push({
              kind: "widget",
              events: [ev],
              toolCalls: [],
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
            const group = groups[i];
            if (
              group?.kind === "widget" &&
              group.widgetToolUseID === id &&
              toolIdentity(group.events[0]?.source, id) === toolIdentity(ev.source, id)
            ) {
              widgetGroup = group;
              break;
            }
          }
          if (widgetGroup) {
            widgetGroup.events.push(ev);
            widgetGroup.widgetTitle = ev.widget.title;
            widgetGroup.widgetHTML = ev.widget.html;
          } else {
            groups.push({
              kind: "widget",
              events: [ev],
              toolCalls: [],
              widgetToolUseID: id,
              widgetTitle: ev.widget.title,
              widgetHTML: ev.widget.html,
              widgetDone: false,
            });
          }
        }
        break;
      }
      case "rateLimit": {
        const rl = ev.rateLimit;
        if (!rl) break;
        if (rl.status === "allowed_warning") {
          const percentage = rateLimitPercentage(rl.utilization);
          if (previousWarningPercentages.get(rl.rateLimitType) === percentage) break;
          previousWarningPercentages.set(rl.rateLimitType, percentage);
          groups.push({ kind: "other", events: [ev], toolCalls: [] });
        } else {
          previousWarningPercentages.delete(rl.rateLimitType);
          // Rejections remain visible; "allowed" is not interesting.
          if (rl.status === "rejected") {
            groups.push({ kind: "other", events: [ev], toolCalls: [] });
          }
        }
        break;
      }
      case "subagentStart":
      case "subagentEnd":
      case "nativeSubagent":
      case "backgroundCommand":
        // Skip: subagent lifecycle events are not rendered yet. Explicitly
        // listed to avoid creating OTHER groups that act as hard barriers.
        break;
      case "log":
        // Setup output is rendered in TaskDetail's persistent setup-log panel,
        // rather than inside a turn that can be elided.
        break;
      case "init":
      case "result":
      case "error":
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
    if (g.messageKey) {
      merged.push(g);
      continue;
    }
    if (g.kind === "action" && g.toolCalls.length > 0) {
      // Find the nearest non-text, non-thinking-only anchor action group.
      let anchor: MessageGroup | undefined;
      for (let i = merged.length - 1; i >= 0; i--) {
        const m = merged[i];
        if (!m) throw new Error("Missing message group");
        if (m.messageKey) break;
        if (m.kind === "text" || (m.kind === "action" && m.toolCalls.length === 0)) continue;
        anchor = m;
        break;
      }
      // Absorb any trailing thinking-only action groups from the merged list.
      const thinkingEvents: EventMessage[] = [];
      while (merged.length > 0) {
        const last = merged[merged.length - 1];
        if (!last || last.messageKey || last.kind !== "action" || last.toolCalls.length > 0) break;
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
        merged.push({
          kind: "action",
          events: [...thinkingEvents, ...g.events],
          toolCalls: g.toolCalls,
        });
        continue;
      }
    } else if (g.kind === "action" && g.toolCalls.length === 0) {
      // Thinking-only group immediately following a tool-call group: absorb into it.
      const last = merged[merged.length - 1];
      if (last && !last.messageKey && last.kind === "action" && last.toolCalls.length > 0) {
        last.events.push(...g.events);
        continue;
      }
    } else if (g.kind === "text") {
      // Text group with embedded thinking (from initial-pass absorption) following
      // a tool-call group: move thinking events into the tool group so they render
      // inside the collapsed tool block, not as a standalone ThinkingCard.
      const last = merged[merged.length - 1];
      if (last && !last.messageKey && last.kind === "action" && last.toolCalls.length > 0) {
        const thinkingEvs = g.events.filter((e) => e.kind === "thinking" || e.kind === "thinkingDelta");
        if (thinkingEvs.length > 0) {
          last.events.push(...thinkingEvs);
          g.events = g.events.filter((e) => e.kind !== "thinking" && e.kind !== "thinkingDelta");
        }
      }
    }
    merged.push(g);
  }

  // Coalesce thinking within a run. A run is a stretch of groups containing
  // no boundary (other, ask, userInput) and no real model text. When a run
  // holds thinking in more than one group — e.g. a long tool loop where each
  // turn emits thinking plus a blank text line — the thinking events
  // consolidate into one thinking-only group at the run's first thinking
  // position, so the UI shows one collapsed card instead of one per turn.
  // Tool calls and whitespace-only text do not break a run; a group with
  // real text ends the run after its leading thinking. Whitespace-only text
  // groups are dropped.
  const isThinkingEv = (e: EventMessage) => e.kind === "thinking" || e.kind === "thinkingDelta";
  const renderedText = (g: MessageGroup) => {
    if (g.content) return g.content.text;
    const final = g.events.findLast((e) => e.kind === "text");
    if (final) return final.text?.text ?? "";
    return g.events
      .filter((e) => e.kind === "textDelta")
      .map((e) => e.textDelta?.text ?? "")
      .join("");
  };
  const isBlankTextEv = (e: EventMessage) =>
    (e.kind === "text" && (e.text?.text ?? "").trim() === "") ||
    (e.kind === "textDelta" && (e.textDelta?.text ?? "").trim() === "");
  const droppableEmptyGroup = (g: MessageGroup, events: EventMessage[]) =>
    (g.kind === "text" || (g.kind === "action" && g.toolCalls.length === 0)) &&
    events.length > 0 &&
    events.every((e) => e.kind === "usage" || isBlankTextEv(e));

  const finalGroups: MessageGroup[] = [];
  let holder: MessageGroup | undefined;
  let pendingIdx = -1;
  const resetRun = () => {
    holder = undefined;
    pendingIdx = -1;
  };

  for (const g of merged) {
    if (g.messageKey) {
      resetRun();
      finalGroups.push(g);
      continue;
    }
    if (g.kind !== "text" && g.kind !== "action") {
      resetRun();
      finalGroups.push(g);
      continue;
    }
    const think = g.events.filter(isThinkingEv);
    if (think.length === 0) {
      if (droppableEmptyGroup(g, g.events)) continue;
      if (g.kind === "text" && renderedText(g).trim() !== "") resetRun();
      finalGroups.push(g);
      continue;
    }
    const hasRealText = g.kind === "text" && renderedText(g).trim() !== "";
    const rest = g.events.filter((e) => !isThinkingEv(e));
    const dropRest = droppableEmptyGroup(g, rest);
    const timingSegment = dropRest ? g.events : think;
    const pushRest = () => {
      if (rest.length > 0 && !dropRest) finalGroups.push({ ...g, events: rest });
    };
    if (holder) {
      holder.events.push(...think);
      holder.timingSegments?.push(timingSegment);
      pushRest();
    } else if (pendingIdx >= 0) {
      // Second thinking group in the run: promote the first group's thinking
      // into a new holder at the first position; the first group's remainder
      // (e.g. its tool calls) stays right after the holder.
      const firstIdx = pendingIdx;
      const first = finalGroups[firstIdx];
      if (!first) throw new Error("Missing pending thinking group");
      const fRest = first.events.filter((e) => !isThinkingEv(e));
      const dropFirstRest = droppableEmptyGroup(first, fRest);
      const h: MessageGroup = {
        kind: "action",
        events: [...first.events.filter(isThinkingEv), ...think],
        toolCalls: [],
        timingSegments: [dropFirstRest ? first.events : first.events.filter(isThinkingEv), timingSegment],
      };
      finalGroups[firstIdx] = h;
      holder = h;
      pendingIdx = -1;
      if (fRest.length > 0 && !dropFirstRest) {
        finalGroups.splice(firstIdx + 1, 0, { ...first, events: fRest });
      }
      pushRest();
    } else {
      // First thinking group of the run: keep in place until a second
      // thinking group arrives, so single-thinking runs render unchanged.
      pendingIdx = finalGroups.length;
      finalGroups.push(g);
    }
    if (hasRealText) resetRun();
  }

  // Claude Code emits the final assistant response twice: once as assistant
  // text and again as the successful result payload. Keep the result group as
  // the canonical completion card, but retain preceding text when it differs.
  for (let i = 1; i < finalGroups.length; i++) {
    const group = finalGroups[i];
    if (!group) throw new Error("Missing result group");
    const result = group.events.find((event) => event.kind === "result")?.result;
    const previous = finalGroups[i - 1];
    if (
      result &&
      !result.isError &&
      result.result.trim() !== "" &&
      previous?.kind === "text" &&
      renderedText(previous).trim() === result.result.trim()
    ) {
      if (previous.messageKey) group.resultDuplicatesText = true;
      else {
        group.resultReplacesText = true;
        finalGroups.splice(i - 1, 1);
        i--;
      }
    }
  }

  // Mark tool calls as implicitly done when later events prove completion.
  // Within a group, every non-last non-background call is done (the agent
  // moved on to the next call). Later root-owned groups finish legacy root calls;
  // unresolved child activity does not provide completion evidence for them.
  const lastRootGroupIdx = finalGroups.findLastIndex((group) => group.events.some((event) => !isChildEvent(event)));
  for (const [i, g] of finalGroups.entries()) {
    if (g.tool || g.kind !== "action" || g.toolCalls.length === 0) continue;
    if (i < lastRootGroupIdx) {
      for (const tc of g.toolCalls) tc.done = true;
    } else {
      // Last action group: mark all but the final non-background call as done.
      for (let j = 0; j < g.toolCalls.length - 1; j++) {
        const call = g.toolCalls[j];
        if (call && !call.use.background) call.done = true;
      }
    }
  }
  return finalGroups;
}

// Incremental wrapper around groupMessages. Each instance owns one stream's
// cache, so concurrently mounted task views cannot affect each other.
// Pure streaming deltas extend the cached groups; structural events fall back
// to a full regroup. Updated groups use copy-on-write so Solid can observe each
// streaming batch through reference changes.
export class IncrementalMessageGrouper {
  private previousLength = 0;
  private previousWarningPercentages = new Map<string, number>();
  private result: MessageGroup[] | null = null;
  private byIdentity = new Map<string, number>();
  private byTool = new Map<string, number>();
  private ended = false;
  private pendingLegacyGroup = -1;

  finish(): MessageGroup[] {
    if (this.ended) return this.result ?? [];
    this.ended = true;
    this.result = (this.result ?? []).map((group) =>
      group.tool
        ? {
            ...group,
            tool: { ...group.tool, ended: true },
            toolCalls: group.toolCalls.map((call) => (call.use.background ? call : { ...call, done: true })),
          }
        : group,
    );
    return this.result;
  }
  private firstEvent: EventMessage | null = null;
  private lastEvent: EventMessage | null = null;

  reset(): void {
    this.previousLength = 0;
    this.previousWarningPercentages.clear();
    this.result = null;
    this.byIdentity.clear();
    this.byTool.clear();
    this.pendingLegacyGroup = -1;
    this.ended = false;
    this.firstEvent = null;
    this.lastEvent = null;
  }

  resetAfter(msgs: ReadonlyArray<EventMessage>): void {
    this.reset();
    this.previousWarningPercentages = rateLimitWarningPercentagesAfter(msgs);
  }

  group(msgs: EventMessage[]): MessageGroup[] {
    const previous = this.result;
    if (
      previous &&
      this.previousLength <= msgs.length &&
      (this.previousLength === 0 || (msgs[0] === this.firstEvent && msgs[this.previousLength - 1] === this.lastEvent))
    ) {
      const newEvents = msgs.slice(this.previousLength);
      if (newEvents.length === 0) return previous;
      const canExtend =
        newEvents.length > 0 &&
        newEvents.every(
          (ev) =>
            ev.kind === "thinkingDelta" ||
            ev.kind === "textDelta" ||
            ev.kind === "toolOutputDelta" ||
            ev.kind === "widgetDelta" ||
            (ev.source !== undefined && toolEventID(ev) !== null) ||
            messageIdentity(ev) !== null,
        );
      if (canExtend && previous.length > 0) {
        const result = [...previous];
        const mutableGroup = (index: number): MessageGroup => {
          const group = result[index];
          if (!group) throw new Error("Missing mutable message group");
          if (group !== previous[index]) return group;
          const copy = { ...group, events: group.content || group.tool ? group.events : [...group.events] };
          result[index] = copy;
          return copy;
        };
        const appendGroup = (group: MessageGroup): void => {
          if (this.pendingLegacyGroup >= 0 && group.events.some((event) => !isChildEvent(event))) {
            const mutable = mutableGroup(this.pendingLegacyGroup);
            mutable.toolCalls = mutable.toolCalls.map((call) => (call.done ? call : { ...call, done: true }));
            this.pendingLegacyGroup = -1;
          }
          result.push(group);
        };

        for (const ev of newEvents) {
          const toolID = toolEventID(ev);
          if (ev.kind === "toolResult" && toolID) {
            const index = result.findIndex(
              (group) =>
                group.kind === "widget" &&
                group.widgetToolUseID === toolID &&
                toolIdentity(group.events[0]?.source, toolID) === toolIdentity(ev.source, toolID),
            );
            if (index >= 0) {
              const widget = mutableGroup(index);
              widget.widgetDone = true;
              widget.events.push(ev);
              continue;
            }
          }
          if (toolID !== null && (ev.source || this.byTool.has(toolIdentity(ev.source, toolID)))) {
            const key = toolIdentity(ev.source, toolID);
            const index = this.byTool.get(key);
            if (index !== undefined) appendScopedTool(mutableGroup(index), ev, key, this.ended);
            else {
              const group: MessageGroup = { kind: "action", messageKey: `tool:${key}`, events: [], toolCalls: [] };
              appendScopedTool(group, ev, key, this.ended);
              this.byTool.set(key, result.length);
              appendGroup(group);
            }
            continue;
          }
          const messageKey = messageIdentity(ev);
          if (messageKey !== null) {
            const index = this.byIdentity.get(messageKey);
            if (index !== undefined) appendIdentifiedMessage(mutableGroup(index), ev);
            else {
              this.byIdentity.set(messageKey, result.length);
              const group: MessageGroup = {
                kind: ev.kind === "thinking" || ev.kind === "thinkingDelta" ? "action" : "text",
                messageKey,
                events: [],
                toolCalls: [],
              };
              appendIdentifiedMessage(group, ev);
              appendGroup(group);
            }
            continue;
          }
          const lastGroupIndex = result.length - 1;
          const lastGroup = result[lastGroupIndex];
          if (!lastGroup) throw new Error("Missing streaming message group");
          if (ev.kind === "thinkingDelta") {
            if (!lastGroup.messageKey && lastGroup.kind === "action") {
              mutableGroup(lastGroupIndex).events.push(ev);
            } else {
              appendGroup({ kind: "action", events: [ev], toolCalls: [] });
            }
          } else if (ev.kind === "textDelta") {
            if (!lastGroup.messageKey && lastGroup.kind === "text") {
              mutableGroup(lastGroupIndex).events.push(ev);
            } else if (!lastGroup.messageKey && lastGroup.kind === "action" && lastGroup.toolCalls.length === 0) {
              const group = mutableGroup(lastGroupIndex);
              group.kind = "text";
              group.events.push(ev);
            } else {
              appendGroup({ kind: "text", events: [ev], toolCalls: [] });
            }
          } else if (ev.kind === "toolOutputDelta") {
            const id = ev.toolOutputDelta?.toolUseID;
            if (id) {
              let found = false;
              for (let i = result.length - 1; i >= 0; i--) {
                const group = result[i];
                if (!group) throw new Error("Missing message group");
                if (!isLegacyToolGroup(group)) continue;
                if (group.toolCalls.some((call) => matchesToolCall(call, ev.source, id))) {
                  const mutable = mutableGroup(i);
                  mutable.toolCalls = mutable.toolCalls.map((call) =>
                    matchesToolCall(call, ev.source, id)
                      ? { ...call, output: (call.output ?? "") + (ev.toolOutputDelta?.delta ?? "") }
                      : call,
                  );
                  found = true;
                  break;
                }
              }
              if (!found) return this.regroup(msgs);
            }
          } else if (ev.kind === "widgetDelta") {
            const id = ev.widgetDelta?.toolUseID;
            if (id) {
              let found = false;
              for (let i = result.length - 1; i >= 0; i--) {
                const group = result[i];
                if (!group) throw new Error("Missing message group");
                if (
                  group.kind === "widget" &&
                  group.widgetToolUseID === id &&
                  toolIdentity(group.events[0]?.source, id) === toolIdentity(ev.source, id)
                ) {
                  const mutable = mutableGroup(i);
                  mutable.events.push(ev);
                  mutable.widgetHTML = (mutable.widgetHTML ?? "") + (ev.widgetDelta?.delta ?? "");
                  found = true;
                  break;
                }
              }
              if (!found) {
                appendGroup({
                  kind: "widget",
                  events: [ev],
                  toolCalls: [],
                  widgetToolUseID: id,
                  widgetHTML: ev.widgetDelta?.delta ?? "",
                  widgetDone: false,
                });
              }
            }
          }
        }
        this.result = result;
        this.previousLength = msgs.length;
        this.firstEvent = msgs[0] ?? null;
        this.lastEvent = msgs.at(-1) ?? null;
        return result;
      }
    }
    return this.regroup(msgs);
  }

  private regroup(msgs: EventMessage[]): MessageGroup[] {
    const ended = this.ended;
    this.ended = false;
    this.result = groupMessagesAfter(msgs, this.previousWarningPercentages);
    this.previousLength = msgs.length;
    this.firstEvent = msgs[0] ?? null;
    this.lastEvent = msgs.at(-1) ?? null;
    this.byIdentity.clear();
    this.byTool.clear();
    this.pendingLegacyGroup = -1;
    this.result.forEach((group, index) => {
      if (group.tool) this.byTool.set(group.tool.key, index);
      else if (group.messageKey) this.byIdentity.set(group.messageKey, index);
      if (!group.tool && group.kind === "action" && group.toolCalls.some((call) => !call.done)) {
        this.pendingLegacyGroup = index;
      }
    });
    return ended ? this.finish() : this.result;
  }
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
  let resultPayload: EventResult | undefined;

  function flush() {
    if (current.length > 0) {
      const durationMs = resultDurationMs ?? Math.max(0, lastTs - firstTs);
      turns.push({ groups: current, toolCount, textCount, durationMs, result: resultPayload });
      current = [];
      toolCount = 0;
      textCount = 0;
      firstTs = 0;
      lastTs = 0;
      hasTs = false;
      resultDurationMs = undefined;
      resultPayload = undefined;
    }
  }

  for (const g of groups) {
    current.push(g);
    const range = g.content?.range ?? g.tool?.range ?? g.range;
    if (range) {
      firstTs = hasTs ? Math.min(firstTs, range.start) : range.start;
      lastTs = Math.max(lastTs, range.end);
      hasTs = true;
    }
    if (g.kind === "action") {
      toolCount += g.toolCalls.length;
    } else if (g.kind === "text") {
      textCount++;
    } else if (g.resultReplacesText) {
      textCount++;
    }
    for (const ev of g.events) {
      if (ev.ts > 0) {
        if (!hasTs) {
          firstTs = ev.ts;
          lastTs = ev.ts;
          hasTs = true;
        } else {
          firstTs = Math.min(firstTs, ev.ts);
          lastTs = Math.max(lastTs, ev.ts);
        }
      }
      if (ev.kind === "result" && ev.result !== undefined && !isChildEvent(ev)) {
        resultPayload = ev.result;
        const durationMs = Math.round((ev.result.duration ?? 0) * 1000);
        if (durationMs > 0) {
          resultDurationMs = durationMs;
        }
      }
    }
    if (g.kind === "other" && g.events.some((ev) => ev.kind === "result" && !isChildEvent(ev))) {
      flush();
    }
  }
  flush();
  return turns;
}

const groupSessionsCache = new WeakMap<ReadonlyArray<EventMessage>, Session[]>();

// Splits messages into sessions at init (only when sessionID changes) and compaction/clear events.
// The boundary event starts the new session and is NOT passed to groupMessages.
// Re-invocations of Claude Code that share the same sessionID are NOT new sessions — they are
// just new turns within the same session.
export function groupSessions(msgs: EventMessage[]): Session[] {
  const cached = groupSessionsCache.get(msgs);
  if (cached !== undefined) return cached;
  const sessions: Session[] = [];
  let segment: EventMessage[] = [];
  let boundaryEvent: EventMessage | undefined;
  let currentSessionID: string | undefined;

  function flushSession() {
    const groups = aggregateToolGroups(groupMessages(segment));
    const turns = groupTurns(groups);
    let toolCount = 0;
    let textCount = 0;
    let durationMs = 0;
    for (const t of turns) {
      toolCount += t.toolCount;
      textCount += t.textCount;
      durationMs += t.durationMs;
    }
    sessions.push({ boundaryEvent, turns, toolCount, textCount, durationMs });
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
    if (ev.kind === "init" && !isChildEvent(ev)) {
      const newID = ev.init?.sessionID;
      if (newID !== currentSessionID) {
        // Restart init supplies the identity of the context just opened by a
        // clear or compaction. Its prompt may already precede this init.
        const openContext = boundaryEvent?.kind === "system" && !segment.some((event) => event.kind === "result");
        if (!openContext) {
          if (boundaryEvent !== undefined) flushAndCarry();
          boundaryEvent = ev;
        }
        currentSessionID = newID;
      }
      // Same sessionID: re-invocation within the same session; skip the init event entirely.
    } else if (
      ev.kind === "system" &&
      (ev.system?.subtype === "compact_boundary" || ev.system?.subtype === "context_cleared")
    ) {
      if (boundaryEvent !== undefined) flushAndCarry();
      boundaryEvent = ev;
      currentSessionID = undefined; // The restarted init supplies this context's identity.
    } else {
      segment.push(ev);
    }
  }
  if (segment.length > 0 || boundaryEvent !== undefined) {
    flushSession();
  }
  groupSessionsCache.set(msgs, sessions);
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
export function buildTurnItems(
  turns: Turn[],
  expandedTurnKeys: ReadonlySet<string>,
  sessionKey: string,
  inPastSession = false,
): MsgItem[] {
  const items: MsgItem[] = [];
  for (const [i, turn] of turns.entries()) {
    const turnKey = `${sessionKey}:turn:${i}:${turn.groups[0]?.events[0]?.ts ?? ""}`;
    if (expandedTurnKeys.has(turnKey)) {
      items.push({
        kind: "expandedHeader",
        turn,
        turnKey,
        key: `hdr:${turnKey}`,
        ...(inPastSession ? { indent: "session" as const } : {}),
      });
      for (const [j, group] of turn.groups.entries()) {
        items.push({
          kind: "group",
          group,
          isLive: false,
          key: group.messageKey ? `message:${group.messageKey}` : `${turnKey}-g${j}`,
          indent: "turn",
        });
      }
    } else {
      items.push({
        kind: "elided",
        turn,
        key: turnKey,
        ...(inPastSession ? { indent: "session" as const } : {}),
      });
    }
  }
  return items;
}

// Builds flat items for past (non-current) sessions.
// Each past session is elided to a single row by default; click to expand.
export function buildPastSessionItems(
  sessions: Session[],
  expandedSessionKeys: ReadonlySet<string>,
  expandedTurnKeys: ReadonlySet<string>,
): MsgItem[] {
  const items: MsgItem[] = [];
  for (const [i, session] of sessions.entries()) {
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

export function toolCallDurations(events: readonly EventMessage[]): ReadonlyMap<string, number> {
  const starts = new Map<string, number>();
  const durations = new Map<string, number>();
  for (const event of events) {
    const use = event.toolUse;
    if (event.kind === "toolUse" && use && event.ts > 0) {
      starts.set(use.toolUseID, event.ts);
      continue;
    }
    const result = event.toolResult;
    if (event.kind !== "toolResult" || !result) continue;
    const start = starts.get(result.toolUseID) ?? 0;
    const duration = toolResultDurationMs(result, start, event.ts);
    if (duration !== null) durations.set(result.toolUseID, duration);
  }
  return durations;
}

export function toolResultDurationMs(result: EventToolResult, startTs: number, resultTs: number): number | null {
  if (result.duration > 0) return result.duration * 1000;
  return startTs > 0 && resultTs > startTs ? resultTs - startTs : null;
}

export function toolCallDurationMs(call: ToolCall, durations: ReadonlyMap<string, number>): number | null {
  if (call.durationMs !== undefined) return call.durationMs;
  if (call.result) {
    const explicit = toolResultDurationMs(call.result, 0, 0);
    if (explicit !== null) return explicit;
  }
  return durations.get(call.use.toolUseID) ?? null;
}

export function turnSummary(turn: Turn): string {
  const parts: string[] = [];
  if (turn.textCount > 0) {
    parts.push(turn.textCount === 1 ? "1 message" : `${turn.textCount} messages`);
  }
  if (turn.toolCount > 0) {
    parts.push(turn.toolCount === 1 ? "1 tool call" : `${turn.toolCount} tool calls`);
  }
  return parts.length > 0 ? parts.join(", ") : "empty turn";
}

export function sessionSummary(session: Session): string {
  const parts: string[] = [];
  if (session.boundaryEvent?.kind === "init" && session.boundaryEvent.init) {
    const id = session.boundaryEvent.init.sessionID;
    parts.push(`Session ${id.slice(0, 8)}`);
  } else if (session.boundaryEvent?.system?.subtype === "context_cleared") {
    parts.push("Cleared session");
  } else {
    parts.push("Compacted session");
  }
  parts.push(session.turns.length === 1 ? "1 turn" : `${session.turns.length} turns`);
  if (session.textCount > 0) {
    parts.push(session.textCount === 1 ? "1 message" : `${session.textCount} messages`);
  }
  if (session.toolCount > 0) {
    parts.push(session.toolCount === 1 ? "1 tool call" : `${session.toolCount} tool calls`);
  }
  return parts.join(" \u00b7 ");
}

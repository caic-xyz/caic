// Pure grouping and turn-splitting logic for agent event streams.
import type { EventMessage, EventToolUse, EventToolResult, EventAsk } from "@sdk/types.gen";

export interface MessageGroup {
  kind: "text" | "action" | "ask" | "userInput" | "other";
  events: EventMessage[];
  // For "action" groups: paired tool_use and tool_result events (empty for thinking-only).
  toolCalls: ToolCall[];
  // For "ask" groups: the ask payload.
  ask?: EventAsk;
  // For "ask" groups: the user's submitted answer (from the following userInput).
  answerText?: string;
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
}

// Tool names (case-insensitive) that are async and emit explicit toolResult
// events. All other Claude Code tools complete synchronously and are done
// as soon as their toolUse event is emitted.
const ASYNC_TOOLS = new Set(["bash", "task"]);

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
          // Search all tool groups for the matching toolUseID — results may
          // arrive after intervening text/other groups, not just the last group.
          let matched = false;
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
        const prev = lastGroup();
        if (prev && prev.kind === "ask" && !prev.answerText) {
          prev.answerText = ev.userInput?.text;
          prev.events.push(ev);
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

  function flush() {
    if (current.length > 0) {
      turns.push({ groups: current, toolCount, textCount });
      current = [];
      toolCount = 0;
      textCount = 0;
    }
  }

  for (const g of groups) {
    current.push(g);
    if (g.kind === "action") {
      toolCount += g.toolCalls.length;
    } else if (g.kind === "text") {
      textCount++;
    }
    if (g.kind === "other" && g.events.some((ev) => ev.kind === "result")) {
      flush();
    }
  }
  flush();
  return turns;
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
  return parts.length > 0 ? parts.join(", ") : "empty turn";
}


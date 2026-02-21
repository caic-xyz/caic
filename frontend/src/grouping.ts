// Pure grouping and turn-splitting logic for Claude Code event streams.
import type { ClaudeEventMessage, ClaudeEventToolUse, ClaudeEventToolResult, ClaudeEventAsk } from "@sdk/types.gen";

export interface MessageGroup {
  kind: "text" | "tool" | "ask" | "userInput" | "other";
  events: ClaudeEventMessage[];
  // For "tool" groups: paired tool_use and tool_result events.
  toolCalls: ToolCall[];
  // For "ask" groups: the ask payload.
  ask?: ClaudeEventAsk;
  // For "ask" groups: the user's submitted answer (from the following userInput).
  answerText?: string;
}

// A tool_use event paired with its optional tool_result.
// done is true when the tool has completed — either via an explicit result
// event or implicitly because a later event arrived (the agent moved on).
export interface ToolCall {
  use: ClaudeEventToolUse;
  result?: ClaudeEventToolResult;
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

export function groupMessages(msgs: ClaudeEventMessage[]): MessageGroup[] {
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
        } else {
          groups.push({ kind: "text", events: [ev], toolCalls: [] });
        }
        break;
      }
      case "textDelta": {
        const last = lastGroup();
        if (last && last.kind === "text") {
          last.events.push(ev);
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
          if (last && last.kind === "tool" && !usageSinceLastTool) {
            // Consecutive toolUse in the same AssistantMessage — merge.
            last.events.push(ev);
            last.toolCalls.push(call);
          } else if (!usageSinceLastTool) {
            // Same AssistantMessage but intervening text; find the most
            // recent tool group to coalesce into.
            let coalesced = false;
            for (let i = groups.length - 1; i >= 0; i--) {
              if (groups[i].kind === "tool") {
                groups[i].events.push(ev);
                groups[i].toolCalls.push(call);
                coalesced = true;
                break;
              }
            }
            if (!coalesced) {
              groups.push({ kind: "tool", events: [ev], toolCalls: [call] });
            }
          } else {
            // New AssistantMessage — start a new tool group.
            groups.push({ kind: "tool", events: [ev], toolCalls: [call] });
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
            if (g.kind !== "tool") continue;
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
            groups.push({ kind: "tool", events: [ev], toolCalls: [] });
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
          if (last && (last.kind === "text" || last.kind === "tool")) {
            last.events.push(ev);
          } else {
            groups.push({ kind: "other", events: [ev], toolCalls: [] });
          }
        }
        break;
      case "todo":
        // Rendered by TodoPanel from messages() directly; skip here to avoid
        // splitting consecutive tool groups.
        break;
      case "diffStat":
        // Metadata-only; live diff stat shown in the task list via Task.diffStat.
        break;
      default:
        groups.push({ kind: "other", events: [ev], toolCalls: [] });
        break;
    }
  }

  // Merge tool groups separated only by text/usage groups.  The agent often
  // emits short commentary between tool turns ("Let me read...", "Now let me
  // edit...").  Without merging, each turn shows as a separate 1-tool block.
  // ask, userInput, and other groups act as hard boundaries that prevent
  // merging.  Text groups between tool groups are kept for display; tool
  // calls are consolidated into the first tool group of each run.
  const merged: MessageGroup[] = [];
  for (const g of groups) {
    if (g.kind === "tool") {
      // Find the nearest non-text group in merged to check for a tool anchor.
      let anchor: MessageGroup | undefined;
      for (let i = merged.length - 1; i >= 0; i--) {
        if (merged[i].kind !== "text") {
          anchor = merged[i];
          break;
        }
      }
      if (anchor && anchor.kind === "tool") {
        // Merge tool calls into the earlier tool group.
        anchor.events.push(...g.events);
        anchor.toolCalls.push(...g.toolCalls);
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
  const lastToolGroupIdx = merged.findLastIndex((g) => g.kind === "tool");
  for (let i = 0; i < merged.length; i++) {
    const g = merged[i];
    if (g.kind !== "tool") continue;
    if (i < lastToolGroupIdx || i < merged.length - 1) {
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
    if (g.kind === "tool") {
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

export function turnHasExitPlanMode(turn: Turn): boolean {
  return turn.groups.some((g) =>
    g.kind === "tool" && g.toolCalls.some((tc) => tc.use.name === "ExitPlanMode"),
  );
}

// Extracts the plan file content from the Write tool call that wrote to
// .claude/plans/ in this turn. Returns undefined if not found.
export function turnPlanContent(turn: Turn): string | undefined {
  for (const g of turn.groups) {
    if (g.kind !== "tool") continue;
    for (const tc of g.toolCalls) {
      if (tc.use.name !== "Write") continue;
      const input = tc.use.input as Record<string, unknown> | undefined;
      if (!input) continue;
      const fp = input.file_path ?? input.filePath;
      if (typeof fp === "string" && fp.includes(".claude/plans/") && typeof input.content === "string") {
        return input.content;
      }
    }
  }
  return undefined;
}

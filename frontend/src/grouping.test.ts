// Tests for groupMessages and groupTurns logic.
import { describe, it, expect } from "vitest";
import { groupMessages, groupTurns, groupSessions, turnSummary } from "./grouping";
import type { EventMessage } from "@sdk/types.gen";

function toolUseEvent(id: string, name: string): EventMessage {
  return { kind: "toolUse", ts: 0, toolUse: { toolUseID: id, name, input: {} } };
}

function toolResultEvent(id: string): EventMessage {
  return { kind: "toolResult", ts: 0, toolResult: { toolUseID: id, duration: 0.1 } };
}

function textDeltaEvent(text: string): EventMessage {
  return { kind: "textDelta", ts: 0, textDelta: { text } };
}

function usageEvent(): EventMessage {
  return {
    kind: "usage", ts: 0,
    usage: { inputTokens: 100, outputTokens: 50, cacheCreationInputTokens: 0, cacheReadInputTokens: 0, model: "test" },
  };
}

function resultEvent(): EventMessage {
  return {
    kind: "result", ts: 0,
    result: {
      subtype: "success", isError: false, result: "done",
      totalCostUSD: 0.01, duration: 1.0, durationAPI: 0.9,
      numTurns: 1,
      usage: { inputTokens: 100, outputTokens: 50, cacheCreationInputTokens: 0, cacheReadInputTokens: 0, model: "test" },
    },
  };
}

describe("groupMessages", () => {
  it("consecutive tool uses form one group", () => {
    const groups = groupMessages([toolUseEvent("t1", "Read"), toolUseEvent("t2", "Bash")]);
    expect(groups).toHaveLength(1);
    expect(groups[0].kind).toBe("action");
    expect(groups[0].toolCalls).toHaveLength(2);
  });

  it("synchronous tools in last group are done immediately", () => {
    // Bash is async (emits toolResult); Read is synchronous (no toolResult).
    // Even before Bash's result arrives, Read should show as done.
    const groups = groupMessages([toolUseEvent("t1", "Bash"), toolUseEvent("t2", "Read")]);
    expect(groups).toHaveLength(1);
    expect(groups[0].toolCalls[0].done).toBe(false); // Bash: async, pending
    expect(groups[0].toolCalls[1].done).toBe(true);  // Read: sync, already done
  });

  it("async tool is marked done when its toolResult arrives", () => {
    const groups = groupMessages([
      toolUseEvent("t1", "Bash"),
      toolResultEvent("t1"),
    ]);
    expect(groups[0].toolCalls[0].done).toBe(true);
    expect(groups[0].toolCalls[0].result?.toolUseID).toBe("t1");
  });

  it("toolResult matches backwards across groups", () => {
    const groups = groupMessages([
      toolUseEvent("t1", "Bash"),
      textDeltaEvent("text"),
      toolResultEvent("t1"),
    ]);
    expect(groups).toHaveLength(2);
    expect(groups[0].kind).toBe("action");
    expect(groups[0].toolCalls[0].done).toBe(true);
    expect(groups[0].toolCalls[0].result?.toolUseID).toBe("t1");
  });

  it("non-last tool groups are implicitly marked done", () => {
    const groups = groupMessages([
      toolUseEvent("t1", "Read"),
      usageEvent(),
      textDeltaEvent("text"),
      toolUseEvent("t2", "Bash"),
    ]);
    // After merge pass: [TOOL(Read, Bash), TEXT]
    expect(groups).toHaveLength(2);
    expect(groups[0].toolCalls[0].done).toBe(true); // Read
    expect(groups[0].toolCalls[1].done).toBe(true); // Bash (implicit from non-last-group pass)
  });

  it("tool groups separated by text merge into one", () => {
    const groups = groupMessages([
      toolUseEvent("t1", "Read"),
      usageEvent(),
      textDeltaEvent("commentary"),
      toolUseEvent("t2", "Bash"),
      usageEvent(),
      textDeltaEvent("more"),
      toolUseEvent("t3", "Edit"),
    ]);
    expect(groups).toHaveLength(3); // [TOOL(t1+t2+t3), TEXT, TEXT]
    expect(groups[0].kind).toBe("action");
    expect(groups[0].toolCalls).toHaveLength(3);
  });

  it("thinking events are absorbed into an adjacent tool group", () => {
    // Realistic pattern: usage ends the first assistant message, then thinking
    // precedes the next tool call in a new assistant message.
    const groups = groupMessages([
      toolUseEvent("t1", "Read"),
      usageEvent(),
      { kind: "thinking", ts: 0, thinking: { text: "hmm" } },
      { kind: "subagentStart", ts: 0, subagentStart: { taskID: "sa1", description: "explore" } },
      toolUseEvent("t2", "Bash"),
      { kind: "subagentEnd", ts: 0, subagentEnd: { taskID: "sa1", status: "completed" } },
    ]);
    // Thinking is absorbed into the merged tool group; no standalone thinking group.
    const toolGroup = groups.find((g) => g.kind === "action");
    expect(toolGroup?.toolCalls).toHaveLength(2);
    expect(toolGroup?.events.some((e) => e.kind === "thinking")).toBe(true);
    // Subagent events don't create groups.
    expect(groups.some((g) => g.kind === "other")).toBe(false);
  });

  it("thinking followed by usage does not create a barrier before tool use", () => {
    // usage after a thinking-only group must not create an OTHER barrier that
    // prevents the merge pass from absorbing thinking into the tool group.
    const groups = groupMessages([
      { kind: "thinkingDelta", ts: 0, thinkingDelta: { text: "thinking..." } },
      usageEvent(),
      toolUseEvent("t1", "Read"),
    ]);
    expect(groups.some((g) => g.kind === "other")).toBe(false);
    expect(groups).toHaveLength(1);
    const toolGroup = groups[0];
    expect(toolGroup.kind).toBe("action");
    expect(toolGroup.toolCalls).toHaveLength(1);
    expect(toolGroup.events.some((e) => e.kind === "thinkingDelta")).toBe(true);
  });

  it("thinking immediately after a tool group is absorbed into it", () => {
    // The agent may start a new thinking block right after tool calls complete,
    // before any text commentary. It should merge into the preceding tool group.
    const groups = groupMessages([
      toolUseEvent("t1", "Read"),
      usageEvent(),
      { kind: "thinkingDelta", ts: 0, thinkingDelta: { text: "analyzing..." } },
    ]);
    expect(groups).toHaveLength(1);
    expect(groups[0].kind).toBe("action");
    expect(groups[0].toolCalls).toHaveLength(1);
    expect(groups[0].events.some((e) => e.kind === "thinkingDelta")).toBe(true);
  });

  it("thinking followed by text is absorbed into the text group", () => {
    // Standalone thinking before text commentary (no tools) must not produce a
    // separate Thinking block; it should be inside the text group instead.
    const groups = groupMessages([
      { kind: "thinkingDelta", ts: 0, thinkingDelta: { text: "thinking..." } },
      textDeltaEvent("hello"),
    ]);
    expect(groups).toHaveLength(1);
    expect(groups[0].kind).toBe("text");
    expect(groups[0].events.some((e) => e.kind === "thinkingDelta")).toBe(true);
    expect(groups[0].events.some((e) => e.kind === "textDelta")).toBe(true);
  });
});

describe("groupSessions", () => {
  it("splits on init events", () => {
    const msgs: EventMessage[] = [
      { kind: "init", ts: 1, init: { model: "m", agentVersion: "1", sessionID: "s1", tools: [], cwd: "/", harness: "claude" } },
      textDeltaEvent("session 1"),
      resultEvent(),
      { kind: "init", ts: 2, init: { model: "m", agentVersion: "1", sessionID: "s2", tools: [], cwd: "/", harness: "claude" } },
      textDeltaEvent("session 2"),
    ];
    const sessions = groupSessions(msgs);
    expect(sessions).toHaveLength(2);
    expect(sessions[0].boundaryEvent?.kind).toBe("init");
    expect(sessions[0].turns).toHaveLength(1);
    expect(sessions[1].boundaryEvent?.kind).toBe("init");
    expect(sessions[1].turns).toHaveLength(1);
  });

  it("splits on compact_boundary system events", () => {
    const msgs: EventMessage[] = [
      { kind: "init", ts: 1, init: { model: "m", agentVersion: "1", sessionID: "s1", tools: [], cwd: "/", harness: "claude" } },
      textDeltaEvent("before compact"),
      resultEvent(),
      { kind: "system", ts: 2, system: { subtype: "compact_boundary" } },
      textDeltaEvent("after compact"),
    ];
    const sessions = groupSessions(msgs);
    expect(sessions).toHaveLength(2);
    expect(sessions[0].boundaryEvent?.kind).toBe("init");
    expect(sessions[1].boundaryEvent?.system?.subtype).toBe("compact_boundary");
  });

  it("no boundary events produces one session with no boundaryEvent", () => {
    const msgs: EventMessage[] = [
      textDeltaEvent("hello"),
      resultEvent(),
      textDeltaEvent("world"),
    ];
    const sessions = groupSessions(msgs);
    expect(sessions).toHaveLength(1);
    expect(sessions[0].boundaryEvent).toBeUndefined();
    expect(sessions[0].turns).toHaveLength(2);
  });

  it("pre-init prompt is absorbed into the first session, not a separate implicit session", () => {
    // The user's initial prompt arrives as a userInput event before the init event.
    // It must appear in the same session as the init, not as a phantom "Compacted session".
    const msgs: EventMessage[] = [
      { kind: "userInput", ts: 0, userInput: { text: "hello" } },
      { kind: "init", ts: 1, init: { model: "m", agentVersion: "1", sessionID: "s1", tools: [], cwd: "/", harness: "claude" } },
      textDeltaEvent("response"),
    ];
    const sessions = groupSessions(msgs);
    expect(sessions).toHaveLength(1);
    expect(sessions[0].boundaryEvent?.kind).toBe("init");
    // The userInput event must be in this session's segment
    const allEvents = sessions[0].turns.flatMap((t) => t.groups.flatMap((g) => g.events));
    expect(allEvents.some((e) => e.kind === "userInput")).toBe(true);
  });

  it("userInput between sessions is carried into the next session, not left in the previous", () => {
    // After a session result, the user types a message, then a new init arrives.
    // The userInput should appear in session 2 (the one it triggered), not session 1.
    const msgs: EventMessage[] = [
      { kind: "init", ts: 1, init: { model: "m", agentVersion: "1", sessionID: "s1", tools: [], cwd: "/", harness: "claude" } },
      textDeltaEvent("response"),
      resultEvent(),
      { kind: "userInput", ts: 2, userInput: { text: "follow-up" } },
      { kind: "init", ts: 3, init: { model: "m", agentVersion: "1", sessionID: "s2", tools: [], cwd: "/", harness: "claude" } },
      textDeltaEvent("session 2 response"),
    ];
    const sessions = groupSessions(msgs);
    expect(sessions).toHaveLength(2);
    const s1Events = sessions[0].turns.flatMap((t) => t.groups.flatMap((g) => g.events));
    const s2Events = sessions[1].turns.flatMap((t) => t.groups.flatMap((g) => g.events));
    expect(s1Events.some((e) => e.kind === "userInput")).toBe(false);
    expect(s2Events.some((e) => e.kind === "userInput")).toBe(true);
  });

  it("turn starting with userInput is not empty when the agent response follows in the same session", () => {
    // After carrying the userInput into the next session, the resulting turn should have
    // textCount > 0 (agent replied), so turnSummary does not return "empty turn".
    const msgs: EventMessage[] = [
      { kind: "init", ts: 1, init: { model: "m", agentVersion: "1", sessionID: "s1", tools: [], cwd: "/", harness: "claude" } },
      textDeltaEvent("first response"),
      resultEvent(),
      { kind: "userInput", ts: 2, userInput: { text: "follow-up" } },
      { kind: "init", ts: 3, init: { model: "m", agentVersion: "1", sessionID: "s2", tools: [], cwd: "/", harness: "claude" } },
      textDeltaEvent("second response"),
      resultEvent(),
    ];
    const sessions = groupSessions(msgs);
    expect(sessions).toHaveLength(2);
    const lastTurn = sessions[1].turns[sessions[1].turns.length - 1];
    expect(lastTurn.textCount).toBeGreaterThan(0);
  });

  it("userInput before compact_boundary is carried into the compacted session", () => {
    const msgs: EventMessage[] = [
      { kind: "init", ts: 1, init: { model: "m", agentVersion: "1", sessionID: "s1", tools: [], cwd: "/", harness: "claude" } },
      textDeltaEvent("first response"),
      resultEvent(),
      { kind: "userInput", ts: 2, userInput: { text: "continue" } },
      { kind: "system", ts: 3, system: { subtype: "compact_boundary" } },
      textDeltaEvent("compacted response"),
    ];
    const sessions = groupSessions(msgs);
    expect(sessions).toHaveLength(2);
    const s2Events = sessions[1].turns.flatMap((t) => t.groups.flatMap((g) => g.events));
    expect(s2Events.some((e) => e.kind === "userInput")).toBe(true);
  });

  it("repeated init with the same sessionID does not create a new session", () => {
    // Claude Code re-invocations within the same conversation reuse the same sessionID.
    // Only a different sessionID or compact_boundary should create a new top-level group.
    const msgs: EventMessage[] = [
      { kind: "init", ts: 1, init: { model: "m", agentVersion: "1", sessionID: "s1", tools: [], cwd: "/", harness: "claude" } },
      textDeltaEvent("first response"),
      resultEvent(),
      { kind: "userInput", ts: 2, userInput: { text: "follow-up" } },
      { kind: "init", ts: 3, init: { model: "m", agentVersion: "1", sessionID: "s1", tools: [], cwd: "/", harness: "claude" } },
      textDeltaEvent("second response"),
      resultEvent(),
    ];
    const sessions = groupSessions(msgs);
    expect(sessions).toHaveLength(1);
    expect(sessions[0].turns).toHaveLength(2);
  });

  it("boundary event alone produces a session with empty turns", () => {
    const msgs: EventMessage[] = [
      { kind: "init", ts: 1, init: { model: "m", agentVersion: "1", sessionID: "s1", tools: [], cwd: "/", harness: "claude" } },
    ];
    const sessions = groupSessions(msgs);
    expect(sessions).toHaveLength(1);
    expect(sessions[0].turns).toHaveLength(0);
  });

  it("session toolCount and textCount aggregate turns", () => {
    const msgs: EventMessage[] = [
      { kind: "init", ts: 1, init: { model: "m", agentVersion: "1", sessionID: "s1", tools: [], cwd: "/", harness: "claude" } },
      toolUseEvent("t1", "Read"),
      resultEvent(),
      textDeltaEvent("text"),
      resultEvent(),
    ];
    const sessions = groupSessions(msgs);
    expect(sessions).toHaveLength(1);
    expect(sessions[0].toolCount).toBe(1);
    expect(sessions[0].textCount).toBe(1);
  });
});

describe("groupTurns", () => {
  it("result event splits turns", () => {
    const events: EventMessage[] = [
      textDeltaEvent("first turn"),
      toolUseEvent("t1", "Read"),
      resultEvent(),
      textDeltaEvent("second turn"),
    ];
    const groups = groupMessages(events);
    const turns = groupTurns(groups);
    expect(turns).toHaveLength(2);
    expect(turns[0].toolCount).toBe(1);
    expect(turns[0].textCount).toBe(1);
    expect(turns[1].toolCount).toBe(0);
    expect(turns[1].textCount).toBe(1);
  });

  it("durationMs comes from result event duration", () => {
    const events: EventMessage[] = [textDeltaEvent("text"), resultEvent()];
    const groups = groupMessages(events);
    const turns = groupTurns(groups);
    expect(turns).toHaveLength(1);
    expect(turns[0].durationMs).toBe(1000); // resultEvent() has duration: 1.0s
  });

  it("turnSummary formats correctly", () => {
    const turn = { groups: [], toolCount: 3, textCount: 2, durationMs: 5000 };
    expect(turnSummary(turn)).toBe("2 messages, 3 tool calls · 5s");
  });
});

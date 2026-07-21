// Tests for groupMessages and groupTurns logic.

import { describe, it, expect } from "vitest";

import type { EventMessage, ISOTimestamp } from "@sdk/types.gen";

import { IncrementalMessageGrouper, groupMessages, groupTurns, groupSessions, turnSummary, buildTurnItems, buildPastSessionItems } from "./grouping";

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

  it("non-last tool calls in last group are marked done", () => {
    // First call is done (agent moved to second); last call is still pending.
    const groups = groupMessages([toolUseEvent("t1", "Bash"), toolUseEvent("t2", "Read")]);
    expect(groups).toHaveLength(1);
    expect(groups[0].toolCalls[0].done).toBe(true);
    expect(groups[0].toolCalls[1].done).toBe(false);
  });

  it("tool is marked done when its toolResult arrives", () => {
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

  it("thinking before text after tool group is moved into the tool group", () => {
    // Regression: thinking-2 between a tool group and text was absorbed into
    // the text group by the initial pass, then rendered as a standalone
    // ThinkingCard outside the tool group. The merge pass should extract
    // thinking events from the text group into the preceding tool group.
    const groups = groupMessages([
      toolUseEvent("t1", "Read"),
      usageEvent(),
      { kind: "thinking", ts: 0, thinking: { text: "reflecting" } },
      textDeltaEvent("The result is..."),
    ]);
    expect(groups).toHaveLength(2); // [tool group, text group]
    const toolGroup = groups.find((g) => g.kind === "action");
    const textGroup = groups.find((g) => g.kind === "text");
    expect(toolGroup?.events.some((e) => e.kind === "thinking")).toBe(true);
    expect(textGroup?.events.some((e) => e.kind === "thinking")).toBe(false);
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

  it("incremental grouping keeps streaming thinking presentation stable", () => {
    const toolThenThinking = [
      toolUseEvent("t1", "Read"),
      toolResultEvent("t1"),
      { kind: "thinkingDelta", ts: 1, thinkingDelta: { text: "analyzing" } },
    ] satisfies EventMessage[];

    const grouper = new IncrementalMessageGrouper();
    grouper.group(toolThenThinking.slice(0, 2));
    let inc = grouper.group(toolThenThinking);
    let full = groupMessages(toolThenThinking);
    expect(inc).toEqual(full);
    expect(inc).toHaveLength(1);
    expect(inc[0].events.some((e) => e.kind === "thinkingDelta")).toBe(true);

    const thinkingThenText = [
      { kind: "thinkingDelta", ts: 1, thinkingDelta: { text: "thinking" } },
      { kind: "textDelta", ts: 2, textDelta: { text: "answer" } },
    ] satisfies EventMessage[];

    grouper.reset();
    grouper.group(thinkingThenText.slice(0, 1));
    inc = grouper.group(thinkingThenText);
    full = groupMessages(thinkingThenText);
    expect(inc).toEqual(full);
    expect(inc).toHaveLength(1);
    expect(inc[0].kind).toBe("text");
    expect(inc[0].events.map((e) => e.kind)).toEqual(["thinkingDelta", "textDelta"]);
  });

  it("incremental grouping completes tool calls when text starts", () => {
    const events = [toolUseEvent("t1", "Read"), textDeltaEvent("done")] satisfies EventMessage[];
    const grouper = new IncrementalMessageGrouper();

    const firstSnapshot = grouper.group(events.slice(0, 1));
    const updated = grouper.group(events);

    expect(updated).toEqual(groupMessages(events));
    expect(updated[0].toolCalls[0].done).toBe(true);
    expect(firstSnapshot[0].toolCalls[0].done).toBe(false);
  });

  it("incremental grouping completes tool calls when a widget starts", () => {
    const events = [
      toolUseEvent("t1", "show_widget"),
      { kind: "widgetDelta", ts: 1, widgetDelta: { toolUseID: "t1", delta: "<p>done</p>" } },
    ] satisfies EventMessage[];
    const grouper = new IncrementalMessageGrouper();

    grouper.group(events.slice(0, 1));
    const updated = grouper.group(events);

    expect(updated).toEqual(groupMessages(events));
    expect(updated[0].toolCalls[0].done).toBe(true);
  });

  it("incremental groupers keep immutable, isolated stream snapshots", () => {
    const firstGrouper = new IncrementalMessageGrouper();
    const secondGrouper = new IncrementalMessageGrouper();
    const firstEvent = textDeltaEvent("first");
    const firstSnapshot = firstGrouper.group([firstEvent]);

    secondGrouper.group([textDeltaEvent("other stream")]);
    const updated = firstGrouper.group([firstEvent, textDeltaEvent(" second")]);

    expect(updated).not.toBe(firstSnapshot);
    expect(updated[0]).not.toBe(firstSnapshot[0]);
    expect(firstSnapshot[0].events).toEqual([firstEvent]);
    expect(updated[0].events.map((event) => event.textDelta?.text)).toEqual(["first", " second"]);
  });

  it("widgetDelta events create a widget group", () => {
    const groups = groupMessages([
      { kind: "widgetDelta", ts: 0, widgetDelta: { toolUseID: "w1", delta: "<h1>" } },
      { kind: "widgetDelta", ts: 0, widgetDelta: { toolUseID: "w1", delta: "Hi</h1>" } },
    ]);
    expect(groups).toHaveLength(1);
    expect(groups[0].kind).toBe("widget");
    expect(groups[0].widgetToolUseID).toBe("w1");
    expect(groups[0].widgetHTML).toBe("<h1>Hi</h1>");
    expect(groups[0].widgetDone).toBe(false);
  });

  it("widget event finalises widget group from deltas", () => {
    const groups = groupMessages([
      { kind: "widgetDelta", ts: 0, widgetDelta: { toolUseID: "w1", delta: "<h1>" } },
      { kind: "widget", ts: 0, widget: { toolUseID: "w1", title: "Chart", html: "<h1>Done</h1>" } },
    ]);
    expect(groups).toHaveLength(1);
    expect(groups[0].kind).toBe("widget");
    expect(groups[0].widgetHTML).toBe("<h1>Done</h1>");
    expect(groups[0].widgetTitle).toBe("Chart");
  });

  it("widget event alone creates a widget group (replay)", () => {
    const groups = groupMessages([
      { kind: "widget", ts: 0, widget: { toolUseID: "w1", title: "Test", html: "<p>hi</p>" } },
    ]);
    expect(groups).toHaveLength(1);
    expect(groups[0].kind).toBe("widget");
    expect(groups[0].widgetHTML).toBe("<p>hi</p>");
    expect(groups[0].widgetTitle).toBe("Test");
  });

  it("toolResult for widget marks widgetDone", () => {
    const groups = groupMessages([
      { kind: "widgetDelta", ts: 0, widgetDelta: { toolUseID: "w1", delta: "<p>x</p>" } },
      { kind: "toolResult", ts: 0, toolResult: { toolUseID: "w1", duration: 0.1 } },
    ]);
    expect(groups).toHaveLength(1);
    expect(groups[0].kind).toBe("widget");
    expect(groups[0].widgetDone).toBe(true);
  });

  it("userInput after ask+result is grouped with the ask", () => {
    const askEvent: EventMessage = {
      kind: "ask", ts: 1,
      ask: {
        toolUseID: "ask_1",
        questions: [{ question: "Which?", options: [{ label: "A" }, { label: "B" }] }],
      },
    };
    const groups = groupMessages([askEvent, resultEvent(), { kind: "userInput", ts: 3, userInput: { text: "A" } }]);
    const askGroup = groups.find((g) => g.kind === "ask");
    expect(askGroup?.answerText).toBe("A");
  });

  it("rateLimit warning creates other group", () => {
    const groups = groupMessages([
      { kind: "rateLimit", ts: 1, rateLimit: { status: "allowed_warning", rateLimitType: "five_hour", utilization: 0.8 } },
    ]);
    expect(groups).toHaveLength(1);
    expect(groups[0].kind).toBe("other");
  });

  it("rateLimit allowed is filtered out", () => {
    const groups = groupMessages([
      { kind: "rateLimit", ts: 1, rateLimit: { status: "allowed", rateLimitType: "five_hour", utilization: 0.3 } },
    ]);
    expect(groups).toHaveLength(0);
  });

  it("rateLimit rejected creates other group", () => {
    const groups = groupMessages([
      { kind: "rateLimit", ts: 1, rateLimit: { status: "rejected", resetsAt: "2024-03-21T04:26:40Z" as ISOTimestamp, rateLimitType: "seven_day", utilization: 1.0 } },
    ]);
    expect(groups).toHaveLength(1);
    expect(groups[0].kind).toBe("other");
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

  it("durationMs uses result.duration directly (per-invocation, not cumulative)", () => {
    // ResultMessage.DurationMs is per-invocation wall-clock time for that turn.
    const makeResult = (duration: number): EventMessage => ({
      kind: "result", ts: 0,
      result: {
        subtype: "success", isError: false, result: "done",
        totalCostUSD: 0.01, duration, durationAPI: duration * 0.9,
        numTurns: 1,
        usage: { inputTokens: 100, outputTokens: 50, cacheCreationInputTokens: 0, cacheReadInputTokens: 0, model: "test" },
      },
    });
    const events: EventMessage[] = [
      textDeltaEvent("turn 1"),
      makeResult(1.0),  // turn 1 took 1s
      textDeltaEvent("turn 2"),
      makeResult(3.0),  // turn 2 took 3s
    ];
    const groups = groupMessages(events);
    const turns = groupTurns(groups);
    expect(turns).toHaveLength(2);
    expect(turns[0].durationMs).toBe(1000); // 1.0s → 1000ms
    expect(turns[1].durationMs).toBe(3000); // 3.0s → 3000ms
  });

  it("turnSummary formats correctly", () => {
    const turn = { groups: [], toolCount: 3, textCount: 2, durationMs: 5000 };
    expect(turnSummary(turn)).toBe("2 messages, 3 tool calls · 5s");
  });
});

describe("buildTurnItems", () => {
  it("all turns are elidable when passed to buildTurnItems", () => {
    // buildTurnItems elidates every turn it receives — the caller is responsible
    // for excluding the last completed turn (which should always be expanded).
    const events: EventMessage[] = [
      textDeltaEvent("turn 1"),
      resultEvent(),
      textDeltaEvent("turn 2"),
      resultEvent(),
    ];
    const turns = groupTurns(groupMessages(events));
    const items = buildTurnItems(turns, new Set(), "session:0:");
    expect(items).toHaveLength(2);
    expect(items.every((i) => i.kind === "elided")).toBe(true);
  });

  it("expanded turn emits header + group items", () => {
    const events: EventMessage[] = [textDeltaEvent("text"), resultEvent()];
    const turns = groupTurns(groupMessages(events));
    const turnKey = `session:0::turn:0:${turns[0].groups[0]?.events[0]?.ts ?? ""}`;
    const items = buildTurnItems(turns, new Set([turnKey]), "session:0:");
    expect(items[0].kind).toBe("expandedHeader");
    expect(items[1].kind).toBe("group");
  });

  it("produces identical keys for two calls with the same inputs", () => {
    // Regression: if buildTurnItems creates new MsgItem objects on every call,
    // the <Match when={..} keyed> pattern in TaskDetail remounts DOM elements
    // (Solid uses reference equality for keyed keys), causing flickering.
    // The fix splits items() into per-concern createMemo calls so stable parts
    // retain object identity. This test validates that keys are deterministic:
    // any caching layer can use them as stable lookup keys.
    const events: EventMessage[] = [
      textDeltaEvent("turn 1"),
      resultEvent(),
      textDeltaEvent("turn 2"),
      resultEvent(),
    ];
    const turns = groupTurns(groupMessages(events));
    const sessionKey = "session:0:";
    const expanded = new Set<string>();
    const a = buildTurnItems(turns, expanded, sessionKey);
    const b = buildTurnItems(turns, expanded, sessionKey);
    expect(a.length).toBe(b.length);
    for (let i = 0; i < a.length; i++) {
      expect(a[i].key).toBe(b[i].key);
    }
  });

  it("turn keys from different sessions do not collide", () => {
    // Validate that the sessionKey prefix is embedded in the turn key so
    // turns from different sessions never share a key, even with index 0 and ts 0.
    const events: EventMessage[] = [textDeltaEvent("msg"), resultEvent()];
    const turns = groupTurns(groupMessages(events));
    const a = buildTurnItems(turns, new Set(), "session:0:");
    const b = buildTurnItems(turns, new Set(), "session:1:");
    const keysA = new Set(a.map((i) => i.key));
    const keysB = new Set(b.map((i) => i.key));
    for (const k of keysA) {
      expect(keysB.has(k)).toBe(false);
    }
  });

  it("expanded turn items include the sessionKey in their key prefix", () => {
    // Expanded turn groups use keys like "hdr:<turnKey>" and "<turnKey>-g0".
    // The turnKey itself must embed the sessionKey to avoid collisions when
    // two sessions have turns at index 0 with the same first-event ts.
    const events: EventMessage[] = [textDeltaEvent("text"), resultEvent()];
    const turns = groupTurns(groupMessages(events));
    const sessionKey = "session:1:";
    const turnKey = `${sessionKey}turn:0:${turns[0].groups[0]?.events[0]?.ts ?? ""}`;
    const items = buildTurnItems(turns, new Set([turnKey]), sessionKey);
    for (const item of items) {
      expect(item.key).toContain("session:1:");
    }
  });
});

describe("buildPastSessionItems", () => {
  it("produces identical keys for two calls with the same inputs", () => {
    // Past sessions are memoized via createMemo in TaskDetail. If keys change
    // identity between calls, the keyed Match components remount and cause flickering.
    const msgs: EventMessage[] = [
      { kind: "init", ts: 1, init: { model: "m", agentVersion: "1", sessionID: "s1", tools: [], cwd: "/", harness: "claude" } },
      textDeltaEvent("session 1"),
      resultEvent(),
      { kind: "init", ts: 2, init: { model: "m", agentVersion: "1", sessionID: "s2", tools: [], cwd: "/", harness: "claude" } },
      textDeltaEvent("session 2"),
    ];
    const sessions = groupSessions(msgs);
    const a = buildPastSessionItems(sessions, new Set(), new Set());
    const b = buildPastSessionItems(sessions, new Set(), new Set());
    expect(a.length).toBe(b.length);
    for (let i = 0; i < a.length; i++) {
      expect(a[i].key).toBe(b[i].key);
    }
  });

  it("session keys from different calls with same inputs are stable", () => {
    // When a past session is collapsed (sessionElided), its key must not change
    // across recomputation frames, otherwise the elided row remounts.
    const msgs: EventMessage[] = [
      { kind: "init", ts: 10, init: { model: "m", agentVersion: "1", sessionID: "s1", tools: [], cwd: "/", harness: "claude" } },
      textDeltaEvent("text"),
      resultEvent(),
    ];
    const sessions = groupSessions(msgs);
    const items = buildPastSessionItems(sessions, new Set(), new Set());
    // All items from past sessions should be sessionElided when not expanded.
    expect(items.every((i) => i.kind === "sessionElided")).toBe(true);
    const keys = items.map((i) => i.key);
    // Keys should be unique.
    expect(new Set(keys).size).toBe(keys.length);
    // Session key should include the boundary event timestamp.
    expect(keys[0]).toContain("10");
  });
});

// Performance benchmarks: validate that groupMessages + groupTurns scale linearly
// for realistic workloads. Pi-style thinking_delta torrents are the stress case.
//
// Thresholds (Node.js, approximate):
// - 1k  events: < 5 ms   (typical Claude Code turn)
// - 10k events: < 50 ms  (large pi turn)
// - 65k events: < 500 ms (extreme case from caic-43 trace)
describe("performance benchmarks", () => {
  function generateThinkingDeltas(count: number): EventMessage[] {
    const msgs: EventMessage[] = [
      {
        kind: "thinking", ts: 1777988039500,
        thinking: { text: "Let me think about this..." },
      },
    ];
    for (let i = 0; i < count; i++) {
      msgs.push({
        kind: "thinkingDelta", ts: 1777988039509 + i,
        thinkingDelta: { text: `word${i} ` },
      });
    }
    return msgs;
  }

  it("single-call performance scales acceptably", () => {
    const sizes = [1000, 10000, 65000];
    for (const size of sizes) {
      const msgs = generateThinkingDeltas(size);
      const start = performance.now();
      const groups = groupMessages(msgs);
      const turns = groupTurns(groups);
      const elapsed = performance.now() - start;
      // Force consumption so dead code elimination doesn't skip work.
      expect(turns.length).toBeGreaterThanOrEqual(0);
      // eslint-disable-next-line no-console -- benchmark diagnostic output
      console.log(`groupMessages+groupTurns(${size} events): ${elapsed.toFixed(1)} ms`);
      const maxMs = size === 1000 ? 50 : size === 10000 ? 200 : 3000;
      expect(elapsed).toBeLessThan(maxMs);
    }
  });

  it("incremental performance (batched deltas) scales acceptably", () => {
    const totalEvents = 10000;
    const batchSize = 50;
    const batches = totalEvents / batchSize;
    let cumulative: EventMessage[] = [];
    let totalTime = 0;
    for (let i = 0; i < batches; i++) {
      const newDeltas = Array.from({ length: batchSize }, (_, j) => ({
        kind: "thinkingDelta" as const,
        ts: 1777988039509 + (i * batchSize + j),
        thinkingDelta: { text: `word${i * batchSize + j} ` },
      }));
      cumulative = [...cumulative, ...newDeltas];
      const start = performance.now();
      const groups = groupMessages(cumulative);
      groupTurns(groups);
      totalTime += performance.now() - start;
    }
    // eslint-disable-next-line no-console -- benchmark diagnostic output
    console.log(
      `groupMessages incremental (${totalEvents} events, ${batchSize}/batch, ${batches} batches): total=${totalTime.toFixed(1)}ms avg=${(totalTime / batches).toFixed(2)}ms`,
    );
    expect(totalTime).toBeLessThan(5000);
  });
});

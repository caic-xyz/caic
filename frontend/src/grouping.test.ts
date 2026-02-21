// Tests for groupMessages and groupTurns logic.
import { describe, it, expect } from "vitest";
import { groupMessages, groupTurns, turnSummary } from "./grouping";
import type { ClaudeEventMessage } from "@sdk/types.gen";

function toolUseEvent(id: string, name: string): ClaudeEventMessage {
  return { kind: "toolUse", ts: 0, toolUse: { toolUseID: id, name, input: {} } };
}

function toolResultEvent(id: string): ClaudeEventMessage {
  return { kind: "toolResult", ts: 0, toolResult: { toolUseID: id, duration: 0.1 } };
}

function textDeltaEvent(text: string): ClaudeEventMessage {
  return { kind: "textDelta", ts: 0, textDelta: { text } };
}

function usageEvent(): ClaudeEventMessage {
  return {
    kind: "usage", ts: 0,
    usage: { inputTokens: 100, outputTokens: 50, cacheCreationInputTokens: 0, cacheReadInputTokens: 0, model: "test" },
  };
}

function resultEvent(): ClaudeEventMessage {
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
    expect(groups[0].kind).toBe("tool");
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
    expect(groups[0].kind).toBe("tool");
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
    expect(groups[0].kind).toBe("tool");
    expect(groups[0].toolCalls).toHaveLength(3);
  });
});

describe("groupTurns", () => {
  it("result event splits turns", () => {
    const events: ClaudeEventMessage[] = [
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

  it("turnSummary formats correctly", () => {
    const turn = { groups: [], toolCount: 3, textCount: 2 };
    expect(turnSummary(turn)).toBe("2 messages, 3 tool calls");
  });
});

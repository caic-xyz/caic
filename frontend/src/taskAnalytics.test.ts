// Tests compact analytics across chunk boundaries, native actors, authoritative estimates, and payload release.
import { describe, it } from "node:test";
import { expect } from "@tests/expect";
import type { EventMessage } from "@sdk/types.gen";
import { deriveTaskTimings } from "./timing";
import { toolCallDurations, toolIdentity } from "./grouping";
import { TaskAnalytics } from "./taskAnalytics";

function output(_recordID: number, message: EventMessage): EventMessage {
  return message;
}
function result(text: string): EventMessage {
  return {
    kind: "result",
    ts: 5000,
    result: {
      subtype: "success",
      isError: false,
      result: text,
      totalCostUSD: 4,
      duration: 2,
      durationAPI: 1,
      numTurns: 1,
      usage: {
        inputTokens: 10,
        outputTokens: 5,
        cacheCreationInputTokens: 2,
        cacheReadInputTokens: 3,
        reasoningOutputTokens: 1,
        reportedModel: "",
      },
    },
  };
}

describe("compact task analytics", () => {
  it("keeps scoped tool totals across raw chunks and excludes child results from root turns", () => {
    const fold = new TaskAnalytics();
    const root = { threadID: "root", turnID: "turn" };
    const child = { threadID: "child", agentID: "native:child", turnID: "turn" };
    fold.append(
      output(255, {
        kind: "toolUse",
        ts: 1000,
        source: root,
        toolUse: { name: "Read", toolUseID: "same", input: { content: "large input" } },
      }),
    );
    fold.append(
      output(256, {
        kind: "toolUse",
        ts: 2000,
        source: child,
        toolUse: { name: "Bash", toolUseID: "same", input: {} },
      }),
    );
    fold.append(
      output(257, { kind: "toolResult", ts: 3500, source: root, toolResult: { toolUseID: "same", duration: 0 } }),
    );
    fold.append(
      output(258, { kind: "toolResult", ts: 4000, source: child, toolResult: { toolUseID: "same", duration: 1 } }),
    );
    fold.append(output(259, { ...result("child result"), source: child }));
    fold.append(output(260, result("root result")));
    expect(fold.snapshot().tools).toEqual([
      { name: "Read", calls: 1, durationMs: 2500 },
      { name: "Bash", calls: 1, durationMs: 1000 },
    ]);
    expect(fold.snapshot().turns).toHaveLength(1);
    expect(fold.snapshot().turns[0]?.usage.inputTokens).toBe(10);
  });

  it("merges rerouted models/cumulative estimates by raw position without retaining transcript objects", () => {
    const fold = new TaskAnalytics();
    fold.append(
      output(1, {
        kind: "init",
        ts: 1,
        init: {
          reportedModel: "first",
          reportedEffort: "",
          agentVersion: "",
          sessionID: "s",
          tools: [],
          cwd: "",
          harness: "claude",
        },
      }),
    );
    fold.append(output(2, { kind: "text", ts: 2, text: { text: "TRANSCRIPT_SENTINEL".repeat(1000) } }));
    const pricedResult = result("RESULT_SENTINEL".repeat(1000)).result;
    if (!pricedResult) throw new Error("Missing test result");
    fold.append({ kind: "result", ts: 5000, result: pricedResult });
    fold.estimates([{ result: pricedResult, model: "second", cost: { usd: 2.5, source: "estimated" } }]);
    expect(fold.snapshot().turns[0]?.model).toBe("second");
    expect(fold.snapshot().costUSD).toBe(2.5);
    expect(fold.snapshot().costComplete).toBe(true);
    expect(JSON.stringify(fold)).not.toContain("SENTINEL");
    expect(Object.keys(fold.snapshot().turns[0] ?? {})).not.toContain("event");
    expect(Object.keys(fold.snapshot().turns[0] ?? {})).not.toContain("result");
    fold.estimates([{ result: pricedResult, model: "second", cost: null }]);
    expect(fold.snapshot().costComplete).toBe(false);
    expect(fold.snapshot().turns[0]?.model).toBe("second");
  });
});

function retainedStrings(value: unknown, seen: Set<object>): string[] {
  if (typeof value === "string") return [value];
  if (typeof value !== "object" || value === null || seen.has(value)) return [];
  seen.add(value);
  if (value instanceof Map)
    return [...value].flatMap(([key, item]) => [...retainedStrings(key, seen), ...retainedStrings(item, seen)]);
  if (value instanceof Set) return [...value].flatMap((item) => retainedStrings(item, seen));
  return Object.values(value).flatMap((item) => retainedStrings(item, seen));
}

it("matches the existing full timing/tool folds on the same scoped conversation", () => {
  const root = { threadID: "root", turnID: "one" };
  const child = { threadID: "child", agentID: "native:child", turnID: "child-one" };
  const first = result("result body");
  const events: EventMessage[] = [
    {
      kind: "init",
      ts: 1,
      init: {
        reportedModel: "model",
        reportedEffort: "",
        agentVersion: "",
        sessionID: "s",
        tools: [],
        cwd: "",
        harness: "claude",
      },
    },
    { kind: "userInput", ts: 1000, userInput: { text: "initial" } },
    { kind: "thinking", ts: 1200, thinking: { text: "THINKING_PAYLOAD" } },
    {
      kind: "toolUse",
      ts: 2000,
      source: root,
      toolUse: { toolUseID: "same", name: "Bash", input: { command: "INPUT_PAYLOAD" } },
    },
    { kind: "toolUse", ts: 2500, source: child, toolUse: { toolUseID: "same", name: "Read", input: { path: "x" } } },
    { kind: "toolResult", ts: 3500, source: child, toolResult: { toolUseID: "same", duration: 0.2 } },
    {
      kind: "toolOutputDelta",
      ts: 3600,
      source: root,
      toolOutputDelta: { toolUseID: "same", delta: "OUTPUT_PAYLOAD" },
    },
    { kind: "toolResult", ts: 4000, source: root, toolResult: { toolUseID: "same", duration: 0 } },
    { ...first, source: child },
    first,
    {
      kind: "commitSnapshot",
      ts: 6000,
      commitSnapshot: {
        repositoryCommits: [],
        changeStat: { files: 1, linesAdded: 2, linesDeleted: 3, binaryFiles: 0 },
      },
    },
    { kind: "userInput", ts: 8000, userInput: { text: "continue" } },
    { kind: "system", ts: 8500, system: { subtype: "context_cleared" } },
    {
      kind: "toolUse",
      ts: 9000,
      toolUse: { toolUseID: "pending", name: "Write", input: { content: "PENDING_PAYLOAD" } },
    },
  ];
  const reference = deriveTaskTimings(events);
  const fold = new TaskAnalytics();
  for (const [index, event] of events.entries()) fold.append(output(index + 1, event));
  const actual = fold.snapshot();
  expect(
    actual.turns.map((turn) => ({
      usage: turn.usage,
      durationMs: turn.durationMs,
      durationAPIMs: turn.durationAPIMs,
      waitMs: turn.waitMs,
      changeStat: turn.changeStat,
    })),
  ).toEqual(
    reference.turns.map((turn) => ({
      usage: turn.result.usage,
      durationMs: turn.result.duration * 1000,
      durationAPIMs: turn.result.durationAPI * 1000,
      waitMs: turn.waitMs,
      changeStat: turn.changeStat,
    })),
  );
  // The conversation fallback helper uses provider-local IDs; scope each
  // actor/turn before comparing its full fold with inclusive tool totals.
  const actors = new Map<string, EventMessage[]>();
  for (const event of events) {
    const key = toolIdentity(event.source, "");
    const actor = actors.get(key) ?? [];
    actor.push(event);
    actors.set(key, actor);
  }
  const fullDurations = [...actors.values()].flatMap((actor) => [...toolCallDurations(actor).values()]);
  expect(actual.tools.reduce((sum, tool) => sum + tool.durationMs, 0)).toBe(
    fullDurations.reduce((sum, value) => sum + value, 0),
  );
  expect(actual.tools.reduce((sum, tool) => sum + tool.calls, 0)).toBe(fullDurations.length);
  expect(retainedStrings(fold, new Set()).some((value) => value.includes("PAYLOAD"))).toBe(false);
  expect(actual.costComplete).toBe(false);
});

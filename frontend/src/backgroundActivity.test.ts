// Tests background-count wording and the running items listed from history metadata.

import { describe, it } from "node:test";
import { expect } from "@tests/expect";
import type { HistoryActivity, HistoryBackgroundCommand, ISOTimestamp } from "@sdk/types.gen";
import { activeBackgroundItems, backgroundCounts, backgroundLabel, commandSettled } from "./backgroundActivity";
import { toolIdentity } from "./grouping";

describe("backgroundCounts", () => {
  it("lists non-zero counts as command, agent, then watch", () => {
    expect(backgroundCounts({ commands: 2, watches: 3, agents: 0 })).toEqual([
      { kind: "command", count: 2 },
      { kind: "watch", count: 3 },
    ]);
  });

  it("lists nothing without running work", () => {
    expect(backgroundCounts(undefined)).toEqual([]);
    expect(backgroundCounts({ commands: 0, watches: 0, agents: 0 })).toEqual([]);
  });
});

describe("backgroundLabel", () => {
  it("names each count with correct plurals", () => {
    expect(backgroundLabel(backgroundCounts({ commands: 1, watches: 1, agents: 1 }))).toBe(
      "Background: 1 command · 1 agent · 1 watch",
    );
    expect(backgroundLabel(backgroundCounts({ commands: 2, watches: 3, agents: 2 }))).toBe(
      "Background: 2 commands · 2 agents · 3 watches",
    );
  });
});

const startedAt = "2026-07-08T12:00:00Z" as ISOTimestamp;

function command(id: string, fields: Partial<HistoryBackgroundCommand>): HistoryBackgroundCommand {
  return { id, status: "running", startedAt, ...fields };
}

function activity(id: string, fields: Partial<HistoryActivity>): HistoryActivity {
  return {
    id,
    sectionID: "s",
    status: "running",
    anchorAt: startedAt,
    first: { recordID: 1, index: 0 },
    bodyThrough: 0,
    ownershipThrough: 0,
    ...fields,
  };
}

describe("activeBackgroundItems", () => {
  const metadata = {
    backgroundCommands: [
      command("w", { watch: true, wakesAgent: true, label: "results", toolUseID: "toolu_w" }),
      command("done", { status: "completed", wakesAgent: true, toolUseID: "toolu_d" }),
      command("c", { label: "checks", wakesAgent: true, toolUseID: "toolu_c", source: { agentID: "child" } }),
      command("bare", {}),
    ],
    activities: [
      activity("fg", { label: "foreground" }),
      activity("bg", { label: "review", background: true, startedAt }),
      activity("bg-done", { background: true, status: "completed" }),
    ],
  };

  it("lists running commands, background agents, then watches with their tool calls", () => {
    expect(activeBackgroundItems(metadata, false)).toEqual([
      {
        kind: "command",
        kindName: "Command",
        id: "c",
        label: "checks",
        startedAt: Date.parse(startedAt),
        toolKey: toolIdentity({ agentID: "child" }, "toolu_c"),
        ownerAgentID: "child",
      },
      {
        kind: "command",
        kindName: "Command",
        id: "bare",
        label: "bare",
        startedAt: Date.parse(startedAt),
        toolKey: null,
        ownerAgentID: null,
      },
      {
        kind: "agent",
        kindName: "Agent",
        id: "bg",
        label: "review",
        startedAt: Date.parse(startedAt),
        toolKey: null,
        ownerAgentID: null,
      },
      {
        kind: "watch",
        kindName: "Watch",
        id: "w",
        label: "results",
        startedAt: Date.parse(startedAt),
        toolKey: toolIdentity(undefined, "toolu_w"),
        ownerAgentID: null,
      },
    ]);
  });

  it("lists only the commands that do not wake the agent for a settled task", () => {
    expect(activeBackgroundItems(metadata, true).map((item) => item.id)).toEqual(["bare"]);
  });

  it("names an agent by its reported kind, a batch as Batch, and otherwise Agent", () => {
    const names = activeBackgroundItems(
      {
        backgroundCommands: [],
        activities: [
          activity("explore", { kind: "Explore", background: true }),
          activity("batch", { scope: "batch", background: true }),
          activity("plain", { background: true }),
        ],
      },
      false,
    ).map((item) => item.kindName);
    expect(names).toEqual(["Explore", "Batch", "Agent"]);
  });

  it("lists nothing for missing metadata", () => {
    expect(activeBackgroundItems(undefined, false)).toEqual([]);
  });
});

describe("commandSettled", () => {
  it("makes only a command that wakes the agent stale in a settled task", () => {
    expect(commandSettled({ wakesAgent: true }, true)).toBe(true);
    expect(commandSettled({}, true)).toBe(false);
    expect(commandSettled({ wakesAgent: true }, false)).toBe(false);
  });
});

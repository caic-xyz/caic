// Tests canonical native and background-command lifecycle folding, replay identity, timing, and partial observability.

import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { expect } from "@tests/expect";
import type { EventBackgroundCommand, EventMessage, EventNativeSubagent } from "@sdk/types.gen";
import type { MessageGroup, MsgItem } from "./grouping";
import {
  BackgroundCommandTracker,
  NativeActivityTracker,
  assignNativeAnchors,
  backgroundCommandStatus,
  nativeActivityStatus,
  type BackgroundCommandActivity,
  type NativeActivity,
} from "./nativeSubagents";

function event(
  id: string,
  status: EventNativeSubagent["status"],
  ts: number,
  fields: Partial<EventNativeSubagent>,
): EventMessage {
  return {
    kind: "nativeSubagent",
    ts,
    nativeSubagent: { id, status, ...fields },
  };
}

describe("NativeActivityTracker", () => {
  it("keeps repeated delegation IDs in separate root namespaces and leaves ambiguous history unattributed", () => {
    const a: EventMessage = {
      kind: "text",
      ts: 1,
      source: { threadID: "root-a", delegationToolUseID: "spawn" },
      text: { text: "From A" },
    };
    const b: EventMessage = {
      kind: "text",
      ts: 2,
      source: { threadID: "root-b", delegationToolUseID: "spawn" },
      text: { text: "From B" },
    };
    const tracker = new NativeActivityTracker();
    tracker.derive([a, b]);
    const mapped = [
      a,
      b,
      { ...event("native:a", "running", 3, { toolUseID: "spawn" }), source: { threadID: "root-a" } },
      { ...event("native:b", "running", 4, { toolUseID: "spawn" }), source: { threadID: "root-b" } },
    ];
    const activities = tracker.derive(mapped);
    expect(tracker.ownerFor(a)).toBe("native:a");
    expect(tracker.ownerFor(b)).toBe("native:b");
    expect(activities.map((entry) => entry.messages?.[0]?.text?.text)).toEqual(["From A", "From B"]);
    expect(new NativeActivityTracker().derive(mapped)).toEqual(activities);
    const unknown = new NativeActivityTracker();
    const ambiguous = unknown.derive([a, b, event("native:unknown", "running", 3, { toolUseID: "spawn" })]);
    expect(unknown.ownerFor(a)).toBeNull();
    expect(unknown.ownerFor(b)).toBeNull();
    assert.ok(ambiguous[0]);
    expect(ambiguous[0].messages).toBeUndefined();
  });
  it("correlates early delegation output and folds a grandchild lifecycle before actor routing", () => {
    const source = { delegationToolUseID: "spawn", turnID: "turn" };
    const early: EventMessage = {
      kind: "toolOutputDelta",
      ts: 1,
      source,
      toolOutputDelta: { toolUseID: "call", delta: "early output" },
    };
    const tracker = new NativeActivityTracker();
    expect(tracker.derive([early])).toEqual([]);
    expect(tracker.ownerFor(early)).toBeNull();
    const messages: EventMessage[] = [
      early,
      event("native:child", "running", 2, { toolUseID: "spawn" }),
      {
        kind: "toolUse",
        ts: 3,
        source: { ...source, agentID: "native:child" },
        toolUse: { toolUseID: "call", name: "Read", input: {} },
      },
      {
        ...event("native:grandchild", "running", 4, { toolUseID: "call" }),
        source: { ...source, agentID: "native:child" },
      },
      event("native:child", "completed", 5, {}),
    ];
    const activities = tracker.derive(messages);
    expect(activities.map((a) => a.id)).toEqual(["native:child", "native:grandchild"]);
    expect(tracker.ownerFor(early)).toBe("native:child");
    expect(activities[0]?.messages).toEqual([early, messages[2]]);
    expect(new NativeActivityTracker().derive(messages)).toEqual(activities);
  });

  it("routes long child messages without reading or grouping their text payloads", () => {
    const source = {
      agentID: "codex:thread:child",
      threadID: "child",
      rootThreadID: "parent",
      turnID: "turn",
      itemID: "answer",
    };
    let reads = 0;
    const messages: EventMessage[] = Array.from({ length: 10000 }, (_, i) => ({
      kind: "textDelta",
      ts: i + 1,
      source,
      get textDelta() {
        reads++;
        return { text: "word " };
      },
    }));
    const tracker = new NativeActivityTracker();
    const initial = tracker.derive(messages)[0];
    expect(initial?.messages).toHaveLength(10000);
    expect(initial?.messages?.[0]).toBe(messages[0]);
    expect(reads).toBe(0);
    const next: EventMessage = { kind: "textDelta", ts: 10001, source, textDelta: { text: "continued" } };
    messages.push(next);
    const current = tracker.derive(messages)[0];
    expect(current?.messages?.at(-1)).toBe(next);
    expect(current?.messages).toHaveLength(10001);
    expect(reads).toBe(0);
  });

  it("folds child content before lifecycle, keeps its anchor and clears on replaced history", () => {
    const source = {
      agentID: "codex:thread:child",
      threadID: "child",
      turnID: "turn",
      itemID: "answer",
      rootThreadID: "parent",
    };
    const messages: EventMessage[] = [
      { kind: "textDelta", ts: 1000, source, textDelta: { text: "A bow" } },
      event("codex:thread:child", "running", 1500, { label: "Research" }),
      { kind: "textDelta", ts: 2000, source, textDelta: { text: "tie" } },
      { kind: "text", ts: 3000, source, text: { text: "A bowtie." } },
      event("codex:thread:child", "completed", 4000, { result: "A bowtie." }),
    ];
    const tracker = new NativeActivityTracker();
    expect(tracker.derive(messages.slice(0, 1))[0]).toMatchObject({ status: "unknown", anchorTs: 1000 });
    const child = tracker.derive(messages)[0];
    assert.ok(child);
    expect(child).toMatchObject({ status: "completed", label: "Research", anchorTs: 1000 });
    expect(child.messages).toEqual([messages[0], messages[2], messages[3]]);
    expect(new NativeActivityTracker().derive(messages)).toEqual([child]);
    expect(tracker.derive([{ kind: "text", ts: 10, text: { text: "Other history" } }])).toEqual([]);
  });

  it("does not guess child ownership when the authoritative root is absent", () => {
    expect(
      new NativeActivityTracker().derive([
        {
          kind: "textDelta",
          ts: 1,
          source: { threadID: "unknown", turnID: "turn", itemID: "item" },
          textDelta: { text: "Visible" },
        },
      ]),
    ).toEqual([]);
  });

  it("keeps multiple child messages and exact completions separate", () => {
    const source = {
      agentID: "codex:thread:child",
      threadID: "child",
      turnID: "turn",
      itemID: "first",
      rootThreadID: "parent",
    };
    const second = { ...source, itemID: "second" };
    const messages: EventMessage[] = [
      { kind: "textDelta", ts: 1, source, textDelta: { text: "First partial" } },
      { kind: "textDelta", ts: 2, source: second, textDelta: { text: "Second partial" } },
      { kind: "text", ts: 3, source, text: { text: "First complete" } },
      { kind: "textDelta", ts: 4, source: second, textDelta: { text: " continued" } },
      { kind: "text", ts: 5, source: second, text: { text: "Second complete" } },
    ];
    const tracker = new NativeActivityTracker();
    const initial = tracker.derive(messages.slice(0, 2))[0]?.messages;
    expect(initial).toEqual(messages.slice(0, 2));
    const final = tracker.derive(messages)[0]?.messages;
    expect(final).toEqual(messages);
    expect(final?.[0]).toBe(messages[0]);
    expect(final?.at(-1)).toBe(messages.at(-1));
    expect(new NativeActivityTracker().derive(messages)[0]?.messages).toEqual(final);
  });
  it("folds concurrent agents and a batch across compaction without reopening terminal work", () => {
    const tracker = new NativeActivityTracker();
    const messages = [
      event("a", "running", 1000, { groupID: "g", toolUseID: "t" }),
      event("b", "running", 2000, { groupID: "g" }),
      event("batch", "running", 2000, { scope: "batch" }),
    ];
    expect(tracker.derive(messages)).toHaveLength(3);
    const done: EventMessage[] = [
      ...messages,
      event("a", "completed", 3000, { result: "joke" }),
      { kind: "system", ts: 4000, system: { subtype: "compact_boundary" } },
      event("a", "running", 1000, {}),
      event("b", "failed", 5000, { result: "failure" }),
      event("batch", "interrupted", 6000, {}),
    ];
    const folded = tracker.derive(done);
    expect(folded.map((s) => s.status)).toEqual(["completed", "failed", "interrupted"]);
    expect(folded[0]).toMatchObject({
      startedAt: 1000,
      endedAt: 3000,
      result: "joke",
      toolUseID: "t",
    });
    const restored = new NativeActivityTracker().derive(done.map((ev) => ({ ...ev })));
    expect(restored).toEqual(folded);
    expect(tracker.derive(done)).toBe(folded);
  });

  it("anchors an active card at its spawn and moves it once when it settles", () => {
    const tracker = new NativeActivityTracker();
    const spawn = event("a", "running", 100, {});
    const heartbeat = event("a", "running", 500, {});
    const done = event("a", "completed", 900, { result: "done" });
    const spawnActivity = tracker.derive([spawn])[0];
    assert.ok(spawnActivity !== undefined);
    expect(spawnActivity.anchorTs).toBe(100);
    // A running heartbeat must not drag an active card toward newer content.
    const runningActivity = tracker.derive([spawn, heartbeat])[0];
    assert.ok(runningActivity !== undefined);
    expect(runningActivity.anchorTs).toBe(100);
    const completedActivity = tracker.derive([spawn, heartbeat, done])[0];
    assert.ok(completedActivity !== undefined);
    expect(completedActivity.anchorTs).toBe(900);
  });

  it("does not infer running or success from metadata, parent result, or settled state", () => {
    const tracker = new NativeActivityTracker();
    const folded = tracker.derive([
      event("partial", "unknown", 1, {}),
      event("unfinished", "running", 2, {}),
      { kind: "result", ts: 3 },
    ]);
    assert.ok(folded[0] !== undefined);
    expect(folded[0].startedAt).toBeNull();
    assert.ok(folded[1] !== undefined);
    expect(folded[1].status).toBe("running");
    expect(nativeActivityStatus(folded[1], true)).toBe("Last observed running · outcome unknown");
    expect(nativeActivityStatus(folded[0], false)).toBe("Status unknown");
  });

  it("keeps a detached launch marked after later foreground observations", () => {
    const tracker = new NativeActivityTracker();
    const folded = tracker.derive([
      event("child", "running", 1000, { background: true }),
      event("child", "paused", 2000, {}),
    ]);
    assert.ok(folded[0] !== undefined);
    expect(folded[0].background).toBe(true);
    expect(folded[0].status).toBe("paused");
  });

  it("lets a terminal result replace an interim one but not another terminal one", () => {
    const tracker = new NativeActivityTracker();
    const folded = tracker.derive([
      event("wf", "running", 1000, {}),
      event("wf", "paused", 2000, { result: "Output file: /tmp/wf.md" }),
      event("wf", "completed", 3000, { result: "the actual joke" }),
      event("wf", "completed", 4000, { result: "a noisier repeat" }),
    ]);
    assert.ok(folded[0] !== undefined);
    expect(folded[0].status).toBe("completed");
    expect(folded[0].result).toBe("the actual joke");
  });

  it("keeps a paused run out of the active count and resumable", () => {
    const tracker = new NativeActivityTracker();
    const folded = tracker.derive([
      event("wf", "unknown", 1000, { scope: "batch" }),
      event("wf", "running", 2000, { scope: "batch" }),
      event("wf", "paused", 3000, { scope: "batch" }),
    ]);
    assert.ok(folded[0] !== undefined);
    expect(folded[0].status).toBe("paused");
    expect(nativeActivityStatus(folded[0], false)).toBe("Paused");
    // A later running observation resumes the same orchestration.
    const resumed = tracker.derive([
      event("wf", "unknown", 1000, { scope: "batch" }),
      event("wf", "running", 2000, { scope: "batch" }),
      event("wf", "paused", 3000, { scope: "batch" }),
      event("wf", "running", 4000, { scope: "batch" }),
    ]);
    assert.ok(resumed[0] !== undefined);
    expect(resumed[0].status).toBe("running");
  });

  it("resets on history replacement and ignores uncorrelatable observations", () => {
    const tracker = new NativeActivityTracker();
    tracker.derive([bgEvent("a", "running", 1, {})]);
    expect(tracker.derive([event("b", "completed", 2, {}), event("", "running", 3, {})]).map((s) => s.id)).toEqual([
      "b",
    ]);
    expect(tracker.derive([])).toEqual([]);
  });
});

function group(key: string, ts: number): MsgItem {
  const messageGroup: MessageGroup = {
    kind: "text",
    events: [{ kind: "text", ts, text: { text: "content" } }],
    toolCalls: [],
  };
  return { kind: "group", group: messageGroup, isLive: false, key };
}

function elidedTurn(key: string, ts: number): MsgItem {
  return {
    kind: "elided",
    key,
    turn: {
      groups: [{ kind: "text", events: [{ kind: "text", ts, text: { text: "content" } }], toolCalls: [] }],
      toolCount: 0,
      textCount: 0,
      durationMs: 0,
    },
  };
}

function activity(id: string, anchorTs: number): NativeActivity {
  return { id, status: "running", startedAt: null, endedAt: null, anchorTs };
}

function sessionHeader(key: string): MsgItem {
  return {
    kind: "sessionBoundary",
    key,
    event: {
      kind: "init",
      ts: 0,
      init: { sessionID: "s", reportedModel: "m", agentVersion: "1", cwd: "/", harness: "claude" },
    },
  };
}

describe("assignNativeAnchors", () => {
  it("anchors each card to the content item it last changed in", () => {
    const items = [group("first", 100), group("second", 200)];
    const byAnchor = assignNativeAnchors(items, [activity("old", 150), activity("new", 250)]);
    expect(byAnchor.get("first")?.map((a) => a.id)).toEqual(["old"]);
    expect(byAnchor.get("second")?.map((a) => a.id)).toEqual(["new"]);
  });

  it("falls back to the first item for an activity with no timestamp", () => {
    const items = [group("first", 100), group("second", 200)];
    const byAnchor = assignNativeAnchors(items, [activity("early", 0)]);
    expect(byAnchor.get("first")?.map((a) => a.id)).toEqual(["early"]);
  });

  it("anchors to a collapsed turn that represents the settled content", () => {
    const byAnchor = assignNativeAnchors([elidedTurn("turn", 300)], [activity("settled", 250)]);
    expect(byAnchor.get("turn")?.map((a) => a.id)).toEqual(["settled"]);
  });

  it("ignores items that only introduce following content", () => {
    const items: MsgItem[] = [sessionHeader("session"), group("content", 100)];
    const byAnchor = assignNativeAnchors(items, [activity("a", 50)]);
    expect([...byAnchor.keys()]).toEqual(["content"]);
  });

  it("assigns nothing when no item can anchor content", () => {
    const byAnchor = assignNativeAnchors([sessionHeader("session")], [activity("a", 1)]);
    expect(byAnchor.size).toBe(0);
  });
});

function bgEvent(
  id: string,
  status: EventBackgroundCommand["status"],
  ts: number,
  fields: Partial<EventBackgroundCommand>,
): EventMessage {
  return {
    kind: "backgroundCommand",
    ts,
    backgroundCommand: { id, status, ...fields },
  };
}

describe("BackgroundCommandTracker", () => {
  it("folds concurrent commands across a result and later notifications", () => {
    const tracker = new BackgroundCommandTracker();
    const messages: EventMessage[] = [
      bgEvent("cmd-a", "running", 1000, { label: "Install and run lint", toolUseID: "t1" }),
      bgEvent("cmd-b", "running", 1010, { label: "Wait for lint", toolUseID: "t2" }),
      { kind: "result", ts: 1100 },
      bgEvent("cmd-a", "completed", 1200, { exitCode: 0, result: "completed (exit code 0)" }),
    ];
    const folded = tracker.derive(messages);
    expect(folded).toHaveLength(2);
    expect(folded[0]).toMatchObject({
      id: "cmd-a",
      status: "completed",
      exitCode: 0,
      toolUseID: "t1",
      startedAt: 1000,
      endedAt: 1200,
      anchorTs: 1200,
    });
    expect(folded[1]).toMatchObject({ id: "cmd-b", status: "running", endedAt: null });
    const restored = new BackgroundCommandTracker().derive(messages.map((ev) => ({ ...ev })));
    expect(restored).toEqual(folded);
    expect(tracker.derive(messages)).toBe(folded);
  });

  it("keeps a running card at its spawn and repositions it once when it settles", () => {
    const tracker = new BackgroundCommandTracker();
    const spawn = bgEvent("cmd", "running", 100, {});
    const repeat = bgEvent("cmd", "running", 500, {});
    const done = bgEvent("cmd", "completed", 900, { exitCode: 0 });
    const spawnActivity = tracker.derive([spawn])[0];
    assert.ok(spawnActivity !== undefined);
    expect(spawnActivity.anchorTs).toBe(100);
    const runningActivity = tracker.derive([spawn, repeat])[0];
    assert.ok(runningActivity !== undefined);
    expect(runningActivity.anchorTs).toBe(100);
    const completedActivity = tracker.derive([spawn, repeat, done])[0];
    assert.ok(completedActivity !== undefined);
    expect(completedActivity.anchorTs).toBe(900);
  });

  it("never invents a terminal outcome when the parent settles first", () => {
    const tracker = new BackgroundCommandTracker();
    const folded = tracker.derive([bgEvent("cmd", "running", 1, {}), { kind: "result", ts: 3 } as EventMessage]);
    assert.ok(folded[0] !== undefined);
    expect(folded[0].status).toBe("running");
    expect(backgroundCommandStatus(folded[0], true)).toBe("Last observed running · outcome unknown");
    expect(backgroundCommandStatus(folded[0], false)).toBe("Running");
  });

  it("freezes a terminal lifecycle and lets the first terminal result stand", () => {
    const tracker = new BackgroundCommandTracker();
    const folded = tracker.derive([
      bgEvent("cmd", "running", 1000, {}),
      bgEvent("cmd", "completed", 2000, { result: "completed (exit code 0)", exitCode: 0 }),
      bgEvent("cmd", "failed", 3000, { result: "a noisier repeat" }),
      bgEvent("cmd", "running", 4000, {}),
    ]);
    expect(folded[0]).toMatchObject({ status: "completed", result: "completed (exit code 0)", exitCode: 0 });
  });

  it("upgrades an interim result with the terminal one", () => {
    const tracker = new BackgroundCommandTracker();
    const folded = tracker.derive([
      bgEvent("cmd", "running", 1000, { result: "started" }),
      bgEvent("cmd", "completed", 2000, { result: "completed (exit code 0)" }),
    ]);
    assert.ok(folded[0] !== undefined);
    expect(folded[0].result).toBe("completed (exit code 0)");
  });

  it("carries toolUseID, label, and outputRef from whichever observation reported them", () => {
    const tracker = new BackgroundCommandTracker();
    const folded = tracker.derive([
      bgEvent("cmd", "running", 1000, { toolUseID: "t1", label: "Re-run lint" }),
      bgEvent("cmd", "completed", 2000, { outputRef: "/tmp/tasks/cmd.output" }),
    ]);
    expect(folded[0]).toMatchObject({ toolUseID: "t1", label: "Re-run lint", outputRef: "/tmp/tasks/cmd.output" });
  });

  it("resets on history replacement and ignores uncorrelatable observations", () => {
    const tracker = new BackgroundCommandTracker();
    tracker.derive([bgEvent("a", "running", 1, {})]);
    expect(
      tracker.derive([bgEvent("b", "completed", 2, { exitCode: 1 }), bgEvent("", "running", 3, {})]).map((s) => s.id),
    ).toEqual(["b"]);
    expect(tracker.derive([])).toEqual([]);
  });
});

function command(anchorTs: number): BackgroundCommandActivity {
  return { id: "cmd", status: "running", startedAt: null, endedAt: null, anchorTs };
}

describe("assignNativeAnchors for background commands", () => {
  it("anchors a running card at its spawn and a settled card where it settled", () => {
    const items = [group("spawn", 100), group("settle", 200)];
    const byAnchor = assignNativeAnchors(items, [command(150), { ...command(250), status: "completed" }]);
    expect(byAnchor.get("spawn")).toHaveLength(1);
    expect(byAnchor.get("settle")).toHaveLength(1);
  });
});

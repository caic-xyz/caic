// Tests canonical native lifecycle folding, replay identity, timing, and partial observability.

import { describe, it } from "node:test";
import { expect } from "@tests/expect";
import type { EventMessage, EventNativeSubagent } from "@sdk/types.gen";
import type { MessageGroup, MsgItem } from "./grouping";
import {
  NativeActivityTracker,
  assignNativeAnchors,
  nativeActivityStatus,
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
    expect(tracker.derive([spawn])[0].anchorTs).toBe(100);
    // A running heartbeat must not drag an active card toward newer content.
    expect(tracker.derive([spawn, heartbeat])[0].anchorTs).toBe(100);
    expect(tracker.derive([spawn, heartbeat, done])[0].anchorTs).toBe(900);
  });

  it("does not infer running or success from metadata, parent result, or settled state", () => {
    const tracker = new NativeActivityTracker();
    const folded = tracker.derive([
      event("partial", "unknown", 1, {}),
      event("unfinished", "running", 2, {}),
      { kind: "result", ts: 3 },
    ]);
    expect(folded[0].startedAt).toBeNull();
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
    expect(folded[0].status).toBe("paused");
    expect(nativeActivityStatus(folded[0], false)).toBe("Paused");
    // A later running observation resumes the same orchestration.
    const resumed = tracker.derive([
      event("wf", "unknown", 1000, { scope: "batch" }),
      event("wf", "running", 2000, { scope: "batch" }),
      event("wf", "paused", 3000, { scope: "batch" }),
      event("wf", "running", 4000, { scope: "batch" }),
    ]);
    expect(resumed[0].status).toBe("running");
  });

  it("resets on history replacement and ignores uncorrelatable observations", () => {
    const tracker = new NativeActivityTracker();
    tracker.derive([event("a", "running", 1, {})]);
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
    kind: "sessionHeader",
    session: { turns: [], toolCount: 0, textCount: 0, durationMs: 0 },
    sessionKey: key,
    key,
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

// Tests canonical native lifecycle folding, replay identity, timing, and partial observability.

import { describe, expect, it } from "vitest";
import type { EventMessage, EventNativeSubagent } from "@sdk/types.gen";
import { NativeActivityTracker, nativeActivityStatus } from "./nativeSubagents";

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
    expect(
      tracker
        .derive([event("b", "completed", 2, {}), event("", "running", 3, {})])
        .map((s) => s.id),
    ).toEqual(["b"]);
    expect(tracker.derive([])).toEqual([]);
  });
});

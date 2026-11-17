// Tests that a turn timeline refreshes on task-stream changes only while it can still change, one request at a time and spaced, keeping unchanged spans, catches a completion its reads missed, and backs off failed refreshes.

import assert from "node:assert/strict";
import { afterEach, describe, it } from "node:test";
import { createRoot, createSignal } from "solid-js";
import { vi } from "@tests/expect";
import { historyManifest } from "@tests/history";

import type { HistoryManifest, HistoryTurnSpan, HistoryTurnTimeline, ISOTimestamp } from "@sdk/types.gen";
import { createTurnTimelineLoad, liveRefreshIntervalMs, maxRetryDelayMs, retryDelayMs } from "./turnTimelineLoad";

afterEach(() => vi.useRealTimers());

function timeline(spans: HistoryTurnSpan[], ended: boolean): HistoryTurnTimeline {
  return {
    number: 2,
    startedAt: "2026-01-01T00:00:00Z" as ISOTimestamp,
    endedAt: ended ? ("2026-01-01T00:00:30Z" as ISOTimestamp) : undefined,
    durationMs: 30_000,
    modelMs: 0,
    spans,
    omitted: [],
    calls: [],
    tools: [],
  };
}

const settle = () => vi.advanceTimersByTimeAsync(0);

// A response is a timeline, a pending timeline, or a failure.
type Response = HistoryTurnTimeline | Promise<HistoryTurnTimeline> | Error;

function setup(responses: Response[]) {
  return createRoot((dispose) => {
    const [manifest, setManifest] = createSignal<HistoryManifest | null>(historyManifest([], ""));
    const [through, setThrough] = createSignal(1);
    const [turn, setTurn] = createSignal({ number: 2, running: true });
    const requests: number[] = [];
    const load = createTurnTimelineLoad(
      {
        manifest,
        through,
        readTurn: async (number) => {
          requests.push(number);
          const next = responses.shift();
          if (!next) throw new Error("unexpected timeline request");
          if (next instanceof Error) throw next;
          return next;
        },
      },
      () => turn().number,
      () => turn().running,
    );
    // Each change replaces the turn object, as a manifest update does.
    const setRunning = (running: boolean) => setTurn({ number: 2, running });
    return { load, requests, setManifest, setThrough, setRunning, dispose };
  });
}

describe("createTurnTimelineLoad", () => {
  it("refreshes a running turn on checkpoints, spaced and only while it changes, then once at completion", async () => {
    vi.useFakeTimers();
    const tool: HistoryTurnSpan = { kind: "tool", startMs: 0, endMs: 1_000, label: "Bash", status: "completed" };
    const edit: HistoryTurnSpan = { kind: "tool", startMs: 2_000, label: "Edit", status: "running" };
    const { load, requests, setManifest, setThrough, setRunning, dispose } = setup([
      timeline([tool], false),
      timeline([tool, edit], false),
      timeline([tool, { ...edit, endMs: 3_000, status: "completed" }], false),
      timeline([tool, { ...edit, endMs: 3_000, status: "completed" }], true),
    ]);
    try {
      await settle();
      assert.equal(load.state(), "ready");
      assert.deepEqual(requests, [2]);
      const first = load.timeline()?.spans?.[0];

      setThrough(2);
      await settle();
      assert.equal(requests.length, 2);
      assert.equal(load.timeline()?.spans?.length, 2);
      // The unchanged span keeps its identity, so its element keeps focus.
      assert.equal(load.timeline()?.spans?.[0], first);

      // Checkpoints within the interval coalesce into one later refresh.
      setThrough(3);
      setThrough(4);
      await settle();
      assert.equal(requests.length, 2);
      await vi.advanceTimersByTimeAsync(liveRefreshIntervalMs);
      assert.equal(requests.length, 3);
      assert.equal(load.timeline()?.spans?.[1]?.status, "completed");

      // Nothing changes, so nothing is requested.
      await vi.advanceTimersByTimeAsync(10 * liveRefreshIntervalMs);
      assert.equal(requests.length, 3);

      setRunning(false);
      await vi.advanceTimersByTimeAsync(liveRefreshIntervalMs);
      assert.equal(requests.length, 4);
      assert.ok(load.timeline()?.endedAt);
      // A new object for the same completed turn, such as a repriced one,
      // neither reloads nor refreshes it.
      setRunning(false);
      await settle();
      assert.equal(load.state(), "ready");
      assert.equal(requests.length, 4);

      // A completed turn without running work ignores later task changes.
      setThrough(5);
      setManifest(historyManifest([], ""));
      await vi.advanceTimersByTimeAsync(10 * liveRefreshIntervalMs);
      assert.equal(requests.length, 4);
    } finally {
      dispose();
    }
  });

  it("refreshes a completed turn on manifest changes while a command it started runs", async () => {
    vi.useFakeTimers();
    const command: HistoryTurnSpan = { kind: "background", startMs: 1_000, label: "npm run dev", status: "running" };
    const { load, requests, setManifest, setThrough, setRunning, dispose } = setup([
      timeline([command], true),
      timeline([{ ...command, endMs: 90_000, status: "completed" }], true),
    ]);
    setRunning(false);
    try {
      await settle();
      assert.deepEqual(requests, [2]);

      // Records alone do not change a completed turn.
      setThrough(9);
      await vi.advanceTimersByTimeAsync(liveRefreshIntervalMs);
      assert.equal(requests.length, 1);

      setManifest(historyManifest([], ""));
      await settle();
      assert.equal(requests.length, 2);
      assert.equal(load.timeline()?.spans?.[0]?.status, "completed");

      setManifest(historyManifest([], ""));
      await vi.advanceTimersByTimeAsync(10 * liveRefreshIntervalMs);
      assert.equal(requests.length, 2);
    } finally {
      dispose();
    }
  });

  it("refreshes a turn that completes while its first load is pending", async () => {
    vi.useFakeTimers();
    let resolve: (timeline: HistoryTurnTimeline) => void = () => {};
    const pending = new Promise<HistoryTurnTimeline>((done) => {
      resolve = done;
    });
    const { load, requests, setRunning, dispose } = setup([pending, timeline([], true)]);
    try {
      await settle();
      assert.equal(load.state(), "loading");
      setRunning(false);
      resolve(timeline([], false));
      await settle();
      assert.equal(load.state(), "ready");
      assert.equal(requests.length, 2);
      assert.ok(load.timeline()?.endedAt);

      await vi.advanceTimersByTimeAsync(10 * liveRefreshIntervalMs);
      assert.equal(requests.length, 2);
    } finally {
      dispose();
    }
  });

  it("retries a failed refresh until the completed turn's timeline ends", async () => {
    vi.useFakeTimers();
    const { load, requests, setRunning, dispose } = setup([
      timeline([], false),
      new Error("network"),
      timeline([], true),
    ]);
    try {
      await settle();
      assert.deepEqual(requests, [2]);
      setRunning(false);
      await settle();
      assert.equal(requests.length, 2);
      assert.equal(load.timeline()?.endedAt, undefined);

      // The retry keeps the refresh spacing.
      await vi.advanceTimersByTimeAsync(liveRefreshIntervalMs - 1);
      assert.equal(requests.length, 2);
      await vi.advanceTimersByTimeAsync(1);
      assert.equal(requests.length, 3);
      assert.ok(load.timeline()?.endedAt);

      await vi.advanceTimersByTimeAsync(10 * liveRefreshIntervalMs);
      assert.equal(requests.length, 3);
    } finally {
      dispose();
    }
  });

  it("backs off failed refreshes from 1 s to 30 s, shows the error, and resets after a success", async () => {
    vi.useFakeTimers();
    assert.deepEqual(
      [1, 2, 3, 4, 5, 6, 7, 8].map(retryDelayMs),
      [1_000, 2_000, 4_000, 8_000, 16_000, 30_000, 30_000, 30_000],
    );
    const failure = new Error("server down");
    const { load, requests, setRunning, dispose } = setup([
      timeline([], false),
      ...Array.from({ length: 7 }, () => failure),
      timeline([], false),
      failure,
      timeline([], true),
    ]);
    try {
      await settle();
      setRunning(false);
      await settle();
      assert.equal(requests.length, 2);
      // The failed refresh shows the error and keeps the timeline.
      assert.equal(load.state(), "error");
      assert.equal(load.message(), "server down");
      assert.ok(load.timeline());

      // Each retry waits twice as long as the previous one, up to the cap.
      for (const wait of [1_000, 2_000, 4_000, 8_000, 16_000, maxRetryDelayMs]) {
        const before: number = requests.length;
        await vi.advanceTimersByTimeAsync(wait - 1);
        assert.equal(requests.length, before, `retry before ${wait} ms`);
        await vi.advanceTimersByTimeAsync(1);
        assert.equal(requests.length, before + 1, `retry after ${wait} ms`);
      }
      assert.equal(requests.length, 8);

      // A manual retry fires at once and its success clears the error.
      load.retry();
      await settle();
      assert.equal(requests.length, 9);
      assert.equal(load.state(), "ready");

      // The timeline still lacks its end; after the success the backoff
      // restarts at 1 s.
      await vi.advanceTimersByTimeAsync(liveRefreshIntervalMs);
      assert.equal(requests.length, 10);
      assert.equal(load.state(), "error");
      await vi.advanceTimersByTimeAsync(1_000);
      assert.equal(requests.length, 11);
      assert.equal(load.state(), "ready");
      assert.ok(load.timeline()?.endedAt);

      await vi.advanceTimersByTimeAsync(10 * maxRetryDelayMs);
      assert.equal(requests.length, 11);
    } finally {
      dispose();
    }
  });
});

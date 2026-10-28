// Tests disk-history SSE replay buffering, native retry ownership, and lifecycle cleanup.

import assert from "node:assert/strict";
import { afterEach, beforeEach, describe, it } from "node:test";
import { createRoot, createSignal } from "solid-js";
import { expect, vi } from "@tests/expect";

import type { TaskEventsHandlers } from "@sdk/api.gen";
import type { EventMessage } from "@sdk/types.gen";

import { api } from "./api";
import { createTaskEventTimeline } from "./taskEventTimeline";

const taskEventStreamMock = vi.spyOn(api, "taskEvents");

interface FakeEventSource {
  close: ReturnType<typeof vi.fn>;
  onerror: ((event: Event) => void) | null;
}

interface Connection {
  handlers: TaskEventsHandlers;
  source: FakeEventSource;
}

function captureConnections(): Connection[] {
  const connections: Connection[] = [];
  taskEventStreamMock.mockImplementation((_id: string, _backward: string, handlers: TaskEventsHandlers) => {
    const source: FakeEventSource = { close: vi.fn(), onerror: null };
    connections.push({ handlers, source });
    return source as unknown as EventSource;
  });
  return connections;
}

function textEvent(text: string, ts: number): EventMessage {
  return { kind: "textDelta", ts, textDelta: { text } };
}

describe("createTaskEventTimeline", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    taskEventStreamMock.mockReset();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("publishes replay atomically, batches live deltas, and replaces stale history", () => {
    const connections = captureConnections();
    const timeline = createRoot((dispose) => ({
      dispose,
      ...createTaskEventTimeline({
        taskId: () => "task",
        taskState: () => "stopped",
      }),
    }));
    assert.ok(connections[0] !== undefined);
    const handlers = connections[0].handlers;

    handlers.onMessage(textEvent("history", 1));
    expect(timeline.messages()).toEqual([]);
    handlers.onReady?.();
    expect(timeline.messages()).toEqual([textEvent("history", 1)]);
    const epoch = timeline.epoch();

    handlers.onMessage(textEvent(" live", 2));
    expect(timeline.messages()).toHaveLength(1);
    vi.advanceTimersByTime(100);
    expect(timeline.messages()).toEqual([textEvent("history", 1), textEvent(" live", 2)]);

    expect(timeline.epoch()).toBe(epoch);
    handlers.onReset?.();
    handlers.onMessage(textEvent("replacement", 3));
    handlers.onReady?.();
    expect(timeline.messages()).toEqual([textEvent("replacement", 3)]);
    expect(timeline.epoch()).toBeGreaterThan(epoch);
    timeline.dispose();
  });

  it("renders disk previews without opening a tail connection", () => {
    const connections = captureConnections();
    const timeline = createRoot((dispose) => ({
      dispose,
      ...createTaskEventTimeline({ taskId: () => "task", taskState: () => "purged" }),
    }));
    const handlers = connections[0]?.handlers;
    assert.ok(handlers !== undefined);
    const preview = [textEvent("recent", 2), textEvent("latest", 3)];
    handlers.onBackward?.({ messages: preview, eventId: "" });
    expect(timeline.messages()).toEqual([]);
    expect(timeline.preview()).toEqual({ prompt: "", response: "recentlatest" });
    expect(connections).toHaveLength(1);
    handlers.onMessage(textEvent("canonical", 1));
    expect(timeline.messages()).toEqual([]);
    expect(timeline.preview()).toEqual({ prompt: "", response: "recentlatest" });
    handlers.onReady?.();
    expect(timeline.messages()).toEqual([textEvent("canonical", 1)]);
    expect(timeline.preview()).toBeNull();
    expect(timeline.loading()).toBe(false);
    timeline.dispose();
  });

  it("owns resource replacement, decoder recovery, and disk-native zero pricing", async () => {
    const connections = captureConnections();
    const timeline = createRoot((dispose) => ({
      dispose,
      ...createTaskEventTimeline({ taskId: () => "task", taskState: () => "stopped" }),
    }));
    try {
      const handlers = connections[0]?.handlers;
      assert.ok(handlers);
      const sample = {
        ts: 1,
        cpuPerc: 0,
        memUsed: 0,
        memLimit: 0,
        memPerc: 0,
        netRx: 0,
        netTx: 0,
        blockRead: 0,
        blockWrite: 0,
        diskUsed: -1,
      };
      handlers.onStatsSnapshot?.({ samples: [sample] });
      handlers.onStats?.({ ...sample, diskUsed: 20 });
      assert.equal(timeline.stats()[0]?.diskUsed, 20);
      handlers.onStatsSnapshot?.({ samples: [{ ...sample, ts: 2 }] });
      assert.deepEqual(
        timeline.stats().map((value) => value.ts),
        [2],
      );
      handlers.onMessage({
        kind: "result",
        ts: 1,
        result: {
          subtype: "success",
          isError: false,
          result: "done",
          duration: 1,
          durationAPI: 1,
          numTurns: 1,
          totalCostUSD: 0,
          cost: { usd: 0, source: "reported" },
          usage: {
            inputTokens: 0,
            outputTokens: 0,
            cacheCreationInputTokens: 0,
            cacheReadInputTokens: 0,
            reportedModel: "model",
          },
        },
      });
      handlers.onReady?.();
      const costs = await timeline.readCosts(new AbortController().signal);
      assert.equal(costs[0]?.result, timeline.messages()[0]?.result);
      assert.equal(costs[0]?.cost?.usd, 0);
      handlers.onError(new Error("Invalid HistoryStats.samples"));
      assert.equal(timeline.error(), "Invalid HistoryStats.samples");
      assert.equal(connections[0]?.source.close.mock.calls.length, 1);
      timeline.retry();
      connections[1]?.handlers.onStatsSnapshot?.({ samples: [] });
      connections[1]?.handlers.onReady?.();
      assert.equal(timeline.error(), null);
      assert.deepEqual(timeline.stats(), []);
    } finally {
      timeline.dispose();
    }
  });

  it("leaves visibility, connectivity, and nonterminal retries to EventSource", () => {
    const connections = captureConnections();
    const dispose = createRoot((rootDispose) => {
      createTaskEventTimeline({
        taskId: () => "task",
        taskState: () => "stopped",
      });
      return rootDispose;
    });
    const connection = connections[0];
    assert.ok(connection !== undefined);
    connection.handlers.onReady?.();
    if (!connection.source.onerror) throw new Error("native error handler not registered");

    document.dispatchEvent(new Event("visibilitychange"));
    window.dispatchEvent(new Event("offline"));
    window.dispatchEvent(new Event("online"));
    connection.source.onerror(new Event("error"));

    expect(connections).toHaveLength(1);
    expect(connection.source.close).not.toHaveBeenCalled();
    dispose();
    expect(connection.source.close).toHaveBeenCalledOnce();
  });

  it("closes completed terminal streams and reconnects when the task becomes stopped", () => {
    const connections = captureConnections();
    const owner = createRoot((dispose) => {
      const [state, setState] = createSignal("stopped");
      createTaskEventTimeline({
        taskId: () => "task",
        taskState: state,
      });
      return { dispose, setState };
    });

    owner.setState("failed");
    assert.ok(connections[0] !== undefined);
    expect(connections[0].source.close).toHaveBeenCalledOnce();
    const terminalConnection = connections[1];
    assert.ok(terminalConnection !== undefined);
    if (!terminalConnection.source.onerror) throw new Error("native error handler not registered");

    terminalConnection.source.onerror(new Event("error"));
    expect(terminalConnection.source.close).not.toHaveBeenCalled();

    terminalConnection.handlers.onReady?.();
    terminalConnection.source.onerror(new Event("error"));
    expect(terminalConnection.source.close).toHaveBeenCalledOnce();

    owner.setState("stopped");
    expect(connections).toHaveLength(3);
    assert.ok(connections[2] !== undefined);
    expect(connections[2].source.close).not.toHaveBeenCalled();
    owner.dispose();
  });

  it("reports terminal history failure and explicitly retries without accepting stale callbacks", () => {
    const connections = captureConnections();
    const owner = createRoot((dispose) => ({
      dispose,
      ...createTaskEventTimeline({ taskId: () => "task", taskState: () => "purged" }),
    }));
    const first = connections[0];
    assert.ok(first);
    expect(owner.loading()).toBe(true);
    first.handlers.onHistoryError?.({ message: "unavailable" });
    expect(first.source.close).toHaveBeenCalledOnce();
    expect(owner.error()).toBe("unavailable");
    expect(owner.loading()).toBe(false);
    owner.retry();
    expect(owner.error()).toBeNull();
    expect(owner.loading()).toBe(true);
    const next = connections[1];
    assert.ok(next);
    first.handlers.onMessage(textEvent("stale", 1));
    next.handlers.onMessage(textEvent("recovered", 2));
    expect(owner.messages()).toEqual([]);
    next.handlers.onReady?.();
    expect(owner.messages()).toEqual([textEvent("recovered", 2)]);
    expect(owner.loading()).toBe(false);
    owner.dispose();
  });

  it("replaces the connection and ignores stale callbacks when the task changes", () => {
    const connections = captureConnections();
    const owner = createRoot((dispose) => {
      const [taskId, setTaskId] = createSignal("first");
      return {
        dispose,
        setTaskId,
        ...createTaskEventTimeline({
          taskId,
          taskState: () => "stopped",
        }),
      };
    });
    const first = connections[0];
    assert.ok(first !== undefined);
    first.handlers.onMessage(textEvent("first", 1));
    first.handlers.onReady?.();

    owner.setTaskId("second");
    expect(first.source.close).toHaveBeenCalledOnce();
    expect(connections).toHaveLength(2);
    expect(owner.messages()).toEqual([]);

    first.handlers.onMessage(textEvent("stale", 2));
    assert.ok(connections[1] !== undefined);
    connections[1].handlers.onMessage(textEvent("second", 3));
    connections[1].handlers.onReady?.();
    expect(owner.messages()).toEqual([textEvent("second", 3)]);
    owner.dispose();
  });
});

// Focused tests for task SSE replay buffering, native retry ownership, and cleanup.

import { createRoot, createSignal } from "solid-js";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { TaskEventsHandlers } from "@sdk/api.gen";
import type { EventMessage } from "@sdk/types.gen";

const { taskEventStreamMock } = vi.hoisted(() => ({
  taskEventStreamMock: vi.fn(),
}));

vi.mock("./api", () => ({
  taskEventStream: taskEventStreamMock,
}));

import { createTaskEventTimeline } from "./taskEventTimeline";

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
  taskEventStreamMock.mockImplementation((_id: string, handlers: TaskEventsHandlers) => {
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
        taskState: () => "running",
        onError: vi.fn(),
      }),
    }));
    const handlers = connections[0].handlers;

    handlers.onMessage(textEvent("history", 1));
    expect(timeline.messages()).toEqual([]);
    handlers.onReady?.();
    expect(timeline.messages()).toEqual([textEvent("history", 1)]);
    expect(timeline.epoch()).toBe(1);

    handlers.onMessage(textEvent(" live", 2));
    expect(timeline.messages()).toHaveLength(1);
    vi.advanceTimersByTime(100);
    expect(timeline.messages()).toEqual([textEvent("history", 1), textEvent(" live", 2)]);

    handlers.onReset?.();
    handlers.onMessage(textEvent("replacement", 3));
    handlers.onReady?.();
    expect(timeline.messages()).toEqual([textEvent("replacement", 3)]);
    expect(timeline.epoch()).toBe(2);
    timeline.dispose();
  });

  it("leaves visibility, connectivity, and nonterminal retries to EventSource", () => {
    const connections = captureConnections();
    const dispose = createRoot((rootDispose) => {
      createTaskEventTimeline({
        taskId: () => "task",
        taskState: () => "running",
        onError: vi.fn(),
      });
      return rootDispose;
    });
    const connection = connections[0];
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

  it("closes completed terminal streams and reconnects when the task restarts", () => {
    const connections = captureConnections();
    const owner = createRoot((dispose) => {
      const [state, setState] = createSignal("running");
      createTaskEventTimeline({
        taskId: () => "task",
        taskState: state,
        onError: vi.fn(),
      });
      return { dispose, setState };
    });

    owner.setState("failed");
    expect(connections[0].source.close).toHaveBeenCalledOnce();
    const terminalConnection = connections[1];
    if (!terminalConnection.source.onerror) throw new Error("native error handler not registered");

    terminalConnection.source.onerror(new Event("error"));
    expect(terminalConnection.source.close).not.toHaveBeenCalled();

    terminalConnection.handlers.onReady?.();
    terminalConnection.source.onerror(new Event("error"));
    expect(terminalConnection.source.close).toHaveBeenCalledOnce();

    owner.setState("running");
    expect(connections).toHaveLength(3);
    expect(connections[2].source.close).not.toHaveBeenCalled();
    owner.dispose();
  });

  it("closes and reports terminal history failures", () => {
    const connections = captureConnections();
    const onError = vi.fn();
    const dispose = createRoot((rootDispose) => {
      createTaskEventTimeline({
        taskId: () => "task",
        taskState: () => "running",
        onError,
      });
      return rootDispose;
    });
    connections[0].handlers.onHistoryError?.({ message: "unavailable" });

    expect(connections[0].source.close).toHaveBeenCalledOnce();
    expect(onError).toHaveBeenCalledWith("Task history error: unavailable");
    dispose();
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
          taskState: () => "running",
          onError: vi.fn(),
        }),
      };
    });
    const first = connections[0];
    first.handlers.onMessage(textEvent("first", 1));
    first.handlers.onReady?.();

    owner.setTaskId("second");
    expect(first.source.close).toHaveBeenCalledOnce();
    expect(connections).toHaveLength(2);
    expect(owner.messages()).toEqual([]);

    first.handlers.onMessage(textEvent("stale", 2));
    connections[1].handlers.onMessage(textEvent("second", 3));
    connections[1].handlers.onReady?.();
    expect(owner.messages()).toEqual([textEvent("second", 3)]);
    owner.dispose();
  });
});

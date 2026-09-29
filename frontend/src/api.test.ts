// Tests task SSE API parsing for history errors and resume resets.

import { beforeEach, describe, it } from "node:test";
import { expect, vi } from "@tests/expect";

import { api, taskEventStream } from "./api";

let errorListener: EventListener | undefined;
let backwardListener: EventListener | undefined;
let resetListener: EventListener | undefined;
let eventSourceURL = "";

class FakeEventSource {
  readonly close = vi.fn();

  constructor(url: string) {
    eventSourceURL = url;
  }

  addEventListener(type: string, listener: EventListener) {
    if (type === "error") errorListener = listener;
    if (type === "backward") backwardListener = listener;
    if (type === "reset") resetListener = listener;
  }
}

describe("taskEventStream", () => {
  beforeEach(() => {
    errorListener = undefined;
    backwardListener = undefined;
    resetListener = undefined;
    eventSourceURL = "";
    vi.stubGlobal("EventSource", FakeEventSource);
  });

  it("ignores native connection failures without error data", () => {
    const onError = vi.fn();
    const onHistoryError = vi.fn();
    taskEventStream("task", { onMessage: vi.fn(), onError, onHistoryError });

    if (!errorListener) throw new Error("history error listener not registered");
    errorListener(new Event("error"));

    expect(onHistoryError).not.toHaveBeenCalled();
    expect(onError).not.toHaveBeenCalled();
  });

  it("reports server-requested timeline resets", () => {
    const onReset = vi.fn();
    taskEventStream("task", { onMessage: vi.fn(), onError: vi.fn(), onReset });

    if (!resetListener) throw new Error("reset listener not registered");
    resetListener(new MessageEvent("reset", { data: "{}" }));

    expect(onReset).toHaveBeenCalledOnce();
  });

  it("validates backward-loading boundaries independently from canonical messages", () => {
    const onBackward = vi.fn();
    taskEventStream("task", { onMessage: vi.fn(), onError: vi.fn(), onBackward });

    if (!backwardListener) throw new Error("backward listener not registered");
    backwardListener(
      new MessageEvent("backward", {
        data: '{"message":{"kind":"text","ts":1,"text":{"text":"latest"}},"eventId":"v1/timeline/memory/4/0"}',
      }),
    );

    expect(onBackward).toHaveBeenCalledWith({
      message: { kind: "text", ts: 1, text: { text: "latest" } },
      eventId: "v1/timeline/memory/4/0",
    });
  });

  it("starts a tail stream from the backward event ID", () => {
    api.taskEventStreamFromLastEventID("task/id", "v1/timeline/memory/4/0", {
      onMessage: vi.fn(),
      onError: vi.fn(),
    });

    expect(eventSourceURL).toBe("/api/caic/v1/tasks/task%2Fid/events?last-event-id=v1%2Ftimeline%2Fmemory%2F4%2F0");
  });

  it("validates terminal history error payloads separately from native failures", () => {
    const onError = vi.fn();
    const onHistoryError = vi.fn();
    taskEventStream("task", { onMessage: vi.fn(), onError, onHistoryError });

    if (!errorListener) throw new Error("history error listener not registered");
    errorListener(new MessageEvent("error", { data: '{"message":"task history is unavailable"}' }));

    expect(onHistoryError).toHaveBeenCalledWith({ message: "task history is unavailable" });
    expect(onError).not.toHaveBeenCalled();
  });

  it("reports malformed named history errors as validation failures", () => {
    const onError = vi.fn();
    const onHistoryError = vi.fn();
    taskEventStream("task", { onMessage: vi.fn(), onError, onHistoryError });

    if (!errorListener) throw new Error("history error listener not registered");
    errorListener(new MessageEvent("error", { data: '{"message":42}' }));

    expect(onHistoryError).not.toHaveBeenCalled();
    expect(onError).toHaveBeenCalledOnce();
  });
});

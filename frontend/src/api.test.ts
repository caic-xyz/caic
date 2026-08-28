// Tests task SSE API parsing for history errors and resume resets.

import { beforeEach, describe, expect, it, vi } from "vitest";

import { taskEventStream } from "./api";

let errorListener: EventListener | undefined;
let resetListener: EventListener | undefined;

class FakeEventSource {
  readonly close = vi.fn();

  constructor(_url: string) {}

  addEventListener(type: string, listener: EventListener) {
    if (type === "error") errorListener = listener;
    if (type === "reset") resetListener = listener;
  }
}

describe("taskEventStream", () => {
  beforeEach(() => {
    errorListener = undefined;
    resetListener = undefined;
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

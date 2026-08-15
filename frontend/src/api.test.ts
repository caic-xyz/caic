// Tests task SSE API event parsing, including terminal history errors.

import { beforeEach, describe, expect, it, vi } from "vitest";

import { taskEventStream } from "./api";

let errorListener: EventListener | undefined;

class FakeEventSource {
  readonly close = vi.fn();

  constructor(_url: string) {}

  addEventListener(type: string, listener: EventListener) {
    if (type === "error") errorListener = listener;
  }
}

describe("taskEventStream", () => {
  beforeEach(() => {
    vi.stubGlobal("EventSource", FakeEventSource);
  });

  it("ignores native connection failures without error data", () => {
    const onError = vi.fn();
    const onHistoryError = vi.fn();
    taskEventStream("task", vi.fn(), onError, undefined, undefined, onHistoryError);

    if (!errorListener) throw new Error("history error listener not registered");
    errorListener(new Event("error"));

    expect(onHistoryError).not.toHaveBeenCalled();
    expect(onError).not.toHaveBeenCalled();
  });

  it("validates terminal history error payloads separately from native failures", () => {
    const onError = vi.fn();
    const onHistoryError = vi.fn();
    taskEventStream("task", vi.fn(), onError, undefined, undefined, onHistoryError);

    if (!errorListener) throw new Error("history error listener not registered");
    errorListener(new MessageEvent("error", { data: '{"message":"task history is unavailable"}' }));

    expect(onHistoryError).toHaveBeenCalledWith({ message: "task history is unavailable" });
    expect(onError).not.toHaveBeenCalled();
  });
});

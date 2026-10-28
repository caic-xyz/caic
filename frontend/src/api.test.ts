// Tests session-bound CSRF requests and disk-history SSE parsing, previews, resets, and errors.

import { beforeEach, describe, it } from "node:test";
import { expect, vi } from "@tests/expect";

import { api } from "./api";

let errorListener: EventListener | undefined;
let backwardListener: EventListener | undefined;
let resetListener: EventListener | undefined;
let statsSnapshotListener: EventListener | undefined;
let eventSourceURL = "";

it("keeps concurrent logout requests bound to their originating sessions with CSRF protection", async () => {
  const requests: Headers[] = [];
  const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (_input, init) => {
    requests.push(new Headers(init?.headers));
    return Response.json({ status: "ok" });
  });
  try {
    await Promise.all([api.logout("session-a"), api.logout("session-b")]);
    expect(requests.map((headers) => headers.get("X-Caic-Session"))).toEqual(["session-a", "session-b"]);
    expect(requests.map((headers) => headers.get("X-Caic-CSRF"))).toEqual(["1", "1"]);
  } finally {
    fetchMock.mockRestore();
  }
});

class FakeEventSource {
  readonly close = vi.fn();

  constructor(url: string) {
    eventSourceURL = url;
  }

  addEventListener(type: string, listener: EventListener) {
    if (type === "error") errorListener = listener;
    if (type === "backward") backwardListener = listener;
    if (type === "reset") resetListener = listener;
    if (type === "statsSnapshot") statsSnapshotListener = listener;
  }
}

describe("taskEvents", () => {
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
    api.taskEvents("task", "1", { onMessage: vi.fn(), onError, onHistoryError });

    if (!errorListener) throw new Error("history error listener not registered");
    errorListener(new Event("error"));

    expect(onHistoryError).not.toHaveBeenCalled();
    expect(onError).not.toHaveBeenCalled();
  });

  it("reports server-requested timeline resets", () => {
    const onReset = vi.fn();
    api.taskEvents("task", "1", { onMessage: vi.fn(), onError: vi.fn(), onReset });

    if (!resetListener) throw new Error("reset listener not registered");
    resetListener(new MessageEvent("reset", { data: "{}" }));

    expect(onReset).toHaveBeenCalledOnce();
  });

  it("validates backward-loading boundaries independently from canonical messages", () => {
    const onBackward = vi.fn();
    api.taskEvents("task/id", "1", { onMessage: vi.fn(), onError: vi.fn(), onBackward });
    expect(eventSourceURL).toBe("/api/caic/v1/tasks/task%2Fid/events?backward=1");

    if (!backwardListener) throw new Error("backward listener not registered");
    backwardListener(
      new MessageEvent("backward", {
        data: '{"messages":[{"kind":"text","ts":1,"text":{"text":"latest"}}],"eventId":""}',
      }),
    );

    expect(onBackward).toHaveBeenCalledWith({
      messages: [{ kind: "text", ts: 1, text: { text: "latest" } }],
      eventId: "",
    });
  });

  it("validates terminal history error payloads separately from native failures", () => {
    const onError = vi.fn();
    const onHistoryError = vi.fn();
    api.taskEvents("task", "1", { onMessage: vi.fn(), onError, onHistoryError });

    if (!errorListener) throw new Error("history error listener not registered");
    errorListener(new MessageEvent("error", { data: '{"message":"task history is unavailable"}' }));

    expect(onHistoryError).toHaveBeenCalledWith({ message: "task history is unavailable" });
    expect(onError).not.toHaveBeenCalled();
  });

  it("validates resource snapshots through the generated stream decoder", () => {
    const onError = vi.fn();
    const onStatsSnapshot = vi.fn();
    api.taskEvents("task", "1", { onMessage: vi.fn(), onError, onStatsSnapshot });
    if (!statsSnapshotListener) throw new Error("resource snapshot listener not registered");
    statsSnapshotListener(new MessageEvent("statsSnapshot", { data: '{"samples":[]}' }));
    expect(onStatsSnapshot).toHaveBeenCalledWith({ samples: [] });
    statsSnapshotListener(new MessageEvent("statsSnapshot", { data: '{"samples":[{"ts":"broken"}]}' }));
    expect(onError).toHaveBeenCalledOnce();
  });

  it("reports malformed named history errors as validation failures", () => {
    const onError = vi.fn();
    const onHistoryError = vi.fn();
    api.taskEvents("task", "1", { onMessage: vi.fn(), onError, onHistoryError });

    if (!errorListener) throw new Error("history error listener not registered");
    errorListener(new MessageEvent("error", { data: '{"message":42}' }));

    expect(onHistoryError).not.toHaveBeenCalled();
    expect(onError).toHaveBeenCalledOnce();
  });
});

// Tests session-bound CSRF requests and atomic multipart prompt submissions.

import { it } from "node:test";
import { expect, vi } from "@tests/expect";

import type { Prompt } from "@sdk/types.gen";
import { api, type PromptOptions } from "./api";

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

for (const [name, submit] of [
  [
    "create",
    (options: PromptOptions) => api.createTask({ initialPrompt: { text: "look" }, harness: "claude" }, options),
  ],
  ["input", (options: PromptOptions) => api.sendInput("task", { prompt: { text: "look" }, queue: true }, options)],
  ["restart", (options: PromptOptions) => api.restartTask("task", { prompt: { text: "look" } }, options)],
  ["fork", (options: PromptOptions) => api.forkTask("task", { prompt: { text: "look" } }, options)],
] as const) {
  it(`submits ${name} prompt and binary files in one CSRF-protected request`, async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(Response.json({ status: "ok" }));
    try {
      const first = new Blob(["first image"], { type: "image/png" });
      const second = new Blob(["second image"], { type: "image/jpeg" });
      const read = vi.spyOn(first, "arrayBuffer");
      const controller = new AbortController();
      await submit({ images: [first, second], signal: controller.signal });
      expect(fetchMock).toHaveBeenCalledOnce();
      const init = fetchMock.mock.calls[0]?.[1];
      expect(new Headers(init?.headers).get("X-Caic-CSRF")).toBe("1");
      expect(new Headers(init?.headers).has("Content-Type")).toBe(false);
      expect(read).not.toHaveBeenCalled();
      expect(init?.body).toBeInstanceOf(FormData);
      const form = init?.body as FormData;
      const metadata = JSON.parse(String(form.get("request"))) as {
        initialPrompt?: Prompt;
        prompt?: Prompt;
      };
      expect(metadata.initialPrompt ?? metadata.prompt).toEqual({
        text: "look",
        images: [{ url: "cid:image-0" }, { url: "cid:image-1" }],
      });
      expect([...form.keys()]).toEqual(["request", "image-0", "image-1"]);
      expect(await (form.get("image-0") as Blob).text()).toBe("first image");
      expect((form.get("image-1") as Blob).type).toBe("image/jpeg");
      controller.abort();
      expect(init?.signal?.aborted).toBe(true);
    } finally {
      vi.restoreAllMocks();
    }
  });
}

it("rejects a closed draft before dispatch and surfaces server validation errors", async () => {
  const fetchMock = vi
    .spyOn(globalThis, "fetch")
    .mockResolvedValue(
      Response.json(
        { error: { code: "BAD_REQUEST", message: "image content does not match its media type" } },
        { status: 400 },
      ),
    );
  try {
    const controller = new AbortController();
    controller.abort(new Error("Draft closed"));
    await expect(
      api.sendInput("task", { prompt: { text: "look" } }, { images: [new Blob(["x"])], signal: controller.signal }),
    ).rejects.toThrow("Draft closed");
    expect(fetchMock).not.toHaveBeenCalled();
    await expect(
      api.sendInput(
        "task",
        { prompt: { text: "look" } },
        { images: [new Blob(["x"], { type: "image/png" })], signal: new AbortController().signal },
      ),
    ).rejects.toThrow("image content does not match its media type");
  } finally {
    vi.restoreAllMocks();
  }
});

it("keeps text-only submissions as JSON", async () => {
  const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(Response.json({ status: "ok" }));
  try {
    await api.sendInput("task", { prompt: { text: "hello" } }, { images: [], signal: new AbortController().signal });
    expect(fetchMock.mock.calls[0]?.[1]?.body).toBe(JSON.stringify({ prompt: { text: "hello" } }));
    expect(new Headers(fetchMock.mock.calls[0]?.[1]?.headers).get("Content-Type")).toBe("application/json");
  } finally {
    vi.restoreAllMocks();
  }
});

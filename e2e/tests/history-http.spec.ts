// Browser coverage for exact native SSE cursors and live handoff through gated history bootstrap.
import { test, expect, createTaskAPI, waitForTaskState } from "../helpers";
import type { HistoryEvent } from "../../sdk/caic/ts/v1/types.gen";

declare global {
  interface Window {
    historyHTTPState?: { records: number[]; kinds: string[]; checkpoint: string; errors: string[] };
    historyBootstrapState?: { records: number[]; checkpoint: string };
  }
}

test("native raw SSE resumes after bootstrap without replaying its prefix", async ({ api, page }) => {
  const id = await createTaskAPI(api, "history SSE contract");
  await waitForTaskState(api, id, "waiting");
  const b = (await api.getHistoryManifest(id)).bootstrap;
  await page.goto("/");
  await page.evaluate(
    ({ id, cursor }) => {
      const state = { records: [] as number[], kinds: [] as string[], checkpoint: "", errors: [] as string[] };
      window.historyHTTPState = state;
      const source = new EventSource(`/api/caic/v1/tasks/${id}/history/events?cursor=${encodeURIComponent(cursor)}`);
      source.onmessage = (event: MessageEvent<string>) => {
        const message = JSON.parse(event.data) as HistoryEvent;
        state.records.push(message.recordID);
        state.kinds.push(message.message.kind);
      };
      source.addEventListener("checkpoint", (event) => {
        state.checkpoint = (event as MessageEvent<string>).lastEventId;
      });
      source.addEventListener("error", (event) => {
        if (event instanceof MessageEvent) state.errors.push(String(event.data));
      });
      window.addEventListener("pagehide", () => source.close(), { once: true });
    },
    { id, cursor: b.cursor },
  );
  const state = () =>
    page.evaluate(() => {
      const state = window.historyHTTPState;
      if (!state) throw new Error("history stream state missing");
      return state;
    });
  await expect.poll(async () => (await state()).checkpoint).toBe(b.cursor);
  expect((await state()).records).toEqual([]);
  await api.sendInput(id, { prompt: { text: "continue raw SSE" } });
  await waitForTaskState(api, id, "waiting");
  const next = (await api.getHistoryManifest(id)).bootstrap;
  await expect.poll(async () => (await state()).checkpoint).toBe(next.cursor);
  const live = await state();
  expect(live.records.length).toBeGreaterThan(0);
  expect(live.records.every((r) => r > b.through && r <= next.through)).toBe(true);
  expect(live.kinds).toEqual(expect.arrayContaining(["textDelta", "text", "result"]));
  expect(live.errors).toEqual([]);
});

test("live output survives a held current-context body without prefix replay", async ({ api, page, uniquePrompt }) => {
  const id = await createTaskAPI(api, uniquePrompt("held history bootstrap"));
  await waitForTaskState(api, id, "waiting");
  const initial = (await api.getHistoryManifest(id)).bootstrap;
  await page.addInitScript((taskID) => {
    const NativeEventSource = window.EventSource;
    window.historyBootstrapState = { records: [], checkpoint: "" };
    window.EventSource = class extends NativeEventSource {
      constructor(url: string | URL, configuration?: EventSourceInit) {
        super(url, configuration);
        if (String(url).includes(`/tasks/${taskID}/history/events`)) {
          this.addEventListener("message", (event: MessageEvent<string>) => {
            const output = JSON.parse(event.data) as { recordID: number };
            window.historyBootstrapState?.records.push(output.recordID);
          });
          this.addEventListener("checkpoint", (event: MessageEvent<string>) => {
            if (window.historyBootstrapState) window.historyBootstrapState.checkpoint = event.lastEventId;
          });
        }
      }
    };
  }, id);
  let admit: () => void = () => {};
  const held = new Promise<void>((resolve) => {
    admit = resolve;
  });
  let release: () => void = () => {};
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  let reads = 0;
  await page.route(`**/tasks/${id}/history/**/slice/**`, async (route) => {
    reads++;
    const response = await route.fetch();
    admit();
    await gate;
    await route.fulfill({ response });
  });
  try {
    await page.goto(`/task/@${id}`);
    await held;
    const conversation = page.getByRole("region", { name: "Task conversation" });
    await expect(conversation).toHaveAttribute("aria-busy", "true");
    await api.sendInput(id, { prompt: { text: "continue while history body is held" } });
    await waitForTaskState(api, id, "waiting");
    const latest = (await api.getHistoryManifest(id)).bootstrap;
    await expect.poll(() => page.evaluate(() => window.historyBootstrapState?.checkpoint ?? "")).toBe(latest.cursor);
    await expect(conversation.getByText("A SQL query walks into a bar", { exact: false })).toHaveCount(0);
    release();
    await expect(conversation).toHaveAttribute("aria-busy", "false");
    await expect(conversation.getByText("A SQL query walks into a bar", { exact: false })).toHaveCount(1);
    const records = await page.evaluate(() => window.historyBootstrapState?.records ?? []);
    expect(records.every((record) => record > initial.through)).toBe(true);
    expect(reads).toBe(1);
  } finally {
    release();
  }
});

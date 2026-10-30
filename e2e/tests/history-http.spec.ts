// Browser coverage for exact native history SSE handoff without prefix replay.
import { test, expect, createTaskAPI, waitForTaskState } from "../helpers";
import type { HistoryEvent } from "../../sdk/caic/ts/v1/types.gen";

declare global {
  interface Window {
    historyHTTPState?: { records: number[]; kinds: string[]; checkpoint: string; errors: string[] };
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

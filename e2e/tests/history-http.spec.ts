// Browser/API coverage for immutable raw history resources, native HTTP-cache reuse, and exact live handoff.
import { test, expect, createTaskAPI, waitForTaskState, recentHistoryChunk } from "../helpers";
import type { HistoryEvent } from "../../sdk/caic/ts/v1/types.gen";

// Routing disables Chromium's HTTP cache. This suite must exercise the native
// cache, including reuse after the application document and its JS are released.
test.use({ taskPreferences: null });

declare global {
  interface Window {
    historyHTTPState?: { records: number[]; checkpoint: string; errors: string[] };
  }
}

test("raw history API exposes deterministic ranges and independent mutable views", async ({ api }) => {
  const id = await createTaskAPI(api, "history HTTP API contract");
  await waitForTaskState(api, id, "waiting");
  const b = await api.getHistoryBootstrap(id);
  const recent = recentHistoryChunk(b);
  expect(b.recordsPerChunk).toBe(256);
  expect(b.cursor).toMatch(new RegExp(`^${b.projectionVersion}\\.${b.generation}\\.${b.through}\\.end$`));
  const args = [id, b.scope, b.projectionVersion, b.generation, String(recent.first), String(recent.last)] as const;
  const events = await api.getHistoryChunk(...args);
  expect(events.length).toBeGreaterThan(0);
  expect(events.some((e) => e.message.kind === "result")).toBe(true);
  expect(events.every((e) => e.recordID >= recent.first && e.recordID <= recent.last)).toBe(true);
  expect(new Set(events.map((e) => `${e.recordID}:${e.index}`)).size).toBe(events.length);
  const costs = await api.getHistoryCosts(id, b.generation, String(b.through));
  expect(costs.every((c) => events.some((e) => e.recordID === c.recordID && e.index === c.index))).toBe(true);
  await api.sendInput(id, { prompt: { text: "continue history contract" } });
  await waitForTaskState(api, id, "waiting");
  const next = await api.getHistoryBootstrap(id);
  expect(next.generation).toBe(b.generation);
  expect(next.through).toBeGreaterThan(b.through);
  expect(await api.getHistoryChunk(...args)).toEqual(events);
});

for (const state of ["waiting", "stopped", "purged"])
  test(`immutable ${state} chunk bodies survive reload in the browser HTTP cache`, async ({ api, page }) => {
    const id = await createTaskAPI(api, "history browser cache contract");
    await waitForTaskState(api, id, "waiting");
    if (state !== "waiting") {
      await api.stopTask(id);
      await waitForTaskState(api, id, "stopped");
    }
    if (state === "purged") {
      await api.purgeTask(id);
      await waitForTaskState(api, id, "purged");
    }
    const b = await api.getHistoryBootstrap(id);
    const recent = recentHistoryChunk(b);
    await page.goto("/");
    const read = () =>
      page.evaluate(async (url) => {
        performance.clearResourceTimings();
        const response = await fetch(url);
        const body = await response.text();
        const entries = performance.getEntriesByName(new URL(url, location.href).href) as PerformanceResourceTiming[];
        return { body, cache: response.headers.get("cache-control"), bytes: entries.at(-1)?.transferSize };
      }, recent.url);
    const cold = await read();
    expect(cold.cache).toContain("private");
    expect(cold.cache).toContain("immutable");
    expect(cold.bytes).toBeGreaterThan(0);
    const events = JSON.parse(cold.body) as HistoryEvent[];
    expect(events.length).toBeGreaterThan(0);
    // Reload releases the previous document's JS references. The new document
    // fetches the identical URL without an application transcript cache.
    await page.reload();
    const warm = await read();
    expect(warm.body).toEqual(cold.body);
    expect(warm.bytes).toBe(0);
  });

test("native raw SSE resumes after bootstrap without replaying its prefix", async ({ api, page }) => {
  const id = await createTaskAPI(api, "history SSE contract");
  await waitForTaskState(api, id, "waiting");
  const b = await api.getHistoryBootstrap(id);
  await page.goto("/");
  await page.evaluate(
    ({ id, cursor }) => {
      const state = { records: [] as number[], checkpoint: "", errors: [] as string[] };
      window.historyHTTPState = state;
      const source = new EventSource(`/api/caic/v1/tasks/${id}/history/events?cursor=${encodeURIComponent(cursor)}`);
      source.onmessage = (event: MessageEvent<string>) =>
        state.records.push((JSON.parse(event.data) as HistoryEvent).recordID);
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
  const next = await api.getHistoryBootstrap(id);
  await expect.poll(async () => (await state()).checkpoint).toBe(next.cursor);
  const live = await state();
  expect(live.records.length).toBeGreaterThan(0);
  expect(live.records.every((r) => r > b.through && r <= next.through)).toBe(true);
  expect(live.errors).toEqual([]);
});

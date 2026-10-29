// Browser coverage for quiet automatic history loading, HTTP-cache reuse, inactive release, and cold-JS reloads.
import { test, expect, createTaskAPI, waitForTaskState, recentHistoryChunk } from "../helpers";
import { writeFile } from "node:fs/promises";

// Playwright routing disables Chromium's HTTP cache, including preferences routing.
// Trace snapshots retain detached DOM and its event-handler closures, which
// would measure the test recorder instead of the application's ownership.
test.use({ taskPreferences: null, trace: "off" });

test("recent history stays usable while the full backlog loads automatically", async ({ api, page }, testInfo) => {
  const id = await createTaskAPI(api, "FAKE_CONCURRENT_STREAMS automatic backlog");
  await waitForTaskState(api, id, "running");
  await expect.poll(async () => (await api.getHistoryBootstrap(id)).through).toBeGreaterThan(10);
  for (const gate of ["CONTINUE", "RECONNECT", "A_COMPLETE", "FINISH", "ROOT_FINISH"])
    await api.sendInput(id, { prompt: { text: `FAKE_CONCURRENT_STREAMS_${gate}` } });
  await waitForTaskState(api, id, "waiting");
  const bootstrap = await api.getHistoryBootstrap(id);
  expect(bootstrap.through).toBeGreaterThan(1000);
  const release = Promise.withResolvers<void>();
  let requested = false;
  await page.route("**/history/**/chunks/1/*", async (route) => {
    requested = true;
    await release.promise;
    await route.continue();
  });
  try {
    await page.goto(`/task/@${id}`);
    await expect(page.getByText("Concurrent review finished", { exact: true })).toBeVisible();
    // No scroll, touch, keyboard, expansion, or pagination action starts backfill.
    await expect.poll(() => requested).toBe(true);
    await expect(page.getByTestId("task-message-area")).toHaveAttribute("aria-busy", "true");
    await expect(page.getByRole("region", { name: "Recent activity" })).toBeVisible();
    const spinner = page.getByRole("status", { name: "Loading history", exact: true });
    await expect(spinner).toBeVisible();
    expect(
      await spinner.evaluate((el) => {
        const conversation = document.querySelector('[data-testid="task-message-area"]');
        return conversation !== null && el.getBoundingClientRect().bottom <= conversation.getBoundingClientRect().top;
      }),
    ).toBe(true);
    await page.screenshot({ path: testInfo.outputPath("history-loading.png") });
    await expect(page.getByTestId("native-subagent-card")).toHaveCount(0);
    await expect(page.getByTestId("tool-call")).toHaveCount(0);
    const followup = "live output during automatic backlog loading";
    await api.sendInput(id, { prompt: { text: followup } });
    await expect(page.getByText(followup, { exact: true })).toBeVisible();
    await waitForTaskState(api, id, "waiting");
    release.resolve();
    await expect(page.getByTestId("task-message-area")).toHaveAttribute("aria-busy", "false");
    await expect(spinner).toHaveCount(0);
    await expect(page.getByText("Setup logs", { exact: true })).toHaveCount(1);
    await expect(page.getByTestId("native-subagent-card")).toHaveCount(2);
    await expect(page.getByText(followup, { exact: true })).toHaveCount(1);
  } finally {
    release.resolve();
  }
});

for (const { width, state } of [
  { width: 390, state: "waiting" },
  { width: 1280, state: "waiting" },
  { width: 1280, state: "stopped" },
  { width: 1280, state: "purged" },
]) {
  test(`${state} history is cached across task switches and reload at ${width}px`, async ({ api, page }, testInfo) => {
    await page.setViewportSize({ width, height: 900 });
    const errors: string[] = [];
    const chunkRequests: string[] = [];
    page.on("request", (request) => {
      if (/\/history\/.*\/chunks\//.test(request.url())) chunkRequests.push(request.url());
    });
    page.on("pageerror", (error) => errors.push(error.message));
    const id = await createTaskAPI(api, "FAKE_CONCURRENT_STREAMS cached window");
    // Running is published after the task opens its authoritative raw log.
    await waitForTaskState(api, id, "running");
    await expect.poll(async () => (await api.getHistoryBootstrap(id)).through).toBeGreaterThan(10);
    for (const gate of ["CONTINUE", "RECONNECT", "A_COMPLETE", "FINISH", "ROOT_FINISH"]) {
      await api.sendInput(id, { prompt: { text: `FAKE_CONCURRENT_STREAMS_${gate}` } });
    }
    await waitForTaskState(api, id, "waiting");
    if (state !== "waiting") {
      await api.stopTask(id);
      await waitForTaskState(api, id, "stopped");
    }
    if (state === "purged") {
      await api.purgeTask(id);
      await waitForTaskState(api, id, "purged");
    }
    const other = await createTaskAPI(api, "other cached conversation");
    await waitForTaskState(api, other, "waiting");
    const bootstrap = await api.getHistoryBootstrap(id);
    expect(bootstrap.through).toBeGreaterThan(1000);
    const cdp = await page.context().newCDPSession(page);
    await cdp.send("Network.enable");
    const retainedChildOutputs = async () => {
      await page.evaluate(
        () => new Promise<void>((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))),
      );
      await cdp.send("HeapProfiler.collectGarbage");
      const prototype = await cdp.send("Runtime.evaluate", {
        expression: "Object.prototype",
        objectGroup: "history-retention",
      });
      if (!prototype.result.objectId) throw new Error("Missing Object prototype");
      const objects = await cdp.send("Runtime.queryObjects", {
        prototypeObjectId: prototype.result.objectId,
        objectGroup: "history-retention",
      });
      if (!objects.objects.objectId) throw new Error("Missing heap object collection");
      try {
        const count = await cdp.send("Runtime.callFunctionOn", {
          objectId: objects.objects.objectId,
          functionDeclaration:
            "function () { const outputs = this.filter(o => o.kind === 'toolOutputDelta' && o.source?.threadID === 'child-a'); return { count: outputs.length, bytes: outputs.reduce((n, e) => n + JSON.stringify(e).length * 2, 0) }; }",
          returnByValue: true,
        });
        return count.result.value as { count: number; bytes: number };
      } finally {
        await cdp.send("Runtime.releaseObjectGroup", { objectGroup: "history-retention" });
      }
    };
    const timings: { task: string; recentMs: number; completeMs: number }[] = [];
    const open = async (taskID: string) => {
      const started = Date.now();
      if (page.url() === "about:blank") await page.goto(`/task/@${taskID}`);
      else {
        await page.getByTestId("new-task-button").click();
        await expect(page).toHaveURL(/\/$/);
        await page.evaluate(() => performance.clearResourceTimings());
        const card = page.locator(`[data-task-id='${taskID}']`);
        if (taskID === id && state === "purged" && !(await card.isVisible()))
          for (const group of await page.getByRole("button", { name: /^Purged \(/ }).all()) await group.click();
        await card.focus();
        await card.press("Enter");
      }
      if (taskID === id) await expect(page.getByText("Concurrent review finished", { exact: true })).toBeVisible();
      const recentMs = Date.now() - started;
      await expect(page.getByTestId("task-message-area")).toHaveAttribute("aria-busy", "false");
      if (taskID === id) await expect(page.getByText("Setup logs", { exact: true })).toHaveCount(1);
      timings.push({ task: taskID === id ? "history" : "other", recentMs, completeMs: Date.now() - started });
    };
    const chunkTransfers = () =>
      page.evaluate(() =>
        (performance.getEntriesByType("resource") as PerformanceResourceTiming[])
          .filter((entry) => /\/history\/.*\/chunks\//.test(entry.name))
          .map((entry) => ({ url: entry.name, transferSize: entry.transferSize })),
      );
    const warmChunks: { url: string; transferSize: number }[][] = [];
    const expectWarmChunks = async () => {
      const chunks = await chunkTransfers();
      expect(chunks.length).toBeGreaterThan(1);
      expect(
        chunks.every((chunk) => chunk.transferSize === 0),
        JSON.stringify(chunks),
      ).toBe(true);
      warmChunks.push(chunks);
    };
    await open(id);
    expect(chunkRequests.some((url) => /\/chunks\/1\//.test(url))).toBe(true);
    const chunkURL = recentHistoryChunk(bootstrap).url;
    const transferred = () =>
      page.evaluate((url) => {
        const entries = performance.getEntriesByName(new URL(url, location.href).href) as PerformanceResourceTiming[];
        return entries.at(-1)?.transferSize;
      }, chunkURL);
    expect(await transferred()).toBeGreaterThan(0);
    const activeHeap = await retainedChildOutputs();
    expect(activeHeap.count).toBeGreaterThan(100);
    await page.screenshot({ path: testInfo.outputPath(`history-backlog-${width}.png`) });
    const inactiveHeap: { count: number; bytes: number }[] = [];
    // Chromium's native FocusEvent/KeyboardEvent template caches can retain the
    // last delegated dispatch path and a few detached DOM handlers. Verify a
    // small byte/count plateau, rather than asserting universal zero objects.
    for (let cycle = 0; cycle < 4; cycle++) {
      await open(other);
      const retained = await retainedChildOutputs();
      inactiveHeap.push(retained);
      expect(retained.count).toBeLessThanOrEqual(2048);
      expect(retained.bytes).toBeLessThan(2 << 20);
      await open(id);
      await expectWarmChunks();
    }
    const firstInactive = inactiveHeap[0],
      lastInactive = inactiveHeap.at(-1);
    expect(firstInactive).toBeDefined();
    expect(lastInactive).toBeDefined();
    expect(lastInactive?.count).toBeLessThanOrEqual((firstInactive?.count ?? 0) + 32);
    expect(lastInactive?.bytes).toBeLessThanOrEqual((firstInactive?.bytes ?? 0) + (16 << 10));
    const evidencePath = testInfo.outputPath("history-cache-heap.json");
    await writeFile(evidencePath, JSON.stringify({ state, activeHeap, inactiveHeap, timings, warmChunks }));
    await testInfo.attach("history-cache-heap.json", { path: evidencePath, contentType: "application/json" });
    await page.reload();
    await expect(page.getByTestId("task-message-area")).toHaveAttribute("aria-busy", "false");
    await expectWarmChunks();
    const cachedBody = await page.evaluate(async (url) => (await fetch(url)).text(), chunkURL);
    // Evict the real HTTP cache without interception. Reload must recover the
    // identical transcript from the server, then ordinary switching warms it.
    await cdp.send("Network.clearBrowserCache");
    await page.reload();
    await expect(page.getByTestId("task-message-area")).toHaveAttribute("aria-busy", "false");
    const evictedTransferBytes = await transferred();
    expect(evictedTransferBytes).toBeGreaterThan(0);
    await expect(page.getByText("Concurrent review finished", { exact: true })).toBeVisible();
    expect(await page.evaluate(async (url) => (await fetch(url)).text(), chunkURL)).toBe(cachedBody);
    await open(other);
    await open(id);
    await expectWarmChunks();
    const evictionPath = testInfo.outputPath("history-cache-eviction.json");
    await writeFile(evictionPath, JSON.stringify({ evictedTransferBytes, rewarmedTransferBytes: 0 }));
    await testInfo.attach("history-cache-eviction.json", { path: evictionPath, contentType: "application/json" });
    await expect(page.getByTestId("task-message-area")).toHaveAttribute("aria-busy", "false");
    if (state === "waiting") {
      const followup = `cached history follow-up ${width}`;
      const chunksBeforeLive = chunkRequests.length;
      await api.sendInput(id, { prompt: { text: followup } });
      await expect(page.getByText(followup, { exact: true })).toBeVisible();
      await waitForTaskState(api, id, "waiting");
      expect(chunkRequests).toHaveLength(chunksBeforeLive);
    }
    expect(errors).toEqual([]);
  });
}

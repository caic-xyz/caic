// Browser coverage for lazy archived transcripts, stable actor caching, and released decoded payloads.
import { test, expect, createTaskAPI, waitForTaskState } from "../helpers";
import { writeFile } from "node:fs/promises";
import type { CDPSession, Page } from "@playwright/test";

// Playwright routing disables Chromium's HTTP cache, including preferences routing.
// Trace snapshots retain detached DOM and its event-handler closures, which
// would measure the test recorder instead of the application's ownership.
test.use({ taskPreferences: null, trace: "off" });

async function retainedChildOutputs(page: Page, cdp: CDPSession) {
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
}

test("compacted bodies stay unloaded until disclosure while the current session and statistics work", async ({
  api,
  page,
}) => {
  const id = await createTaskAPI(api, "FAKE_SKILLS load section history");
  await waitForTaskState(api, id, "waiting");
  await api.sendInput(id, { prompt: { text: "FAKE_CONCURRENT_STREAMS partial archive cancellation" } });
  await waitForTaskState(api, id, "running");
  for (const gate of ["CONTINUE", "RECONNECT", "A_COMPLETE", "FINISH", "ROOT_FINISH"])
    await api.sendInput(id, { prompt: { text: `FAKE_CONCURRENT_STREAMS_${gate}` } });
  await waitForTaskState(api, id, "waiting");
  await api.sendInput(id, { prompt: { text: "FAKE_SKILLS compact" } });
  await waitForTaskState(api, id, "waiting");
  const manifest = await api.getHistoryManifest(id);
  expect(manifest.sections.length).toBeGreaterThan(1);
  const old = manifest.sections[0]?.body;
  expect(old).toBeDefined();
  const release = Promise.withResolvers<void>();
  const finished = Promise.withResolvers<void>();
  const cdp = await page.context().newCDPSession(page);
  let requested = false;
  const pattern = `**/history/**/slice/${old!.first.recordID}/${old!.first.index}/**`;
  await page.route(pattern, async (route) => {
    requested = true;
    await release.promise;
    await route.continue();
    finished.resolve();
  });
  try {
    await page.goto(`/task/@${id}`);
    await expect(page.getByTestId("task-message-area")).toHaveAttribute("aria-busy", "false");
    expect(requested).toBe(false);
    await page.getByRole("link", { name: "Task statistics" }).click();
    await expect(page.getByTestId("task-analytics-charts")).toBeVisible();
    expect(requested).toBe(false);
    await page.goBack();
    await page.getByRole("button", { name: /Compacted session/ }).click();
    await expect.poll(() => requested).toBe(true);
    await expect(page.getByRole("status", { name: "Loading session history" })).toBeVisible();
    // Other pages in this four-request batch can already be decoded while its
    // first page is gated. Closing must release those partial results too.
    await expect.poll(async () => (await retainedChildOutputs(page, cdp)).count).toBeGreaterThan(100);
    await page.getByRole("button", { name: /Compacted session/ }).click();
    await expect(page.getByRole("status", { name: "Loading session history" })).toHaveCount(0);
    expect((await retainedChildOutputs(page, cdp)).count).toBe(0);
    release.resolve();
    await finished.promise;
    expect((await retainedChildOutputs(page, cdp)).count).toBe(0);
    await page.getByRole("button", { name: /Compacted session/ }).click();
    await expect(page.getByRole("navigation", { name: "Previous context skills" })).toBeVisible();
    await expect(page.getByTestId("history-section").locator('[data-anchor-key^="turn:"]').first()).toBeVisible();
    await expect(page.getByRole("status", { name: "Loading session history" })).toHaveCount(0);
  } finally {
    release.resolve();
    await cdp.detach();
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
      if (/\/history\/.*\/slice\//.test(request.url())) chunkRequests.push(request.url());
    });
    page.on("pageerror", (error) => errors.push(error.message));
    const id = await createTaskAPI(api, "FAKE_CONCURRENT_STREAMS cached window");
    // Running is published after the task opens its authoritative raw log.
    await waitForTaskState(api, id, "running");
    await expect.poll(async () => (await api.getHistoryManifest(id)).bootstrap.through).toBeGreaterThan(10);
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
    const bootstrap = (await api.getHistoryManifest(id)).bootstrap;
    expect(bootstrap.through).toBeGreaterThan(1000);
    const cdp = await page.context().newCDPSession(page);
    await cdp.send("Network.enable");
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
          .filter((entry) => /\/history\/.*\/slice\//.test(entry.name))
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
    const coldResponse = page.waitForResponse(
      (response) => response.url().includes(`/tasks/${id}/history/`) && response.url().includes("/slice/"),
    );
    await open(id);
    const cacheControl = (await coldResponse).headers()["cache-control"];
    expect(cacheControl).toContain("private");
    expect(cacheControl).toContain("immutable");
    expect(chunkRequests.length).toBeGreaterThan(0);
    const chunkURL = chunkRequests.at(-1)!;
    const transferred = () =>
      page.evaluate((url) => {
        const entries = performance.getEntriesByName(new URL(url, location.href).href) as PerformanceResourceTiming[];
        return entries.at(-1)?.transferSize;
      }, chunkURL);
    expect(await transferred()).toBeGreaterThan(0);
    const activeHeap = await retainedChildOutputs(page, cdp);
    expect(activeHeap.count).toBeGreaterThan(100);
    const inactiveHeap: { count: number; bytes: number }[] = [];
    // Chromium's native FocusEvent/KeyboardEvent template caches can retain the
    // last delegated dispatch path and a few detached DOM handlers. Verify a
    // small byte/count plateau, rather than asserting universal zero objects.
    for (let cycle = 0; cycle < 4; cycle++) {
      await open(other);
      const retained = await retainedChildOutputs(page, cdp);
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

for (const width of [390, 1280]) {
  test(`archived transcripts release memory and reuse HTTP cache at ${width}px`, async ({ api, page }, testInfo) => {
    await page.setViewportSize({ width, height: 900 });
    const errors: string[] = [];
    const requests: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    page.on("request", (request) => {
      if (/\/history\/.*\/(slice|activity)\//.test(request.url())) requests.push(request.url());
    });
    const id = await createTaskAPI(api, "FAKE_CONCURRENT_STREAMS archived cache and retention");
    await waitForTaskState(api, id, "running");
    for (const gate of ["CONTINUE", "RECONNECT", "A_COMPLETE", "FINISH", "ROOT_FINISH"])
      await api.sendInput(id, { prompt: { text: `FAKE_CONCURRENT_STREAMS_${gate}` } });
    await waitForTaskState(api, id, "waiting");
    await api.sendInput(id, { prompt: { text: "FAKE_SKILLS compact" } });
    await waitForTaskState(api, id, "waiting");
    const manifest = await api.getHistoryManifest(id);
    const archived = manifest.sections[0];
    const current = manifest.sections.at(-1);
    if (!archived?.body || !current?.body) throw new Error("Missing section fixture");
    const oldFirst = archived.body.first.recordID;
    const oldLast = archived.body.last.recordID;
    const archivedRequest = (url: string) => {
      const first = Number(new URL(url).pathname.split("/slice/")[1]?.split("/")[0]);
      return Number.isFinite(first) && first >= oldFirst && first <= oldLast;
    };
    const cdp = await page.context().newCDPSession(page);
    await cdp.send("Network.enable");
    await page.goto(`/task/@${id}`);
    await expect(page.getByTestId("task-message-area")).toHaveAttribute("aria-busy", "false");
    await expect(page.getByText("Context compacted; skills must be loaded again.", { exact: true })).toBeVisible();
    expect(requests.filter(archivedRequest)).toEqual([]);
    expect(requests.filter((url) => url.includes("/activity/"))).toEqual([]);
    await expect(page.getByTestId("native-subagent-card")).toHaveCount(0);
    const baseline = await retainedChildOutputs(page, cdp);
    expect(baseline.count).toBe(0);
    const section = page.locator(`[data-testid="history-section"][data-section-id="${archived.id}"]`);
    const toggle = section.getByRole("button", { name: /Compacted session/ });
    const bodyTransfers = () =>
      page.evaluate(() =>
        (performance.getEntriesByType("resource") as PerformanceResourceTiming[])
          .filter((entry) => /\/history\/.*\/(slice|activity)\//.test(entry.name))
          .map((entry) => ({
            url: entry.name,
            transferSize: entry.transferSize,
            decodedBodySize: entry.decodedBodySize,
          })),
      );
    const open = async () => {
      await toggle.click();
      await expect(section.locator('[data-anchor-key^="turn:"]').first()).toBeVisible();
      const actor = section.locator('[data-native-id="codex:thread:child-a"]');
      await actor.locator(":scope > summary").click();
      await expect(actor.getByRole("listitem").filter({ hasText: "A bowtie uses" })).toBeVisible();
      await expect(actor.getByRole("status", { name: "Loading subagent history" })).toHaveCount(0);
    };
    await page.evaluate(() => performance.clearResourceTimings());
    await open();
    const expanded = await retainedChildOutputs(page, cdp);
    expect(expanded.count).toBeGreaterThan(1000);
    const coldTransfers = await bodyTransfers();
    expect(coldTransfers.some((entry) => archivedRequest(entry.url) && entry.transferSize > 0)).toBe(true);
    const actorURL = coldTransfers.find((entry) => entry.url.includes("/activity/"))?.url;
    if (!actorURL) throw new Error("Missing actor body request");
    const expectCachedBodies = (entries: Awaited<ReturnType<typeof bodyTransfers>>) => {
      expect(
        entries.some((entry) => entry.url === actorURL && entry.transferSize === 0),
        JSON.stringify(entries),
      ).toBe(true);
      expect(entries.filter((entry) => archivedRequest(entry.url)).every((entry) => entry.transferSize === 0)).toBe(
        true,
      );
      // A checkpoint can beat its paired manifest. Coalescing that race may
      // fetch an empty actor suffix, never another copy of the full body.
      for (const entry of entries.filter((item) => item.url.includes("/activity/") && item.url !== actorURL)) {
        expect(new URL(entry.url).searchParams.has("afterRecordID")).toBe(true);
        expect(entry.decodedBodySize).toBeLessThanOrEqual(4);
      }
      expect(entries.some((entry) => archivedRequest(entry.url))).toBe(true);
    };
    const actorPrefixRequests = () =>
      requests.filter(
        (url) => url.includes("/activity/nativeSubagent/codex%3Athread%3Achild-a/") && !url.includes("afterRecordID"),
      );
    const beforeRouting = (await api.getHistoryManifest(id)).metadata.routingRevision;
    const childSummary = section.locator('[data-native-id="codex:thread:child-a"] > summary');
    await childSummary.focus();
    await api.sendInput(id, { prompt: { text: "FAKE_NATIVE_SUBAGENTS unrelated actor relationships" } });
    await waitForTaskState(api, id, "waiting");
    await expect
      .poll(async () => (await api.getHistoryManifest(id)).metadata.routingRevision)
      .toBeGreaterThan(beforeRouting);
    await expect(page.locator('[data-native-id="joker"]')).toBeVisible();
    await expect(childSummary).toBeFocused();
    expect(actorPrefixRequests()).toHaveLength(1);
    await toggle.click();
    await expect(section.locator('[data-anchor-key^="turn:"]')).toHaveCount(0);
    const collapsed = await retainedChildOutputs(page, cdp);
    console.log(JSON.stringify({ width, baseline, expanded, collapsed }));
    expect(collapsed.count).toBe(0);
    // The old actor is unchanged while root history advances. Its full body URL
    // must stay cached rather than using the task's new global checkpoint.
    const followup = `unrelated root activity for archived cache ${width}`;
    await api.sendInput(id, { prompt: { text: followup } });
    await waitForTaskState(api, id, "waiting");
    await expect(page.getByText(followup, { exact: true })).toBeVisible();
    const cycles: { count: number; bytes: number }[] = [];
    const warmTransfers: Awaited<ReturnType<typeof bodyTransfers>>[] = [];
    for (let cycle = 0; cycle < 3; cycle++) {
      await page.evaluate(() => performance.clearResourceTimings());
      await open();
      const warm = await bodyTransfers();
      expectCachedBodies(warm);
      warmTransfers.push(warm);
      await toggle.click();
      const retained = await retainedChildOutputs(page, cdp);
      cycles.push(retained);
      expect(retained.count).toBe(0);
    }
    const other = await createTaskAPI(api, "other task while the archived transcript is released");
    await waitForTaskState(api, other, "waiting");
    await page.getByTestId("new-task-button").click();
    await page.locator(`[data-task-id='${other}']`).click();
    await expect(page.getByTestId("task-message-area")).toHaveAttribute("aria-busy", "false");
    expect((await retainedChildOutputs(page, cdp)).count).toBe(0);
    await page.getByTestId("new-task-button").click();
    await page.locator(`[data-task-id='${id}']`).click();
    await expect(page.getByTestId("task-message-area")).toHaveAttribute("aria-busy", "false");
    await page.reload();
    await expect(page.getByTestId("task-message-area")).toHaveAttribute("aria-busy", "false");
    expect((await retainedChildOutputs(page, cdp)).count).toBe(0);
    await page.evaluate(() => performance.clearResourceTimings());
    await open();
    const reloaded = await bodyTransfers();
    expectCachedBodies(reloaded);
    await toggle.click();
    await cdp.send("Network.clearBrowserCache");
    await page.evaluate(() => performance.clearResourceTimings());
    await open();
    const evicted = await bodyTransfers();
    expect(evicted.some((entry) => archivedRequest(entry.url) && entry.transferSize > 0)).toBe(true);
    expect(evicted.some((entry) => entry.url === actorURL && entry.transferSize > 0)).toBe(true);
    await toggle.click();
    await page.getByTestId("new-task-button").click();
    await expect(page).toHaveURL(/\/$/);
    const disposed = await retainedChildOutputs(page, cdp);
    expect(disposed.count).toBe(0);
    const evidence = testInfo.outputPath("archive-cache-retention.json");
    await writeFile(
      evidence,
      JSON.stringify({
        width,
        baseline,
        expanded,
        collapsed,
        cycles,
        disposed,
        coldTransfers,
        warmTransfers,
        reloaded,
        evicted,
      }),
    );
    await testInfo.attach("archive-cache-retention.json", { path: evidence, contentType: "application/json" });
    expect(errors).toEqual([]);
    await cdp.detach();
  });
}

test("growing archived payload leaves current-context browser work bounded", async ({ api, page }, testInfo) => {
  const id = await createTaskAPI(api, "FAKE_SKILLS load archived growth");
  await waitForTaskState(api, id, "waiting");
  const samples: { archivedSections: number; bodyBytes: number; manifestBytes: number; bodyRequests: number }[] = [];
  for (let count = 1; count <= 4; count++) {
    await api.sendInput(id, { prompt: { text: `FAKE_SKILLS load ${"archived-payload ".repeat(4096)}` } });
    await waitForTaskState(api, id, "waiting");
    await api.sendInput(id, { prompt: { text: "FAKE_SKILLS compact" } });
    await waitForTaskState(api, id, "waiting");
    if (count !== 1 && count !== 4) continue;
    const manifest = await api.getHistoryManifest(id);
    const current = manifest.sections.at(-1)?.body;
    if (!current) throw new Error("Missing current section");
    await page.goto(`/task/@${id}`);
    await expect(page.getByTestId("task-message-area")).toHaveAttribute("aria-busy", "false");
    await expect(page.getByTestId("history-section")).toHaveCount(count);
    const resources = await page.evaluate(() =>
      (performance.getEntriesByType("resource") as PerformanceResourceTiming[])
        .filter((entry) => entry.name.includes("/history/"))
        .map((entry) => ({ url: entry.name, bytes: entry.decodedBodySize })),
    );
    const bodies = resources.filter((entry) => entry.url.includes("/slice/"));
    expect(bodies).toHaveLength(1);
    expect(Number(new URL(bodies[0]!.url).pathname.split("/slice/")[1]?.split("/")[0])).toBe(current.first.recordID);
    await expect(page.getByText(/archived-payload/)).toHaveCount(0);
    samples.push({
      archivedSections: count,
      bodyBytes: bodies.reduce((sum, entry) => sum + entry.bytes, 0),
      manifestBytes: resources
        .filter((entry) => entry.url.includes("/manifest"))
        .reduce((sum, entry) => sum + entry.bytes, 0),
      bodyRequests: bodies.length,
    });
    await page.getByTestId("new-task-button").click();
    await expect(page).toHaveURL(/\/$/);
  }
  expect(samples).toHaveLength(2);
  expect(samples[1]!.bodyBytes).toBeLessThan(samples[0]!.bodyBytes + 128);
  expect(samples[1]!.manifestBytes).toBeGreaterThan(samples[0]!.manifestBytes);
  const evidence = testInfo.outputPath("archived-growth.json");
  await writeFile(evidence, JSON.stringify(samples));
  await testInfo.attach("archived-growth.json", { path: evidence, contentType: "application/json" });
});

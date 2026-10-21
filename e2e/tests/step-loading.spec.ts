// Browser coverage for STEP loading controls, cancellation, retry, and worker cleanup using gated downloads.
import { ok } from "node:assert/strict";
import type { Page } from "@playwright/test";
import { readFileSync } from "node:fs";
import { zipSync } from "three/addons/libs/fflate.module.js";
import { test, expect, createTaskAPI, waitForTaskState, collectStepTimings } from "../helpers";

const cube = readFileSync("node_modules/occt-import-js/test/testfiles/simple-basic-cube/cube.stp", "utf8");
const archive = Buffer.from(zipSync({ "cube.stp": new TextEncoder().encode(cube) }));

function nextStepWorker(page: Page) {
  // Attach the close listener at creation, before a fast import can finish
  // while Playwright is still completing the click that opened the viewer.
  return page
    .waitForEvent("worker", (worker) => worker.url().includes("stepWorker"))
    .then((worker) => ({
      worker,
      closed: new Promise<void>((resolve) => worker.once("close", () => resolve())),
    }));
}

for (const width of [390, 1280]) {
  test(`STEP loading survives fullscreen transitions at ${width}px`, async ({ page, context, api, uniquePrompt }) => {
    await page.setViewportSize({ width, height: 900 });
    const diagnostics = await collectStepTimings(page);
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    const gate = Promise.withResolvers<void>();
    const requested = Promise.withResolvers<void>();
    let loads = 0;
    await context.route("**/raw?*", async (route) => {
      loads++;
      requested.resolve();
      await gate.promise;
      await route.fulfill({ contentType: "application/zip", body: archive });
    });
    try {
      const id = await createTaskAPI(api, uniquePrompt("Inspect [Cube](/cube.stpz)."));
      await waitForTaskState(api, id, "waiting");
      await page.goto(`/task/@${id}`);
      const started = nextStepWorker(page);
      await page.getByRole("link", { name: "Cube", exact: true }).click();
      const { worker, closed } = await started;
      await requested.promise;
      const progress = page.getByRole("progressbar", { name: "Downloading model" });
      await expect(progress).toBeVisible();
      await expect(progress).not.toHaveAttribute("value");
      await page.getByRole("button", { name: "Full screen", exact: true }).click();
      await expect(progress).toBeVisible();
      await page.getByRole("button", { name: "Exit full screen", exact: true }).click();
      await expect(progress).toBeVisible();
      expect(loads).toBe(1);
      expect(page.workers()).toContain(worker);
      expect(page.workers().filter((worker) => worker.url().includes("stepWorker"))).toHaveLength(1);
      await page.screenshot({ path: test.info().outputPath("step-downloading.png") });
      gate.resolve();
      await expect(page.getByText("12 triangles", { exact: true })).toBeVisible({ timeout: 30_000 });
      await closed;
      await expect.poll(() => diagnostics.length).toBe(1);
      ok(diagnostics[0]);
      expect(diagnostics[0].stages.occtParseMs).toBeGreaterThan(0);
      expect(diagnostics[0].stages.firstFrameMs).toBeGreaterThan(0);
      expect(diagnostics[0].unaccountedMs).toBeGreaterThanOrEqual(0);
      await expect(page.getByLabel(/^3D model/)).toBeVisible();
      expect(page.workers().filter((worker) => worker.url().includes("stepWorker"))).toHaveLength(0);
      expect(loads).toBe(1);
      expect(errors).toEqual([]);
    } finally {
      gate.resolve();
      await context.unrouteAll({ behavior: "wait" });
    }
  });

  test(`STEP cancellation, retry, and file switching at ${width}px`, async ({ page, context, api, uniquePrompt }) => {
    await page.setViewportSize({ width, height: 900 });
    const diagnostics = await collectStepTimings(page);
    let loads = 0;
    const gate = Promise.withResolvers<void>();
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    await context.route("**/raw?*", async (route) => {
      const path = new URL(route.request().url()).searchParams.get("path");
      if (path === "/cube.stpz" && ++loads !== 2) {
        // These requests must remain pending until cancellation, navigation,
        // or closing the view terminates their workers. Retry gets real bytes.
        await gate.promise;
        await route.abort("aborted");
        return;
      }
      await route.fulfill({
        contentType: "application/octet-stream",
        body: path === "/cube.stpz" ? archive : Buffer.from(cube),
      });
    });
    try {
      const id = await createTaskAPI(api, uniquePrompt("Inspect [Compressed](/cube.stpz) and [Cube](/cube.step)."));
      await waitForTaskState(api, id, "waiting");
      await page.goto(`/task/@${id}`);
      const started = nextStepWorker(page);
      await page.getByRole("link", { name: "Compressed", exact: true }).click();
      const { closed } = await started;
      await expect.poll(() => loads).toBe(1);
      await expect(page.getByRole("progressbar", { name: "Downloading model" })).toBeVisible();
      await page.getByRole("button", { name: "Cancel loading", exact: true }).click();
      await closed;
      expect(diagnostics).toHaveLength(0);
      await expect(page.getByRole("status")).toHaveText("Model loading cancelled.");
      await expect(page.getByRole("button", { name: "Retry", exact: true })).toBeFocused();
      const retryStarted = nextStepWorker(page);
      await page.getByRole("button", { name: "Retry", exact: true }).click();
      const { closed: retryClosed } = await retryStarted;
      await expect(page.getByText("12 triangles", { exact: true })).toBeVisible();
      await retryClosed;
      await expect.poll(() => diagnostics.length).toBe(1);
      ok(diagnostics[0]);
      expect(loads).toBe(2);
      await page.keyboard.press("Escape");
      const switchedStarted = nextStepWorker(page);
      await page.getByRole("link", { name: "Compressed", exact: true }).click();
      const { closed: switchedClosed } = await switchedStarted;
      await expect.poll(() => loads).toBe(3);
      await expect(page.getByRole("progressbar", { name: "Downloading model" })).toBeVisible();
      if (width === 390) {
        await page.goBack();
        await switchedClosed;
      }
      await page.getByRole("link", { name: "Cube", exact: true }).click();
      await switchedClosed;
      await expect(page.getByTitle("/cube.step", { exact: true })).toBeVisible();
      await expect(page.getByText("12 triangles", { exact: true })).toBeVisible();
      await expect(page.getByRole("alert")).toHaveCount(0);
      await page.keyboard.press("Escape");
      const closingStarted = nextStepWorker(page);
      await page.getByRole("link", { name: "Compressed", exact: true }).click();
      const { closed: closingClosed } = await closingStarted;
      await expect.poll(() => loads).toBe(4);
      await expect(page.getByRole("progressbar", { name: "Downloading model" })).toBeVisible();
      await page.keyboard.press("Escape");
      await closingClosed;
      expect(page.workers().filter((worker) => worker.url().includes("stepWorker"))).toHaveLength(0);
      expect(errors).toEqual([]);
    } finally {
      gate.resolve();
      await context.unrouteAll({ behavior: "wait" });
    }
  });
}

test("STEP worker startup errors are recoverable", async ({ page, context, api, uniquePrompt }) => {
  let attempts = 0;
  await context.route("**/assets/stepWorker-*.js", (route) => {
    attempts++;
    return attempts === 1 ? route.abort("failed") : route.continue();
  });
  await context.route("**/raw?*", (route) => route.fulfill({ contentType: "application/octet-stream", body: cube }));
  const id = await createTaskAPI(api, uniquePrompt("Inspect [Cube](/cube.step)."));
  await waitForTaskState(api, id, "waiting");
  await page.goto(`/task/@${id}`);
  await page.getByRole("link", { name: "Cube", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("Failed to start STEP import worker");
  await page.getByRole("button", { name: "Retry", exact: true }).click();
  await expect(page.getByText("12 triangles", { exact: true })).toBeVisible();
  await expect(page.getByRole("alert")).toHaveCount(0);
  expect(attempts).toBe(2);
});

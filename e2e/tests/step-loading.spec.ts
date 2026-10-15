// Browser coverage and measurements for responsive STEP loading, progress, and cancellation.
import type { Worker } from "@playwright/test";
import { readFileSync } from "node:fs";
import { zipSync } from "three/addons/libs/fflate.module.js";
import { test, expect, createTaskAPI, waitForTaskState } from "../helpers";

const cube = readFileSync("node_modules/occt-import-js/test/testfiles/simple-basic-cube/cube.stp", "utf8");
const padded = cube.replace("ISO-10303-21;", "ISO-10303-21;\n" + ("/*" + "x".repeat(1000) + "*/\n").repeat(2 * 1024));
const archive = Buffer.from(zipSync({ "cube.stp": new TextEncoder().encode(padded) }));

function workerClosed(worker: Worker): Promise<void> {
  return new Promise((resolve) => worker.once("close", () => resolve()));
}

test("large STEP loading keeps the UI responsive", async ({ page, context, api, uniquePrompt }) => {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await context.route("**/raw?*", (route) => route.fulfill({ contentType: "application/zip", body: archive }));
  const id = await createTaskAPI(api, uniquePrompt("Inspect [Large](/large.stpz)."));
  await waitForTaskState(api, id, "waiting");
  await page.goto(`/task/@${id}`);
  await expect(page.getByRole("link", { name: "Large", exact: true })).toBeVisible();
  const monitor = await page.evaluateHandle(() => {
    let previous = performance.now();
    let longest = 0;
    let ticks = 0;
    const timer = setInterval(() => {
      const now = performance.now();
      longest = Math.max(longest, now - previous);
      previous = now;
      ticks++;
    }, 16);
    return {
      stop() {
        clearInterval(timer);
        return { longestMs: longest, ticks };
      },
    };
  });
  const started = page.waitForEvent("worker", (worker) => worker.url().includes("stepWorker"));
  await page.getByRole("link", { name: "Large", exact: true }).click();
  const closed = workerClosed(await started);
  const progress = page.getByRole("progressbar", { name: "Parsing STEP" });
  await expect(progress).toBeVisible();
  await expect(progress).not.toHaveAttribute("value");
  await page.screenshot({ path: test.info().outputPath("step-parsing.png") });
  await expect(page.getByText("12 triangles", { exact: true })).toBeVisible({ timeout: 30_000 });
  const timing = await monitor.evaluate((value) => value.stop());
  console.log(JSON.stringify({ stepLoading: timing }));
  expect(timing.ticks).toBeGreaterThan(50);
  expect(timing.longestMs).toBeLessThan(1000);
  await closed;
  expect(page.workers().filter((worker) => worker.url().includes("stepWorker"))).toHaveLength(0);
  expect(errors).toEqual([]);
});

for (const width of [390, 1280]) {
  test(`STEP cancellation, retry, and file switching at ${width}px`, async ({ page, context, api, uniquePrompt }) => {
    await page.setViewportSize({ width, height: 900 });
    let loads = 0;
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    await context.route("**/raw?*", (route) => {
      loads++;
      const path = new URL(route.request().url()).searchParams.get("path");
      return route.fulfill({
        contentType: "application/octet-stream",
        body:
          path === "/large.stpz"
            ? loads === 2
              ? Buffer.from(zipSync({ "cube.stp": new TextEncoder().encode(cube) }))
              : archive
            : Buffer.from(cube),
      });
    });
    const id = await createTaskAPI(api, uniquePrompt("Inspect [Large](/large.stpz) and [Cube](/cube.step)."));
    await waitForTaskState(api, id, "waiting");
    await page.goto(`/task/@${id}`);
    const started = page.waitForEvent("worker", (worker) => worker.url().includes("stepWorker"));
    await page.getByRole("link", { name: "Large", exact: true }).click();
    const closed = workerClosed(await started);
    await expect(page.getByRole("progressbar", { name: "Parsing STEP" })).toBeVisible();
    await page.getByRole("button", { name: "Full screen", exact: true }).click();
    await page.getByRole("button", { name: "Exit full screen", exact: true }).click();
    await expect(page.getByRole("progressbar", { name: "Parsing STEP" })).toBeVisible();
    await page.getByRole("button", { name: "Cancel loading", exact: true }).click();
    await closed;
    await expect(page.getByRole("status")).toHaveText("Model loading cancelled.");
    await expect(page.getByRole("button", { name: "Retry", exact: true })).toBeFocused();
    const retryStarted = page.waitForEvent("worker", (worker) => worker.url().includes("stepWorker"));
    await page.getByRole("button", { name: "Retry", exact: true }).click();
    const retryClosed = workerClosed(await retryStarted);
    // Retry returns a small but still compressed version of the same cube.
    await expect(page.getByText("12 triangles", { exact: true })).toBeVisible();
    await retryClosed;
    expect(loads).toBe(2);
    await page.keyboard.press("Escape");
    const switchedStarted = page.waitForEvent("worker", (worker) => worker.url().includes("stepWorker"));
    await page.getByRole("link", { name: "Large", exact: true }).click();
    const switchedClosed = workerClosed(await switchedStarted);
    await expect(page.getByRole("progressbar", { name: "Parsing STEP" })).toBeVisible();
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
    const closingStarted = page.waitForEvent("worker", (worker) => worker.url().includes("stepWorker"));
    await page.getByRole("link", { name: "Large", exact: true }).click();
    const closingClosed = workerClosed(await closingStarted);
    await expect(page.getByRole("progressbar", { name: "Parsing STEP" })).toBeVisible();
    await page.keyboard.press("Escape");
    await closingClosed;
    expect(page.workers().filter((worker) => worker.url().includes("stepWorker"))).toHaveLength(0);
    expect(errors).toEqual([]);
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

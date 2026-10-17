// Browser coverage for direct faceted STEP import and opt-in timing of the supplied dense assembly.
import { readFileSync } from "node:fs";
import { zipSync } from "three/addons/libs/fflate.module.js";
import { createFacetedStep } from "../../frontend/tests/step-fixture";
import { test, expect, createTaskAPI, waitForTaskState } from "../helpers";

for (const width of [390, 1280]) {
  test(`faceted STEP assemblies skip the CAD kernel at ${width}px`, async ({ page, context, api, uniquePrompt }) => {
    await page.setViewportSize({ width, height: 900 });
    const archive = Buffer.from(zipSync({ "assembly.stp": createFacetedStep(8, false) }));
    const errors: string[] = [];
    const wasm: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    page.on("console", (message) => {
      if (message.type() === "error") errors.push(message.text());
    });
    page.on("request", (request) => {
      if (/occt-import-js.*\.wasm/.test(request.url())) wasm.push(request.url());
    });
    await context.route("**/raw?*", (route) => route.fulfill({ contentType: "application/zip", body: archive }));
    const id = await createTaskAPI(api, uniquePrompt("Inspect [Assembly](/assembly.stpz)."));
    await waitForTaskState(api, id, "waiting");
    await page.goto(`/task/@${id}`);
    await page.getByRole("link", { name: "Assembly", exact: true }).click();
    await expect(page.getByText("32 triangles", { exact: true })).toBeVisible();
    await expect(page.getByLabel(/^3D model/)).toBeVisible();
    await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    await page.screenshot({ path: test.info().outputPath("faceted-assembly.png") });
    expect(wasm).toEqual([]);
    expect(errors).toEqual([]);
  });
}

// Local performance reproduction; the private 24 MB model is not a CI fixture.
// CAIC_E2E_FACETED_ASSEMBLY=/home/user/src/assembly.stpz pnpm exec playwright test \
//   --config e2e/playwright.config.ts e2e/tests/step-facets.spec.ts
test("supplied 192599-triangle faceted STEP assembly finishes loading", async ({
  page,
  context,
  api,
  uniquePrompt,
}) => {
  const path = process.env.CAIC_E2E_FACETED_ASSEMBLY;
  test.skip(!path, "Set CAIC_E2E_FACETED_ASSEMBLY to the supplied assembly.stpz");
  if (!path) return;
  const errors: string[] = [];
  const wasm: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("console", (message) => {
    if (message.type() === "error") errors.push(message.text());
  });
  page.on("request", (request) => {
    if (/occt-import-js.*\.wasm/.test(request.url())) wasm.push(request.url());
  });
  const body = readFileSync(path);
  await context.route("**/raw?*", (route) => route.fulfill({ contentType: "application/zip", body }));
  const id = await createTaskAPI(api, uniquePrompt("Inspect [Assembly](/assembly.stpz)."));
  await waitForTaskState(api, id, "waiting");
  await page.goto(`/task/@${id}`);
  const monitor = await page.evaluateHandle(() => {
    let last = performance.now();
    let longest = 0;
    const timer = setInterval(() => {
      const now = performance.now();
      longest = Math.max(longest, now - last);
      last = now;
    }, 16);
    return {
      stop() {
        clearInterval(timer);
        return longest;
      },
    };
  });
  const start = performance.now();
  await page.getByRole("link", { name: "Assembly", exact: true }).click();
  await expect(page.getByText("192,599 triangles", { exact: true })).toBeVisible({ timeout: 30_000 });
  const loadMs = performance.now() - start;
  const longestMainThreadMs = await monitor.evaluate((value) => value.stop());
  console.log(JSON.stringify({ facetedAssembly: { loadMs, longestMainThreadMs } }));
  await expect(page.getByLabel(/^3D model/)).toBeVisible();
  await page.screenshot({ path: test.info().outputPath("dense-faceted-assembly.png") });
  expect(wasm).toEqual([]);
  expect(errors).toEqual([]);
  expect(longestMainThreadMs).toBeLessThan(1000);
});

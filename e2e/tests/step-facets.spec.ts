// Browser coverage for direct faceted STEP imports, dense assembly geometry, and worker cleanup.
import { zipSync } from "three/addons/libs/fflate.module.js";
import { createDenseFacetedStep, createFacetedStep } from "../../frontend/tests/step-fixture";
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

test("dense faceted STEP assembly imports and releases its worker", async ({ page, context, api, uniquePrompt }) => {
  const errors: string[] = [];
  const wasm: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("console", (message) => {
    if (message.type() === "error") errors.push(message.text());
  });
  page.on("request", (request) => {
    if (/occt-import-js.*\.wasm/.test(request.url())) wasm.push(request.url());
  });
  // Four tetrahedron faces, each split into 220² triangles: 193,600 total.
  const body = Buffer.from(zipSync({ "assembly.stp": createDenseFacetedStep(220) }));
  await context.route("**/raw?*", (route) => route.fulfill({ contentType: "application/zip", body }));
  const id = await createTaskAPI(api, uniquePrompt("Inspect [Assembly](/assembly.stpz)."));
  await waitForTaskState(api, id, "waiting");
  await page.goto(`/task/@${id}`);
  const start = performance.now();
  const started = page.waitForEvent("worker", (worker) => worker.url().includes("stepWorker"));
  await page.getByRole("link", { name: "Assembly", exact: true }).click();
  await started;
  await expect(page.getByText("193,600 triangles", { exact: true })).toBeVisible({ timeout: 30_000 });
  const loadMs = performance.now() - start;
  console.log(JSON.stringify({ facetedAssembly: { loadMs } }));
  await expect.poll(() => page.workers().filter((worker) => worker.url().includes("stepWorker"))).toHaveLength(0);
  await expect(page.getByLabel(/^3D model/)).toBeVisible();
  await page.screenshot({ path: test.info().outputPath("dense-faceted-assembly.png") });
  expect(wasm).toEqual([]);
  expect(errors).toEqual([]);
});

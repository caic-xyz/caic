// Browser coverage for faceted STEP geometry, rigid assemblies, polygons, and responsive dense loading.
import { ok } from "node:assert/strict";
import { zipSync } from "three/addons/libs/fflate.module.js";
import {
  createDenseFacetedStep,
  createFacetedStep,
  createFramedStep,
  createPolygonStep,
} from "../../frontend/tests/step-fixture";
import { test, expect, createTaskAPI, waitForTaskState, collectStepTimings } from "../helpers";

for (const width of [390, 1280]) {
  test(`faceted STEP assemblies skip the CAD kernel at ${width}px`, async ({ page, context, api, uniquePrompt }) => {
    await page.setViewportSize({ width, height: 900 });
    // Desktop also exercises nested, reused parts with independently reversed
    // transformation items and representation references through the worker.
    const source =
      width === 390
        ? createFacetedStep(8, false)
        : new TextEncoder().encode(
            createFramedStep(8, true, true)
              .replace(
                /ITEM_DEFINED_TRANSFORMATION\('','',#(\d+),#(\d+)\)/g,
                "ITEM_DEFINED_TRANSFORMATION('','',#$2,#$1)",
              )
              .replace(
                /REPRESENTATION_RELATIONSHIP\('','',#(\d+),#(\d+)\)/g,
                "REPRESENTATION_RELATIONSHIP('','',#$2,#$1)",
              ),
          );
    const archive = Buffer.from(zipSync({ "assembly.stp": source }));
    const diagnostics = await collectStepTimings(page);
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
    await expect.poll(() => diagnostics.length).toBe(1);
    ok(diagnostics[0]);
    expect(diagnostics[0].stages.occtInitMs).toBe(0);
    expect(diagnostics[0].stages.occtParseMs).toBe(0);
    expect(diagnostics[0].stages.directParseMs).toBeGreaterThan(0);
    expect(diagnostics[0].stages.packingMs).toBeGreaterThan(0);
    expect(diagnostics[0].stages.firstFrameMs).toBeGreaterThan(0);
    for (const value of Object.values(diagnostics[0].stages)) expect(value).toBeGreaterThanOrEqual(0);
    expect(wasm).toEqual([]);
    expect(errors).toEqual([]);
  });

  test(`concave polygon STEP preserves its preview without the CAD kernel at ${width}px`, async ({
    page,
    context,
    api,
    uniquePrompt,
  }) => {
    await page.setViewportSize({ width, height: 900 });
    const source = createPolygonStep(
      [
        [0, 0],
        [2, 0],
        [2, 1],
        [1, 1],
        [1, 2],
        [0, 2],
      ],
      true,
      1,
    );
    const archive = Buffer.from(zipSync({ "polygon.stp": new TextEncoder().encode(source) }));
    const wasm: string[] = [];
    const errors: string[] = [];
    page.on("request", (r) => {
      if (/occt-import-js.*\.wasm/.test(r.url())) wasm.push(r.url());
    });
    page.on("pageerror", (e) => errors.push(e.message));
    page.on("console", (message) => {
      if (message.type() === "error") errors.push(message.text());
    });
    await context.route("**/raw?*", (r) => r.fulfill({ contentType: "application/zip", body: archive }));
    const id = await createTaskAPI(api, uniquePrompt("Inspect [Polygon](/polygon.stpz)."));
    await waitForTaskState(api, id, "waiting");
    await page.goto(`/task/@${id}`);
    await page.getByRole("link", { name: "Polygon", exact: true }).click();
    await expect(page.getByText("20 triangles", { exact: true })).toBeVisible();
    await expect(page.getByLabel(/^3D model/)).toBeVisible();
    await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    await page.screenshot({ path: test.info().outputPath("polygon-prism.png") });
    expect(wasm).toEqual([]);
    expect(errors).toEqual([]);
  });
}

test("dense faceted STEP assembly imports and releases its worker", async ({ page, context, api, uniquePrompt }) => {
  const diagnostics = await collectStepTimings(page);
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
  await expect.poll(() => diagnostics.length).toBe(1);
  ok(diagnostics[0]);
  console.log(JSON.stringify({ facetedAssembly: { loadMs, diagnostics: diagnostics[0] } }));
  await expect.poll(() => page.workers().filter((worker) => worker.url().includes("stepWorker"))).toHaveLength(0);
  await expect(page.getByLabel(/^3D model/)).toBeVisible();
  await page.screenshot({ path: test.info().outputPath("dense-faceted-assembly.png") });
  expect(wasm).toEqual([]);
  expect(errors).toEqual([]);
});

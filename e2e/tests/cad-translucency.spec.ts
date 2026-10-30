// Browser coverage for camera-depth translucency, icon controls, mode changes, and curved STEP rendering.
import type { Locator, Page } from "@playwright/test";
import { ok } from "node:assert/strict";
import { readFileSync } from "node:fs";
import { strToU8, unzipSync, zipSync } from "three/addons/libs/fflate.module.js";
import { create3mfFixture } from "../../frontend/tests/cad-fixture";
import { test, expect, createTaskAPI, waitForTaskState } from "../helpers";

function depthFixture(): Buffer {
  const files = unzipSync(create3mfFixture());
  files["3D/3dmodel.model"] =
    strToU8(`<model unit="millimeter" xmlns="http://schemas.microsoft.com/3dmanufacturing/core/2015/02">
  <resources>
    <basematerials id="1"><base name="Red" displaycolor="#FF0000"/></basematerials>
    <object id="2" type="model" pid="1" pindex="0"><mesh>
      <vertices><vertex x="-5" y="-5" z="0"/><vertex x="5" y="-5" z="0"/>
        <vertex x="5" y="5" z="0"/><vertex x="-5" y="5" z="0"/></vertices>
      <triangles><triangle v1="0" v2="1" v3="2"/><triangle v1="0" v2="2" v3="3"/></triangles>
    </mesh></object>
  </resources>
  <build><item objectid="2" transform="1 0 0 0 1 0 0 0 1 -20 0 0"/>
    <item objectid="2" transform="1 0 0 0 1 0 0 0 1 20 0 0"/></build>
</model>`);
  return Buffer.from(zipSync(files));
}

async function renderedFrame(canvas: Locator): Promise<Buffer> {
  // Inspect the WebGL buffer in the same animation frame as rendering, before
  // the browser discards it. Element screenshots also include floating controls
  // and tooltips, and scroll the page while capturing the canvas.
  const png = await canvas.evaluate(async (element: HTMLCanvasElement) => {
    await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
    return element.toDataURL("image/png").slice("data:image/png;base64,".length);
  });
  return Buffer.from(png, "base64");
}

async function stableFrame(canvas: Locator): Promise<Buffer> {
  let frame = await renderedFrame(canvas);
  await expect
    .poll(async () => {
      const next = await renderedFrame(canvas);
      const same = next.equals(frame);
      frame = next;
      return same;
    })
    .toBe(true);
  return frame;
}

// Compare rendered alpha against the same opaque planes.
// The camera looks down at Z=0, so the nearer plane is lower in the image.
async function planeOpacities(page: Page, solid: Buffer, translucent: Buffer) {
  return page.evaluate(
    async ({ solid, translucent }) => {
      async function pixels(base64: string) {
        const bytes = Uint8Array.from(atob(base64), (char) => char.charCodeAt(0));
        const bitmap = await createImageBitmap(new Blob([bytes], { type: "image/png" }));
        const canvas = document.createElement("canvas");
        canvas.width = bitmap.width;
        canvas.height = bitmap.height;
        const ctx = canvas.getContext("2d")!;
        ctx.drawImage(bitmap, 0, 0);
        bitmap.close();
        return ctx.getImageData(0, 0, canvas.width, canvas.height);
      }
      const before = await pixels(solid);
      const after = await pixels(translucent);
      const { width, height, data } = before;
      const seen = new Uint8Array(width * height);
      function channel(data: Uint8ClampedArray, index: number): number {
        const value = data[index];
        if (value === undefined) throw new Error(`Image channel ${index} is missing`);
        return value;
      }
      const planes: { y: number; opacity: number }[] = [];
      function red(index: number) {
        return channel(data, index * 4) > 80 && channel(data, index * 4 + 1) < 80 && channel(data, index * 4 + 2) < 80;
      }
      for (let index = 0; index < seen.length; index++) {
        if (seen[index] || !red(index)) continue;
        const queue = [index];
        seen[index] = 1;
        let y = 0;
        let opacity = 0;
        for (let offset = 0; offset < queue.length; offset++) {
          const pixel = queue[offset];
          if (pixel === undefined) throw new Error("Plane traversal queue is incomplete");
          y += Math.floor(pixel / width);
          opacity += channel(after.data, pixel * 4 + 3) / channel(data, pixel * 4 + 3);
          for (const neighbor of [pixel - width, pixel + width, pixel - 1, pixel + 1]) {
            if (neighbor < 0 || neighbor >= width * height || seen[neighbor] || !red(neighbor)) continue;
            seen[neighbor] = 1;
            queue.push(neighbor);
          }
        }
        if (queue.length > 30) planes.push({ y: y / queue.length, opacity: opacity / queue.length });
      }
      return planes.sort((a, b) => a.y - b.y);
    },
    { solid: solid.toString("base64"), translucent: translucent.toString("base64") },
  );
}

for (const width of [390, 1280]) {
  test(`CAD translucency fades near surfaces and keeps icon controls usable at ${width}px`, async ({
    page,
    context,
    api,
    uniquePrompt,
  }) => {
    await page.setViewportSize({ width, height: 900 });
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    page.on("console", (message) => {
      if (message.type() === "error") errors.push(message.text());
    });
    await context.route("**/raw?*", (route) => route.fulfill({ contentType: "model/3mf", body: depthFixture() }));
    const id = await createTaskAPI(api, uniquePrompt("Inspect [Planes](/planes.3mf) and [Other](/other.3mf)."));
    await waitForTaskState(api, id, "waiting");
    await page.goto(`/task/@${id}`);
    await page.getByRole("link", { name: "Planes", exact: true }).click();
    await expect(page.getByText("4 triangles", { exact: true })).toBeVisible();
    const mode = page.getByRole("button", { name: /^Display mode:/ });
    await expect(mode).toHaveAccessibleName("Display mode: Solid. Switch to wireframe");
    const modeNode = await mode.elementHandle();
    await expect(mode.locator("svg")).toHaveCount(1);
    expect(await mode.innerText()).toBe("");
    await page.getByRole("button", { name: "View controls", exact: true }).focus();
    await page.getByRole("button", { name: "Hide axes", exact: true }).click();
    const canvas = page.getByLabel("3D model", { exact: true });
    const solid = await stableFrame(canvas);
    await mode.focus();
    await page.keyboard.press("Space");
    await expect(mode).toHaveAccessibleName("Display mode: Wireframe. Switch to translucent");
    await expect(mode).toBeFocused();
    expect((await stableFrame(canvas)).equals(solid)).toBe(false);

    await page.keyboard.press("Enter");
    await expect(mode).toHaveAccessibleName("Display mode: Translucent. Switch to solid");
    await expect(mode).toBeFocused();
    expect(await modeNode?.evaluate((node) => node.isConnected)).toBe(true);
    const transparent = await stableFrame(canvas);
    const planes = await planeOpacities(page, solid, transparent);
    expect(planes).toHaveLength(2);
    ok(planes[0]);
    ok(planes[1]);
    expect(planes[0].opacity).toBeGreaterThan(planes[1].opacity + 0.2);
    expect(planes[1].opacity).toBeGreaterThan(0.1);
    expect(planes[0].opacity).toBeLessThan(0.85);

    await mode.click();
    await expect(mode).toHaveAccessibleName("Display mode: Solid. Switch to wireframe");
    const restored = await stableFrame(canvas);
    const opaquePlanes = await planeOpacities(page, solid, restored);
    expect(opaquePlanes).toHaveLength(2);
    for (const plane of opaquePlanes) expect(plane.opacity).toBeCloseTo(1, 2);
    await mode.click();
    await mode.click();
    await canvas.hover();
    const beforeZoom = await stableFrame(canvas);
    await page.mouse.wheel(0, -150);
    await expect.poll(async () => (await renderedFrame(canvas)).equals(beforeZoom)).toBe(false);
    const zoomedTransparent = await stableFrame(canvas);
    await mode.click();
    const zoomedSolid = await stableFrame(canvas);
    const zoomed = await planeOpacities(page, zoomedSolid, zoomedTransparent);
    expect(zoomed).toHaveLength(2);
    for (const [i, plane] of planes.entries()) {
      const zoomedPlane = zoomed[i];
      ok(zoomedPlane);
      expect(Math.abs(zoomedPlane.opacity - plane.opacity)).toBeLessThan(0.04);
    }
    await mode.click();
    await mode.click();
    await page.getByRole("button", { name: "Full screen", exact: true }).click();
    await expect(mode).toHaveAccessibleName("Display mode: Translucent. Switch to solid");
    await page.getByRole("button", { name: "Exit full screen", exact: true }).click();
    // A hovered server warning can consume Escape before the document pane.
    if (width === 390) await page.getByRole("button", { name: "Close view", exact: true }).click();
    await page.getByRole("link", { name: "Other", exact: true }).click();
    await expect(page.getByText("4 triangles", { exact: true })).toBeVisible();
    // Display modes belong to each document; a new file starts solid.
    await expect(mode).toHaveAccessibleName("Display mode: Solid. Switch to wireframe");
    await mode.click();
    await expect(mode).toHaveAccessibleName("Display mode: Wireframe. Switch to translucent");
    await mode.click();
    await expect(mode).toHaveAccessibleName("Display mode: Translucent. Switch to solid");
    expect(errors).toEqual([]);
  });
}

test("curved STEP translucency renders without shader errors", async ({ page, context, api, uniquePrompt }) => {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("console", (message) => {
    if (message.type() === "error") errors.push(message.text());
  });
  const step = readFileSync("node_modules/occt-import-js/test/testfiles/conical-surface/conical-surface.step");
  await context.route("**/raw?*", (route) => route.fulfill({ contentType: "application/octet-stream", body: step }));
  const id = await createTaskAPI(api, uniquePrompt("Inspect [Cone](/cone.step)."));
  await waitForTaskState(api, id, "waiting");
  await page.goto(`/task/@${id}`);
  await page.getByRole("link", { name: "Cone", exact: true }).click();
  await expect(page.getByText("1,318 triangles", { exact: true })).toBeVisible();
  const canvas = page.getByLabel(/^3D model/);
  const solid = await stableFrame(canvas);
  const mode = page.getByRole("button", { name: /^Display mode:/ });
  await mode.click();
  await mode.click();
  expect((await stableFrame(canvas)).equals(solid)).toBe(false);
  const bounds = await canvas.boundingBox();
  if (!bounds) throw new Error("Missing CAD canvas");
  await page.mouse.move(bounds.x + bounds.width / 2, bounds.y + bounds.height / 2);
  await page.mouse.down();
  await page.mouse.move(bounds.x + bounds.width / 2 + 70, bounds.y + bounds.height / 2 + 30, { steps: 8 });
  await page.mouse.up();

  expect(errors).toEqual([]);
});

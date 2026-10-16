// Browser coverage for CAD orientation axes, keyboard toggling, camera tracking, and viewport containment.
import type { Page } from "@playwright/test";
import { test, expect, createTaskAPI, waitForTaskState } from "../helpers";
import { create3mfFixture } from "../../frontend/tests/cad-fixture";

async function axesFrame(page: Page): Promise<Buffer> {
  const bounds = await page.getByLabel(/^3D model/).boundingBox();
  if (!bounds) throw new Error("Missing CAD canvas");
  const clip = { x: bounds.x + bounds.width - 136, y: bounds.y + 8, width: 128, height: 128 };
  let frame = await page.screenshot({ clip });
  // OrbitControls damps rotation over several frames. Wait for a stable image,
  // rather than sleeping or comparing a frame while the camera still moves.
  // Software rendering on CI can take longer than the default five seconds
  // to render enough frames for damping to settle.
  await expect
    .poll(
      async () => {
        const next = await page.screenshot({ clip });
        const same = next.equals(frame);
        frame = next;
        return same;
      },
      { timeout: 15_000 },
    )
    .toBe(true);
  return frame;
}

async function frameTimes(page: Page) {
  return page.evaluate(async () => {
    const times: number[] = [];
    let previous = await new Promise<number>((resolve) => requestAnimationFrame(resolve));
    for (let i = 0; i < 90; i++) {
      const now = await new Promise<number>((resolve) => requestAnimationFrame(resolve));
      times.push(now - previous);
      previous = now;
    }
    times.sort((a, b) => a - b);
    return { medianMs: times[45], p95Ms: times[85] };
  });
}

for (const width of [390, 1280]) {
  test(`CAD axes follow orientation and toggle at ${width}px`, async ({ page, api, uniquePrompt }) => {
    await page.setViewportSize({ width, height: 900 });
    const errors: string[] = [];
    const requests: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    page.on("console", (message) => {
      if (message.type() === "error") errors.push(message.text());
    });
    page.on("request", (request) => requests.push(request.url()));
    await page.route("**/raw?*", async (route) => {
      await route.fulfill({ contentType: "model/3mf", body: Buffer.from(create3mfFixture()) });
    });
    const id = await createTaskAPI(api, uniquePrompt("Inspect [Model](/model.3mf)."));
    await waitForTaskState(api, id, "waiting");
    await page.goto(`/task/@${id}`);
    await page.getByRole("link", { name: "Model", exact: true }).click();
    await expect(page.getByText("2 triangles", { exact: true })).toBeVisible();
    const axes = page.getByRole("button", { name: "Axes", exact: true });
    await expect(axes).toHaveAttribute("aria-pressed", "true");
    const initial = await axesFrame(page);
    await page.screenshot({ path: test.info().outputPath("axes-enabled.png") });

    await axes.focus();
    await page.keyboard.press("Space");
    await expect(axes).toHaveAttribute("aria-pressed", "false");
    await expect(axes).toBeFocused();
    expect((await axesFrame(page)).equals(initial)).toBe(false);
    await page.screenshot({ path: test.info().outputPath("axes-disabled.png") });
    const hiddenTiming = width === 1280 ? await frameTimes(page) : null;
    await page.keyboard.press("Enter");
    await expect(axes).toHaveAttribute("aria-pressed", "true");
    expect((await axesFrame(page)).equals(initial)).toBe(true);
    if (width === 1280) {
      console.log(JSON.stringify({ width, axesHidden: hiddenTiming, axesVisible: await frameTimes(page) }));
    }

    const canvas = page.getByLabel("3D model with XYZ reference frame", { exact: true });
    const bounds = await canvas.boundingBox();
    if (!bounds) throw new Error("Missing CAD canvas");
    const center = { x: bounds.x + bounds.width / 2, y: bounds.y + bounds.height / 2 };
    await page.mouse.move(center.x, center.y);
    await page.mouse.down();
    await page.mouse.move(center.x + 50, center.y + 20, { steps: 8 });
    await page.mouse.up();
    const rotated = await axesFrame(page);
    expect(rotated.equals(initial)).toBe(false);
    await page.screenshot({ path: test.info().outputPath("axes-orbit.png") });

    await page.mouse.move(center.x, center.y);
    await page.mouse.down({ button: "right" });
    await page.mouse.move(center.x + 12, center.y + 8, { steps: 4 });
    await page.mouse.up({ button: "right" });
    await page.mouse.wheel(0, 100);
    expect((await axesFrame(page)).equals(rotated)).toBe(true);

    await page.setViewportSize({ width: width + 40, height: 940 });
    expect((await axesFrame(page)).equals(rotated)).toBe(true);
    const buttonBounds = await axes.boundingBox();
    expect(buttonBounds && buttonBounds.x >= 0 && buttonBounds.x + buttonBounds.width <= width + 40).toBeTruthy();
    await page.getByRole("button", { name: "Reset camera", exact: true }).click();
    expect((await axesFrame(page)).equals(initial)).toBe(true);
    expect(errors).toEqual([]);
    await test.info().attach("network-requests", { body: JSON.stringify(requests), contentType: "application/json" });
  });
}

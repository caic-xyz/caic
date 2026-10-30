// Browser coverage for CAD corner hover/tap actions, axis restoration, drag-through rotation, and camera tracking.
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

for (const width of [390, 1280]) {
  test.describe(`corner controls at ${width}px`, () => {
    test.use({ hasTouch: width === 390 });
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
      const canvas = page.getByLabel(/^3D model/);
      const bounds = await canvas.boundingBox();
      if (!bounds) throw new Error("Missing CAD canvas");
      const center = { x: bounds.x + bounds.width / 2, y: bounds.y + bounds.height / 2 };
      const axis = { x: bounds.x + bounds.width - 72, y: bounds.y + 72 };
      const trigger = page.getByRole("button", { name: "View controls", exact: true });
      const panel = page.getByRole("group", { name: "View controls", exact: true });
      async function reveal() {
        if (width === 390) await page.touchscreen.tap(axis.x, axis.y);
        else await page.mouse.move(axis.x, axis.y);
        await expect(panel).toBeVisible();
      }
      const initial = await axesFrame(page);

      await expect(panel).toBeHidden();
      await reveal();

      if (width === 390) await page.touchscreen.tap(center.x, center.y);
      else {
        // Moving through the gap into the action panel must not dismiss it.
        await page.mouse.move(axis.x, bounds.y + 138);
        await expect(panel).toBeVisible();
        await page.getByRole("button", { name: "Reset view", exact: true }).hover();
        await page.mouse.move(center.x, center.y);
      }
      await expect(panel).toBeHidden();
      if (width === 1280) {
        await reveal();
        await page.mouse.click(axis.x, axis.y);
        await expect(panel).toBeVisible();
        await page.mouse.move(center.x, center.y);
        await expect(panel).toBeVisible();
        await page.mouse.click(center.x, center.y);
        await expect(panel).toBeHidden();
      }
      await reveal();
      await page.getByRole("button", { name: "Hide axes", exact: true }).click();
      await expect(page.getByLabel("3D model", { exact: true })).toBeVisible();
      await expect(trigger).toBeFocused();
      await expect(trigger.locator("svg")).toBeVisible();
      expect((await axesFrame(page)).equals(initial)).toBe(false);

      if (width === 390) await trigger.tap();
      else await trigger.hover();
      await page.getByRole("button", { name: "Reset view", exact: true }).click();
      await expect(page.getByLabel("3D model", { exact: true })).toBeVisible();
      if (width === 1280) {
        // Restored pointer focus must not make subsequent hover panels sticky.
        await trigger.hover();
        await expect(panel).toBeVisible();
        await page.mouse.move(center.x, center.y);
        await expect(panel).toBeHidden();
      }
      await trigger.press("Enter");
      await page.keyboard.press("Tab");
      await page.keyboard.press("Tab");
      await expect(page.getByRole("button", { name: "Show axes", exact: true })).toBeFocused();
      await page.keyboard.press("Space");
      await expect(page.getByLabel("3D model with XYZ reference frame", { exact: true })).toBeVisible();
      await page.getByRole("button", { name: /^Display mode:/ }).focus();
      await page.mouse.move(center.x, center.y);
      expect((await axesFrame(page)).equals(initial)).toBe(true);

      // A drag that starts on the axes reaches OrbitControls and never opens actions.
      await page.mouse.move(axis.x, axis.y);
      await page.mouse.down();
      await page.mouse.move(axis.x - 50, axis.y + 20, { steps: 8 });
      await page.mouse.up();
      await expect(panel).toBeHidden();
      const rotated = await axesFrame(page);
      expect(rotated.equals(initial)).toBe(false);

      await page.mouse.move(center.x, center.y);
      await page.mouse.down({ button: "right" });
      await page.mouse.move(center.x + 12, center.y + 8, { steps: 4 });
      await page.mouse.up({ button: "right" });
      await page.mouse.wheel(0, 100);
      expect((await axesFrame(page)).equals(rotated)).toBe(true);

      await page.setViewportSize({ width: width + 40, height: 940 });
      expect((await axesFrame(page)).equals(rotated)).toBe(true);
      const buttonBounds = await trigger.boundingBox();
      expect(buttonBounds && buttonBounds.x >= 0 && buttonBounds.x + buttonBounds.width <= width + 40).toBeTruthy();
      await trigger.focus();
      await page.getByRole("button", { name: "Reset view", exact: true }).click();
      await page.getByRole("button", { name: /^Display mode:/ }).focus();
      await page.mouse.move(center.x, center.y);
      expect((await axesFrame(page)).equals(initial)).toBe(true);
      await trigger.focus();
      await page.keyboard.press("Tab");
      await page.keyboard.press("Escape");
      await expect(panel).toBeHidden();
      await expect(trigger).toBeFocused();
      await expect(page.getByTestId("document-pane")).toBeVisible();
      await page.getByRole("button", { name: "Full screen", exact: true }).click();
      await trigger.focus();
      await page.keyboard.press("Escape");
      await expect(panel).toBeHidden();
      await expect(page.getByRole("button", { name: "Exit full screen", exact: true })).toBeVisible();
      await page.keyboard.press("Escape");
      await expect(page.getByRole("button", { name: "Full screen", exact: true })).toBeVisible();
      expect(errors).toEqual([]);
      await test.info().attach("network-requests", { body: JSON.stringify(requests), contentType: "application/json" });
    });
  });
}

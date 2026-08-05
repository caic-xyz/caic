// Generate screenshots for the documentation site.
//
// Run with: pnpm exec playwright test --config e2e/playwright.config.ts gen-screenshots
// Output: e2e/screenshots/frontend/
import { test, expect, createTaskAPI, waitForTaskState, convertPngsToWebp } from "../helpers";
import type { Locator } from "@playwright/test";
import type { Harness } from "../../sdk/caic/ts/v1/types.gen";
import path from "path";
import { fileURLToPath } from "url";

const screenshotDir = path.join(
  path.dirname(fileURLToPath(import.meta.url)),
  "..",
  "screenshots",
  "frontend",
);

async function requiredBox(locator: Locator) {
  const box = await locator.boundingBox();
  if (!box) throw new Error("expected visible layout control");
  return box;
}

test.describe.configure({ mode: "serial" });

test("generate documentation screenshots", async ({ page, api }) => {
  // AVIF encoding via ffmpeg is slow; the default 60s is too tight.
  test.setTimeout(120_000);
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto("/");

  // Wait for repos to load.
  await expect(
    page
      .getByTestId("repo-chips")
      .locator("[data-testid^='chip-label-']")
      .first(),
  ).toBeVisible();

  // Screenshot 1: Settings — realistic home-relative mounts with layout checks.
  await api.updatePreferences({
    settings: {
      autoFixOnCIFailure: false,
      autoFixOnPROpen: false,
      customMounts: [
        { hostPath: "~/.claude", containerPath: "", enabled: true, readOnly: false },
        { hostPath: "~/.cache/huggingface", containerPath: "", enabled: true, readOnly: false },
      ],
    },
  });
  await page.setViewportSize({ width: 1600, height: 900 });
  await page.goto("/settings");
  const mountRows = page.getByTestId("custom-mount-row");
  await expect(mountRows).toHaveCount(2);
  for (let i = 0; i < 2; i++) {
    const row = mountRows.nth(i);
    const hostInput = await requiredBox(row.getByLabel("Host path"));
    const arrow = await requiredBox(row.getByTestId("mapping-arrow"));
    const containerInput = await requiredBox(row.getByLabel("Container path"));
    const readOnly = await requiredBox(row.getByTestId("mount-read-only"));
    const remove = await requiredBox(row.getByRole("button"));

    expect(hostInput.x + hostInput.width).toBeLessThanOrEqual(arrow.x);
    expect(arrow.x + arrow.width).toBeLessThanOrEqual(containerInput.x);
    expect(containerInput.x + containerInput.width).toBeLessThanOrEqual(readOnly.x);
    expect(readOnly.x + readOnly.width).toBeLessThanOrEqual(remove.x);
    expect(Math.abs(arrow.y + arrow.height / 2 - (containerInput.y + containerInput.height / 2))).toBeLessThanOrEqual(1);
    expect(Math.abs(readOnly.y + readOnly.height / 2 - (containerInput.y + containerInput.height / 2))).toBeLessThanOrEqual(1);
  }
  await page.screenshot({
    path: path.join(screenshotDir, "settings-mounts.png"),
  });
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto("/");
  await expect(
    page
      .getByTestId("repo-chips")
      .locator("[data-testid^='chip-label-']")
      .first(),
  ).toBeVisible();

  // Create tasks that will reach different states for a populated task list.
  // Task 1: demo scenario — "fix" triggers demo mode (will complete with tool uses).
  const id1 = await createTaskAPI(
    api,
    "Fix token expiry bug in auth middleware",
  );
  await waitForTaskState(api, id1, "waiting", 30_000);

  // Task 2: plan mode — "plan" triggers plan mode.
  const id2 = await createTaskAPI(
    api,
    "Plan the rate limiting implementation for API endpoints",
  );
  await waitForTaskState(api, id2, "has_plan", 30_000);

  // Task 3: ask mode — "which" triggers ask mode.
  const id3 = await createTaskAPI(
    api,
    "Which storage backend should we use for session data?",
  );
  await waitForTaskState(api, id3, "asking", 30_000);

  // Task 4: widget — "FAKE_WIDGET" triggers widget mode.
  const id4 = await createTaskAPI(
    api,
    "FAKE_WIDGET Explain light refraction in water",
  );
  await waitForTaskState(api, id4, "waiting", 30_000);

  // Reload to get fresh state.
  await page.goto("/");
  await expect(
    page
      .getByTestId("repo-chips")
      .locator("[data-testid^='chip-label-']")
      .first(),
  ).toBeVisible();

  // Wait for task cards to appear.
  await expect(page.locator("[data-task-id]").first()).toBeVisible({
    timeout: 10_000,
  });

  // Screenshot 1: Interacting with an agent — task detail with tool uses.
  const bugFixCard = page.locator(`[data-task-id="${id1}"]`);
  await expect(bugFixCard).toBeVisible({ timeout: 10_000 });
  await bugFixCard.click();
  await page.waitForTimeout(500);
  await page.screenshot({
    path: path.join(screenshotDir, "task-detail.png"),
  });

  // Screenshot 3: Plan mode.
  const planCard = page.locator(`[data-task-id="${id2}"]`);
  if ((await planCard.count()) > 0) {
    await planCard.click();
    await page.waitForTimeout(500);
    await page.screenshot({
      path: path.join(screenshotDir, "task-plan.png"),
    });
  }

  // Screenshot 4: Ask mode.
  const askCard = page.locator(`[data-task-id="${id3}"]`);
  if ((await askCard.count()) > 0) {
    await askCard.click();
    await page.waitForTimeout(500);
    await page.screenshot({
      path: path.join(screenshotDir, "task-ask.png"),
    });
  }

  // Screenshot 5: Widget — generative UI with interactive SVG diagram.
  const widgetCard = page.locator(`[data-task-id="${id4}"]`);
  if ((await widgetCard.count()) > 0) {
    await widgetCard.click();
    const iframe = page.locator("iframe[title='light_refraction_in_water']");
    await expect(iframe).toBeVisible({ timeout: 10_000 });
    // Wait for iframe content to render (scripts run after widgetDone).
    await page.waitForTimeout(1500);
    await page.screenshot({
      path: path.join(screenshotDir, "task-widget.png"),
    });

    // Animate the angle slider and capture frames for AVIF animation.
    const frame = page.frameLocator(
      "iframe[title='light_refraction_in_water']",
    );
    const slider = frame.locator("#slider");
    if ((await slider.count()) > 0) {
      const fs = await import("fs");
      const tmpDir = path.join(screenshotDir, ".widget-frames");
      fs.mkdirSync(tmpDir, { recursive: true });

      // Sweep angle from 5° to 85° in steps, capturing each frame.
      const angles = [
        5, 10, 15, 20, 25, 30, 35, 40, 45, 50, 55, 60, 65, 70, 75, 80, 85,
      ];
      for (let i = 0; i < angles.length; i++) {
        await slider.fill(String(angles[i]));
        await page.waitForTimeout(80);
        await page.screenshot({
          path: path.join(tmpDir, `frame-${String(i).padStart(3, "0")}.png`),
        });
      }
      // Reverse sweep for smooth loop.
      for (let i = angles.length - 2; i > 0; i--) {
        await slider.fill(String(angles[i]));
        await page.waitForTimeout(80);
        await page.screenshot({
          path: path.join(
            tmpDir,
            `frame-${String(angles.length + (angles.length - 2 - i)).padStart(3, "0")}.png`,
          ),
        });
      }

      // AVIF encoding is for documentation assets and can be too slow for CI.
      if (!process.env.CI) {
        const { execFileSync } = await import("child_process");
        try {
          execFileSync(
            "ffmpeg",
            [
              "-y",
              "-framerate",
              "10",
              "-i",
              `${tmpDir}/frame-%03d.png`,
              "-c:v",
              "libaom-av1",
              "-crf",
              "30",
              "-b:v",
              "0",
              "-pix_fmt",
              "yuv420p",
              path.join(screenshotDir, "task-widget.avif"),
            ],
            { stdio: "pipe", timeout: 60_000 },
          );
        } catch (e) {
          console.error("AVIF encoding failed:", (e as Error).message);
        }
      }
      // Clean up frames.
      fs.rmSync(tmpDir, { recursive: true, force: true });
    }
  }

  // Screenshot 6: VNC display — fake IDE screenshot in noVNC viewer.
  const harnesses = await api.listHarnesses();
  const repos = await api.listRepos();
  const vncResp = await api.createTask({
    initialPrompt: { text: "Show the VNC display" },
    repos: [{ name: repos[0].path }],
    harness: harnesses[0].name as Harness,
    display: true,
  });
  await waitForTaskState(api, vncResp.id, "waiting", 30_000);

  // Reload to get fresh state.
  await page.goto("/");
  await expect(
    page
      .getByTestId("repo-chips")
      .locator("[data-testid^='chip-label-']")
      .first(),
  ).toBeVisible();

  // Find the VNC task and navigate to it.
  const vncTask = await api.getTask(vncResp.id);
  expect(vncTask).toBeTruthy();

  // Click the VNC task card (client-side navigation, no page reload).
  const vncCard = page.locator(`[data-task-id="${vncTask!.id}"]`);
  await expect(vncCard).toBeVisible({ timeout: 10_000 });
  await vncCard.click();
  await page.waitForTimeout(500);

  // Click the VNC link to open the viewer.
  const vncLink = page.getByRole("link", { name: "VNC" });
  await expect(vncLink).toBeVisible({ timeout: 10_000 });
  await vncLink.click();

  // Wait for noVNC canvas to appear and render the fake screenshot.
  await expect(page.locator("canvas")).toBeVisible({ timeout: 15_000 });
  // Give noVNC time to complete the RFB handshake and render the framebuffer.
  await page.waitForTimeout(2000);
  await page.screenshot({
    path: path.join(screenshotDir, "task-vnc.png"),
  });

  // Screenshot 7: Mobile — task detail at phone viewport.
  await page.goto("/");
  await expect(
    page
      .getByTestId("repo-chips")
      .locator("[data-testid^='chip-label-']")
      .first(),
  ).toBeVisible();
  const bugFixCard2 = page.locator(`[data-task-id="${id1}"]`);
  await expect(bugFixCard2).toBeVisible({ timeout: 10_000 });
  await bugFixCard2.click();
  await page.waitForTimeout(500);
  await page.setViewportSize({ width: 390, height: 844 });
  await page.waitForTimeout(300);
  // Verify the context menu toggle is visible at mobile width.
  const contextToggle = page.locator("[aria-label='Context actions']");
  await expect(contextToggle).toBeVisible({ timeout: 3_000 });
  await page.screenshot({
    path: path.join(screenshotDir, "task-detail-mobile.png"),
  });
  // Restore desktop viewport.
  await page.setViewportSize({ width: 1280, height: 800 });

  // Screenshot 8: Scrolled task list — bottom alpha fade cues more cards.
  const scrollTaskIds: string[] = [];
  for (let i = 1; i <= 8; i++) {
    const id = await createTaskAPI(
      api,
      `Scroll gradient demo task ${String(i).padStart(2, "0")}`,
    );
    scrollTaskIds.push(id);
  }
  await Promise.all(
    scrollTaskIds.map((id) => waitForTaskState(api, id, "waiting", 30_000)),
  );

  await page.goto("/");
  await expect(
    page
      .getByTestId("repo-chips")
      .locator("[data-testid^='chip-label-']")
      .first(),
  ).toBeVisible();
  await expect(page.locator("[data-task-id]").first()).toBeVisible({
    timeout: 10_000,
  });
  const taskList = page.getByTestId("task-list");
  await expect.poll(async () =>
    taskList.evaluate((el) => el.scrollHeight - el.clientHeight),
  ).toBeGreaterThan(0);
  await taskList.evaluate((el) => {
    el.scrollTop = Math.min(260, el.scrollHeight - el.clientHeight);
    el.dispatchEvent(new Event("scroll", { bubbles: true }));
  });
  await expect.poll(async () =>
    taskList.evaluate((el) => el.scrollTop),
  ).toBeGreaterThan(0);
  await expect.poll(async () =>
    taskList.evaluate((el) => getComputedStyle(el, "::before").opacity),
  ).toBe("1");
  await page.screenshot({
    path: path.join(screenshotDir, "task-list-scrolled.png"),
  });

  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/");
  await expect(
    page
      .getByTestId("repo-chips")
      .locator("[data-testid^='chip-label-']")
      .first(),
  ).toBeVisible();
  const mobileTaskList = page.getByTestId("task-list");
  await expect.poll(async () =>
    mobileTaskList.evaluate((el) => el.scrollHeight - el.clientHeight),
  ).toBeGreaterThan(0);
  await mobileTaskList.evaluate((el) => {
    el.scrollTop = Math.min(280, el.scrollHeight - el.clientHeight);
    el.dispatchEvent(new Event("scroll", { bubbles: true }));
  });
  await expect.poll(async () =>
    mobileTaskList.evaluate((el) => el.scrollTop),
  ).toBeGreaterThan(0);
  await expect.poll(async () =>
    mobileTaskList.evaluate((el) => getComputedStyle(el, "::before").opacity),
  ).toBe("1");
  await page.screenshot({
    path: path.join(screenshotDir, "task-list-scrolled-mobile.png"),
  });

  await convertPngsToWebp(screenshotDir);
});

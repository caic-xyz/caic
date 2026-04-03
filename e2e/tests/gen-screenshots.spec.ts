// Generate screenshots for the documentation site.
//
// Run with: pnpm exec playwright test --config e2e/playwright.config.ts gen-screenshots
// Output: e2e/screenshots/
import { test, expect, createTaskAPI, waitForTaskState } from "../helpers";
import path from "path";
import { fileURLToPath } from "url";

const screenshotDir = path.join(
  path.dirname(fileURLToPath(import.meta.url)),
  "..",
  "screenshots",
);

test.describe.configure({ mode: "serial" });

test("generate documentation screenshots", async ({ page, api }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto("/");

  // Wait for repos to load.
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
  const bugFixCard = page.locator("[data-task-id]", {
    hasText: "Fix token expiry",
  });
  await bugFixCard.first().click();
  await page.waitForTimeout(500);
  await page.screenshot({
    path: path.join(screenshotDir, "task-detail.png"),
  });

  // Screenshot 3: Plan mode.
  const planCard = page.locator("[data-task-id]", {
    hasText: "Plan the rate",
  });
  if ((await planCard.count()) > 0) {
    await planCard.first().click();
    await page.waitForTimeout(500);
    await page.screenshot({
      path: path.join(screenshotDir, "task-plan.png"),
    });
  }

  // Screenshot 4: Ask mode.
  const askCard = page.locator("[data-task-id]", {
    hasText: "Which storage",
  });
  if ((await askCard.count()) > 0) {
    await askCard.first().click();
    await page.waitForTimeout(500);
    await page.screenshot({
      path: path.join(screenshotDir, "task-ask.png"),
    });
  }

  // Screenshot 5: Widget — generative UI with interactive SVG diagram.
  const widgetCard = page.locator("[data-task-id]", {
    hasText: "Explain light refraction",
  });
  if ((await widgetCard.count()) > 0) {
    await widgetCard.first().click();
    const iframe = page.locator("iframe[title='light_refraction_in_water']");
    await expect(iframe).toBeVisible({ timeout: 10_000 });
    // Wait for iframe content to render (scripts run after widgetDone).
    await page.waitForTimeout(1500);
    await page.screenshot({
      path: path.join(screenshotDir, "task-widget.png"),
    });

    // Animate the angle slider and capture frames for AVIF animation.
    const frame = page.frameLocator("iframe[title='light_refraction_in_water']");
    const slider = frame.locator("#slider");
    if ((await slider.count()) > 0) {
      const fs = await import("fs");
      const tmpDir = path.join(screenshotDir, ".widget-frames");
      fs.mkdirSync(tmpDir, { recursive: true });

      // Sweep angle from 5° to 85° in steps, capturing each frame.
      const angles = [5, 10, 15, 20, 25, 30, 35, 40, 45, 50, 55, 60, 65, 70, 75, 80, 85];
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
          path: path.join(tmpDir, `frame-${String(angles.length + (angles.length - 2 - i)).padStart(3, "0")}.png`),
        });
      }

      // Encode frames to AVIF animation via ffmpeg.
      const { execSync } = await import("child_process");
      try {
        execSync(
          `ffmpeg -y -framerate 10 -i "${tmpDir}/frame-%03d.png" ` +
          `-c:v libaom-av1 -crf 30 -b:v 0 -pix_fmt yuv420p ` +
          `"${path.join(screenshotDir, "task-widget.avif")}"`,
          { stdio: "pipe", timeout: 60_000 },
        );
      } catch (e) {
        console.error("AVIF encoding failed:", (e as Error).message);
      }
      // Clean up frames.
      fs.rmSync(tmpDir, { recursive: true, force: true });
    }
  }

  // Screenshot 6: Mobile — task detail at phone viewport.
  await bugFixCard.first().click();
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

  // Convert remaining PNG screenshots to lossless AVIF (skip if AVIF already exists, e.g. animation).
  const fs2 = await import("fs");
  const { execSync: exec2 } = await import("child_process");
  const pngs = fs2.readdirSync(screenshotDir).filter((f: string) => f.endsWith(".png"));
  for (const png of pngs) {
    const src = path.join(screenshotDir, png);
    const dst = path.join(screenshotDir, png.replace(/\.png$/, ".avif"));
    if (fs2.existsSync(dst)) {
      fs2.unlinkSync(src);
      continue;
    }
    try {
      exec2(
        `ffmpeg -y -i "${src}" -c:v libaom-av1 -still-picture 1 -crf 0 -b:v 0 "${dst}"`,
        { stdio: "pipe", timeout: 60_000 },
      );
      fs2.unlinkSync(src);
    } catch (e) {
      console.error(`AVIF conversion failed for ${png}:`, (e as Error).message);
    }
  }
});

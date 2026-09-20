// Generate screenshots for the documentation site.
//
// Run with: make screenshots-check (or make screenshots-update to accept changes)
// Output: e2e/screenshots/frontend/{desktop,mobile}/
import { test, expect, createTaskAPI, waitForTaskState, convertPngsToWebp } from "../helpers";
import { captureScreenshot, prepareVisualPage, screenshotDir, screenshotRoot } from "../visual";
import type { Locator } from "@playwright/test";
import path from "path";

async function requiredBox(locator: Locator) {
  const box = await locator.boundingBox();
  if (!box) throw new Error("expected visible layout control");
  return box;
}

async function stabilizeWidget(locator: Locator) {
  await locator.evaluate(async (body) => {
    const style = document.createElement("style");
    style.textContent = `
      *, *::before, *::after {
        animation: none !important;
        transition: none !important;
      }
      svg line, svg path, svg polygon {
        shape-rendering: crispEdges;
      }
    `;
    body.append(style);
    await document.fonts.ready;
    await new Promise<void>((resolve) => {
      requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
    });
  });
}

test.describe.configure({ mode: "serial" });

test("generate documentation screenshots", async ({ page, api }) => {
  // AVIF encoding via ffmpeg is slow; the default 60s is too tight.
  test.setTimeout(120_000);
  await prepareVisualPage(page);
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto("/");

  // Wait for repos to load.
  await expect(page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first()).toBeVisible();

  // Screenshot 1: Settings — realistic home-relative mounts with layout checks.
  await api.updatePreferences({
    settings: {
      autoFixOnCIFailure: false,
      autoFixOnPROpen: false,
      purgeDelay: 15_000_000_000,
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
    expect(Math.abs(arrow.y + arrow.height / 2 - (containerInput.y + containerInput.height / 2))).toBeLessThanOrEqual(
      1,
    );
    expect(
      Math.abs(readOnly.y + readOnly.height / 2 - (containerInput.y + containerInput.height / 2)),
    ).toBeLessThanOrEqual(1);
  }
  await page.evaluate(() => {
    if (document.activeElement instanceof HTMLElement) document.activeElement.blur();
  });
  await captureScreenshot(page, "desktop", "settings-mounts.png");
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto("/");
  await expect(page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first()).toBeVisible();

  // Create tasks that will reach different states for a populated task list.
  // Task 1: a long title and mapped repository state exercise the dense task
  // card and detail-header layouts in every visual capture.
  const repos = await api.listRepos();
  const harnesses = await api.listHarnesses();
  expect(repos.length).toBeGreaterThanOrEqual(2);
  expect(harnesses.length).toBeGreaterThan(0);
  const detailTask = await api.createTask({
    initialPrompt: { text: "Fix OAuth security hardening and migration across service boundaries" },
    repos: [{ name: repos[0].path }, { name: repos[1].path }],
    harness: harnesses[0].name,
  });
  const id1 = detailTask.id;
  await waitForTaskState(api, id1, "waiting", 30_000);

  // Task 2: plan mode — "plan" triggers plan mode.
  const id2 = await createTaskAPI(api, "Plan the rate limiting implementation for API endpoints");
  await waitForTaskState(api, id2, "has_plan", 30_000);

  // Task 3: ask mode — "which" triggers ask mode.
  const id3 = await createTaskAPI(api, "Which storage backend should we use for session data?");
  await waitForTaskState(api, id3, "asking", 30_000);

  // Task 4: widget — "FAKE_WIDGET" triggers widget mode.
  const id4 = await createTaskAPI(api, "FAKE_WIDGET Explain light refraction in water");
  await waitForTaskState(api, id4, "waiting", 30_000);

  // Reload to get fresh state.
  await page.goto("/");
  await expect(page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first()).toBeVisible();

  // Wait for task cards to appear.
  await expect(page.locator("[data-task-id]").first()).toBeVisible({
    timeout: 10_000,
  });

  // Screenshot 1: Interacting with an agent — task detail with tool uses.
  const bugFixCard = page.locator(`[data-task-id="${id1}"]`);
  await expect(bugFixCard).toBeVisible({ timeout: 10_000 });
  await bugFixCard.click();
  await expect(page.getByTestId("task-setup").locator("summary")).toContainText(/(?:\d+ms|\d+\.\d+s)/);
  const toolSummary = page.getByText("4/4 tools: Read, Edit ×2, Bash");
  await expect(toolSummary).toBeVisible({ timeout: 10_000 });
  await toolSummary.click();
  await expect(page.getByTestId("tool-duration").filter({ hasText: /^180ms$/ })).toBeVisible();
  await expect(page.getByTestId("tool-duration").filter({ hasText: /^0:01$/ })).toBeVisible();
  await expect(page.getByTestId("turn-duration").filter({ hasText: /^0:02$/ })).toBeVisible();
  const desktopHeaderStats = page.getByTestId("task-detail-header").getByTestId("repo-state-diff-stats");
  await expect(desktopHeaderStats).toHaveCount(2);
  for (let i = 0; i < 2; i++) {
    await expect(desktopHeaderStats.nth(i)).toBeVisible();
  }
  await captureScreenshot(page, "desktop", "task-detail.png");

  // Screenshot 2: Repository changes with an expanded per-file patch.
  await page
    .getByTestId("task-detail-header")
    .getByRole("link", { name: /changed files.*commit ahead of upstream/ })
    .first()
    .click();
  await page.getByTitle("Collapse sidebar").click();
  await expect(page.getByTitle("Expand sidebar")).toBeVisible();
  await page.setViewportSize({ width: 800, height: 720 });
  expect(page.viewportSize()).toEqual({ width: 800, height: 720 });
  await expect(page.getByText("Repository changes", { exact: true })).toBeVisible();
  await expect(page.getByText("caic-0", { exact: true }).first()).toBeVisible();
  await expect(page.getByText("origin/main", { exact: true }).first()).toBeVisible();
  await expect(page.getByText("1 commit ahead", { exact: true })).toBeVisible();
  await expect(page.getByText("Commits ahead (1)", { exact: true })).toBeVisible();
  await expect(page.getByText("Uncommitted changes (1)", { exact: true })).toBeVisible();
  await expect(page.getByText("0 commits ahead · 1 behind", { exact: true })).toBeVisible();
  await expect(page.getByText("Commits ahead (0)", { exact: true })).toBeVisible();
  await expect(page.getByText("Uncommitted changes (2)", { exact: true })).toBeVisible();
  const appDiff = page.getByRole("button", {
    name: "frontend/src/App.tsx",
  });
  await expect(appDiff).toHaveAttribute("aria-expanded", "false");
  await appDiff.click();
  await expect(appDiff).toHaveAttribute("aria-expanded", "true");
  await expect(page.getByText("-  <span>Task status</span>", { exact: true })).toBeVisible();
  await expect(page.getByText("+  <span>Repository changes</span>", { exact: true })).toBeVisible();
  await expect
    .poll(() =>
      page.evaluate(() => ({
        documentFits: document.documentElement.scrollWidth <= innerWidth,
        paneFits: (() => {
          const pane = document.querySelector<HTMLElement>('[data-testid="detail-pane"]');
          return pane !== null && pane.scrollWidth <= pane.clientWidth;
        })(),
      })),
    )
    .toEqual({ documentFits: true, paneFits: true });
  await page.mouse.move(0, 0);
  await captureScreenshot(page, "desktop", "task-repository-changes.png");
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.getByTitle("Expand sidebar").click();
  await expect(page.getByTitle("Collapse sidebar")).toBeVisible();

  // Screenshot 3: Plan mode.
  const planCard = page.locator(`[data-task-id="${id2}"]`);
  if ((await planCard.count()) > 0) {
    await planCard.click();
    await expect(page.getByTestId("plan-content")).toBeVisible();
    await captureScreenshot(page, "desktop", "task-plan.png");
  }

  // Screenshot 4: Ask mode.
  const askCard = page.locator(`[data-task-id="${id3}"]`);
  if ((await askCard.count()) > 0) {
    await askCard.click();
    await expect(page.getByTestId("ask-option-In-memory (sync.Map)")).toBeVisible();
    await captureScreenshot(page, "desktop", "task-ask.png");
  }

  // Screenshot 5: Widget — generative UI with interactive SVG diagram.
  const widgetCard = page.locator(`[data-task-id="${id4}"]`);
  if ((await widgetCard.count()) > 0) {
    await widgetCard.click();
    await expect(page.getByText("CI: passed", { exact: true })).toBeVisible({
      timeout: 30_000,
    });
    const iframe = page.locator("iframe[title='light_refraction_in_water']");
    await expect(iframe).toBeVisible({ timeout: 10_000 });

    const frame = page.frameLocator("iframe[title='light_refraction_in_water']");
    const widgetBody = frame.locator("body");
    const slider = frame.locator("#slider");
    await expect(slider).toBeVisible();
    await stabilizeWidget(widgetBody);
    const messageArea = page.getByTestId("task-message-area");
    await messageArea.evaluate((el) => {
      el.scrollTop = 0;
    });
    await expect.poll(() => messageArea.evaluate((el) => el.scrollTop)).toBe(0);
    await captureScreenshot(page, "desktop", "task-widget.png");

    // Animate the angle slider and capture frames for AVIF animation.
    if ((await slider.count()) > 0) {
      const fs = await import("fs");
      const tmpDir = path.join(screenshotDir("desktop"), ".widget-frames");
      fs.mkdirSync(tmpDir, { recursive: true });

      // Rasterize the animation in a fixed top-level viewport. Capturing the
      // iframe in place makes text rasterization depend on its outer-page
      // subpixel position.
      const widgetHTML = await frame.locator("html").evaluate((html) => html.outerHTML);
      const animationPage = await page.context().newPage();
      await prepareVisualPage(animationPage);
      await animationPage.setViewportSize({ width: 640, height: 800 });
      await animationPage.setContent(`<!doctype html>${widgetHTML}`);
      const animationBody = animationPage.locator("body");
      const animationSlider = animationPage.locator("#slider");
      await expect(animationSlider).toBeVisible();
      await stabilizeWidget(animationBody);

      // Sweep angle from 5° to 85° in steps, capturing each frame.
      const angles = [5, 10, 15, 20, 25, 30, 35, 40, 45, 50, 55, 60, 65, 70, 75, 80, 85];
      const settleWidgetFrame = () =>
        animationBody.evaluate(
          () =>
            new Promise<void>((resolve) => {
              requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
            }),
        );
      for (let i = 0; i < angles.length; i++) {
        await animationSlider.fill(String(angles[i]));
        await settleWidgetFrame();
        await animationBody.screenshot({
          caret: "hide",
          path: path.join(tmpDir, `frame-${String(i).padStart(3, "0")}.png`),
        });
      }
      // Reverse sweep for smooth loop.
      for (let i = angles.length - 2; i > 0; i--) {
        await animationSlider.fill(String(angles[i]));
        await settleWidgetFrame();
        await animationBody.screenshot({
          caret: "hide",
          path: path.join(tmpDir, `frame-${String(angles.length + (angles.length - 2 - i)).padStart(3, "0")}.png`),
        });
      }
      await animationPage.close();

      const { execFileSync } = await import("child_process");
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
          "-threads",
          "1",
          "-row-mt",
          "0",
          "-tiles",
          "1x1",
          "-f",
          "avif",
          path.join(screenshotDir("desktop"), "task-widget.avif"),
        ],
        { stdio: "pipe", timeout: 60_000 },
      );
      // Clean up frames.
      fs.rmSync(tmpDir, { recursive: true, force: true });
    }
  }

  // Screenshot 6: VNC display — fake IDE screenshot in noVNC viewer.
  const vncResp = await api.createTask({
    initialPrompt: { text: "Show the VNC display" },
    repos: [{ name: repos[0].path }],
    harness: harnesses[0].name,
    display: true,
  });
  await waitForTaskState(api, vncResp.id, "waiting", 30_000);

  // Reload to get fresh state.
  await page.goto("/");
  await expect(page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first()).toBeVisible();

  // Find the VNC task and navigate to it.
  const vncTask = await api.getTask(vncResp.id);
  expect(vncTask).toBeTruthy();

  // Click the VNC task card (client-side navigation, no page reload).
  const vncCard = page.locator(`[data-task-id="${vncTask!.id}"]`);
  await expect(vncCard).toBeVisible({ timeout: 10_000 });
  await vncCard.click();

  // Click the VNC link to open the viewer.
  const vncLink = page.getByRole("link", { name: "VNC" });
  await expect(vncLink).toBeVisible({ timeout: 10_000 });
  await vncLink.click();

  // Wait for noVNC canvas to appear and render the fake screenshot.
  const canvas = page.locator("canvas");
  await expect(canvas).toBeVisible({ timeout: 15_000 });
  await expect
    .poll(() =>
      canvas.evaluate((el) => {
        if (!(el instanceof HTMLCanvasElement)) return false;
        const context = el.getContext("2d");
        if (!context || el.width === 0 || el.height === 0) return false;
        return context.getImageData(0, 0, el.width, el.height).data.some((v) => v !== 0);
      }),
    )
    .toBe(true);
  await captureScreenshot(page, "desktop", "task-vnc.png");

  // Screenshot 7: Mobile — task detail at phone viewport.
  await page.goto("/");
  await expect(page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first()).toBeVisible();
  const bugFixCard2 = page.locator(`[data-task-id="${id1}"]`);
  await expect(bugFixCard2).toBeVisible({ timeout: 10_000 });
  await bugFixCard2.click();
  await page.setViewportSize({ width: 390, height: 844 });
  // Verify the context menu toggle is visible at mobile width.
  const contextToggle = page.locator("[aria-label='Context actions']");
  await expect(contextToggle).toBeVisible({ timeout: 3_000 });
  await captureScreenshot(page, "mobile", "task-detail-mobile.png");

  // Screenshot 8: Dense task-detail header at the width where a larger phone
  // or narrow desktop pane needs compact repository state markers.
  await page.setViewportSize({ width: 525, height: 320 });
  const detailHeader = page.getByTestId("task-detail-header");
  const headerStats = detailHeader.getByTestId("repo-state-diff-stats");
  await expect(headerStats).toHaveCount(2);
  for (let i = 0; i < 2; i++) {
    await expect(headerStats.nth(i)).toBeHidden();
  }
  // Task statistics is a router link to the task's stats route, not a button.
  const taskStatistics = detailHeader.getByRole("link", { name: "Task statistics" });
  await expect(taskStatistics).toBeVisible();
  const titleBox = await detailHeader
    .locator(":scope > span")
    .filter({ hasText: "Fix OAuth security hardening" })
    .first()
    .boundingBox();
  expect(titleBox?.width).toBeLessThanOrEqual(128);
  const [repositoryStateBox, taskStatisticsBox] = await Promise.all([
    headerStats.first().locator("xpath=../..").boundingBox(),
    taskStatistics.boundingBox(),
  ]);
  expect(repositoryStateBox).not.toBeNull();
  expect(taskStatisticsBox).not.toBeNull();
  expect(
    Math.abs(
      repositoryStateBox!.y + repositoryStateBox!.height / 2 - taskStatisticsBox!.y - taskStatisticsBox!.height / 2,
    ),
  ).toBeLessThanOrEqual(1);
  await expect.poll(() => detailHeader.evaluate((el) => el.scrollWidth <= el.clientWidth)).toBe(true);
  await captureScreenshot(page, "mobile", "task-detail-header-compact.png");
  // Restore desktop viewport.
  await page.setViewportSize({ width: 1280, height: 800 });

  // Screenshot 9: Scrolled task list — bottom alpha fade cues more cards.
  const scrollTaskIds: string[] = [];
  for (let i = 1; i <= 8; i++) {
    const id = await createTaskAPI(api, `Scroll gradient demo task ${String(i).padStart(2, "0")}`);
    scrollTaskIds.push(id);
  }
  await Promise.all(scrollTaskIds.map((id) => waitForTaskState(api, id, "waiting", 30_000)));

  await page.goto("/");
  await expect(page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first()).toBeVisible();
  await expect(page.locator("[data-task-id]").first()).toBeVisible({
    timeout: 10_000,
  });
  await expect
    .poll(
      async () => {
        const statuses = await page
          .getByTestId("ci-status")
          .evaluateAll((nodes) => nodes.map((node) => (node as HTMLElement).dataset.status));
        return statuses.length > 0 && statuses.every((status) => status === "success");
      },
      { timeout: 15_000 },
    )
    .toBe(true);
  const taskList = page.getByTestId("task-list");
  await expect.poll(async () => taskList.evaluate((el) => el.scrollHeight - el.clientHeight)).toBeGreaterThan(0);
  await taskList.evaluate((el) => {
    el.scrollTop = Math.min(260, el.scrollHeight - el.clientHeight);
    el.dispatchEvent(new Event("scroll", { bubbles: true }));
  });
  await expect.poll(async () => taskList.evaluate((el) => el.scrollTop)).toBeGreaterThan(0);
  await expect.poll(async () => taskList.evaluate((el) => getComputedStyle(el, "::before").opacity)).toBe("1");
  await captureScreenshot(page, "desktop", "task-list-scrolled.png");

  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/");
  await expect(page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first()).toBeVisible();
  const mobileTaskList = page.getByTestId("task-list");
  await expect.poll(async () => mobileTaskList.evaluate((el) => el.scrollHeight - el.clientHeight)).toBeGreaterThan(0);
  await mobileTaskList.evaluate((el) => {
    el.scrollTop = Math.min(280, el.scrollHeight - el.clientHeight);
    el.dispatchEvent(new Event("scroll", { bubbles: true }));
  });
  await expect.poll(async () => mobileTaskList.evaluate((el) => el.scrollTop)).toBeGreaterThan(0);
  await expect.poll(async () => mobileTaskList.evaluate((el) => getComputedStyle(el, "::before").opacity)).toBe("1");
  await captureScreenshot(page, "mobile", "task-list-scrolled-mobile.png");

  // Screenshot 10: Native subagents — inline lifecycle cards anchored to the
  // parent narration that preceded each run settling.
  const nativeId = await createTaskAPI(api, "Delegate the auth review to three parallel subagents");
  await waitForTaskState(api, nativeId, "waiting", 30_000);
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto("/");
  await expect(page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first()).toBeVisible();
  const nativeTaskCard = page.locator(`[data-task-id="${nativeId}"]`);
  await expect(nativeTaskCard).toBeVisible({ timeout: 10_000 });
  await nativeTaskCard.click();
  const nativeCards = page.getByTestId("native-subagent-card");
  await expect(nativeCards).toHaveCount(4);
  // Expand the completed run so the showcase includes the card content.
  await nativeCards.first().locator("summary").click();
  await expect(nativeCards.first().getByText("Read me before you judge me.")).toBeVisible();
  const nativeMessageArea = page.getByTestId("task-message-area");
  await nativeMessageArea.evaluate((el) => {
    el.scrollTop = 0;
  });
  await captureScreenshot(page, "desktop", "task-native-subagents.png");

  await convertPngsToWebp(screenshotRoot);
});

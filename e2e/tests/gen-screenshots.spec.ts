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

  // Task 4: another demo (will complete) — "update" triggers demo mode.
  const id4 = await createTaskAPI(
    api,
    "Update CI pipeline to run tests in parallel",
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
});

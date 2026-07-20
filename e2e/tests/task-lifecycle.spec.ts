// End-to-end tests for the task lifecycle using a fake backend.
import { test, expect, waitForTaskState, fillContentEditable, createTaskAPI } from "../helpers";

test("create task, verify streaming text and result, then purge", async ({ page, api }) => {
  await page.goto("/");

  // Wait for repos to load (a chip appears in the strip).
  await expect(page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first()).toBeVisible();

  // Use a unique prompt to avoid collisions with parallel tests.
  const prompt = `e2e lifecycle ${Date.now()}`;

  // Fill prompt and submit.
  await fillContentEditable(page.getByTestId("prompt-input"), prompt);
  await page.getByTestId("submit-task").click();

  // Task creation navigates directly to TaskDetail; the initial event stream
  // should render there without requiring a task-list click/reopen.
  await expect(page).toHaveURL(/\/task\//);

  // Wait for the assistant message from the fake agent. The fake backend emits
  // streaming text deltas followed by the final assistant message containing a
  // joke. The first joke in the rotation is always the same.
  await expect(
    page.getByText("Why do programmers prefer dark mode?").first(),
  ).toBeVisible({ timeout: 15_000 });

  // Wait for the result message.
  await expect(page.locator("strong", { hasText: "Done" })).toBeVisible({
    timeout: 10_000,
  });

  // Resolve the task ID and wait for "waiting" state via API before clicking
  // stop. The UI may show the result message before the SSE delivers the
  // "waiting" state; clicking stop while still "running" triggers a
  // window.confirm() that Playwright auto-dismisses as false, skipping the
  // stop.
  const tasks = await api.listTasks();
  const task = tasks.find((t) => t.initialPrompt === prompt);
  expect(task).toBeTruthy();
  await waitForTaskState(api, task!.id, "waiting");

  // The Stop button (trash can) appears on hover over the task card in the sidebar.
  // Scope to the specific card to avoid strict-mode violations from parallel tests.
  const taskCard = page.locator("[data-task-id]", { hasText: prompt });
  await taskCard.hover();
  const stopBtn = taskCard.getByTestId("stop-task");
  await expect(stopBtn).toBeVisible({ timeout: 15_000 });

  // Wait for the SSE event to update the frontend state to "waiting".
  // If the UI still shows "running", the stop button triggers a
  // window.confirm() that Playwright auto-dismisses as false.
  await expect(taskCard.getByTestId("state-badge")).toHaveText("waiting", { timeout: 15_000 });

  await stopBtn.click();

  // Poll API until our task is "stopped".
  await waitForTaskState(api, task!.id, "stopped");

  // Now purge the stopped task via API and verify it reaches "purged".
  await api.purgeTask(task!.id);
  await waitForTaskState(api, task!.id, "purged");
});

test("setup logs remain visible after task-detail replay", async ({ page, api }, testInfo) => {
  const id = await createTaskAPI(api, "Verify setup log replay");
  await waitForTaskState(api, id, "waiting", 30_000);

  await page.goto(`/task/@${id}`);
  const setup = page.getByTestId("task-setup");
  const setupLogs = page.getByTestId("task-setup-logs");
  const messageArea = page.getByTestId("task-message-area");
  await expect(setup).toBeVisible();
  await expect(messageArea.getByTestId("task-setup")).toBeVisible();
  await expect(setup).not.toHaveAttribute("open");
  await expect(setupLogs).not.toBeVisible();

  await setup.getByText("Setup logs").click();
  await expect(setupLogs).toContainText("Fake runtime setup complete");

  // Reopening the detail stream preserves setup output and restores its
  // collapsed default instead of burying it in a historical turn.
  await page.reload();
  await expect(setup).toBeVisible();
  await expect(messageArea.getByTestId("task-setup")).toBeVisible();
  await expect(setup).not.toHaveAttribute("open");
  await page.screenshot({ path: testInfo.outputPath("task-setup-logs.png") });
});

test("task detail desktop layout avoids extra gutters and pane-level horizontal scrolling", async ({ page, api }) => {
  await page.setViewportSize({ width: 950, height: 800 });
  const id = await createTaskAPI(api, "Fix a desktop overflow regression");
  await waitForTaskState(api, id, "waiting", 30_000);

  await page.goto(`/task/@${id}`);
  const messageArea = page.getByTestId("task-message-area");
  const detailPane = page.getByTestId("detail-pane");
  const taskList = page.getByTestId("task-list");
  await expect(messageArea).toBeVisible();

  await expect.poll(async () => messageArea.evaluate((el) => getComputedStyle(el).overflowX)).toBe("hidden");
  await expect.poll(async () => taskList.evaluate((list) => {
    const detail = document.querySelector('[data-testid="detail-pane"]');
    if (!detail) return Number.POSITIVE_INFINITY;
    return detail.getBoundingClientRect().left - list.getBoundingClientRect().right;
  })).toBeLessThanOrEqual(8);

  await page.getByTitle("Collapse sidebar").click();
  const expandButton = page.getByTitle("Expand sidebar");
  await expect(expandButton).toBeVisible();
  await expect.poll(async () => expandButton.evaluate((button) => {
    const detail = document.querySelector('[data-testid="detail-pane"]');
    if (!detail) return Number.POSITIVE_INFINITY;
    return detail.getBoundingClientRect().left - button.getBoundingClientRect().right;
  })).toBeLessThanOrEqual(8);
  await expect(detailPane).toBeVisible();
});

test("add-repo dropdown is visible and not clipped by overflow", async ({ page }) => {
  await page.goto("/");

  // Wait for repos to load.
  await expect(page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first()).toBeVisible();

  // The add-repo button should be present (at least one repo is not yet selected).
  const addBtn = page.getByTestId("add-repo-button");
  await expect(addBtn).toBeVisible();

  // Open the dropdown.
  await addBtn.click();
  const dropdown = page.getByTestId("add-repo-dropdown");
  await expect(dropdown).toBeVisible();

  // The dropdown must not be clipped: its bounding box must be fully within the viewport.
  const box = await dropdown.boundingBox();
  expect(box).not.toBeNull();
  const viewportSize = page.viewportSize();
  expect(viewportSize).not.toBeNull();
  expect(box!.y).toBeGreaterThanOrEqual(0);
  expect(box!.y + box!.height).toBeLessThanOrEqual(viewportSize!.height);
});

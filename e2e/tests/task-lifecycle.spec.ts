// End-to-end tests for the task lifecycle using a fake backend.
import { test, expect, waitForTaskState } from "../helpers";

test("create task, verify streaming text and result, then terminate", async ({ page, api }) => {
  await page.goto("/");

  // Wait for repos to load (a chip appears in the strip).
  await expect(page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first()).toBeVisible();

  // Use a unique prompt to avoid collisions with parallel tests.
  const prompt = `e2e lifecycle ${Date.now()}`;

  // Fill prompt and submit.
  await page.getByTestId("prompt-input").fill(prompt);
  await page.getByTestId("submit-task").click();

  // Click the task card to open TaskDetail.
  await page.getByText(prompt).first().click();

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

  // The Terminate button appears on hover over the task card in the sidebar.
  // Scope to the specific card to avoid strict-mode violations from parallel tests.
  const taskCard = page.locator("[data-task-id]", { hasText: prompt });
  await taskCard.hover();
  const terminateBtn = taskCard.getByTestId("terminate-task");
  await expect(terminateBtn).toBeVisible({ timeout: 15_000 });

  // Accept the confirmation dialog and click Terminate.
  page.once("dialog", (d) => d.accept());
  await terminateBtn.click();

  // Poll API until our task is "terminated" — uses typed helper.
  const tasks = await api.listTasks();
  const task = tasks.find((t) => t.initialPrompt === prompt);
  expect(task).toBeTruthy();
  await waitForTaskState(api, task!.id, "terminated");
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

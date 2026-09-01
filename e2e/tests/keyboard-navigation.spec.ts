// End-to-end keyboard-only navigation through repository selection, task creation, and task focus.

import { test, expect, waitForTaskState } from "../helpers";

test("completes the primary task flow using only the keyboard", async ({ page, api, uniquePrompt }) => {
  await page.goto("/");
  await expect(page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first()).toBeVisible();

  await page.keyboard.press("r");
  const repositoryFilter = page.getByRole("combobox", { name: "Add a repository" });
  await expect(repositoryFilter).toBeFocused();
  const repository = page.getByRole("option").first();
  const repositoryName = await repository.textContent();
  if (!repositoryName) throw new Error("Repository option has no label");
  await page.keyboard.type(repositoryName);
  await page.keyboard.press("Enter");
  await expect(repositoryFilter).not.toBeVisible();

  await page.keyboard.press("/");
  const newTaskPrompt = page.getByTestId("prompt-input");
  await expect(newTaskPrompt).toBeFocused();
  const prompt = uniquePrompt("keyboard navigation");
  await page.keyboard.type(prompt);
  await page.keyboard.press("Enter");

  await expect(page).toHaveURL(/\/task\//);
  const taskRoute = new URL(page.url()).pathname.split("/").at(-1);
  const taskId = taskRoute?.replace(/^@/, "").split("+")[0];
  if (!taskId) throw new Error("Task URL has no task ID");
  await waitForTaskState(api, taskId, "waiting", 30_000);

  const detailPrompt = page.getByTestId("task-detail-prompt");
  const taskCard = page.locator("[data-task-id]", { hasText: prompt });
  await expect(detailPrompt).toBeVisible();
  await expect(taskCard.getByText("waiting", { exact: true })).toBeVisible();
  await page.keyboard.press("/");
  await expect(detailPrompt).toBeFocused();
  await expect(taskCard).toBeVisible();

  await detailPrompt.press("Shift+Tab");
  await expect(taskCard).toBeFocused();
  await taskCard.press("Tab");
  await expect(detailPrompt).toBeFocused();
  await detailPrompt.press("Escape");
  await expect(taskCard).toBeFocused();

  await taskCard.press("F1");
  const shortcutsDialog = page.getByTestId("keyboard-shortcuts-dialog");
  await expect(shortcutsDialog).toBeVisible();
  await shortcutsDialog.press("Escape");
  await expect(page.getByTestId("keyboard-shortcuts-dialog")).not.toBeVisible();
  await expect(taskCard).toBeFocused();
});

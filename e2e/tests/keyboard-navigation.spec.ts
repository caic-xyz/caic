// End-to-end keyboard-only navigation through repository selection, task creation, and task focus.

import { test, expect, waitForTaskState } from "../helpers";

test("completes the primary task flow using only the keyboard", async ({ page, api, uniquePrompt }) => {
  await page.goto("/");
  await expect(page.getByTestId("new-task-form")).toHaveCSS("overflow", "visible");
  const repoChips = page.getByTestId("repo-chips");
  const repoLabel = repoChips.locator("[data-testid^='chip-label-']").first();
  const repoRemove = repoChips.locator("[data-testid^='chip-remove-']").first();
  await expect(repoLabel).toBeVisible();
  await expect(repoLabel.locator("..")).toHaveCSS("overflow", "visible");
  await repoLabel.focus();
  await expect(repoLabel).toHaveCSS("box-shadow", /rgb/);
  await expect(repoLabel).toHaveCSS("border-radius", /px 0px 0px/);
  await repoRemove.focus();
  await expect(repoRemove).toHaveCSS("box-shadow", /rgb/);
  await expect(repoRemove).toHaveCSS("border-radius", /0px .*px .*px 0px/);
  const newTaskPrompt = page.getByTestId("prompt-input");
  await page.keyboard.press("Escape");
  await expect(newTaskPrompt).toBeFocused();

  await newTaskPrompt.press("F2");
  await expect(repoLabel).toBeFocused();
  await page.keyboard.press("Tab");
  await expect(repoRemove).toBeFocused();
  await page.keyboard.press("Tab");
  const addRepository = page.getByTestId("add-repo-button");
  await expect(addRepository).toBeFocused();
  await page.keyboard.press("Enter");
  const repositoryFilter = page.getByRole("combobox", { name: "Manage repositories" });
  await expect(repositoryFilter).toBeFocused();
  await expect(addRepository).toHaveCSS("box-shadow", /rgb/);
  const repository = page.getByRole("option", { selected: false }).first();
  const repositoryName = await repository.textContent();
  if (!repositoryName) throw new Error("Repository option has no label");
  await page.keyboard.type(repositoryName);
  await page.keyboard.press("Enter");
  await expect(repositoryFilter).not.toBeVisible();
  await expect(addRepository).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(newTaskPrompt).toBeFocused();
  const promptRow = newTaskPrompt.locator("..");
  await expect(promptRow).toHaveCSS("box-shadow", /inset/);
  await expect(promptRow).toHaveCSS("border-radius", "12px");
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

  await page.reload();
  await expect(detailPrompt).toBeFocused();
  await expect(detailPrompt.locator("..")).toHaveCSS("box-shadow", /inset/);
  // The collapsed new-task form must stay out of the tab order.
  await expect(page.getByTestId("submit-task")).not.toBeVisible();
  await detailPrompt.press("Shift+Tab");
  await expect(taskCard).toBeFocused();
  await taskCard.press("Shift+Tab");
  await expect(page.locator("[data-testid='new-task-form'] :focus")).toHaveCount(0);
  await taskCard.focus();
  await taskCard.press("Tab");
  await expect(detailPrompt).toBeFocused();
  await detailPrompt.press("Escape");
  await expect(page).toHaveURL("/");
  await expect(newTaskPrompt).toBeFocused();
  await newTaskPrompt.press("Escape");
  await expect(newTaskPrompt).toBeFocused();

  await newTaskPrompt.press("F1");
  const shortcutsDialog = page.getByTestId("keyboard-shortcuts-dialog");
  await expect(shortcutsDialog).toBeVisible();
  await shortcutsDialog.press("Escape");
  await expect(page.getByTestId("keyboard-shortcuts-dialog")).not.toBeVisible();
  await expect(newTaskPrompt).toBeFocused();
});

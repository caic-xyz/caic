// End-to-end keyboard-only navigation through repository selection, task creation, and task focus.

import { test, expect, createTaskAPI, fillContentEditable, waitForTaskState } from "../helpers";

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
  const repository = page
    .getByRole("listbox", { name: "Manage repositories" })
    .getByRole("option", { selected: false })
    .first();
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
  await page.keyboard.press("/");
  await expect(detailPrompt).toBeFocused();
  await taskCard.focus();
  await taskCard.press("Tab");
  await expect(detailPrompt).toBeFocused();
  await detailPrompt.press("Escape");
  await expect(page).toHaveURL("/");
  await expect(newTaskPrompt).toBeFocused();
  await page.getByTestId("new-task-button").focus();
  await page.keyboard.press("/");
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

test("Ctrl+Enter submits from a focused feature toggle without toggling it", async ({ page, uniquePrompt }) => {
  await page.goto("/");
  const prompt = uniquePrompt("shortcut from feature toggle");
  await fillContentEditable(page.getByTestId("prompt-input"), prompt);
  await expect(page.getByTestId("submit-task")).toBeEnabled();
  const toggle = page.getByRole("checkbox", { name: "Enable CAIC MCP delegation for this task" });
  await expect(toggle).toBeChecked();
  await toggle.focus();

  await page.keyboard.press("Enter");
  await expect(toggle).not.toBeChecked();
  await expect(page).toHaveURL("/");

  await expect(toggle).toBeFocused();
  await page.keyboard.press("Control+Enter");
  await expect(page).toHaveURL(/\/task\//);
});

test("Page Up and Page Down switch tasks while preserving prompt drafts", async ({ page, api, uniquePrompt }) => {
  const secondId = await createTaskAPI(api, uniquePrompt("page navigation second"));
  const firstId = await createTaskAPI(api, uniquePrompt("page navigation first"));
  await waitForTaskState(api, firstId, "waiting", 30_000);
  await waitForTaskState(api, secondId, "waiting", 30_000);
  await page.goto(`/task/@${firstId}`);
  const prompt = page.getByTestId("task-detail-prompt");
  await expect(prompt).toBeFocused();
  await prompt.pressSequentially("draft for the first task");

  await prompt.press("PageDown");
  await expect(page).toHaveURL(new RegExp(`/task/@${secondId}\\+`));
  await expect(prompt).toBeFocused();
  await expect(prompt).toHaveText("");
  await prompt.pressSequentially("draft for the second task");

  await prompt.press("PageUp");
  await expect(page).toHaveURL(new RegExp(`/task/@${firstId}\\+`));
  await expect(prompt).toBeFocused();
  await expect(prompt).toHaveText("draft for the first task");

  await prompt.press("Shift+Tab");
  await expect(page.locator(`[data-task-id="${firstId}"]`)).toBeFocused();
  await page.keyboard.press("PageDown");
  await expect(page.locator(`[data-task-id="${secondId}"]`)).toBeFocused();
  await page.keyboard.press("Tab");
  await expect(prompt).toBeFocused();
  await expect(prompt).toHaveText("draft for the second task");
});

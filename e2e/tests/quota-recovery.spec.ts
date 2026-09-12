// E2E tests for guided quota recovery on desktop and mobile task paths.

import type { APIClient, Page } from "../helpers";
import {
  test,
  expect,
  createTaskAPI,
  waitForTaskState,
  fillContentEditable,
} from "../helpers";

async function createQuotaBlockedTask(api: APIClient, prompt: string) {
  const id = await createTaskAPI(api, prompt);
  await waitForTaskState(api, id, "waiting", 30_000);
  let source = await api.getTask(id);
  await expect(async () => {
    source = await api.getTask(id);
    expect(source.rateLimit?.blocked).toBe(true);
  }).toPass({ timeout: 10_000, intervals: [250] });
  return source;
}

async function finishRecovery(page: Page, api: APIClient, sourceID: string, prompt: string) {
  const dialog = page.getByTestId("fork-dialog");
  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole("heading", { name: "Continue after quota limit" })).toBeVisible();
  await expect(page.getByTestId("fork-prompt-input")).toContainText("Continue this task after quota exhaustion");
  await fillContentEditable(page.getByTestId("fork-prompt-input"), prompt);
  await page.getByTestId("fork-submit").click();

  let forkedID = "";
  await expect(async () => {
    const tasks = await api.listTasks();
    const forked = tasks.find((task) => task.initialPrompt === prompt);
    expect(forked).toBeTruthy();
    forkedID = forked!.id;
  }).toPass({ timeout: 10_000, intervals: [250] });

  const info = await api.getTaskInfo(forkedID);
  expect(info.recorded.forkedFromTaskID).toBe(sourceID);
  return { forkedID, info };
}

test("desktop quota recovery continues in a fork and preserves the source", async ({ page, api, uniquePrompt }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  const source = await createQuotaBlockedTask(api, uniquePrompt("FAKE_QUOTA_RECOVERY desktop source"));
  expect(source.rateLimit?.quotaGroup).toBe("claudecode");
  await page.goto(`/task/@${source.id}`);

  const sourceCard = page.locator(`[data-task-id="${source.id}"]`);
  await expect(sourceCard.getByTestId("quota-countdown")).toContainText("out of quota");
  await sourceCard.getByTestId("quota-recovery-card").click();

  const harnessSelect = page.getByRole("combobox", { name: "Fork Harness" });
  await expect(harnessSelect.locator("option")).toHaveText([
    "codex — Available · Recommended",
    "pi — Quota status unknown",
    "claude — Same exhausted quota",
  ]);
  await expect(harnessSelect).toHaveValue("codex");
  await harnessSelect.selectOption("pi");
  await expect(page.getByTestId("fork-target-status")).toContainText("Quota status unknown");
  await harnessSelect.selectOption("codex");

  const forkPrompt = uniquePrompt("edited desktop quota handoff");
  const { forkedID, info } = await finishRecovery(page, api, source.id, forkPrompt);
  expect(info.recorded.harness).toBe("codex");
  await expect(page).toHaveURL(new RegExp(`/task/@${forkedID}`));

  const unchangedSource = await api.getTask(source.id);
  expect(unchangedSource.initialPrompt).toBe(source.initialPrompt);
  expect(unchangedSource.state).toBe("waiting");
  expect(unchangedSource.rateLimit?.blocked).toBe(true);
  expect(unchangedSource.runtime.id).toBe(source.runtime.id);
});

test("mobile quota recovery opens from task detail and navigates to the fork", async ({ page, api, uniquePrompt }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const source = await createQuotaBlockedTask(api, uniquePrompt("FAKE_QUOTA_RECOVERY mobile source"));
  await page.goto(`/task/@${source.id}`);

  await expect(page.getByTestId("task-list")).toBeHidden();
  const recovery = page.getByTestId("quota-recovery-detail");
  await expect(recovery).toContainText("Agent quota exhausted");
  await recovery.getByTestId("quota-recovery-detail-action").click();

  const harnessSelect = page.getByRole("combobox", { name: "Fork Harness" });
  await expect(harnessSelect.locator("option")).toHaveText([
    "codex — Available · Recommended",
    "pi — Quota status unknown",
    "claude — Same exhausted quota",
  ]);
  await expect(harnessSelect).toHaveValue("codex");
  await expect(page.getByTestId("fork-target-status")).toBeVisible();

  const forkPrompt = uniquePrompt("edited mobile quota handoff");
  const { forkedID } = await finishRecovery(page, api, source.id, forkPrompt);
  await expect(page).toHaveURL(new RegExp(`/task/@${forkedID}`));
  await expect(page.getByTestId("detail-pane")).toBeVisible();
  await expect(page.getByTestId("task-list")).toBeHidden();
  expect((await api.getTask(source.id)).rateLimit?.blocked).toBe(true);
});

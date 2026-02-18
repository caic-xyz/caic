// End-to-end tests for the task lifecycle using a fake backend.
import { test, expect, waitForTaskState } from "../helpers";

test("create task, verify streaming text and result, then terminate", async ({ page, api }) => {
  await page.goto("/");

  // Wait for repos to load (select gets an option).
  await expect(page.locator("select option")).not.toHaveCount(0);

  // Use a unique prompt to avoid collisions with parallel tests.
  const prompt = `e2e lifecycle ${Date.now()}`;

  // Fill prompt and submit.
  await page.fill('textarea[placeholder="Describe a task..."]', prompt);
  await page.getByRole("button", { name: "Start a new container with this prompt" }).click();

  // Click the task card to open TaskView.
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

  // The Terminate button (icon with title) should appear once the task is
  // in waiting state.
  const terminateBtn = page.locator('button[title="Terminate"]');
  await expect(terminateBtn).toBeVisible({ timeout: 15_000 });

  // Click Terminate.
  await terminateBtn.click();

  // Poll API until our task is "terminated" — uses typed helper.
  const tasks = await api.listTasks();
  const task = tasks.find((t) => t.task === prompt);
  expect(task).toBeTruthy();
  await waitForTaskState(api, task!.id, "terminated");
});

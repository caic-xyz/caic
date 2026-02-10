// End-to-end tests for the task lifecycle using a fake backend.
import { test, expect } from "@playwright/test";

test("create task, wait for result, and terminate", async ({ page }) => {
  await page.goto("/");

  // Wait for repos to load (select gets an option).
  await expect(page.locator("select option")).not.toHaveCount(0);

  // Fill prompt and submit.
  await page.fill('input[placeholder="Describe a task..."]', "e2e test task");
  await page.getByRole("button", { name: "Run" }).click();

  // Click the task card to open TaskView.
  await page.getByText("e2e test task").first().click();

  // Wait for the assistant message from the fake agent.
  await expect(page.getByText("I completed the requested task.")).toBeVisible({
    timeout: 15_000,
  });

  // Wait for the result message.
  await expect(page.locator("strong", { hasText: "Done" })).toBeVisible({
    timeout: 10_000,
  });

  // The Terminate button should appear once the task is in waiting state.
  const terminateBtn = page.getByRole("button", { name: "Terminate" });
  await expect(terminateBtn).toBeVisible({ timeout: 10_000 });

  // Click Terminate.
  await terminateBtn.click();

  // Poll API until task state is "terminated".
  await expect(async () => {
    const resp = await page.request.get("/api/v1/tasks");
    const tasks = await resp.json();
    expect(tasks[0].state).toBe("terminated");
  }).toPass({ timeout: 15_000, intervals: [500] });
});

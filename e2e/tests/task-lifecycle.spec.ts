// End-to-end tests for the task lifecycle using a fake backend.
import { test, expect } from "@playwright/test";

test("create task, verify streaming text and result, then terminate", async ({ page }) => {
  await page.goto("/");

  // Wait for repos to load (select gets an option).
  await expect(page.locator("select option")).not.toHaveCount(0);

  // Fill prompt and submit.
  await page.fill('textarea[placeholder="Describe a task..."]', "e2e test task");
  await page.getByRole("button", { name: "Run" }).click();

  // Click the task card to open TaskView.
  await page.getByText("e2e test task").first().click();

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

  // The Terminate button should appear once the task is in waiting state.
  const terminateBtn = page.getByRole("button", { name: "Terminate" });
  await expect(terminateBtn).toBeVisible({ timeout: 10_000 });

  // Click Terminate.
  await terminateBtn.click();

  // Poll API until our task is "terminated".
  await expect(async () => {
    const resp = await page.request.get("/api/v1/tasks");
    const tasks = await resp.json();
    const t = tasks.find((t: { task: string }) => t.task === "e2e test task");
    expect(t).toBeTruthy();
    expect(t.state).toBe("terminated");
  }).toPass({ timeout: 15_000, intervals: [500] });
});

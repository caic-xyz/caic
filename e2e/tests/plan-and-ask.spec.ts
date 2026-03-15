// E2E tests for ExitPlanMode "Clear and execute plan" button and AskUserQuestion card.
import { test, expect, waitForTaskState, type APIClient } from "../helpers";

// Submit a task via the UI and poll until the task ID is available via the API.
async function submitAndGetId(
  page: import("@playwright/test").Page,
  api: APIClient,
  prompt: string,
): Promise<string> {
  await page.getByTestId("prompt-input").fill(prompt);
  await page.getByTestId("submit-task").click();
  let taskId = "";
  await expect(async () => {
    const tasks = await api.listTasks();
    const task = tasks.find((t) => t.initialPrompt === prompt);
    expect(task).toBeTruthy();
    taskId = task!.id;
  }).toPass({ timeout: 10_000, intervals: [500] });
  return taskId;
}

test("FAKE_PLAN: clear-plan button appears and restarts task", async ({ page, api }) => {
  await page.goto("/");

  // Wait for repos to load.
  await expect(
    page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first(),
  ).toBeVisible();

  const prompt = `FAKE_PLAN e2e ${Date.now()}`;
  const taskId = await submitAndGetId(page, api, prompt);

  // Open task detail.
  await page.getByText(prompt).first().click();

  // Wait for the task to reach has_plan state.
  await waitForTaskState(api, taskId, "has_plan", 20_000);

  // The "Clear and execute plan" button must be visible.
  const clearBtn = page.getByTestId("clear-and-execute-plan");
  await expect(clearBtn).toBeVisible({ timeout: 10_000 });

  // The plan content should be rendered.
  await expect(page.getByTestId("plan-content")).toBeVisible();

  // Fill in a non-empty prompt so restart doesn't try to read the container plan file.
  await page.getByTestId("task-detail-form").getByPlaceholder("Send message to agent...").fill("execute now");

  // Click the button; the task restarts and settles back to waiting.
  await clearBtn.click();
  await waitForTaskState(api, taskId, "waiting", 20_000);
});

test("FAKE_ASK: AskUserQuestion card renders, accepts answer, submits", async ({ page, api }) => {
  await page.goto("/");

  // Wait for repos to load.
  await expect(
    page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first(),
  ).toBeVisible();

  const prompt = `FAKE_ASK e2e ${Date.now()}`;
  const taskId = await submitAndGetId(page, api, prompt);

  // Open task detail.
  await page.getByText(prompt).first().click();

  // Wait for the task to reach asking state.
  await waitForTaskState(api, taskId, "asking", 20_000);

  // The question and option chips must be visible.
  await expect(page.getByText("Which approach should I use?")).toBeVisible({ timeout: 10_000 });
  await expect(page.getByTestId("ask-option-In-memory (sync.Map)")).toBeVisible();
  await expect(page.getByTestId("ask-option-Redis")).toBeVisible();

  // Select the first option and submit.
  await page.getByTestId("ask-option-In-memory (sync.Map)").click();
  await page.getByTestId("ask-submit").click();

  // The answer is forwarded to the fake agent as a new prompt; it replies
  // with a joke and the task transitions back to waiting.
  await waitForTaskState(api, taskId, "waiting", 20_000);

  // Verify the agent processed the answer by checking for its joke response.
  // The ask turn is collapsed once the new turn completes, so we verify the
  // round-trip via the visible response rather than the collapsed submitted
  // answer div.
  await expect(
    page.getByText("A SQL query walks into a bar").first(),
  ).toBeVisible({ timeout: 10_000 });
});

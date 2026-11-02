// Screenshots of the prompt input with short and long text to verify button layout.
import {
  test,
  expect,
  createTaskAPI,
  waitForCISettle,
  waitForTaskState,
  convertPngsToWebp,
  type APIClient,
} from "../helpers";
import { captureScreenshot, prepareVisualPage, screenshotRoot } from "../visual";

/** Creates the task cards the prompt captures show, newest first. */
async function seedTaskList(api: APIClient) {
  const repos = await api.listRepos();
  const firstRepo = repos[0];
  if (!firstRepo) throw new Error("Fake server has no repository");
  const prompts = [
    "Reduce memory retained by collapsed patches",
    "Document service authentication and callback URLs",
    "Validate uploaded image dimensions before decoding",
    "Add resource charts to the task statistics view",
    "Delegate the auth review to three parallel subagents",
  ];
  const ids: string[] = [];
  for (const text of prompts) {
    const task = await api.createTask({
      initialPrompt: { text },
      repos: [{ name: firstRepo.path }],
      harness: "claude",
      model: "opus-5.5",
      effort: "high",
    });
    ids.push(task.id);
    await waitForTaskState(api, task.id, "waiting", 30_000);
  }
  await waitForCISettle(api, ids);
}

test.describe.configure({ mode: "serial" });

test("prompt input layout screenshots", async ({ page, api }) => {
  test.setTimeout(60_000);
  await prepareVisualPage(page);
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto("/");

  await expect(page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first()).toBeVisible();
  // The task list behind the prompt is part of the capture. The documentation
  // spec usually fills it first; when this spec runs alone, seed it so the
  // capture never shows an empty list. Ask the server, because the cards load
  // after the first paint.
  if ((await api.listTasks()).length === 0) {
    await seedTaskList(api);
    await page.goto("/");
    await expect(page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first()).toBeVisible();
  }
  await expect(page.locator("[data-task-id]").first()).toBeVisible({ timeout: 10_000 });

  const prompt = page.getByTestId("prompt-input");

  // Screenshot 1: main prompt with two words.
  await prompt.fill("hello world");
  await expect(prompt).toContainText("hello world");
  await captureScreenshot(page, "desktop", "prompt-short.png");

  // Screenshot 2: main prompt with long text.
  const longText =
    "Please refactor the authentication middleware to support OAuth2 with PKCE flow, " +
    "update all existing tests to cover the new token refresh logic, and make sure the " +
    "CI pipeline passes with the updated integration test suite including the new edge cases. " +
    "Also add comprehensive error handling for expired tokens, implement automatic token " +
    "rotation with configurable refresh intervals, update the API documentation with the " +
    "new authentication flow diagrams, and ensure backwards compatibility with existing " +
    "client applications that still use the legacy session-based authentication mechanism";
  await prompt.fill(longText);
  await expect(prompt).toContainText(longText);
  await captureScreenshot(page, "desktop", "prompt-long.png");

  // Clear and create a task for detail view.
  await prompt.fill("");
  const id = await createTaskAPI(api, "Fix token expiry bug in auth middleware");
  await waitForTaskState(api, id, "waiting", 30_000);

  await page.goto("/");
  const taskCard = page.locator(`[data-task-id="${id}"]`);
  await expect(taskCard).toBeVisible({ timeout: 10_000 });
  await taskCard.click();

  const detailInput = page.getByTestId("task-detail-form").locator("[role='textbox']");
  await expect(detailInput).toBeVisible();

  // Screenshot 3: task detail input with two words.
  await detailInput.fill("looks good");
  await expect(detailInput).toContainText("looks good");
  await captureScreenshot(page, "desktop", "prompt-detail-short.png");

  // Screenshot 4: task detail input with long text.
  await detailInput.fill(longText);
  await expect(detailInput).toContainText(longText);
  await captureScreenshot(page, "desktop", "prompt-detail-long.png");

  await convertPngsToWebp(screenshotRoot);
});

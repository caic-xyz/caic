// Screenshots of the prompt input with short and long text to verify button layout.
import { test, expect, fillContentEditable, createTaskAPI, waitForTaskState, convertPngsToWebp } from "../helpers";
import { captureScreenshot, prepareVisualPage, screenshotDir } from "../visual";

test.describe.configure({ mode: "serial" });

test("prompt input layout screenshots", async ({ page, api }) => {
  test.setTimeout(60_000);
  await prepareVisualPage(page);
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto("/");

  await expect(
    page
      .getByTestId("repo-chips")
      .locator("[data-testid^='chip-label-']")
      .first(),
  ).toBeVisible();

  const prompt = page.getByTestId("prompt-input");

  // Screenshot 1: main prompt with two words.
  await fillContentEditable(prompt, "hello world");
  await expect(prompt).toContainText("hello world");
  await captureScreenshot(page, "prompt-short.png");

  // Screenshot 2: main prompt with long text.
  const longText =
    "Please refactor the authentication middleware to support OAuth2 with PKCE flow, " +
    "update all existing tests to cover the new token refresh logic, and make sure the " +
    "CI pipeline passes with the updated integration test suite including the new edge cases. " +
    "Also add comprehensive error handling for expired tokens, implement automatic token " +
    "rotation with configurable refresh intervals, update the API documentation with the " +
    "new authentication flow diagrams, and ensure backwards compatibility with existing " +
    "client applications that still use the legacy session-based authentication mechanism";
  await fillContentEditable(prompt, longText);
  await expect(prompt).toContainText(longText);
  await captureScreenshot(page, "prompt-long.png");

  // Clear and create a task for detail view.
  await fillContentEditable(prompt, "");
  const id = await createTaskAPI(api, "Fix token expiry bug in auth middleware");
  await waitForTaskState(api, id, "waiting", 30_000);

  await page.goto("/");
  const taskCard = page.locator(`[data-task-id="${id}"]`);
  await expect(taskCard).toBeVisible({ timeout: 10_000 });
  await taskCard.click();

  const detailInput = page.getByTestId("task-detail-form").locator("[role='textbox']");
  await expect(detailInput).toBeVisible();

  // Screenshot 3: task detail input with two words.
  await fillContentEditable(detailInput, "looks good");
  await expect(detailInput).toContainText("looks good");
  await captureScreenshot(page, "prompt-detail-short.png");

  // Screenshot 4: task detail input with long text.
  await fillContentEditable(detailInput, longText);
  await expect(detailInput).toContainText(longText);
  await captureScreenshot(page, "prompt-detail-long.png");

  await convertPngsToWebp(screenshotDir);
});

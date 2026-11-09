// Browser coverage for follow-up submission, live output, timed history, and clipboard errors.
import { test, expect, createTaskAPI, waitForTaskState } from "../helpers";

test("follow-up input streams a new turn and preserves timed history", async ({ page, api, uniquePrompt }) => {
  const id = await createTaskAPI(api, uniquePrompt("FAKE_DEMO timing propagation"));
  const initial = await waitForTaskState(api, id, "waiting");

  await page.goto(`/task/@${id}`);
  await expect(page.getByTestId("tool-duration").filter({ hasText: /^180ms$/ })).toHaveCount(1);
  await expect(page.getByTestId("tool-duration").filter({ hasText: /^0:01$/ })).toHaveCount(1);
  await expect(page.getByTestId("turn-duration").filter({ hasText: /^0:02$/ })).toBeVisible();

  await page.getByTestId("task-detail-prompt").fill("tell me another");
  await page.getByTestId("send-input").click();
  await expect(page.getByTestId("task-message-area")).toContainText("A SQL query walks into a bar");
  const task = await waitForTaskState(api, id, "waiting");
  expect(task.numTurns).toBeGreaterThan(initial.numTurns);
  await expect(page.getByRole("button", { name: /2 messages, 4 tool calls.*0:02$/ })).toBeVisible();
  await expect(page.getByText(/^wait (?:[1-9]\d*ms|\d+:[0-5]\d(?::[0-5]\d)?)$/)).toBeVisible();
});

test("raw copy reports clipboard failures", async ({ page, api, uniquePrompt }) => {
  const id = await createTaskAPI(api, uniquePrompt("FAKE_DEMO clipboard"));
  await waitForTaskState(api, id, "waiting");
  await page.addInitScript(() => {
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText: () => Promise.reject(new Error("clipboard denied")) },
    });
  });
  await page.goto(`/task/@${id}`);
  const markdown = page.locator("[data-markdown-wrap]").first();
  await markdown.hover();
  await markdown.getByTitle("Show raw").click();
  await markdown.getByTitle("Copy to clipboard").click();
  await expect(markdown.getByRole("status")).toHaveText("Copy failed");
});

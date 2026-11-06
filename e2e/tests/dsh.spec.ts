// Browser coverage for DeepSeek Harness selection, native capabilities, follow-up, and restored history.
import { test, expect, waitForTaskState } from "../helpers";

test("dsh task selection and follow-up survive browser reload", async ({ page, api, uniquePrompt }) => {
  const harnesses = await api.listHarnesses();
  const dsh = harnesses.find((harness) => harness.name === "dsh");
  expect(dsh).toBeTruthy();
  expect(dsh!.logoUrl).toBe("/logos/deepseek.svg");
  expect(dsh!.supportsImages).toBe(false);
  expect(dsh!.supportsCompact).toBe(false);
  expect(dsh!.supportsModelRefresh).toBe(true);

  await page.goto("/");
  await page.getByTestId("harness-select").selectOption("dsh");
  const prompt = uniquePrompt("FAKE_LIFECYCLE dsh task");
  await page.getByTestId("prompt-input").fill(prompt);
  await page.getByTestId("submit-task").click();
  await expect(page).toHaveURL(/\/task\//);
  await expect(page.getByTestId("task-message-area")).toContainText("Lifecycle completed");

  const task = (await api.listTasks()).find((candidate) => candidate.initialPrompt === prompt);
  expect(task).toBeTruthy();
  expect(task!.harness).toBe("dsh");
  const initial = await waitForTaskState(api, task!.id, "waiting");
  await page.getByTestId("task-detail-prompt").fill("tell me another");
  await page.getByTestId("send-input").click();
  await expect(page.getByTestId("task-message-area")).toContainText("A SQL query walks into a bar");
  const continued = await waitForTaskState(api, task!.id, "waiting");
  expect(continued.numTurns).toBeGreaterThan(initial.numTurns);

  await page.reload();
  await expect(page.getByTestId("task-message-area")).toContainText("A SQL query walks into a bar");
  await page.getByRole("button", { name: /1 message/ }).click();
  await expect(page.getByTestId("task-message-area")).toContainText("Lifecycle completed");
});

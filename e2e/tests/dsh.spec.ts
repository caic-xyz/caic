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
  const harnessSelect = page.getByTestId("harness-select");
  await expect(harnessSelect.locator("option[value=dsh]")).toHaveText("DeepSeek Harness");
  await harnessSelect.selectOption("dsh");
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

test("native context occupancy stays independent of billed input and preserves zero", async ({
  page,
  api,
  uniquePrompt,
}) => {
  const repos = await api.listRepos();
  const repo = repos[0];
  expect(repo).toBeTruthy();
  const task = await api.createTask({
    initialPrompt: { text: uniquePrompt("FAKE_CONTEXT_OCCUPANCY") },
    repos: [{ name: repo!.path }],
    harness: "dsh",
  });
  const initial = await waitForTaskState(api, task.id, "waiting");
  expect(initial.contextUsed).toBe(45_000);
  expect(initial.activeInputTokens).toBe(123);
  expect(initial.activeCacheReadTokens).toBe(456);
  await page.goto(`/task/@${task.id}`);
  const meter = page.locator(`[data-task-id="${task.id}"]`).getByTestId("task-card-tokens");
  await expect(meter).toHaveText("45kt/200kt");
  await api.sendInput(task.id, { prompt: { text: "FAKE_CONTEXT_OCCUPANCY ZERO" } });
  await expect(meter).toHaveText("0t/200kt");
  const empty = await waitForTaskState(api, task.id, "waiting");
  expect(empty.contextUsed).toBe(0);
  expect(empty.activeInputTokens).toBe(123);
  expect(empty.activeCacheReadTokens).toBe(456);
  await page.reload();
  await expect(meter).toHaveText("0t/200kt");
  await api.sendInput(task.id, { prompt: { text: "FAKE_CONTEXT_OCCUPANCY RESET" } });
  await expect(meter).toHaveText("579t/200kt");
  const reset = await waitForTaskState(api, task.id, "waiting");
  expect(reset.contextUsed).toBeUndefined();
  await page.reload();
  await expect(meter).toHaveText("579t/200kt");
});

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

for (const width of [390, 1280]) {
  test(`native skill has one presentation at ${width}px`, async ({ page, api, uniquePrompt }, testInfo) => {
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    page.on("console", (message) => {
      if (message.type() === "error") errors.push(message.text());
    });
    page.on("response", (response) => {
      if (response.status() >= 400 && response.url().includes("/api/"))
        errors.push(`${response.status()} ${response.url()}`);
    });
    await page.setViewportSize({ width, height: 900 });
    const task = await api.createTask({ initialPrompt: { text: uniquePrompt("FAKE_NATIVE_SKILL") }, harness: "dsh" });
    await waitForTaskState(api, task.id, "waiting");
    await page.goto(`/task/@${task.id}`);
    for (const reload of [false, true]) {
      if (reload) await page.reload();
      const skills = page.getByRole("navigation", { name: "Loaded skills", exact: true });
      await expect(skills).toHaveCount(1);
      await expect(skills).toHaveText("code-quality");
      await expect(page.getByTestId("task-message-area")).toContainText("Native skill loaded.");
      await expect(page.getByTestId("task-message-area")).not.toContainText("Native skill contents");
      await expect(page.getByTestId("task-message-area").getByText("skill", { exact: true })).toHaveCount(0);
      expect(await skills.evaluate((el) => el.getBoundingClientRect().right)).toBeLessThanOrEqual(width);
      const card = page.getByTestId("native-subagent-card");
      await expect(card.locator("summary")).toContainText("Completion not reported");
      await expect(card.getByTestId("native-subagent-duration")).toHaveCount(0);
      await card.locator("summary").press("Enter");
      await expect(card.getByText("dsh-child", { exact: true })).toBeVisible();
      await expect(card.getByText("Inspect code-quality integration", { exact: true })).toBeVisible();
      await expect(card.getByText("No result reported by the harness.", { exact: true })).toBeVisible();
      expect(await card.evaluate((el) => el.getBoundingClientRect().right)).toBeLessThanOrEqual(width);
    }
    await page.screenshot({ path: testInfo.outputPath("native-skill.png") });
    await page.addStyleTag({ content: "html { font-size: 125%; }" });
    const card = page.getByTestId("native-subagent-card");
    await expect(card.getByText("No result reported by the harness.", { exact: true })).toBeVisible();
    expect(await card.evaluate((el) => el.getBoundingClientRect().right)).toBeLessThanOrEqual(width);
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    await page.screenshot({ path: testInfo.outputPath("native-skill-larger-font.png") });
    expect(errors).toEqual([]);
  });
}

// Tests native activity and background work rendering, keyboard disclosure, mobile containment, background counts, and restored SSE history.
import { ok } from "node:assert/strict";
import { test, expect, createTaskAPI, recentHistoryPage, waitForTaskState } from "../helpers";

for (const width of [390, 1280]) {
  test(`native activity survives reload at ${width}px`, async ({ page, uniquePrompt }, testInfo) => {
    await page.setViewportSize({ width, height: 900 });
    await page.goto("/");
    await expect(page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first()).toBeVisible();
    await page.getByTestId("prompt-input").fill(uniquePrompt("FAKE_NATIVE_SUBAGENTS"));
    await page.getByTestId("submit-task").click();
    await expect(page).toHaveURL(/\/task\//);
    // Cards render inline in the transcript, where each lifecycle settled.
    const cards = page.getByTestId("native-subagent-card");
    await expect(cards).toHaveCount(4);
    const summary = cards.locator("summary").first();
    await summary.focus();
    await page.keyboard.press("Enter");
    await expect(cards.first().getByText("Read me before you judge me.")).toBeVisible();
    await summary.press("Enter");
    await expect(cards.first().locator("pre")).toHaveCount(0);
    await expect(summary).toBeFocused();
    await summary.press("Enter");
    await expect(cards.first().getByText("Read me before you judge me.")).toBeVisible();
    for (const card of await cards.all()) {
      await expect(card.getByRole("link")).toHaveCount(0);
      expect(await card.evaluate((el) => el.getBoundingClientRect().right)).toBeLessThanOrEqual(width);
    }
    await page.reload();
    await expect(cards).toHaveCount(4);
    await expect(page.getByText("Completed", { exact: true })).toBeVisible();
    await expect(page.getByText("Failed", { exact: true })).toBeVisible();
    await expect(page.getByText("Completion not reported", { exact: true })).toBeVisible();
    await expect(page.getByText("Paused", { exact: true })).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath("native-subagents.png") });
  });
}

for (const width of [390, 1280]) {
  test(`background command and watch survive reload at ${width}px`, async ({ page, api, uniquePrompt }, testInfo) => {
    await page.setViewportSize({ width, height: 900 });
    await page.goto("/");
    await expect(page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first()).toBeVisible();
    const prompt = uniquePrompt("FAKE_BACKGROUND_WATCH_ONLY");
    await page.getByTestId("prompt-input").fill(prompt);
    await page.getByTestId("submit-task").click();
    await expect(page).toHaveURL(/\/task\//);
    // A watch never keeps the task running, so it waits with the watch open.
    const task = ((await api.listTasks()) ?? []).find((t) => t.initialPrompt === prompt);
    ok(task);
    const waiting = await waitForTaskState(api, task.id, "waiting");
    expect(waiting.background).toEqual({ commands: 0, watches: 1, agents: 0 });
    // The tool group counts the still-running watch as unfinished.
    const tools = page.getByText(/^1\/2 tools: /);
    await tools.click();
    // Each detached command renders on the tool row that launched it.
    const checks = page.locator(`[data-testid="tool-call"][data-tool-key$='"toolu_bg_checks"]']`);
    const watch = page.locator(`[data-testid="tool-call"][data-tool-key$='"toolu_bg_watch"]']`);
    await expect(checks.getByTestId("background-command-chip")).toHaveText("exit 0");
    await expect(watch.getByTestId("background-command-chip")).toHaveText("watching");
    for (const row of [checks, watch]) {
      expect(await row.evaluate((el) => el.getBoundingClientRect().right)).toBeLessThanOrEqual(width);
    }
    // The running watch keeps ticking on its row past the result.
    const duration = watch.getByTestId("tool-duration");
    await expect(duration).toBeVisible();
    const initialDuration = await duration.textContent();
    await expect(duration).not.toHaveText(initialDuration ?? "");
    // The group's duration covers its rows, so it ticks with the running watch.
    const groupDuration = tools.locator("xpath=ancestor::details/..").getByTestId("timing-duration");
    const initialGroupDuration = await groupDuration.textContent();
    await expect(groupDuration).not.toHaveText(initialGroupDuration ?? "");
    await page.reload();
    await tools.click();
    await expect(checks.getByTestId("background-command-chip")).toHaveText("exit 0");
    await expect(watch.getByTestId("background-command-chip")).toHaveText("watching");
    await expect(duration).toBeVisible();
    // The settled command's output file opens in the document viewer.
    await checks.locator("summary").press("Enter");
    await expect(checks).toHaveAttribute("open", "");
    await page.screenshot({ path: testInfo.outputPath("background-commands.png") });
    await checks.getByTestId("background-command-output").click();
    await expect(page).toHaveURL(
      /\/view\?path=%2Ftmp%2Fclaude-1000%2F-workspace%2Ftest-session%2Ftasks%2Fbchk7x2qa\.output$/,
    );
  });
}

test("background work counts follow each notification turn", async ({ api, uniquePrompt }) => {
  const id = await createTaskAPI(api, uniquePrompt("FAKE_BACKGROUND_WORK"));
  const listed = async () => {
    const task = ((await api.listTasks()) ?? []).find((t) => t.id === id);
    return { state: task?.state, turns: task?.numTurns, background: task?.background ?? null };
  };
  // The command and the agent keep the task running after its result.
  await expect.poll(listed).toEqual({ state: "running", turns: 1, background: { commands: 1, watches: 1, agents: 1 } });
  await api.sendInput(id, { prompt: { text: "FAKE_BACKGROUND_SETTLE_WORK" } });
  // The reply and two notification turns end; only the watch remains, so the
  // task waits for the user.
  await expect.poll(listed).toEqual({ state: "waiting", turns: 4, background: { commands: 0, watches: 1, agents: 0 } });
  await api.sendInput(id, { prompt: { text: "FAKE_BACKGROUND_SETTLE_WATCH" } });
  await expect.poll(listed).toEqual({ state: "waiting", turns: 6, background: null });
  // Each notification turn ends with its own result after the user's turn.
  const bootstrap = (await api.getHistoryManifest(id)).bootstrap;
  const recent = recentHistoryPage(bootstrap);
  const events = await api.getHistorySlice(
    id,
    bootstrap.scope,
    bootstrap.projectionVersion,
    bootstrap.generation,
    String(recent.first),
    "0",
    String(recent.last),
    "end",
  );
  expect(
    (events ?? []).filter((event) => event.message.kind === "result").map((event) => event.message.result?.result),
  ).toEqual([
    "Background work started",
    "Background work running",
    "Checks passed",
    "Review finished",
    "Watch running",
    "Watch ended",
  ]);
});

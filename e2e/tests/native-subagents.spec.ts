// Tests canonical native activity rendering, keyboard disclosure, mobile containment, and restored SSE history.
import { test, expect } from "../helpers";

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
  test(`background command lifecycle survives reload at ${width}px`, async ({ page, uniquePrompt }, testInfo) => {
    await page.setViewportSize({ width, height: 900 });
    await page.goto("/");
    await expect(page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first()).toBeVisible();
    await page.getByTestId("prompt-input").fill(uniquePrompt("FAKE_BACKGROUND_COMMANDS"));
    await page.getByTestId("submit-task").click();
    await expect(page).toHaveURL(/\/task\//);
    // The tool group counts the still-running command as unfinished.
    const tools = page.getByText(/^1\/2 tools: /);
    await tools.click();
    // Each detached command renders on the Bash row that launched it.
    const lint = page.locator(`[data-testid="tool-call"][data-tool-key$='"bgtool"]']`);
    const watch = page.locator(`[data-testid="tool-call"][data-tool-key$='"bgwatch"]']`);
    await expect(lint.getByTestId("background-command-chip")).toHaveText("exit 0");
    await expect(watch.getByTestId("background-command-chip")).toHaveText("running in background");
    for (const row of [lint, watch]) {
      expect(await row.evaluate((el) => el.getBoundingClientRect().right)).toBeLessThanOrEqual(width);
    }
    // The still-running command keeps ticking on its row past the result.
    const duration = watch.getByTestId("tool-duration");
    await expect(duration).toBeVisible();
    const initialDuration = await duration.textContent();
    await expect(duration).not.toHaveText(initialDuration ?? "");
    await page.reload();
    await tools.click();
    await expect(lint.getByTestId("background-command-chip")).toHaveText("exit 0");
    await expect(watch.getByTestId("background-command-chip")).toHaveText("running in background");
    await expect(duration).toBeVisible();
    // The settled command's output file opens in the document viewer.
    await lint.locator("summary").press("Enter");
    await expect(lint).toHaveAttribute("open", "");
    await page.screenshot({ path: testInfo.outputPath("background-commands.png") });
    await lint.getByTestId("background-command-output").click();
    await expect(page).toHaveURL(/\/view\?path=%2Ftmp%2Ftasks%2Flint1\.output$/);
  });
}

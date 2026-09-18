// Tests canonical native activity rendering, keyboard disclosure, mobile containment, and restored SSE history.
import { test, expect, fillContentEditable } from "../helpers";

for (const width of [390, 1280]) {
  test(`native activity survives reload at ${width}px`, async ({ page, uniquePrompt }) => {
    await page.setViewportSize({ width, height: 900 });
    await page.goto("/");
    await expect(
      page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first(),
    ).toBeVisible();
    await fillContentEditable(
      page.getByTestId("prompt-input"),
      uniquePrompt("FAKE_NATIVE_SUBAGENTS"),
    );
    await page.getByTestId("submit-task").click();
    await expect(page).toHaveURL(/\/task\//);
    const panel = page.getByRole("region", {
      name: "Native subagent activity",
    });
    await expect(panel.getByTestId("native-subagent-card")).toHaveCount(4);
    await expect(panel.getByText("0 agents active · 0 batches active")).toBeVisible();
    const summary = panel.locator("summary").first();
    await summary.focus();
    await page.keyboard.press("Enter");
    await expect(panel.getByText("Read me before you judge me.")).toBeVisible();
    await expect(panel.getByRole("link")).toHaveCount(0);
    expect(await panel.evaluate((el) => el.getBoundingClientRect().right)).toBeLessThanOrEqual(
      width,
    );
    await page.reload();
    await expect(panel.getByTestId("native-subagent-card")).toHaveCount(4);
    await expect(panel.getByText("Completed", { exact: true })).toBeVisible();
    await expect(panel.getByText("Failed", { exact: true })).toBeVisible();
    await expect(panel.getByText("Status unknown", { exact: true })).toBeVisible();
    await expect(panel.getByText("Paused", { exact: true })).toBeVisible();
  });
}

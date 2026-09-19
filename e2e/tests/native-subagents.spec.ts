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
    // Cards render inline in the transcript, where each lifecycle settled.
    const cards = page.getByTestId("native-subagent-card");
    await expect(cards).toHaveCount(4);
    const summary = cards.locator("summary").first();
    await summary.focus();
    await page.keyboard.press("Enter");
    await expect(cards.first().getByText("Read me before you judge me.")).toBeVisible();
    for (const card of await cards.all()) {
      await expect(card.getByRole("link")).toHaveCount(0);
      expect(await card.evaluate((el) => el.getBoundingClientRect().right)).toBeLessThanOrEqual(
        width,
      );
    }
    await page.reload();
    await expect(cards).toHaveCount(4);
    await expect(page.getByText("Completed", { exact: true })).toBeVisible();
    await expect(page.getByText("Failed", { exact: true })).toBeVisible();
    await expect(page.getByText("Status unknown", { exact: true })).toBeVisible();
    await expect(page.getByText("Paused", { exact: true })).toBeVisible();
  });
}

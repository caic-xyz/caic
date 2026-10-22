// Browser coverage for context-scoped skill loads, compaction, replay, and recursive Markdown navigation.
import { test, expect, createTaskAPI, waitForTaskState } from "../helpers";

for (const width of [390, 1280]) {
  test(`loaded skills follow context sections at ${width}px`, async ({ page, api, uniquePrompt }) => {
    await page.setViewportSize({ width, height: 900 });
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    const id = await createTaskAPI(api, uniquePrompt("FAKE_SKILLS load"));
    await waitForTaskState(api, id, "waiting");
    await page.goto(`/task/@${id}`);
    const current = page.getByRole("navigation", { name: "Loaded skills", exact: true });
    await expect(current.getByRole("link")).toHaveCount(2);
    await current.getByRole("link", { name: "code-quality", exact: true }).click();
    const pane = page.getByTestId("document-pane");
    await expect(pane.getByRole("heading", { name: "Code quality", exact: true })).toBeVisible();
    await pane.getByRole("link", { name: "reference", exact: true }).click();
    await expect(pane.getByRole("heading", { name: "Quality rules", exact: true })).toBeVisible();
    await pane.getByRole("link", { name: "examples", exact: true }).click();
    await expect(pane.getByRole("heading", { name: "Quality examples", exact: true })).toBeVisible();
    await page.keyboard.press("Escape");
    await api.sendInput(id, { prompt: { text: "FAKE_SKILLS compact" } });
    await waitForTaskState(api, id, "waiting");
    await expect(current).toHaveCount(0);
    const prior = page.getByRole("navigation", { name: "Previous context skills", exact: true });
    await expect(prior).toHaveCount(0);
    await page.getByRole("button", { name: /Compacted session/ }).click();
    await expect(prior.getByRole("link")).toHaveCount(2);
    await prior.getByRole("link", { name: "review", exact: true }).click();
    await expect(pane.getByRole("heading", { name: "Review", exact: true })).toBeVisible();
    await page.keyboard.press("Escape");
    await page.reload();
    await expect(current).toHaveCount(0);
    await expect(prior).toHaveCount(0);
    await page.getByRole("button", { name: /Compacted session/ }).click();
    await expect(prior.getByRole("link")).toHaveCount(2);
    await page.getByRole("button", { name: /Compacted session/ }).click();
    await expect(prior).toHaveCount(0);
    await page.getByRole("button", { name: /Compacted session/ }).click();
    await expect(prior.getByRole("link")).toHaveCount(2);
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    await page.screenshot({ path: test.info().outputPath("skill-context.png") });
    expect(errors).toEqual([]);
  });
}

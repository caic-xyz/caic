// Browser coverage for the context Performance view: transcript entry and end icons, compacted, cleared, and current contexts, stepping, and layout.
import type { Page } from "@playwright/test";
import { createTaskAPI, expect, test, waitForTaskState, type APIClient } from "../helpers";

// threeContextTask ends its first context by compaction and its second by a
// clear; the third is current.
async function threeContextTask(api: APIClient, prompt: string): Promise<string> {
  const id = await createTaskAPI(api, prompt);
  let task = await waitForTaskState(api, id, "waiting");
  for (const text of ["FAKE_COMPACT", "FAKE_CONTEXT_OCCUPANCY RESET"]) {
    const before = task.numTurns;
    await api.sendInput(id, { prompt: { text } });
    await expect.poll(async () => (task = await api.getTask(id)).numTurns, { timeout: 15_000 }).toBeGreaterThan(before);
    task = await waitForTaskState(api, id, "waiting");
  }
  await expect.poll(async () => (await api.getHistoryManifest(id)).sections?.length).toBe(3);
  return id;
}

// The page must not scroll sideways and no part of the view may spill past
// the pane's edge; a wide table scrolls inside its own region.
async function expectContained(page: Page) {
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  const overflow = await page.getByTestId("task-view-pane").evaluate((root) => {
    const edge = root.getBoundingClientRect().right + 1;
    return Array.from(
      root.querySelectorAll<HTMLElement>(
        '[data-testid="context-detail"] :is(h2, p, ol, nav, button, figure, [role="region"])',
      ),
    )
      .filter((el) => el.getBoundingClientRect().right > edge)
      .map((el) => el.textContent);
  });
  expect(overflow).toEqual([]);
}

for (const width of [1280, 390]) {
  test(`a context's time opens its view, which explains how it ended at ${width}px`, async ({
    page,
    api,
    uniquePrompt,
  }, testInfo) => {
    await page.setViewportSize({ width, height: 844 });
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    const id = await threeContextTask(api, uniquePrompt("FAKE_CONTEXT_OCCUPANCY"));

    await page.goto(`/task/@${id}`);
    const conversation = page.getByTestId("conversation-pane");
    const links = conversation.getByTestId("context-performance-link");
    await expect(links).toHaveCount(2);
    // Each archived context's toggle names how it ended with an icon.
    await expect(conversation.getByRole("button", { name: /^Compacted session 1 turn/ })).toBeVisible();
    await expect(conversation.getByRole("button", { name: /^Cleared session 1 turn/ })).toBeVisible();
    await conversation.screenshot({ path: testInfo.outputPath(`context-ends-${width}.png`) });
    // A link, styled like the turn durations: dotted underline.
    expect(await links.first().evaluate((el) => getComputedStyle(el).textDecorationStyle)).toBe("dotted");
    await links.first().click();
    await expect(page).toHaveURL(new RegExp(`/task/@${id}[^/]*/perf/contexts/1$`));

    const pane = page.getByTestId("task-view-pane");
    const breadcrumb = pane.getByRole("navigation", { name: "Breadcrumb" });
    await expect(breadcrumb).toHaveText(/Performance\s*›?\s*Context 1 of 3/);
    const detail = pane.getByTestId("context-detail");
    await expect(detail.getByTestId("context-end")).toHaveText("Compacted session · 150,000 → 20,000 tokens");
    await expect(detail.getByTestId("context-summary-line")).toHaveText(/^1 turn · /);
    await expect(detail.getByTestId("turn-strip").getByRole("link")).toHaveCount(1);
    const growth = detail.getByTestId("context-growth");
    await expect(growth.getByTestId("context-growth-turn")).toHaveCount(1);
    await expect(growth).toContainText("45kt after turn 1");
    await growth.getByText("Turns in this context (1)").click();
    await expect(growth.getByRole("region", { name: "Turns in this context" }).getByRole("row").nth(1)).toContainText(
      "45,000",
    );
    await expectContained(page);
    await page.screenshot({ path: testInfo.outputPath(`context-compacted-${width}.png`), fullPage: true });

    await detail.getByRole("button", { name: "Next context" }).click();
    await expect(page).toHaveURL(/\/perf\/contexts\/2$/);
    await expect(breadcrumb).toContainText("Context 2 of 3");
    await expect(detail.getByTestId("context-end")).toHaveText("Cleared session");
    await expect(detail.getByTestId("turn-strip").getByRole("link")).toHaveAccessibleName(/^Turn 2, /);
    await expectContained(page);
    await page.screenshot({ path: testInfo.outputPath(`context-cleared-${width}.png`), fullPage: true });

    await page.keyboard.press("ArrowRight");
    await expect(page).toHaveURL(/\/perf\/contexts\/3$/);
    await expect(detail.getByTestId("context-end")).toHaveText("Current context");
    await expect(detail.getByRole("button", { name: "Next context" })).toBeDisabled();

    // The strip leads to the context's turns.
    await detail
      .getByTestId("turn-strip")
      .getByRole("link", { name: /^Turn 3, / })
      .click();
    await expect(page).toHaveURL(/\/perf\/turns\/3$/);
    await pane.getByRole("link", { name: "Context 3" }).click();
    await expect(page).toHaveURL(/\/perf\/contexts\/3$/);
    expect(errors).toEqual([]);
  });
}

test("an unknown context reports how many the task has", async ({ page, api, uniquePrompt }) => {
  const id = await createTaskAPI(api, uniquePrompt("context not found"));
  await waitForTaskState(api, id, "waiting");
  await page.goto(`/task/@${id}/perf/contexts/9`);
  const missing = page.getByTestId("task-view-pane").getByTestId("context-not-found");
  await expect(missing).toContainText("Context 9 not found");
  await expect(missing).toContainText("This task has 1 context.");
});

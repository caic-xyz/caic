// Browser coverage for combined Codex thinking previews, keyboard disclosure, and narrow-screen containment.
import { test, expect, createTaskAPI, waitForTaskState } from "../helpers";
import type { EventMessage, HistoryEvent } from "../../sdk/caic/ts/v1/types.gen";
import { fixtureHistoryResponse } from "../../frontend/tests/history";

for (const width of [390, 1280]) {
  test(`combined thinking stays readable at ${width}px`, async ({ api, page }, testInfo) => {
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    await page.setViewportSize({ width, height: 900 });
    const id = await createTaskAPI(api, "Thinking preview fixture");
    await waitForTaskState(api, id, "waiting");
    const source = { threadID: "root", turnID: "turn", itemID: "first" };
    const messages: EventMessage[] = [
      { kind: "thinking", ts: 1000, source, thinking: { text: "**Checking editor font metrics**" } },
      {
        kind: "thinking",
        ts: 2000,
        source: { ...source, itemID: "second" },
        thinking: { text: "**Investigating keyboard timing**" },
      },
      {
        kind: "thinking",
        ts: 3000,
        source: { ...source, itemID: "third" },
        thinking: { text: "Checked the metrics. **Updating timing evidence.**" },
      },
      {
        kind: "text",
        ts: 4000,
        source: { ...source, itemID: "comment" },
        text: { text: "The investigation captured the browser caret reaching the line end." },
      },
      {
        kind: "thinking",
        ts: 5000,
        source: { ...source, itemID: "fourth" },
        thinking: {
          text: "**Preparing an isolated benchmark with a long descriptive heading to check narrow-screen containment**",
        },
      },
    ];
    const events: HistoryEvent[] = messages.map((message, i) => ({ recordID: i + 1, index: 0, message }));
    // Static transcript fixture; streaming updates are exercised by component tests.
    await page.route("**/history/owner/3/test/**", async (route) => {
      const response = fixtureHistoryResponse(route.request().url(), events, "Thinking preview fixture");
      await route.fulfill({ contentType: "application/json", body: await response.text() });
    });
    await page.route(`**/tasks/${id}/history/**`, async (route) => {
      if (route.request().url().includes("/events?")) {
        await route.fulfill({ contentType: "text/event-stream", body: "retry: 60000\n\n" });
        return;
      }
      const response = fixtureHistoryResponse(route.request().url(), events, "Thinking preview fixture");
      await route.fulfill({ contentType: "application/json", body: await response.text() });
    });
    await page.goto(`/task/@${id}`);
    const summaries = page.locator("summary").filter({ hasText: /^Thinking/ });
    await expect(summaries).toHaveCount(2);
    await expect(summaries.first()).toContainText("Updating timing evidence.");
    await expect(summaries.first()).not.toContainText("**");
    const first = summaries.first().locator("..");
    await summaries.first().focus();
    await summaries.first().press("Enter");
    await expect(first).toHaveAttribute("open", "");
    await expect(first.locator("pre")).toContainText("**Checking editor font metrics**");
    await expect(first.locator("pre")).toContainText("**Investigating keyboard timing**");
    await summaries.first().press("Enter");
    await expect(first).not.toHaveAttribute("open");
    await expect(summaries.first()).toBeFocused();
    for (const summary of await summaries.all()) {
      const box = await summary.boundingBox();
      expect(box).not.toBeNull();
      expect((box?.x ?? 0) + (box?.width ?? 0)).toBeLessThanOrEqual(width);
    }
    await page.screenshot({ path: testInfo.outputPath("thinking-preview.png") });
    expect(errors).toEqual([]);
  });
}

// Browser coverage for the turn Performance view: transcript entry, strip, stepping, keyboard, dense tasks, and layout.
import type { Page } from "@playwright/test";
import type { HistoryManifest, HistoryTurn } from "../../sdk/caic/ts/v1/types.gen";
import { createTaskAPI, expect, test, waitForTaskState, type APIClient } from "../helpers";

async function threeTurnTask(api: APIClient, prompt: string): Promise<string> {
  const id = await createTaskAPI(api, prompt);
  for (const [index, text] of ["tell me another", "and one more"].entries()) {
    // The fake agent gives each event a distinct millisecond timestamp, so a
    // burst can put the result ahead of the clock used to stamp the next prompt.
    await expect
      .poll(async () => {
        const turn = (await api.getHistoryManifest(id)).metadata.turns?.[index];
        return turn !== undefined && Date.now() > Date.parse(turn.at);
      })
      .toBe(true);
    await waitForTaskState(api, id, "waiting");
    await api.sendInput(id, { prompt: { text } });
  }
  await expect.poll(async () => (await api.getHistoryManifest(id)).metadata.turns?.length).toBe(3);
  await waitForTaskState(api, id, "waiting");
  return id;
}

// The page must not scroll sideways and the turn sections must not spill
// past the pane's edge; the strip scrolls inside itself.
async function expectContained(page: Page) {
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  const pane = page.getByTestId("task-view-pane");
  const overflow = await pane.evaluate((root) => {
    const edge = root.getBoundingClientRect().right + 1;
    const scope =
      'header, [data-testid="turn-detail"], [data-testid="performance-turns"], [data-testid="turn-not-found"]';
    return Array.from(root.querySelectorAll<HTMLElement>(scope))
      .flatMap((section) => [section, ...section.querySelectorAll<HTMLElement>("h2, p, ol, th, td, nav, button")])
      .filter((el) => el.getBoundingClientRect().right > edge)
      .map((el) => el.textContent);
  });
  expect(overflow).toEqual([]);
}

for (const width of [1280, 390]) {
  test(`turn view opens from the transcript and steps through turns at ${width}px`, async ({
    page,
    api,
    uniquePrompt,
  }, testInfo) => {
    await page.setViewportSize({ width, height: 844 });
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    const id = await threeTurnTask(api, uniquePrompt("FAKE_DEMO turn view"));

    await page.goto(`/task/@${id}`);
    const conversation = page.getByTestId("conversation-pane");
    const link = conversation.getByRole("link", { name: /^Turn 1 performance, / });
    await link.click();
    await expect(page).toHaveURL(new RegExp(`/task/@${id}[^/]*/perf/turns/1$`));

    const pane = page.getByTestId("task-view-pane");
    const breadcrumb = pane.getByRole("navigation", { name: "Breadcrumb" });
    await expect(breadcrumb).toHaveText(/Performance\s*›?\s*Turn 1 of 3/);
    const strip = pane.getByTestId("turn-strip");
    await expect(strip.getByRole("link")).toHaveCount(3);
    await expect(strip.getByRole("link", { name: /^Turn 1, / })).toHaveAttribute("aria-current", "page");
    await expect(pane.getByRole("button", { name: "Previous turn" })).toBeDisabled();
    await expect(pane.getByTestId("turn-summary-line")).toContainText("turn");
    await expect(pane.getByTestId("turn-user-wait")).toContainText("for the next prompt");
    await expect(pane.getByRole("rowheader", { name: "Cost source" })).toBeVisible();
    if (width <= 768) await expect(conversation).toBeHidden();
    else await expect(conversation).toBeVisible();
    await expectContained(page);
    await page.screenshot({ path: testInfo.outputPath(`turn-view-${width}.png`) });

    await page.keyboard.press("ArrowRight");
    await expect(page).toHaveURL(/\/perf\/turns\/2$/);
    await expect(breadcrumb).toContainText("Turn 2 of 3");
    await pane.getByRole("button", { name: "Next turn" }).click();
    await expect(page).toHaveURL(/\/perf\/turns\/3$/);
    await expect(pane.getByRole("button", { name: "Next turn" })).toBeDisabled();
    await expect(pane.getByTestId("turn-user-wait")).toHaveCount(0);

    await breadcrumb.getByRole("link", { name: "Performance" }).click();
    await expect(page).toHaveURL(new RegExp(`/task/@${id}[^/]*/perf$`));
    const overview = pane.getByTestId("performance-turns");
    await expect(overview.getByRole("link")).toHaveCount(3);
    await expectContained(page);
    await page.screenshot({ path: testInfo.outputPath(`turn-overview-${width}.png`) });

    // Browser navigation returns through the visited turns to the conversation.
    await page.goBack();
    await expect(page).toHaveURL(/\/perf\/turns\/3$/);
    await page.goBack();
    await page.goBack();
    await page.goBack();
    await expect(page).toHaveURL(new RegExp(`/task/@${id}[^/]*$`));
    await expect(conversation).toBeVisible();
    expect(errors).toEqual([]);
  });

  test(`dense tasks keep the turn strip usable at ${width}px`, async ({ page, api, uniquePrompt }, testInfo) => {
    await page.setViewportSize({ width, height: 844 });
    const id = await createTaskAPI(api, uniquePrompt("FAKE_DEMO dense turn view"));
    await waitForTaskState(api, id, "waiting");
    // Repeat the one real turn so the strip shows a dense task. The live stream
    // would replace the manifest, so hold it open without events; the manifest
    // alone loads the view and the stream raises no disconnect status.
    await page.route(`**/api/caic/v1/tasks/${id}/history/events?*`, () => new Promise<void>(() => {}));
    let count = 200;
    await page.route(`**/tasks/${id}/history/manifest`, async (route) => {
      const response = await route.fetch();
      const manifest = (await response.json()) as HistoryManifest;
      const [first] = manifest.metadata.turns ?? [];
      if (!first) throw new Error("demo task has no completed turn");
      manifest.metadata.turns = Array.from({ length: count }, (_, i): HistoryTurn => ({
        ...first,
        number: i + 1,
        durationMs: 1_000 + ((i * 37) % 23) * 9_000,
      }));
      await route.fulfill({ response, json: manifest });
    });
    // Each repeated turn shows the real turn's timeline.
    await page.route(`**/tasks/${id}/history/turns/*`, async (route) => {
      const url = new URL(route.request().url());
      url.pathname = url.pathname.replace(/\/\d+$/, "/1");
      const response = await route.fetch({ url: url.toString() });
      await route.fulfill({ response });
    });
    const pane = page.getByTestId("task-view-pane");
    const strip = pane.getByTestId("turn-strip");
    // Bars the strip's own scroll viewport clips, so the reader cannot see them.
    const clipped = () =>
      strip.evaluate((list) => {
        const bounds = list.getBoundingClientRect();
        return Array.from(list.querySelectorAll("a")).filter((bar) => {
          const rect = bar.getBoundingClientRect();
          return rect.left < bounds.left - 0.5 || rect.right > bounds.right + 0.5;
        }).length;
      });

    // 200 turns fit the pane at both widths.
    await page.goto(`/task/@${id}/perf/turns/180`);
    await expect(pane.getByRole("navigation", { name: "Breadcrumb" })).toContainText("Turn 180 of 200");
    await expect(pane.getByRole("alert")).toHaveCount(0);
    await expect(strip.getByRole("link")).toHaveCount(200);
    expect(await clipped()).toBe(0);
    await expect(pane.getByTestId("turn-strip-range")).toHaveCount(0);
    const current = strip.getByRole("link", { name: /^Turn 180, / });
    await expect(current).toBeInViewport();
    await expect(strip.locator('a[tabindex="0"]')).toHaveCount(1);
    await expect(current).toHaveAttribute("tabindex", "0");
    // The current turn's marker stays wider than its 1px bar, so it reads without color.
    const marker = await current.evaluate((bar) => ({
      bar: bar.getBoundingClientRect().width,
      marker: parseFloat(getComputedStyle(bar, "::after").width),
      rem: parseFloat(getComputedStyle(document.documentElement).fontSize),
    }));
    expect(marker.bar).toBeLessThan(4);
    expect(marker.marker).toBeGreaterThanOrEqual(marker.rem / 2);
    await expectContained(page);
    await page.screenshot({ path: testInfo.outputPath(`turn-view-dense-${width}.png`) });

    // A strip that still overflows names the turns it shows around the current one.
    count = 1000;
    await page.goto(`/task/@${id}/perf/turns/900`);
    await expect(pane.getByRole("navigation", { name: "Breadcrumb" })).toContainText("Turn 900 of 1000");
    await expect(pane.getByRole("alert")).toHaveCount(0);
    const range = pane.getByTestId("turn-strip-range");
    await expect(range).toHaveText(/^Turns \d+–\d+ of 1000$/);
    const [first = 0, last = 0] = ((await range.textContent()) ?? "").match(/\d+/g)?.map(Number) ?? [];
    expect(first).toBeLessThan(900);
    expect(last).toBeGreaterThan(900);
    // The range counts the partly visible bar at each clipped edge.
    const hidden = 1000 - (last - first + 1);
    const partial = (await clipped()) - hidden;
    expect(partial).toBeGreaterThanOrEqual(0);
    expect(partial).toBeLessThanOrEqual(2);
    await expect(strip.getByRole("link", { name: /^Turn 900, / })).toBeInViewport();
    await expectContained(page);
    await page.screenshot({ path: testInfo.outputPath(`turn-view-overflow-${width}.png`) });

    await page.goto(`/task/@${id}/perf/turns/1001`);
    await expect(pane.getByRole("heading", { name: "Turn 1001 not found" })).toBeVisible();
    await expectContained(page);
    await page.screenshot({ path: testInfo.outputPath(`turn-view-missing-${width}.png`) });
    await page.unrouteAll({ behavior: "ignoreErrors" });
  });
}

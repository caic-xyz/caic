// Browser coverage for the turn view: a running turn growing live, dense and simple timelines, per-call tokens, tool time, resource lanes, span-to-transcript reveal, and transcript-to-span marks.
import type { Locator, Page } from "@playwright/test";
import type { EventStats } from "../../sdk/caic/ts/v1/types.gen";
import { validateHistoryStats } from "../../sdk/caic/ts/v1/validate.gen";
import { createTaskAPI, expect, test, waitForTaskState, type APIClient } from "../helpers";

// toolKey mirrors the frontend's tool identity for a Claude call.
function toolKey(id: string, agentID = ""): string {
  return JSON.stringify(["", "", "", agentID, id]);
}

function toolRow(page: Page, key: string): Locator {
  return page.getByTestId("conversation-pane").locator(`[data-testid="tool-call"][data-tool-key='${key}']`);
}

type RevealLog = { revealedKeys: string[] };

// The reveal mark lasts 1.6 s, less than the scroll and visibility checks
// before it can take under load, so the page logs each tool row as the mark
// is added. Call before the page loads.
async function recordReveals(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const log = window as unknown as Partial<RevealLog>;
    // A test that loads the page again registers this script again.
    if (log.revealedKeys) return;
    const keys: string[] = [];
    log.revealedKeys = keys;
    new MutationObserver((records) => {
      for (const record of records) {
        const el = record.target as HTMLElement;
        if (el.dataset.testid !== "tool-call" || !el.className.includes("_revealed_")) continue;
        if (!(record.oldValue ?? "").includes("_revealed_")) keys.push(el.dataset.toolKey ?? "");
      }
    }).observe(document, { subtree: true, attributes: true, attributeFilter: ["class"], attributeOldValue: true });
  });
}

// revealedBy runs action and returns the key of the one tool row it reveals.
async function revealedBy(page: Page, action: () => Promise<void>): Promise<string> {
  const count = () => page.evaluate(() => (window as unknown as RevealLog).revealedKeys.length);
  const before = await count();
  await action();
  await expect.poll(count).toBe(before + 1);
  return page.evaluate(() => (window as unknown as RevealLog).revealedKeys.at(-1) ?? "");
}

// The page must not scroll sideways and no span may end past the plot.
async function expectContained(page: Page) {
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  const plot = page.getByTestId("turn-timeline-plot");
  const overflow = await plot.evaluate((root) => {
    const edge = root.getBoundingClientRect().right + 1;
    return Array.from(root.querySelectorAll<HTMLElement>("[data-testid=turn-timeline-span]"))
      .filter((el) => el.getBoundingClientRect().right > edge)
      .map((el) => el.getAttribute("aria-label"));
  });
  expect(overflow).toEqual([]);
}

// The dense turn reports usage for 24 root and 5 subagent calls and uses
// eleven tool names; call labels never overlap and the chart scrolls inside
// its own box.
async function expectCallsAndTools(pane: Locator, path: string) {
  const calls = pane.getByTestId("turn-calls");
  await expect(calls.getByTestId("turn-call")).toHaveCount(29);
  await expect(calls.getByTestId("turn-calls-summary")).toHaveText(
    /^cache read \d+% of input · peak context [\d.]+kt of 200kt \(\d+%\) · [\d.]+kt output$/,
  );
  const labels = await calls.getByTestId("turn-call").evaluateAll((columns) =>
    columns
      .map((column) => column.lastElementChild)
      .filter((label): label is Element => !!label?.textContent)
      .map((label) => {
        const range = document.createRange();
        range.selectNodeContents(label);
        const box = range.getBoundingClientRect();
        return { text: label.textContent, left: box.left, right: box.right };
      }),
  );
  expect(labels.map((label) => label.text)).toEqual(["1", "5", "10", "15", "20", "25", "29"]);
  for (const [i, label] of labels.slice(1).entries()) expect(label.left).toBeGreaterThan(labels[i]?.right ?? 0);
  await calls.getByText(/^Call tokens, ~ marks estimated costs \(29\)$/).click();
  const table = calls.getByRole("region", { name: "Call tokens" });
  await expect(table.getByRole("row")).toHaveCount(30);
  await expect(table.getByRole("row").nth(1)).toContainText("~$");

  const tools = pane.getByTestId("turn-tools");
  await expect(tools).toContainText("Concurrent calls overlap, so totals can exceed the turn time.");
  await expect(tools.getByTestId("turn-tool")).toHaveCount(11);
  // Every column fits the pane, even beside a long MCP tool name.
  const toolTable = tools.getByRole("region", { name: "Tool time" });
  expect(await toolTable.evaluate((wrap) => wrap.scrollWidth <= wrap.clientWidth)).toBe(true);
  await calls.scrollIntoViewIfNeeded();
  await calls.page().screenshot({ path });
  await tools.scrollIntoViewIfNeeded();
  await calls.page().screenshot({ path: path.replace("turn-calls-", "turn-tools-") });
  await calls.getByText(/^Call tokens/).click();
}

async function settledTask(api: APIClient, prompt: string, turns: number): Promise<string> {
  const id = await createTaskAPI(api, prompt);
  await waitForTaskState(api, id, "waiting");
  await expect.poll(async () => (await api.getTask(id)).numTurns, { timeout: 15_000 }).toBeGreaterThanOrEqual(turns);
  return id;
}

for (const width of [1280, 390]) {
  test(`a dense turn's timeline links spans and transcript items at ${width}px`, async ({
    page,
    api,
    uniquePrompt,
  }, testInfo) => {
    await page.setViewportSize({ width, height: 844 });
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    // The harness queues turn 2 when the background command ends.
    const id = await settledTask(api, uniquePrompt("FAKE_TURN_TIMELINE"), 2);

    await recordReveals(page);
    await page.goto(`/task/@${id}/perf/turns/1`);
    const pane = page.getByTestId("task-view-pane");
    const timeline = pane.getByTestId("turn-timeline");
    const spans = timeline.getByTestId("turn-timeline-span");
    await expect(spans.first()).toBeVisible();
    await expect(timeline.getByTestId("turn-timeline-lane")).toHaveText([
      /^Model \(inferred\) /,
      /^Tools 24 calls · /,
      /^Subagents 1$/,
      /^Subagent tools 4 calls$/,
      /^Background 1$/,
    ]);
    const background = timeline.getByRole("button", {
      name: /^Background: Race tests, at .*, ends .* after the turn$/,
    });
    await expect(background).toBeVisible();
    await expect(timeline.getByRole("button", { name: /^Tools: Edit .*, failed$/ })).toBeVisible();
    await expect(timeline.getByRole("button", { name: /^Subagents: Review rate limit tests, / })).toBeVisible();
    await expectContained(page);
    await timeline.scrollIntoViewIfNeeded();
    await page.screenshot({ path: testInfo.outputPath(`turn-timeline-dense-${width}.png`) });
    await expectCallsAndTools(pane, testInfo.outputPath(`turn-calls-dense-${width}.png`));

    // Span to transcript: a subagent's call opens its card and turn.
    const childTest = toolKey("toolu_tl_child_test", "claude:task:tl7review");
    const span = timeline.getByRole("button", { name: /^Subagent tools: Bash go test/ });
    const conversation = page.getByTestId("conversation-pane");
    if (width > 768) {
      await expect(conversation).toBeVisible();
      expect(await revealedBy(page, () => span.press("Enter"))).toBe(childTest);
      await expect(toolRow(page, childTest)).toBeInViewport();
      // The named call opens, not only the card and group around it.
      await expect(toolRow(page, childTest)).toHaveJSProperty("open", true);
      // The timeline keeps focus beside the conversation.
      await expect(span).toBeFocused();

      // A call inside the collapsed turn expands it.
      const failed = timeline.getByRole("button", { name: /^Tools: Edit .*, failed$/ });
      const editKey = toolKey("toolu_tl_edit_1");
      expect(await revealedBy(page, () => failed.click())).toBe(editKey);
      const edit = toolRow(page, editKey);
      await expect(edit).toBeInViewport();
      await expect(edit).toHaveJSProperty("open", true);
      await expect(failed).toBeFocused();

      // Transcript to span: hovering or focusing a call marks its spans.
      const linked = timeline.locator('[data-testid="turn-timeline-span"][class*="_linked_"]');
      await edit.locator("summary").first().hover();
      await expect(linked).toHaveCount(1);
      await expect(failed).toHaveClass(/_linked_/);
      await page.mouse.move(0, 0);
      await expect(linked).toHaveCount(0);
      await background.click();
      const launch = toolRow(page, toolKey("toolu_tl_bg"));
      await expect(launch).toBeInViewport();
      await launch.locator("summary").first().focus();
      // The call and the command it launched share the transcript row.
      await expect(linked).toHaveCount(2);
      await expect(background).toHaveClass(/_linked_/);
      await page.screenshot({ path: testInfo.outputPath(`turn-timeline-linked-${width}.png`) });
    } else {
      // The turn view covers the conversation: activation shows it, then reveals there.
      await expect(conversation).toBeHidden();
      // A 0.3s read draws about 2px wide; a tap above the drawn span still
      // lands in its hit area.
      const read = timeline.getByRole("button", { name: /^Subagent tools: Read .*limits_test\.go/ });
      const box = await read.boundingBox();
      if (!box) throw new Error("the read span has no box");
      expect(box.width).toBeLessThan(4);
      const readKey = toolKey("toolu_tl_child_2", "claude:task:tl7review");
      expect(await revealedBy(page, () => read.click({ position: { x: box.width / 2, y: -4 } }))).toBe(readKey);
      await expect(page).toHaveURL(new RegExp(`/task/@${id}[^/]*$`));
      await expect(conversation).toBeVisible();
      const row = toolRow(page, readKey);
      await expect(row).toBeInViewport();
      await expect(row).toHaveJSProperty("open", true);
      await expect(row.locator("summary").first()).toBeFocused();
      await page.screenshot({ path: testInfo.outputPath(`turn-timeline-revealed-${width}.png`) });

      // Back returns to the timeline, where focusing a transcript item cannot
      // happen: the conversation is hidden, so no span is marked.
      await page.goBack();
      await expect(page).toHaveURL(/\/perf\/turns\/1$/);
      await expect(spans.first()).toBeVisible();
      await expect(timeline.locator("._linked_")).toHaveCount(0);
    }
    expect(errors).toEqual([]);
  });

  test(`a simple turn's timeline stays legible at ${width}px`, async ({ page, api, uniquePrompt }, testInfo) => {
    await page.setViewportSize({ width, height: 844 });
    const id = await settledTask(api, uniquePrompt("FAKE_DEMO simple timeline"), 1);

    await page.goto(`/task/@${id}/perf/turns/1`);
    const timeline = page.getByTestId("task-view-pane").getByTestId("turn-timeline");
    await expect(timeline.getByTestId("turn-timeline-span").first()).toBeVisible();
    await expect(timeline.getByTestId("turn-timeline-lane")).toHaveText([
      /^Model \(inferred\) /,
      /^Tools \d+ calls? · /,
    ]);
    await expectContained(page);
    await timeline.scrollIntoViewIfNeeded();
    await page.screenshot({ path: testInfo.outputPath(`turn-timeline-simple-${width}.png`) });
    // The demo harness reports usage per turn only: one line replaces the chart.
    const calls = page.getByTestId("task-view-pane").getByTestId("turn-calls");
    await expect(calls.getByTestId("turn-calls-empty")).toBeVisible();
    await expect(calls.getByRole("img")).toHaveCount(0);
    await calls.scrollIntoViewIfNeeded();
    await page.screenshot({ path: testInfo.outputPath(`turn-calls-empty-${width}.png`) });

    // The exact values stay available as text.
    await timeline.getByText(/^Timeline spans \(\d+\)$/).click();
    const table = timeline.getByRole("region", { name: "Timeline spans" });
    await expect(table.getByRole("row").nth(1)).toBeVisible();
    // Every column fits the pane without scrolling the table sideways.
    expect(await table.evaluate((wrap) => wrap.scrollWidth <= wrap.clientWidth)).toBe(true);
    await page.screenshot({ path: testInfo.outputPath(`turn-timeline-table-${width}.png`) });
  });
}

// At 390px the turn view covers the conversation; the live view behaves the same.
for (const width of [1280, 390]) {
  test(`a running turn's view grows from the task stream and settles when the turn completes at ${width}px`, async ({
    page,
    api,
    uniquePrompt,
  }, testInfo) => {
    await page.setViewportSize({ width, height: 844 });
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    const id = await createTaskAPI(api, uniquePrompt("FAKE_LIVE_TURN"));
    await waitForTaskState(api, id, "running");
    await expect.poll(async () => (await api.getHistoryManifest(id)).metadata.runningTurn?.number).toBe(1);

    await page.goto(`/task/@${id}/perf/turns/1`);
    const pane = page.getByTestId("task-view-pane");
    const breadcrumb = pane.getByRole("navigation", { name: "Breadcrumb" });
    const timeline = pane.getByTestId("turn-timeline");
    const read = timeline.getByRole("button", { name: /^Tools: Read .*main\.go, / });
    if (width <= 768) await expect(page.getByTestId("conversation-pane")).toBeHidden();
    await expect(breadcrumb).toContainText("Turn 1, running");
    await expect(
      pane.getByTestId("turn-strip").getByRole("link", { name: /^Turn 1, running, \d+\.\d{3} s so far$/ }),
    ).toHaveAttribute("aria-current", "page");
    await expect(pane.getByTestId("turn-summary-line")).toHaveText(/^Running · \d+:\d\d so far/);
    await expect(timeline).toContainText("The turn is running: its latest model time is provisional.");
    await expect(timeline.getByText(/^now /)).toBeVisible();
    await expect(read).toHaveAccessibleName(/, still running$/);
    await page.screenshot({ path: testInfo.outputPath(`turn-live-running-${width}.png`) });

    // The read ends and a test run starts; the focused span keeps its element.
    await read.focus();
    await read.evaluate((el) => (el.dataset.watched = "1"));
    await api.sendInput(id, { prompt: { text: "FAKE_LIVE_CONTINUE" } });
    const run = timeline.getByRole("button", { name: /^Tools: Bash go test/ });
    await expect(run).toHaveAccessibleName(/, still running$/);
    await expect(read).not.toHaveAccessibleName(/still running/);
    await expect(read).toHaveAttribute("data-watched", "1");
    await expect(read).toBeFocused();
    await page.screenshot({ path: testInfo.outputPath(`turn-live-growing-${width}.png`) });

    // Completion refreshes the timeline once more in place.
    await api.sendInput(id, { prompt: { text: "FAKE_LIVE_FINISH" } });
    await waitForTaskState(api, id, "waiting");
    await expect(breadcrumb).toContainText("Turn 1 of 1");
    await expect(pane.getByTestId("turn-summary-line")).not.toContainText("Running");
    await expect(timeline).not.toContainText("The turn is running");
    await expect(timeline.getByText(/^end /)).toBeVisible();
    await expect(run).not.toHaveAccessibleName(/still running/);
    await expect(read).toHaveAttribute("data-watched", "1");
    await expect(read).toBeFocused();
    await expect(pane.getByTestId("turn-strip").getByRole("link", { name: /^Turn 1, \d/ })).toBeVisible();
    await expectContained(page);
    await page.screenshot({ path: testInfo.outputPath(`turn-live-completed-${width}.png`) });
    expect(errors).toEqual([]);
  });
}

// Resource lanes share the timeline's axis: samples inside the turn window
// draw at their time, and samples outside it are left out.
for (const width of [1280, 390]) {
  test(`resource lanes align with the turn timeline at ${width}px`, async ({ page, api, uniquePrompt }, testInfo) => {
    await page.setViewportSize({ width, height: 844 });
    const id = await settledTask(api, uniquePrompt("FAKE_TURN_TIMELINE"), 2);
    const manifest = await api.getHistoryManifest(id);
    const turn = manifest.metadata.turns?.[0];
    if (!turn) throw new Error("the task has no first turn");
    const start = Date.parse(turn.startedAt);
    const samples: EventStats[] = [-0.1, 0.05, 0.3, 0.55, 0.8, 0.98].map((share, index) => ({
      ts: Math.round(start + share * turn.durationMs),
      cpuPerc: [40, 85, 230, 310, 120, 60][index] ?? 0,
      cpuCeiling: 4,
      cpuQuota: 0,
      runtimeCPUs: 8,
      memUsed: (300 + index * 40) * 2 ** 20,
      memLimit: 2 ** 31,
      memPerc: ((300 + index * 40) / 2048) * 100,
      netRx: 0,
      netTx: 0,
      blockRead: 0,
      blockWrite: 0,
      diskUsed: -1,
    }));
    const { through } = manifest.bootstrap;
    await page.route(`**/api/caic/v1/tasks/${id}/history/events?*`, (route) =>
      route.fulfill({
        contentType: "text/event-stream",
        body: `event: statsSnapshot\ndata: ${JSON.stringify(validateHistoryStats({ samples, retained: 60 }))}\n\nevent: checkpoint\ndata: ${JSON.stringify({ cursor: "fixture", through })}\n\n`,
      }),
    );
    await page.goto(`/task/@${id}/perf/turns/1`);
    const resources = page.getByTestId("task-view-pane").getByTestId("turn-resources");
    const lanes = resources.getByTestId("turn-resource-lane");
    await expect(lanes).toHaveText([
      "CPU peak 310.0% · scale 0–400%, 100% = 1 CPU",
      /^Memory peak 24\.\d% of limit · scale 0–100%$/,
    ]);
    await expect(resources.getByTestId("turn-resources-note")).toHaveCount(0);
    const tracks = await page.getByTestId("turn-timeline-plot").evaluate((plot) => {
      const box = (el: Element | null | undefined) => {
        if (!el) throw new Error("missing track");
        const r = el.getBoundingClientRect();
        return { left: r.left, right: r.right };
      };
      const lanes = [...document.querySelectorAll("[data-testid=turn-resource-lane]")];
      return {
        timeline: box(plot.querySelector("[data-lane=model]")?.lastElementChild),
        resources: lanes.map((lane) => box(lane.lastElementChild)),
      };
    });
    for (const track of tracks.resources) {
      expect(Math.abs(track.left - tracks.timeline.left)).toBeLessThanOrEqual(1);
      expect(Math.abs(track.right - tracks.timeline.right)).toBeLessThanOrEqual(1);
    }
    await expectContained(page);
    await resources.scrollIntoViewIfNeeded();
    await page.screenshot({ path: testInfo.outputPath(`turn-resources-${width}.png`) });
    await resources.getByText("Resource samples, CPU 100% per CPU (5)").click();
    const table = resources.getByRole("region", { name: "Resource samples in this turn" });
    await expect(table.getByRole("row")).toHaveCount(6);
    expect(await table.evaluate((wrap) => wrap.scrollWidth <= wrap.clientWidth)).toBe(true);
    await page.screenshot({ path: testInfo.outputPath(`turn-resources-table-${width}.png`) });
  });
}

// Browser zoom at 200% halves the CSS viewport of a 1280px window at twice the
// pixel density; a 20px default font scales every rem-sized part. Neither may
// clip, overlap, or push the chart and tables past the pane.
for (const { name, width, scale, rootFont } of [
  { name: "zoom-200", width: 640, scale: 2, rootFont: "" },
  { name: "font-20px", width: 390, scale: 1, rootFont: "20px" },
  { name: "font-20px", width: 1280, scale: 1, rootFont: "20px" },
]) {
  test.describe(`${name} at ${width}px`, () => {
    test.use({ viewport: { width, height: 900 }, deviceScaleFactor: scale });

    test("the call chart and tool time stay legible", async ({ page, api, uniquePrompt }, testInfo) => {
      const id = await settledTask(api, uniquePrompt("FAKE_TURN_TIMELINE"), 2);
      if (rootFont)
        await page.addInitScript((size) => {
          document.addEventListener("DOMContentLoaded", () => {
            document.documentElement.style.fontSize = size;
          });
        }, rootFont);
      await page.goto(`/task/@${id}/perf/turns/1`);
      const pane = page.getByTestId("task-view-pane");
      await expect(pane.getByTestId("turn-timeline-span").first()).toBeVisible();
      if (rootFont)
        expect(await page.evaluate(() => getComputedStyle(document.documentElement).fontSize)).toBe(rootFont);
      await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
      await expectCallsAndTools(pane, testInfo.outputPath(`turn-calls-${name}-${width}.png`));
    });
  });
}

// Re-pricing costs marks the cost figures instead of inserting a status line,
// so the timeline stays where it was when the estimates arrive.
for (const width of [1280, 390]) {
  test(`pricing completes without moving the turn view at ${width}px`, async ({
    page,
    api,
    uniquePrompt,
  }, testInfo) => {
    await page.setViewportSize({ width, height: 844 });
    const id = await settledTask(api, uniquePrompt("FAKE_TURN_TIMELINE"), 2);
    let release: () => void = () => {};
    const held = new Promise<void>((resolve) => (release = resolve));
    await page.route(/\/history\/costs\?/, async (route) => {
      await held;
      await route.continue();
    });
    await page.goto(`/task/@${id}/perf/turns/1`);
    const pane = page.getByTestId("task-view-pane");
    const timeline = pane.getByTestId("turn-timeline");
    await expect(timeline.getByTestId("turn-timeline-span").first()).toBeVisible();
    const live = page.getByTestId("performance-transient-status");
    await expect(live).toHaveText("Updating cost estimates…");
    const busy = pane.getByTestId("turn-summary-line").locator('[aria-busy="true"]');
    await expect(busy).toHaveText("$0.12");
    // Italic, not underlined like an actionable duration, and readable text.
    const style = await busy.evaluate((el) => {
      const rgb = (value: string) => (value.match(/[\d.]+/g) ?? []).map(Number);
      const luminance = ([r = 0, g = 0, b = 0]: number[]) => {
        const channel = (c: number) => (c / 255 <= 0.03928 ? c / 255 / 12.92 : ((c / 255 + 0.055) / 1.055) ** 2.4);
        return 0.2126 * channel(r) + 0.7152 * channel(g) + 0.0722 * channel(b);
      };
      let surface: Element | null = el;
      while (surface && rgb(getComputedStyle(surface).backgroundColor)[3] === 0) surface = surface.parentElement;
      const computed = getComputedStyle(el);
      const fg = luminance(rgb(computed.color));
      const bg = luminance(rgb(surface ? getComputedStyle(surface).backgroundColor : "rgb(255, 255, 255)"));
      return {
        fontStyle: computed.fontStyle,
        decoration: computed.textDecorationLine,
        contrast: (Math.max(fg, bg) + 0.05) / (Math.min(fg, bg) + 0.05),
      };
    });
    expect(style.fontStyle).toBe("italic");
    expect(style.decoration).not.toContain("underline");
    expect(style.contrast).toBeGreaterThanOrEqual(4.5);
    await page.screenshot({ path: testInfo.outputPath(`turn-pricing-${width}.png`) });
    const top = () => timeline.evaluate((el) => el.getBoundingClientRect().top);
    const before = await top();
    const costs = page.waitForResponse((response) => response.url().includes("/history/costs?"));
    release();
    await costs;
    await expect(live).toHaveText("");
    await expect(pane.locator('[aria-busy="true"]')).toHaveCount(0);
    expect(await top()).toBe(before);
  });
}

// Two short reads of the dense turn sit a few pixels apart: the earlier one's
// drawn box lies inside the later one's hit area. The geometry is read in one
// pass, and clicks use positions relative to the first read. Covering spans
// are those after the first read in document order, whose hit areas paint
// above it; the pass also records which span takes the next click.
async function shortReads(page: Page, id: string) {
  await recordReveals(page);
  await page.goto(`/task/@${id}/perf/turns/1`);
  const timeline = page.getByTestId("task-view-pane").getByTestId("turn-timeline");
  const first = timeline.getByRole("button", { name: /^Tools: Read .*\bevents\.go,/ });
  const next = timeline.getByRole("button", { name: /^Tools: Read .*\bmetrics\.go,/ });
  await expect(first).toBeVisible();
  await expect(next).toBeVisible();
  const geometry = await timeline.getByTestId("turn-timeline-span").evaluateAll((spans) => {
    const find = (file: string) => {
      const el = spans.find((span) => span.getAttribute("aria-label")?.startsWith(`Tools: Read ${file},`));
      if (!el) throw new Error(`missing read of ${file}`);
      return el.getBoundingClientRect();
    };
    const a = find("events.go");
    const b = find("metrics.go");
    // Empty space between the two drawn boxes, inside both hit areas.
    const gap = { x: (a.right + b.left) / 2, y: a.top + a.height / 2 };
    const inside = (r: DOMRect, half: { x: number; y: number }) =>
      Math.abs(gap.x - (r.left + r.width / 2)) <= half.x && Math.abs(gap.y - (r.top + r.height / 2)) <= half.y;
    const drawn = spans.filter((el) => {
      const r = el.getBoundingClientRect();
      return inside(r, { x: r.width / 2, y: r.height / 2 });
    }).length;
    // Only hit areas cover the gap; the browser's hit test names the span
    // that takes it, which must come after the first read.
    const owner = document.elementFromPoint(gap.x, gap.y);
    const firstEl = spans.find((span) => span.getAttribute("aria-label")?.startsWith("Tools: Read events.go,"));
    if (!firstEl) throw new Error("missing read of events.go");
    const later = !!owner && (firstEl.compareDocumentPosition(owner) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0;
    const winner = later ? owner.getAttribute("aria-label") : "";
    // Touch adjustment may pick any covering target; these are the spans
    // after the first read, of any kind, whose 1.5rem hit area covers the gap.
    const half = parseFloat(getComputedStyle(document.documentElement).fontSize) * 0.75;
    const covering = spans
      .slice(spans.indexOf(firstEl) + 1)
      .filter((el) => {
        const r = el.getBoundingClientRect();
        return inside(r, { x: Math.max(r.width / 2, half) + 0.5, y: Math.max(r.height / 2, half) + 0.5 });
      })
      .map((el) => el.getAttribute("aria-label") ?? "");
    // Record the span that takes the next click, before it navigates away.
    document.addEventListener(
      "click",
      (event) => {
        const span = event.target instanceof Element ? event.target.closest("[data-testid=turn-timeline-span]") : null;
        document.documentElement.dataset.tappedSpan = span?.getAttribute("aria-label") ?? "";
      },
      { capture: true, once: true },
    );
    return {
      width: a.width,
      apart: b.left - a.right,
      centers: b.left + b.width / 2 - (a.left + a.width / 2),
      gap: { x: gap.x - a.left, y: gap.y - a.top },
      drawn,
      winner: winner ?? "",
      covering,
    };
  });
  expect(geometry.width).toBeLessThan(4);
  expect(geometry.apart).toBeGreaterThan(1);
  // Within 12px, half the minimum hit area, of each other.
  expect(geometry.centers).toBeLessThan(12);
  expect(geometry.drawn).toBe(0);
  const file = /^Tools: Read (\S+),/.exec(geometry.winner)?.[1];
  if (!file) throw new Error(`a read must take the gap, not ${geometry.winner}`);
  expect(geometry.covering).toContain(geometry.winner);
  return { timeline, first, next, gap: geometry.gap, file, covering: geometry.covering };
}

const firstRead = toolKey("toolu_tl_read_9");
const nextRead = toolKey("toolu_tl_read_10");

test("a short span takes clicks on its drawn box over a hovered or linked neighbour at 1280px", async ({
  page,
  api,
  uniquePrompt,
}) => {
  await page.setViewportSize({ width: 1280, height: 844 });
  const id = await settledTask(api, uniquePrompt("FAKE_TURN_TIMELINE"), 2);
  const { timeline, first, next, gap, file } = await shortReads(page, id);

  // A hovered neighbour does not take the click. The click's hit-target check
  // fails if another span would receive it at the first read's centre.
  await next.hover();
  expect(await revealedBy(page, () => first.click())).toBe(firstRead);
  await expect(first).toBeFocused();

  // A linked neighbour does not take it either: focusing its transcript row
  // marks it, and the mark holds until the click moves focus.
  await toolRow(page, nextRead).locator("summary").first().focus();
  await expect(next).toHaveClass(/_linked_/);
  await expect(timeline.locator('[data-testid="turn-timeline-span"][class*="_linked_"]')).toHaveCount(1);
  expect(await revealedBy(page, () => first.click())).toBe(firstRead);
  await expect(first).toBeFocused();

  // Where only hit areas overlap, the later span takes the click. Force skips
  // the hit-target check, which expects the first read there.
  const gapKey = await revealedBy(page, () => first.click({ position: gap, force: true }));
  await expect(toolRow(page, gapKey)).toContainText(file);
});

test.describe("touch", () => {
  test.use({ hasTouch: true });

  test("a short span takes taps on its drawn box over a hovered neighbour at 390px", async ({
    page,
    api,
    uniquePrompt,
  }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    const id = await settledTask(api, uniquePrompt("FAKE_TURN_TIMELINE"), 2);
    const { first, next } = await shortReads(page, id);
    const conversation = page.getByTestId("conversation-pane");

    await next.hover();
    expect(await revealedBy(page, () => first.tap())).toBe(firstRead);
    await expect(page).toHaveURL(new RegExp(`/task/@${id}[^/]*$`));
    await expect(conversation).toBeVisible();
    await expect(toolRow(page, firstRead).locator("summary").first()).toBeFocused();

    // Where only hit areas overlap, a later span whose hit area covers the
    // tap takes it; touch adjustment chooses among them, whatever their kind.
    const again = await shortReads(page, id);
    await again.first.tap({ position: again.gap, force: true });
    await expect(page).toHaveURL(new RegExp(`/task/@${id}[^/]*$`));
    const tapped = await page.evaluate(() => document.documentElement.dataset.tappedSpan ?? "");
    expect(again.covering).toContain(tapped);
    // The revealed row is the tapped span's: its summary holds the span's tool
    // name and target (the transcript may show a relative path before the basename).
    const focused = page.getByTestId("conversation-pane").locator('[data-testid="tool-call"]:has(> summary:focus)');
    await expect(focused).toHaveCount(1);
    const name = tapped.replace(/^[^:]+: /, "").replace(/, (at|from) .*$/, "");
    const summary = (await focused.locator("summary").first().textContent()) ?? "";
    for (const part of name.split(/\s+/)) expect(summary).toContain(part);
  });
});

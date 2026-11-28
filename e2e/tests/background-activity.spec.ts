// Browser coverage for the task-card background tooltip, task-detail chips, reveal fallbacks, and reload while work settles.
import { ok } from "node:assert/strict";
import type { Page } from "@playwright/test";
import type { TaskBackground } from "../../sdk/caic/ts/v1/types.gen";
import { test, expect, createTaskAPI, type APIClient } from "../helpers";

const toolCall = (page: Page, tool: string) => page.locator(`[data-testid="tool-call"][data-tool-key$='"${tool}"]']`);

async function background(api: APIClient, id: string) {
  const task = ((await api.listTasks()) ?? []).find((t) => t.id === id);
  return { state: task?.state, background: task?.background ?? null };
}

for (const width of [390, 1280]) {
  test.describe(`background work at ${width}px`, () => {
    test.use({ hasTouch: width === 390 });
    test(`background work stays visible while it settles at ${width}px`, async ({
      page,
      api,
      uniquePrompt,
    }, testInfo) => {
      test.setTimeout(90_000);
      await page.setViewportSize({ width, height: 844 });
      const mobile = width < 768;
      const id = await createTaskAPI(api, uniquePrompt("FAKE_BACKGROUND_WORK"));
      const card = page.locator(`[data-task-id="${id}"]`);
      const row = card.getByTestId("task-card-background");
      const strip = page.getByTestId("background-strip");
      const chips = strip.getByTestId("background-chip");
      const prompt = page.getByTestId("task-detail-prompt");

      // The card counts come from the task and the strip from history metadata;
      // each state checks both against the API, before and after a reload.
      const checkState = async (
        name: string,
        expected: TaskBackground | null,
        text: string | null,
        chipNames: string[],
      ) => {
        for (const reload of [false, true]) {
          await test.step(`${name}${reload ? " after reload" : ""}`, async () => {
            await page.goto(mobile ? "/" : `/task/@${id}`);
            if (reload) await page.reload();
            await expect(card).toBeVisible();
            if (text === null) await expect(row).toHaveCount(0);
            else {
              await expect(row).toHaveAccessibleName(text);
              await expect(card).toHaveAccessibleName(new RegExp(text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));
              // The counts follow the context size on the model line and stay
              // inside the card. Tasks from parallel tests reorder the list, so
              // one layout snapshot measures all three boxes.
              const {
                row: rowBox,
                tokens: tokensBox,
                card: cardBox,
              } = await card.evaluate((el) => {
                const rect = (testId: string) =>
                  el.querySelector(`[data-testid="${testId}"]`)?.getBoundingClientRect().toJSON() as
                    DOMRect | undefined;
                return {
                  row: rect("task-card-background"),
                  tokens: rect("task-card-tokens"),
                  card: el.getBoundingClientRect().toJSON() as DOMRect,
                };
              });
              ok(rowBox && tokensBox);
              expect(rowBox.left).toBeGreaterThan(tokensBox.right);
              expect(Math.abs(rowBox.top + rowBox.height / 2 - (tokensBox.top + tokensBox.height / 2))).toBeLessThan(2);
              expect(rowBox.right).toBeLessThanOrEqual(cardBox.right);
              if (name === "running") {
                if (mobile) await row.tap();
                else await row.hover();
                const tooltip = page.getByRole("tooltip").filter({ hasText: "Review the API handlers" });
                await expect(tooltip).toHaveText(
                  /^• Command: Run fix, verify, and test\s+• general-purpose: Review the API handlers \((?:\d+ms|\d+:\d{2}(?::\d{2})?)\)\s+• Watch: fix\/verify\/test results$/,
                );
                if (mobile) await expect(page).toHaveURL(/\/$/);
                const tooltipBox = await tooltip.boundingBox();
                ok(tooltipBox);
                expect(tooltipBox.x).toBeGreaterThanOrEqual(0);
                expect(tooltipBox.x + tooltipBox.width).toBeLessThanOrEqual(width);
                expect(tooltipBox.y).toBeGreaterThanOrEqual(0);
                expect(tooltipBox.y + tooltipBox.height).toBeLessThanOrEqual(844);
                if (!mobile && !reload) {
                  await row.click();
                  await expect(tooltip).toBeHidden();
                  await page.mouse.move(0, 843);
                  await row.hover();
                  await expect(tooltip).toBeVisible();
                  await page.mouse.move(0, 843);
                  await expect(tooltip).toBeHidden();
                  await row.hover();
                }
                // Preserve focus when dismissing the open tooltip. Focusing its
                // trigger can scroll the card list and dismiss the tooltip first.
                await expect(tooltip).toBeVisible();
                await page.keyboard.press("Escape");
                await expect(tooltip).toBeHidden();
              }
            }
            if (mobile) {
              if (!reload) await page.screenshot({ path: testInfo.outputPath(`background-${name}-card-${width}.png`) });
              await card.click();
            }
            await expect(page).toHaveURL(new RegExp(`/task/@${id}`));
            await expect(prompt).toBeVisible();
            if (expected === null) {
              await expect(strip).toHaveCount(0);
            } else {
              await expect(chips).toHaveCount(chipNames.length);
              for (const [i, chipName] of chipNames.entries())
                await expect(chips.nth(i)).toHaveAccessibleName(new RegExp(`^${chipName} \\d+:\\d\\d$`));
              const kinds = await chips.evaluateAll((els) => els.map((el) => el.getAttribute("data-kind")));
              const count = (kind: string) => kinds.filter((k) => k === kind).length;
              expect({ commands: count("command"), watches: count("watch"), agents: count("agent") }).toEqual(expected);
              const stripBox = await strip.boundingBox();
              const promptBox = await prompt.boundingBox();
              ok(stripBox && promptBox);
              expect(stripBox.y + stripBox.height).toBeLessThanOrEqual(promptBox.y);
              expect(promptBox.y + promptBox.height).toBeLessThanOrEqual(844);
              expect(stripBox.x + stripBox.width).toBeLessThanOrEqual(width);
              const chipBoxes = await chips.evaluateAll((els) =>
                els.map((el) => {
                  const r = el.getBoundingClientRect();
                  return { top: r.top, right: r.right };
                }),
              );
              if (mobile) {
                // A phone keeps one scrolling row so the prompt stays in place.
                expect(new Set(chipBoxes.map((b) => b.top)).size).toBe(1);
              } else {
                // A wide pane wraps chips instead of clipping the last one.
                for (const [i, b] of chipBoxes.entries()) {
                  expect(b.right).toBeLessThanOrEqual(stripBox.x + stripBox.width);
                  await expect(chips.nth(i)).toBeInViewport({ ratio: 1 });
                }
              }
            }
            if (!reload) await page.screenshot({ path: testInfo.outputPath(`background-${name}-${width}.png`) });
          });
        }
      };

      // The command and the agent keep the task running after its result.
      const running = { commands: 1, watches: 1, agents: 1 };
      await expect.poll(() => background(api, id)).toEqual({ state: "running", background: running });
      await checkState("running", running, "Background: 1 command · 1 agent · 1 watch", [
        "Command Run fix, verify, and test",
        "general-purpose Review the API handlers",
        "Watch fix/verify/test results",
      ]);

      await test.step("chips reveal their collapsed tool call and agent card", async () => {
        const checks = toolCall(page, "toolu_bg_checks");
        await expect(checks).toBeHidden();
        // Keyboard activation expands the collapsed tool group.
        await chips.nth(0).focus();
        await page.keyboard.press("Enter");
        await expect(checks).toBeInViewport({ ratio: mobile ? 0 : 1 });
        await expect(checks.locator("summary")).toBeFocused();
        await expect(checks.getByTestId("background-command-chip")).toHaveText("background");
        await expect(toolCall(page, "toolu_bg_watch").getByTestId("background-command-chip")).toHaveText("watching");
        const agent = page.locator(`[data-testid="native-subagent-card"][data-background="true"]`);
        await chips.nth(1).click();
        await expect(agent).toBeInViewport();
        await expect(agent.locator("summary")).toBeFocused();
        // Keyboard navigation between chips shows the focus ring.
        await chips.nth(2).focus();
        await page.keyboard.press("Shift+Tab");
        await expect(chips.nth(1)).toBeFocused();
        expect(await chips.nth(1).evaluate((el) => el.matches(":focus-visible"))).toBe(true);
        await page.screenshot({ path: testInfo.outputPath(`background-chip-focus-${width}.png`) });
      });

      // Only the watch remains, and a watch never keeps the task running.
      await api.sendInput(id, { prompt: { text: "FAKE_BACKGROUND_SETTLE_WORK" } });
      const watching = { commands: 0, watches: 1, agents: 0 };
      await expect.poll(() => background(api, id)).toEqual({ state: "waiting", background: watching });
      await checkState("watch", watching, "Background: 1 watch", ["Watch fix/verify/test results"]);

      await test.step("the watch chip expands its elided turn", async () => {
        const watch = toolCall(page, "toolu_bg_watch");
        await expect(watch).toHaveCount(0);
        await chips.first().click();
        await expect(watch).toBeInViewport();
        await expect(watch.locator("summary")).toBeFocused();
        // A pointer activation shows no focus ring, so the row is marked instead.
        await expect(watch).toHaveClass(/revealed/);
        await expect(watch.getByTestId("background-command-chip")).toHaveText("watching");
        await expect(watch.getByText("fix/verify/test results", { exact: true })).toBeVisible();
        await page.screenshot({ path: testInfo.outputPath(`background-watch-revealed-${width}.png`) });
      });

      await api.sendInput(id, { prompt: { text: "FAKE_BACKGROUND_SETTLE_WATCH" } });
      await expect.poll(() => background(api, id)).toEqual({ state: "waiting", background: null });
      await checkState("settled", null, null, []);
    });
  });
}

for (const width of [390, 1280]) {
  test(`chips reveal calls outside the root transcript at ${width}px`, async ({
    page,
    api,
    uniquePrompt,
  }, testInfo) => {
    await page.setViewportSize({ width, height: 844 });
    // Instant scrolling keeps the in-viewport checks free of scroll timing.
    await page.emulateMedia({ reducedMotion: "reduce" });
    const id = await createTaskAPI(api, uniquePrompt("FAKE_BACKGROUND_EDGES"));
    await page.goto(`/task/@${id}`);
    const chips = page.getByTestId("background-strip").getByTestId("background-chip");
    await expect(chips).toHaveCount(4);
    const chip = (label: string) => chips.filter({ hasText: label });
    // Whole at 1280px. The pane scrolls in whole pixels, so a call that ends
    // the content at a fractional height can stay clipped by under a pixel.
    const shown = width === 1280 ? 0.99 : 0;
    // An agent whose harness reports no kind keeps the generic name.
    await expect(chip("Profile the build")).toHaveAccessibleName(/^Agent Profile the build /);

    await test.step("a command in an agent's transcript opens the agent card", async () => {
      const agent = page.locator('[data-native-id="claude:task:edge7agent"]');
      const call = toolCall(page, "toolu_edge_child");
      await expect(call).toHaveCount(0);
      await chip("Profile build").click();
      await expect(agent).toHaveAttribute("open", "");
      await expect(call).toBeInViewport({ ratio: shown });
      await expect(call.locator("summary")).toBeFocused();
      await expect(call).toHaveClass(/revealed/);
      await page.screenshot({ path: testInfo.outputPath(`background-reveal-agent-${width}.png`) });
    });

    await test.step("a command without a call reveals the row current at its start", async () => {
      await chip("Detached cleanup").click();
      const row = page.locator(":focus");
      await expect(row).toHaveAttribute("data-anchor-key", /.+/);
      await expect(row).toBeInViewport();
      await expect(row).toHaveClass(/revealed/);
      await page.screenshot({ path: testInfo.outputPath(`background-reveal-detached-${width}.png`) });
    });

    await test.step("a watch from a compacted context expands it", async () => {
      // The agent settles before the compaction, so its card moves into the
      // archived context while the command in its transcript still runs.
      await api.sendInput(id, { prompt: { text: "FAKE_BACKGROUND_EDGE_AGENT_DONE" } });
      await expect(chips).toHaveCount(3);
      await api.sendInput(id, { prompt: { text: "FAKE_SKILLS compact" } });
      await expect(page.getByTestId("history-section")).toHaveCount(1);
      // A reload drops the card the earlier reveal pinned open.
      await page.reload();
      await expect(page.locator('[data-native-id="claude:task:edge7agent"]')).toHaveCount(0);
      const watch = toolCall(page, "toolu_edge_watch");
      await expect(watch).toHaveCount(0);
      await chip("deploy log errors").click();
      await expect(watch).toBeInViewport();
      await expect(watch.locator("summary")).toBeFocused();
      await expect(watch).toHaveClass(/revealed/);
      await page.screenshot({ path: testInfo.outputPath(`background-reveal-archived-${width}.png`) });
    });

    await test.step("a command of a settled agent in a compacted context opens its card there", async () => {
      const agent = page.getByTestId("history-section").locator('[data-native-id="claude:task:edge7agent"]');
      const call = toolCall(page, "toolu_edge_child");
      await chip("Profile build").click();
      await expect(agent).toHaveAttribute("open", "");
      await expect(call).toBeInViewport({ ratio: shown });
      await expect(call.locator("summary")).toBeFocused();
      await expect(call).toHaveClass(/revealed/);
      await page.screenshot({ path: testInfo.outputPath(`background-reveal-archived-agent-${width}.png`) });
    });
  });
}

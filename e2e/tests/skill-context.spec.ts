// Browser coverage for context skills, disclosures, compacted live-tail scrolling, and recursive Markdown navigation.
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
    // A later root completion must not replace a disclosed older context or
    // steal focus while the user is reading its skills.
    const turns = page.getByRole("button", { name: /^[▶▼]\s*\d+ messages?/ });
    // Skill metadata appears before the disclosed transcript finishes loading.
    await expect(turns).toHaveCount(1);
    const turnCount = await turns.count();
    const context = page.getByRole("button", { name: /Compacted session/ });
    await context.focus();
    await api.sendInput(id, { prompt: { text: "continue current context" } });
    await waitForTaskState(api, id, "waiting");
    await expect(turns).toHaveCount(turnCount + 1);
    await expect(context).toBeFocused();
    await expect(prior.getByRole("link")).toHaveCount(2);
    // Closing a second context must retain the older section's DOM ownership.
    const oldHeader = await context.elementHandle();
    const oldSkill = await prior.getByRole("link", { name: "review", exact: true }).elementHandle();
    await prior.getByRole("link", { name: "review", exact: true }).focus();
    await api.sendInput(id, { prompt: { text: "FAKE_SKILLS compact" } });
    await waitForTaskState(api, id, "waiting");
    await expect(page.getByRole("button", { name: /Compacted session/ })).toHaveCount(2);
    expect(await oldHeader?.evaluate((element) => element.isConnected)).toBe(true);
    expect(await oldSkill?.evaluate((element) => element.isConnected && element === document.activeElement)).toBe(true);
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);

    expect(errors).toEqual([]);
  });
}

for (const width of [390, 1280]) {
  test(`switching to a running compacted task follows its live tail at ${width}px`, async ({
    page,
    api,
    uniquePrompt,
  }) => {
    await page.setViewportSize({ width, height: 700 });
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    const id = await createTaskAPI(api, uniquePrompt("FAKE_SKILLS load"));
    await waitForTaskState(api, id, "waiting");
    for (let i = 0; i < 12; i++) {
      await api.sendInput(id, { prompt: { text: "FAKE_SKILLS compact" } });
      await waitForTaskState(api, id, "waiting");
    }
    await api.sendInput(id, { prompt: { text: "FAKE_CONCURRENT_STREAMS" } });
    await waitForTaskState(api, id, "running");
    const other = await createTaskAPI(api, uniquePrompt("other scroll conversation"));
    await waitForTaskState(api, other, "waiting");
    await page.goto(`/task/@${other}`);
    const area = page.getByTestId("task-message-area");
    const atBottom = async () => {
      await expect(area).toHaveAttribute("aria-busy", "false");
      await expect.poll(() => area.evaluate((el) => el.scrollHeight - el.scrollTop - el.clientHeight)).toBeLessThan(2);
    };
    for (let cycle = 0; cycle < 3; cycle++) {
      if (width === 390) await page.keyboard.press("Escape");
      const card = page.locator(`[data-task-id='${id}']`);
      await card.focus();
      await card.press("Enter");
      await expect(page.getByRole("button", { name: /Compacted session/ })).toHaveCount(12);
      await expect.poll(() => area.evaluate((el) => el.scrollHeight > el.clientHeight)).toBe(true);
      await atBottom();
      if (cycle === 0) {
        await api.sendInput(id, { prompt: { text: "FAKE_CONCURRENT_STREAMS_CONTINUE" } });
        await expect(page.getByText("Parent summary remains separate.", { exact: false })).toBeVisible();
        await atBottom();
        await area.hover();
        await page.mouse.wheel(0, -10_000);
        await expect.poll(() => area.evaluate((el) => el.scrollTop)).toBe(0);
        await api.sendInput(id, { prompt: { text: "FAKE_CONCURRENT_STREAMS_RECONNECT" } });
        await expect(area).toContainText("parent missed output");
        expect(await area.evaluate((el) => el.scrollTop)).toBe(0);
        await area.focus();
        await page.keyboard.press("Control+End");
        await atBottom();
      }
      if (width === 390) await page.keyboard.press("Escape");
      const otherCard = page.locator(`[data-task-id='${other}']`);
      await otherCard.focus();
      await otherCard.press("Enter");
      await atBottom();
    }
    expect(errors).toEqual([]);
  });
}

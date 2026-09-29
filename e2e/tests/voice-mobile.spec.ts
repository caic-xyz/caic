// Browser coverage for the full-screen mobile task view during native voice sessions.

import { createTaskAPI, expect, test, waitForTaskState } from "../helpers";

test("mobile voice task links open details and browser back restores the voice list", async ({ page, api }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.addInitScript(() => {
    Object.assign(window, { goModeHost: { isVoiceConnected: () => true } });
  });
  const id = await createTaskAPI(api, "Review mobile voice task view");
  const task = await waitForTaskState(api, id, "waiting", 30_000);

  await page.goto("/");
  const voiceView = page.getByTestId("mobile-voice-tasks");
  await expect(voiceView).toBeVisible();
  const taskLink = voiceView.locator(`[data-task-id="${id}"]`);
  await expect(taskLink).toContainText(task.title);
  await expect(taskLink).toContainText(/#\d+/);
  await expect(taskLink).toHaveAttribute("href", `/task/@${id}`);
  await expect(page.getByTestId("new-task-button")).toBeHidden();
  await expect(page.getByTestId("new-task-form")).toBeHidden();
  await expect(page.getByTestId("task-list")).toBeHidden();
  await expect.poll(() => voiceView.evaluate((el) => el.scrollWidth <= el.clientWidth)).toBe(true);
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);

  await taskLink.click();
  await expect(page).toHaveURL(new RegExp(`/task/@${id}$`));
  await expect(voiceView).not.toBeVisible();
  await expect(page.getByTestId("detail-pane")).toBeVisible();

  await page.goBack();
  await expect(voiceView).toBeVisible();

  await page.setViewportSize({ width: 900, height: 844 });
  await expect(voiceView).not.toBeVisible();
  await expect(page.getByTestId("task-list")).toBeVisible();
});

test("six long task titles fit above voice controls on a Pixel 6", async ({ page, api }) => {
  await page.setViewportSize({ width: 393, height: 851 });
  await page.addInitScript(() => {
    Object.assign(window, { goModeHost: { isVoiceConnected: () => true } });
  });

  const ids = await Promise.all(
    Array.from({ length: 6 }, (_, index) =>
      createTaskAPI(
        api,
        `Review the complete implementation and carefully explain the next steps for the mobile voice task layout ${index}`,
      ),
    ),
  );
  await Promise.all(ids.map((id) => waitForTaskState(api, id, "waiting", 30_000)));

  await page.goto("/");
  const voiceView = page.getByTestId("mobile-voice-tasks");
  const rows = voiceView.locator("li");
  await expect.poll(() => rows.count()).toBeGreaterThanOrEqual(6);
  await expect.poll(() => rows.nth(5).evaluate((row) => row.getBoundingClientRect().bottom)).toBeLessThan(851 - 180);

  const name = voiceView.locator(`[data-task-id="${ids[0]}"]`).locator("span").nth(1);
  const titleMetrics = await name.evaluate((el) => ({
    height: el.getBoundingClientRect().height,
    lineHeight: Number.parseFloat(getComputedStyle(el).lineHeight),
    fullTitle: el.textContent,
  }));
  expect(titleMetrics.fullTitle?.length).toBeGreaterThan(80);
  expect(titleMetrics.height).toBeLessThanOrEqual(titleMetrics.lineHeight * 2 + 1);
});

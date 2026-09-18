// End-to-end coverage for task statistics navigation and mobile viewport containment.

import { createTaskAPI, expect, test } from "../helpers";

test("task statistics open as a contained mobile detail view", async ({ page, api }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const id = await createTaskAPI(api, "Check mobile task statistics");

  await page.goto(`/task/@${id}`);
  await page.getByRole("link", { name: "Task statistics" }).click();

  await expect(page).toHaveURL(new RegExp(`/task/@${id}/stats$`));
  await expect(page.getByText("Performance", { exact: true })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Resources" })).toBeVisible();
  await expect(page.getByTitle("Back to task")).toBeVisible();
  await expect
    .poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth))
    .toBe(true);
});

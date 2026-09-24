// End-to-end coverage for the cross-task usage dashboard and mobile containment.

import { createTaskAPI, expect, test, waitForTaskState } from "../helpers";

test("usage dashboard shows rollup activity and stays contained on mobile", async ({ page, api }) => {
  const id = await createTaskAPI(api, "Seed the usage dashboard");
  await waitForTaskState(api, id, "waiting");

  await expect(async () => {
    const dashboard = await api.getUsageDashboard();
    expect(dashboard.days.length).toBeGreaterThan(0);
  }).toPass({ timeout: 10_000, intervals: [250] });

  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/usage");

  await expect(page.getByRole("heading", { name: "Usage" })).toBeVisible();
  await expect(page.getByLabel("Usage date range")).toHaveValue("30");
  await expect(page.getByText(/Data since/)).toBeVisible();
  await expect(page.getByRole("region", { name: "Usage summary" })).toBeVisible();
  await expect(page.getByText("Compactions")).toBeVisible();
  await expect(page.getByText("API time")).toBeVisible();
  await expect(page.getByText("Turn wall time")).toBeVisible();
  await expect(page.getByRole("columnheader", { name: "Cache hit" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Tools" })).toBeVisible();
  await expect(page.getByTestId("usage-charts")).toBeVisible();
  await page.getByLabel("Usage date range").selectOption("all");
  await expect(page.getByLabel("Usage date range")).toHaveValue("all");
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
});

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
  // The rollup holds the current day, which is still accumulating, so the trend
  // states that its last point is provisional.
  await expect(page.getByTestId("usage-charts")).toContainText("still accumulating");
  await page.getByLabel("Usage date range").selectOption("all");
  await expect(page.getByLabel("Usage date range")).toHaveValue("all");
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);

  // Drill into the fake task's model: the banner confirms the filter, the
  // URL hash carries it, and clearing restores the unfiltered leaderboard.
  const modelRow = page.getByRole("button", { name: "fake-model" });
  await expect(modelRow).toBeVisible();
  await modelRow.click();
  await expect(page.getByText("Showing usage for")).toBeVisible();
  await expect(page.getByRole("region", { name: "Usage summary" })).toBeVisible();
  await expect(page.getByTestId("usage-charts")).toBeVisible();
  await expect(page).toHaveURL(/#model=fake-model$/);
  await page.getByRole("button", { name: "Show all usage" }).click();
  await expect(page.getByText("Showing usage for")).toBeHidden();
  await expect(page).toHaveURL(/^[^#]*$/);
  await expect(modelRow).toBeVisible();

  // Drill into a harness: the model list stays visible through the
  // harness-by-model cross product, and selecting a model narrows to the pair.
  const harnessPanel = page.locator("section", { has: page.getByRole("heading", { name: "Harnesses" }) });
  await harnessPanel.getByRole("button").first().click();
  await expect(page).toHaveURL(/#harness=/);
  await expect(modelRow).toBeVisible();
  await modelRow.click();
  await expect(page).toHaveURL(/#harness=.+&model=fake-model$/);
  await expect(page.getByText("Showing usage for")).toContainText("harness");
  await expect(page.getByText("Showing usage for")).toContainText("model fake-model");
});

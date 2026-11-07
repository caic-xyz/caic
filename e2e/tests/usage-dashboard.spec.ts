// End-to-end coverage for the cross-task usage dashboard and mobile containment.

import { createTaskAPI, expect, test, waitForTaskState } from "../helpers";
import { usageHistory } from "../visual-fixtures";
import type { UsageDashboardCost } from "../../sdk/caic/ts/v1/types.gen";

test("usage dashboard shows rollup activity and stays contained on mobile", async ({ page, api }) => {
  const id = await createTaskAPI(api, "Seed the usage dashboard");
  await waitForTaskState(api, id, "waiting");

  await expect(async () => {
    const dashboard = await api.getUsageDashboard();
    expect(dashboard.days.length).toBeGreaterThan(0);
  }).toPass({ timeout: 10_000, intervals: [250] });

  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/monitoring");

  await expect(page.getByRole("region", { name: "Usage", exact: true })).toBeVisible();
  await expect(page.getByLabel("Usage date range")).toHaveValue("30");
  await expect(page.getByRole("region", { name: "Usage summary" })).toBeVisible();
  await expect(page.getByRole("region", { name: "Usage summary" }).getByText("Compactions")).toBeVisible();
  await expect(page.getByText("API time")).toBeVisible();
  await expect(page.getByText("Turn wall time")).toBeVisible();
  await expect(page.getByRole("columnheader", { name: "Cache hit" })).toBeVisible();
  // Both leaderboards split compactions so a model or harness that compacts
  // often is visible beside its token totals.
  await expect(page.getByRole("columnheader", { name: "Compactions" })).toHaveCount(2);
  await expect(page.getByRole("heading", { name: "Tools" })).toBeVisible();
  await expect(page.getByTestId("usage-charts")).toBeVisible();
  // Daily totals are a normal table at the end of the panels, newest first.
  await expect(page.getByRole("region", { name: "Daily usage totals" })).toBeVisible();
  await page.getByLabel("Usage date range").selectOption("all");
  await expect(page.getByLabel("Usage date range")).toHaveValue("all");
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);

  // Drill into the fake task's model: the banner confirms the filter, the
  // URL query carries it, and clearing restores the unfiltered leaderboard.
  const modelRow = page.getByRole("button", { name: "fake-model" });
  await expect(modelRow).toBeVisible();
  await modelRow.click();
  await expect(page.getByText("Showing usage for")).toBeVisible();
  await expect(page.getByRole("region", { name: "Usage summary" })).toBeVisible();
  await expect(page.getByTestId("usage-charts")).toBeVisible();
  await expect(page).toHaveURL(/range=all&model=fake-model$/);
  await page.getByRole("button", { name: "Show all usage" }).click();
  await expect(page.getByText("Showing usage for")).toBeHidden();
  await expect(page).toHaveURL(/\/monitoring\?range=all$/);
  await expect(modelRow).toBeVisible();

  await page.reload();
  await expect(page.getByLabel("Usage date range")).toHaveValue("all");
  await page.getByRole("navigation", { name: "Monitoring sections" }).getByRole("link", { name: "System" }).click();
  await expect(page.getByRole("region", { name: "System", exact: true })).toBeVisible();
  await page.getByRole("navigation", { name: "Monitoring sections" }).getByRole("link", { name: "Usage" }).click();
  await expect(page.getByLabel("Usage date range")).toHaveValue("all");

  // Drill into a harness: the model list stays visible through the
  // harness-by-model cross product, and selecting a model narrows to the pair.
  const harnessPanel = page.getByRole("heading", { name: "Harnesses" }).locator("..");
  await harnessPanel.getByRole("button").first().click();
  await expect(page).toHaveURL(/range=all&harness=/);
  await expect(modelRow).toBeVisible();
  await modelRow.click();
  await expect(page).toHaveURL(/range=all&harness=.+&model=fake-model$/);
  await expect(page.getByText("Showing usage for")).toContainText("harness");
  await expect(page.getByText("Showing usage for")).toContainText("model fake-model");
  await page.reload();
  await expect(page.getByText("Showing usage for")).toContainText("model fake-model");
  await page.getByRole("button", { name: "Show all usage" }).click();
  await page.goBack();
  await expect(page.getByText("Showing usage for")).toContainText("model fake-model");
  await expect(page.getByLabel("Usage date range")).toHaveValue("all");
});

test("usage cost distinguishes native zero, estimates, and unknown coverage", async ({ page }, testInfo) => {
  const dashboard = usageHistory();
  const costs: UsageDashboardCost[] = [
    { reportedUSD: 0, estimatedUSD: 0, reportedParts: 1, estimatedParts: 0, unknownParts: 0 },
    { reportedUSD: 0, estimatedUSD: 0, reportedParts: 0, estimatedParts: 0, unknownParts: 1 },
    { reportedUSD: 0, estimatedUSD: 0.25, reportedParts: 0, estimatedParts: 1, unknownParts: 0 },
    { reportedUSD: 0.5, estimatedUSD: 0.25, reportedParts: 1, estimatedParts: 1, unknownParts: 1 },
  ];
  dashboard.days = dashboard.days.slice(0, 4);
  for (const [index, day] of dashboard.days.entries()) {
    const cost = costs[index];
    expect(cost).toBeDefined();
    if (!cost) throw new Error("Missing cost fixture");
    day.cost = cost;
    day.costUSD = cost.reportedUSD + cost.estimatedUSD;
    for (const model of day.models) {
      model.cost = cost;
      model.costUSD = day.costUSD;
    }
    for (const harness of day.harnesses) {
      harness.cost = cost;
      harness.costUSD = day.costUSD;
      for (const model of harness.models) {
        model.cost = cost;
        model.costUSD = day.costUSD;
      }
    }
  }
  await page.route("**/api/caic/v1/usage/dashboard", (route) => route.fulfill({ json: dashboard }));
  await page.goto("/monitoring?range=all");
  const daily = page.getByRole("region", { name: "Daily usage totals" });
  await expect(daily.getByText("$0 · reported", { exact: true })).toBeVisible();
  await expect(daily.getByText("Unknown", { exact: true })).toBeVisible();
  await expect(daily.getByText("$0.25 · estimated", { exact: true })).toBeVisible();
  await expect(daily.getByText("$0.75 known subtotal · incomplete", { exact: true })).toBeVisible();
  const chart = page.getByRole("img", { name: "Cost per day in US dollars" });
  await expect(chart).toBeVisible();
  // A native zero is a plotted point; a missing cost is a gap.
  await expect(chart.locator('g[aria-label="dot"] circle')).toHaveCount(3);
  for (const width of [1280, 390]) {
    await page.setViewportSize({ width, height: 1000 });
    await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    await page
      .getByTestId("usage-charts")
      .screenshot({ path: testInfo.outputPath(`cost-provenance-charts-${width}.png`) });
  }
});

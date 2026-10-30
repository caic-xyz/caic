// Browser coverage for whole-machine history, stale-sample errors, and narrow-screen layout.

import { expect, test } from "../helpers";
import type { ISOTimestamp, SystemStatsResp } from "../../sdk/caic/ts/v1/types.gen";

for (const width of [390, 1280]) {
  test(`system utilization at ${width}px`, async ({ page, api }) => {
    await page.setViewportSize({ width, height: 850 });
    const errors: string[] = [];
    page.on("pageerror", (e) => errors.push(e.message));
    // Validate the real endpoint separately from deterministic chart data.
    const response = await api.getSystemStats();
    expect(Array.isArray(response.samples)).toBe(true);
    for (const sample of response.samples) {
      expect(sample).toMatchObject({
        time: expect.any(String),
        cpus: expect.any(Array),
        memUsed: expect.any(Number),
        memTotal: expect.any(Number),
        memPerc: expect.any(Number),
        netRx: expect.any(Number),
        netTx: expect.any(Number),
        diskUsed: expect.any(Number),
        diskTotal: expect.any(Number),
      });
    }
    const fixture: SystemStatsResp = {
      samples: [0, 1, 2].map((i) => ({
        time: new Date(Date.UTC(2026, 9, 19) + i * 5000).toISOString() as ISOTimestamp,
        cpus: [
          { id: "cpu0", usagePerc: 100 },
          { id: "cpu3", usagePerc: 25 + i },
          { id: "cpu6", usagePerc: 0 },
          { id: "cpu7", usagePerc: 0 },
        ],
        memUsed: 16 * 2 ** 30,
        memTotal: 32 * 2 ** 30,
        memPerc: 50,
        netRx: i * 10000,
        netTx: i * 5000,
        diskUsed: 2428 * 2 ** 30,
        diskTotal: 3662 * 2 ** 30,
      })),
      error: "memory unavailable",
    };
    let requests = 0;
    await page.route("**/api/caic/v1/server/system-stats", (route) =>
      route.fulfill({ json: { ...fixture, error: requests++ === 0 ? fixture.error : undefined } }),
    );
    await page.goto("/monitoring?section=system");
    await expect(page.getByRole("heading", { name: "System", exact: true })).toBeVisible();
    await expect(page.getByLabel("CPU utilization over time")).toBeVisible();
    await expect(page.getByLabel("Memory utilization over time")).toBeVisible();
    await expect(page.getByRole("alert")).toContainText("memory unavailable");
    await expect(page.getByTestId("resource-charts")).toContainText("127.0% · 1.27 / 4 CPUs");
    await expect(page.getByTestId("resource-charts")).toContainText("31.8% of capacity");
    await expect(page.getByTestId("resource-charts")).toContainText("2428 GiB / 3662 GiB");
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    if (width < 620) {
      const cpu = await page.getByLabel("CPU utilization over time").boundingBox();
      const memory = await page.getByLabel("Memory utilization over time").boundingBox();
      if (!cpu || !memory) throw new Error("Resource charts are not visible");
      expect(memory.y).toBeGreaterThanOrEqual(cpu.y + cpu.height);
    }

    await page.getByText("Logical CPUs (4)", { exact: true }).click();
    await expect(page.getByRole("meter", { name: "cpu0 utilization", exact: true })).toHaveAttribute("value", "100");
    await expect(page.getByRole("meter", { name: "cpu3 utilization", exact: true })).toHaveAttribute("value", "27");
    const lastCPU = page.getByRole("meter", { name: "cpu7 utilization", exact: true });
    await lastCPU.scrollIntoViewIfNeeded();
    await expect(lastCPU).toBeInViewport();

    const cpuGraphic = await page.getByLabel("CPU utilization over time").elementHandle();
    await expect(page.getByRole("alert")).toHaveCount(0, { timeout: 10000 });
    expect(await cpuGraphic?.evaluate((el) => el.isConnected)).toBe(true);
    expect(requests).toBeGreaterThanOrEqual(2);
    expect(errors).toEqual([]);
  });
}

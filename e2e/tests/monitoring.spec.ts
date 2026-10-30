// Browser coverage for Monitoring navigation, lazy sections, history, and inactive polling cleanup.

import type { Route } from "@playwright/test";
import type { SystemStatsResp } from "../../sdk/caic/ts/v1/types.gen";
import { expect, test } from "../helpers";

for (const width of [390, 1280]) {
  test(`Monitoring sections at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 844 });
    await page.clock.install();
    const errors: string[] = [];
    page.on("pageerror", (e) => errors.push(e.message));
    const requests = { usage: 0, system: 0, metrics: 0 };
    const scripts: string[] = [];
    page.on("request", (request) => {
      const path = new URL(request.url()).pathname;
      if (path.endsWith("/usage/dashboard")) requests.usage++;
      if (path.endsWith("/server/metrics")) requests.metrics++;
      if (path.endsWith(".js")) scripts.push(path);
    });
    const fixture: SystemStatsResp = { samples: [] };
    let pending: Route | undefined;
    await page.route("**/api/caic/v1/server/system-stats", async (route) => {
      requests.system++;
      if (requests.system === 2) pending = route;
      else await route.fulfill({ json: fixture });
    });

    await page.goto("/");
    await page.getByRole("button", { name: "Menu" }).click();
    const menu = page.getByTestId("account-menu");
    await expect(menu.getByRole("menuitem")).toHaveText(["Settings", "Monitoring", "Keyboard shortcuts"]);
    await menu.getByRole("menuitem", { name: "Monitoring" }).click();
    await expect(page).toHaveURL(/\/monitoring$/);
    await expect(page.getByRole("heading", { name: "Usage", exact: true })).toBeVisible();
    await expect.poll(() => requests.usage).toBe(1);
    expect(requests.system).toBe(0);
    expect(requests.metrics).toBe(0);
    expect(scripts.some((path) => /\/(SystemPage|MetricsPage)-/.test(path))).toBe(false);

    const nav = page.getByRole("navigation", { name: "Monitoring sections" });
    await expect(nav.locator('[aria-current="page"]')).toHaveCount(1);
    const usage = nav.getByRole("link", { name: "Usage", exact: true });
    const system = nav.getByRole("link", { name: "System", exact: true });
    const metrics = nav.getByRole("link", { name: "Metrics", exact: true });
    await expect(usage).toHaveAttribute("aria-current", "page");
    const frame = await nav.boundingBox();

    await usage.focus();
    await usage.press("Tab");
    await expect(system).toBeFocused();
    await system.press("Enter");
    await expect(page).toHaveURL(/\?section=system$/);
    await expect(system).toHaveAttribute("aria-current", "page");
    await expect(page.getByRole("heading", { name: "System", exact: true })).toBeVisible();
    await expect.poll(() => requests.system).toBe(1);
    await expect(page.getByRole("heading", { name: "Usage", exact: true })).toHaveCount(0);
    expect(await nav.boundingBox()).toEqual(frame);

    // Hold the next poll across a section switch: its late completion must not
    // schedule another request after System has been disposed.
    await page.clock.runFor(6000);
    await expect.poll(() => pending !== undefined).toBe(true);
    await metrics.click();
    await expect(metrics).toHaveAttribute("aria-current", "page");
    await expect(page.getByRole("heading", { name: "Metrics", exact: true })).toBeVisible();
    await expect.poll(() => requests.metrics).toBe(1);
    await pending?.fulfill({ json: fixture });
    await page.clock.runFor(11000);
    expect(requests.system).toBe(2);
    expect(requests.usage).toBe(1);
    expect(await nav.boundingBox()).toEqual(frame);

    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);

    await page.goBack();
    await expect(system).toHaveAttribute("aria-current", "page");
    await expect.poll(() => requests.system).toBe(3);
    await page.goForward();
    await expect(metrics).toHaveAttribute("aria-current", "page");
    await page.reload();
    await expect(metrics).toHaveAttribute("aria-current", "page");
    await expect(page.getByRole("heading", { name: "Metrics", exact: true })).toBeVisible();
    await page.goto("/monitoring?section=unknown&range=invalid");
    await expect(usage).toHaveAttribute("aria-current", "page");
    await expect(page.getByLabel("Usage date range")).toHaveValue("30");
    expect(errors).toEqual([]);
  });
}

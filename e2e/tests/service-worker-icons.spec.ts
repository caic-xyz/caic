// Browser coverage for compact quota tags, provider logos, and offline logo caching.

import type { UsageResp } from "../../sdk/caic/ts/v1/types.gen";
import { test, expect } from "../helpers";

test("compact Antigravity badges load the logo and keep provider logos available offline", async ({
  page,
  context,
}) => {
  const usage: UsageResp = {
    local: { windows: [] },
    providers: [
      {
        provider: "antigravity",
        label: "Antigravity",
        logoUrl: "/logos/antigravity.svg",
        authKind: "oauth",
        usageUrl: "",
        fetchStatus: "fresh",
        rateLimits: [
          { label: "7d", window: "gemini-weekly", utilization: 0.1 },
          { label: "5h", window: "gemini-5h", utilization: 0.1 },
          { label: "3p-7d", window: "3p-weekly", utilization: 0.1 },
          { label: "3p-5h", window: "3p-5h", utilization: 0.1 },
        ],
      },
    ],
  };
  await page.route("**/api/caic/v1/usage", (route) => route.fulfill({ json: usage }));
  await page.route("**/api/caic/v1/usage/events", (route) =>
    route.fulfill({
      contentType: "text/event-stream",
      body: `data: ${JSON.stringify(usage)}\n\n`,
    }),
  );
  await page.goto("/");
  await page.evaluate(() => navigator.serviceWorker.ready);
  await page.reload();
  await expect.poll(() => page.evaluate(() => navigator.serviceWorker.controller !== null)).toBe(true);

  const badge = page
    .getByTestId("provider-usage")
    .filter({ has: page.getByRole("img", { name: "Antigravity", exact: true }) });
  await expect(badge.getByTestId("usage-badge")).toHaveText(["7d 10%", "5h 10%", "3p-7d 10%", "3p-5h 10%"]);
  await expect(badge.getByRole("img", { name: "Antigravity", exact: true })).toBeVisible();

  const logos = ["/logos/anthropic.svg", "/logos/antigravity.svg"];
  for (const logo of logos) {
    await expect(page.evaluate(async (url) => (await fetch(url)).ok, logo)).resolves.toBe(true);
  }

  await context.setOffline(true);
  for (const logo of logos) {
    await expect(page.evaluate(async (url) => (await fetch(url)).ok, logo)).resolves.toBe(true);
  }
});

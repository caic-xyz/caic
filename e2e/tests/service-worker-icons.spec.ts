// Browser coverage for provider logos surviving a temporary server outage.

import { test, expect } from "../helpers";

test("a controlled page serves a previously loaded provider logo offline", async ({ page, context }) => {
  await page.goto("/");
  await page.evaluate(() => navigator.serviceWorker.ready);
  await page.reload();
  await expect.poll(() => page.evaluate(() => navigator.serviceWorker.controller !== null)).toBe(true);

  const logo = "/logos/anthropic.svg";
  await expect(page.evaluate(async (url) => (await fetch(url)).ok, logo)).resolves.toBe(true);

  await context.setOffline(true);
  await expect(page.evaluate(async (url) => (await fetch(url)).ok, logo)).resolves.toBe(true);
});

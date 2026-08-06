// End-to-end tests for account-menu layout and navigation.

import { expect, test } from "../helpers";

test("account-menu highlights stay within the dropdown", async ({ page }) => {
  await page.goto("/");
  await page.getByRole("button", { name: "Menu" }).click();

  const menu = page.getByTestId("account-menu");
  const settings = menu.getByRole("link", { name: "Settings" });
  await expect(menu).toBeVisible();
  await settings.hover();

  const menuBox = await menu.boundingBox();
  const settingsBox = await settings.boundingBox();
  expect(menuBox).not.toBeNull();
  expect(settingsBox).not.toBeNull();
  if (!menuBox || !settingsBox) throw new Error("Account menu is not visible");

  expect(settingsBox.x).toBeGreaterThanOrEqual(menuBox.x);
  expect(settingsBox.x + settingsBox.width).toBeLessThanOrEqual(menuBox.x + menuBox.width);
});

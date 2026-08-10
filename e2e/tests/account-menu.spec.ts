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

test("top header keeps its controls within a narrow viewport", async ({ page }) => {
  await page.setViewportSize({ width: 320, height: 568 });
  await page.goto("/");

  const header = page.locator("header");
  const controls = [
    page.getByRole("button", { name: "caic" }),
    page.getByTestId("connection-dot"),
    page.getByRole("button", { name: "Menu" }),
  ];
  await Promise.all(controls.map((control) => expect(control).toBeVisible()));

  const headerBox = await header.boundingBox();
  expect(headerBox).not.toBeNull();
  if (!headerBox) throw new Error("Top header is not visible");

  for (const control of controls) {
    const box = await control.boundingBox();
    expect(box).not.toBeNull();
    if (!box) throw new Error("Header control is not visible");
    expect(box.x).toBeGreaterThanOrEqual(headerBox.x);
    expect(box.x + box.width).toBeLessThanOrEqual(headerBox.x + headerBox.width);
  }
});

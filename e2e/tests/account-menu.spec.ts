// End-to-end tests for account-menu layout and navigation.

import { expect, test } from "../helpers";

test("account-menu highlights stay within the dropdown", async ({ page }) => {
  await page.goto("/");
  const trigger = page.getByRole("button", { name: "Menu" });
  await trigger.focus();
  await trigger.press("ArrowDown");

  const menu = page.getByTestId("account-menu");
  const settings = menu.getByRole("menuitem", { name: "Settings" });
  const metrics = menu.getByRole("menuitem", { name: "Metrics" });
  const usage = menu.getByRole("menuitem", { name: "Usage" });
  const shortcuts = menu.getByRole("menuitem", { name: "Keyboard shortcuts" });
  await expect(menu).toBeVisible();
  await expect(settings).toBeFocused();
  await settings.press("ArrowDown");
  await expect(metrics).toBeFocused();
  await metrics.press("ArrowDown");
  await expect(usage).toBeFocused();
  await usage.press("ArrowDown");
  await expect(shortcuts).toBeFocused();
  await shortcuts.press("ArrowUp");
  await expect(usage).toBeFocused();
  await usage.press("ArrowUp");
  await expect(metrics).toBeFocused();
  await metrics.press("ArrowUp");
  await expect(settings).toBeFocused();
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
    page.getByRole("button", { name: "caic", exact: true }),
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

test("quota pills use the space beside the title and connection dot keeps its side", async ({ page }) => {
  for (const width of [320, 390, 600, 1024]) {
    await page.setViewportSize({ width, height: 720 });
    await page.goto("/");

    const title = page.getByRole("button", { name: "caic", exact: true });
    const dot = page.getByTestId("connection-dot");
    const avatar = page.getByRole("button", { name: "Menu" });
    const firstPill = page.getByTestId("provider-usage").first();
    await expect(firstPill).toBeVisible();

    const [titleBox, dotBox, avatarBox, pillBox] = await Promise.all([
      title.boundingBox(),
      dot.boundingBox(),
      avatar.boundingBox(),
      firstPill.boundingBox(),
    ]);
    expect(titleBox && dotBox && avatarBox && pillBox).toBeTruthy();
    if (!titleBox || !dotBox || !avatarBox || !pillBox) throw new Error("Header content is not visible");

    expect(dotBox.x).toBeGreaterThan(titleBox.x + titleBox.width);
    expect(dotBox.x - (titleBox.x + titleBox.width)).toBeLessThanOrEqual(8);
    expect(dotBox.y + dotBox.height).toBeLessThan(titleBox.y + titleBox.height / 2);
    expect(dotBox.x + dotBox.width).toBeLessThan(pillBox.x);
    expect(pillBox.x + pillBox.width).toBeLessThan(avatarBox.x);
    if (width <= 390) {
      expect(pillBox.y).toBeLessThan(titleBox.y + titleBox.height);
    }
    if (width === 1024) {
      const subtitleBox = await page.getByText("Coding Agents in Containers").boundingBox();
      expect(subtitleBox).not.toBeNull();
      if (!subtitleBox) throw new Error("Header subtitle is not visible");
      expect(subtitleBox.y + subtitleBox.height).toBeGreaterThanOrEqual(titleBox.y + titleBox.height - 2);
    }
  }
});

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

test("quota pills use the space beside the connection word", async ({ page }) => {
  for (const width of [320, 390, 600, 1024]) {
    await page.setViewportSize({ width, height: 720 });
    await page.goto("/");

    const title = page.getByRole("button", { name: "caic", exact: true });
    const avatar = page.getByRole("button", { name: "Menu" });
    const firstPill = page.getByTestId("provider-usage").first();
    await expect(firstPill).toBeVisible();
    await expect(title).toHaveAttribute("data-status", "connected");
    await expect(title).toHaveCSS("color", "rgb(0, 0, 0)");
    await title.hover();
    await expect(title).toHaveCSS("text-decoration-line", "none");

    await expect
      .poll(async () => {
        const [titleBox, avatarBox, pillBox] = await Promise.all([
          title.boundingBox(),
          avatar.boundingBox(),
          firstPill.boundingBox(),
        ]);
        if (!titleBox || !avatarBox || !pillBox) return false;
        return (
          pillBox.x > titleBox.x + titleBox.width &&
          pillBox.x + pillBox.width < avatarBox.x &&
          (width > 390 || pillBox.y < titleBox.y + titleBox.height)
        );
      })
      .toBe(true);
    if (width === 1024) {
      const titleBox = await title.boundingBox();
      const subtitleBox = await page.getByText("Coding Agents in Containers").boundingBox();
      expect(titleBox).not.toBeNull();
      expect(subtitleBox).not.toBeNull();
      if (!titleBox || !subtitleBox) throw new Error("Header title or subtitle is not visible");
      expect(subtitleBox.y + subtitleBox.height).toBeGreaterThanOrEqual(titleBox.y + titleBox.height - 2);
    }
  }
});

test("circular connection wave crosses a stationary word and respects reduced motion", async ({ page }) => {
  await page.goto("/");
  const word = page.getByTestId("new-task-button");
  await expect(word).toHaveAttribute("data-status", "connected");

  // The fake backend settles quickly; set only the visual state to inspect its CSS.
  for (const status of ["settled-loading", "settled-error"]) {
    await word.evaluate((el, value) => el.setAttribute("data-status", value), status);
    await expect(word).toHaveCSS("background-image", /radial-gradient/);
    await expect(word).toHaveCSS("background-clip", "text");
    await expect(word).toHaveCSS("animation-name", /connection-wave/);
    await expect(word).toHaveCSS("transform", "none");
  }

  await page.emulateMedia({ reducedMotion: "reduce" });
  await expect(word).toHaveCSS("animation-name", "none");
});

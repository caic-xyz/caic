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

test("desktop quota pills start beside the subtitle while the avatar stays at the edge", async ({ page }) => {
  await page.setViewportSize({ width: 1200, height: 720 });
  await page.goto("/");

  const header = page.locator("header");
  const subtitle = page.getByText("Coding Agents in Containers");
  const firstPill = page.getByTestId("provider-usage").first();
  const avatar = page.getByRole("button", { name: "Menu" });
  await expect(firstPill).toBeVisible();

  await expect
    .poll(async () => {
      const [headerBox, subtitleBox, pillBox, avatarBox] = await Promise.all([
        header.boundingBox(),
        subtitle.boundingBox(),
        firstPill.boundingBox(),
        avatar.boundingBox(),
      ]);
      if (!headerBox || !subtitleBox || !pillBox || !avatarBox) return false;
      return (
        pillBox.x - (subtitleBox.x + subtitleBox.width) <= 20 &&
        Math.abs(headerBox.x + headerBox.width - avatarBox.x - avatarBox.width) <= 2
      );
    })
    .toBe(true);
});

test("a dense desktop quota row keeps its last pill beside the avatar", async ({ page }) => {
  await page.setViewportSize({ width: 1200, height: 720 });
  await page.goto("/");

  const pills = page.getByTestId("provider-usage");
  await expect(pills.first()).toBeVisible();
  // The fake backend exposes only a few providers. Fill the existing row with
  // the widths of a populated account to exercise its wrapping geometry.
  await expect
    .poll(() =>
      page.evaluate(() => {
        const pill = document.querySelector<HTMLElement>('[data-testid="provider-usage"]');
        const row = pill?.parentElement;
        if (!pill || !row) return false;
        const widths = [147, 77, 83, 77, 84, 36, 35, 131, 34, 33, 35, 35];
        row.replaceChildren(
          ...widths.map((width) => {
            const item = document.createElement("span");
            item.className = pill.className;
            item.dataset.testid = "provider-usage";
            item.style.boxSizing = "border-box";
            item.style.width = `${width}px`;
            item.style.height = "24px";
            return item;
          }),
        );
        return true;
      }),
    )
    .toBe(true);

  const [firstBox, lastBox, avatarBox] = await Promise.all([
    pills.first().boundingBox(),
    pills.last().boundingBox(),
    page.getByRole("button", { name: "Menu" }).boundingBox(),
  ]);
  expect(firstBox).not.toBeNull();
  expect(lastBox).not.toBeNull();
  expect(avatarBox).not.toBeNull();
  if (!firstBox || !lastBox || !avatarBox) throw new Error("Header controls are not visible");
  expect(lastBox.y).toBe(firstBox.y);
  expect(lastBox.y).toBeLessThan(avatarBox.y + avatarBox.height);
  expect(lastBox.y + lastBox.height).toBeGreaterThan(avatarBox.y);
  expect(lastBox.x + lastBox.width).toBeLessThan(avatarBox.x);

  await page.setViewportSize({ width: 900, height: 720 });
  const [headerBox, wrappedAvatarBox] = await Promise.all([
    page.locator("header").boundingBox(),
    page.getByRole("button", { name: "Menu" }).boundingBox(),
  ]);
  expect(headerBox).not.toBeNull();
  expect(wrappedAvatarBox).not.toBeNull();
  if (!headerBox || !wrappedAvatarBox) throw new Error("Header or avatar is not visible");
  expect(Math.abs(wrappedAvatarBox.y - headerBox.y)).toBeLessThanOrEqual(1);
});

test("mobile quota lines use the space below the avatar", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 720 });
  await page.goto("/");

  const pills = page.getByTestId("provider-usage");
  await expect(pills.first()).toBeVisible();
  await pills.first().evaluate((pill) => {
    const row = pill.parentElement;
    if (!row) throw new Error("Quota row is missing");
    const widths = [103, 60, 60, 55, 55, 33, 33, 98, 25, 25, 25, 27];
    row.replaceChildren(
      ...widths.map((width) => {
        const item = document.createElement("span");
        item.className = pill.className;
        item.dataset.testid = "provider-usage";
        item.style.boxSizing = "border-box";
        item.style.width = `${width}px`;
        item.style.height = "20px";
        return item;
      }),
    );
  });

  const boxes = await pills.evaluateAll((items) => items.map((item) => item.getBoundingClientRect().toJSON()));
  const avatarBox = await page.getByRole("button", { name: "Menu" }).boundingBox();
  const headerBox = await page.locator("header").boundingBox();
  expect(avatarBox).not.toBeNull();
  expect(headerBox).not.toBeNull();
  if (!avatarBox || !headerBox) throw new Error("Header or avatar is not visible");
  const firstLineY = boxes[0]?.y;
  expect(firstLineY).toBeDefined();
  const firstLine = boxes.filter((box) => box.y === firstLineY);
  const lowerLines = boxes.filter((box) => box.y > (firstLineY ?? 0));
  expect(boxes[3]?.y).toBe(firstLineY);
  expect(firstLine.every((box) => box.x + box.width <= avatarBox.x)).toBe(true);
  expect(avatarBox.x - Math.max(...firstLine.map((box) => box.x + box.width))).toBeGreaterThanOrEqual(4);
  expect(lowerLines.some((box) => box.x + box.width > avatarBox.x)).toBe(true);
  expect(boxes.every((box) => box.x + box.width <= headerBox.x + headerBox.width)).toBe(true);
});

test("mobile quota pills keep their compact height", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 720 });
  await page.goto("/");

  const firstPill = page.getByTestId("provider-usage").first();
  await expect(firstPill).toBeVisible();
  const pillBox = await firstPill.boundingBox();
  const avatarBox = await page.getByRole("button", { name: "Menu" }).boundingBox();
  expect(pillBox).not.toBeNull();
  expect(avatarBox).not.toBeNull();
  if (!pillBox || !avatarBox) throw new Error("Header controls are not visible");
  expect(pillBox.height).toBeLessThanOrEqual(avatarBox.height);
  const heights = await page
    .getByTestId("provider-usage")
    .evaluateAll((items) => items.map((item) => item.getBoundingClientRect().height));
  expect(Math.max(...heights) - Math.min(...heights)).toBeLessThanOrEqual(2);
});

test("mobile wordmark centers with the first quota pill", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 720 });
  await page.goto("/");

  const pill = page.getByTestId("provider-usage").first();
  await expect(pill).toBeVisible();
  const titleBox = await page.getByRole("button", { name: "caic", exact: true }).boundingBox();
  const pillBox = await pill.boundingBox();
  expect(titleBox).not.toBeNull();
  expect(pillBox).not.toBeNull();
  if (!titleBox || !pillBox) throw new Error("Mobile header controls are not visible");
  const titleCenter = titleBox.y + titleBox.height / 2;
  const pillCenter = pillBox.y + pillBox.height / 2;
  expect(Math.abs(titleCenter - pillCenter)).toBeLessThanOrEqual(2.5);
});

test("circular connection wave crosses a stationary word and respects reduced motion", async ({ page }) => {
  await page.goto("/");
  const word = page.getByTestId("new-task-button");
  await expect(word).toHaveAttribute("data-status", "connected");
  await expect(word).toHaveCSS("background-image", /radial-gradient/);
  await expect(word).toHaveCSS("background-image", /rgb\(255, 255, 255\)/);
  await expect(word).toHaveCSS("animation-name", /connection-wave/);
  await expect(word).toHaveCSS("animation-duration", "6s");
  await expect(word).toHaveCSS("animation-delay", "-2s");
  await expect(word).toHaveCSS("animation-iteration-count", "1");
  await expect(word).toHaveCSS("animation-fill-mode", "forwards");

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
  await word.evaluate((el) => el.setAttribute("data-status", "connected"));
  await expect(word).toHaveCSS("animation-name", "none");
});

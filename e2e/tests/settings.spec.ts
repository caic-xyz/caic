// Browser coverage for settings layout, destination ordering, and mapping edits.

import { expect, test } from "../helpers";
import type { ErrorResponse } from "../../sdk/caic/ts/v1/types.gen";

test("server model reload shows progress, success, and recoverable failure", async ({ page, api }, testInfo) => {
  await page.setViewportSize({ width: 1600, height: 1000 });
  await page.goto("/settings?section=server");
  const models = page.getByRole("region", { name: "Reload models", exact: true });
  const codex = models.getByRole("button", { name: "codex", exact: true });
  const pi = models.getByRole("button", { name: "pi", exact: true });
  await expect(codex).toBeVisible();
  await expect(pi).toBeVisible();
  await expect(models.getByRole("button", { name: "antigravity", exact: true })).toBeVisible();
  await expect(models.getByRole("button")).toHaveCount(3);
  await page.screenshot({ path: testInfo.outputPath("server-ready-desktop.png") });
  const gate = Promise.withResolvers<void>();
  const started = Promise.withResolvers<void>();
  await page.route("**/server/harnesses/codex/refresh", async (route) => {
    started.resolve();
    await gate.promise;
    await route.continue();
  });
  try {
    await codex.click();
    await started.promise;
    await expect(codex).toBeDisabled();
    await expect(pi).toBeDisabled();
    await page.screenshot({ path: testInfo.outputPath("server-reloading-desktop.png") });
  } finally {
    gate.resolve();
  }
  const status = models.getByRole("status", { name: "Model reload status" });
  await expect(status).toHaveText("codex models refreshed.");
  await expect(codex).toBeEnabled();
  await expect(pi).toBeEnabled();
  await expect
    .poll(async () => (await api.listHarnesses()).find((h) => h.name === "codex")?.models.map((m) => m.id))
    .toContain("fake-model-fast");
  await page.screenshot({ path: testInfo.outputPath("server-success-desktop.png") });
  const successColor = await status.evaluate((element) => getComputedStyle(element).color);
  const failure = { error: { code: "INTERNAL_ERROR", message: "Model provider unavailable." } } satisfies ErrorResponse;
  await page.route("**/server/harnesses/pi/refresh", (route) => route.fulfill({ status: 503, json: failure }), {
    times: 1,
  });
  await pi.click();
  const error = models.getByRole("alert", { name: "Model reload status" });
  await expect(error).toHaveText(failure.error.message);
  await expect(error.locator("svg[aria-hidden=true]")).toBeVisible();
  expect(await error.evaluate((element) => getComputedStyle(element).color)).not.toBe(successColor);
  await expect(codex).toBeEnabled();
  await expect(pi).toBeEnabled();
  await page.screenshot({ path: testInfo.outputPath("server-failure-desktop.png") });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({ path: testInfo.outputPath("server-failure-mobile.png") });
  await pi.click();
  await expect(status).toHaveText("pi models refreshed.");
  await expect(error).toHaveCount(0);
  await expect(pi).toBeEnabled();
});

test("desktop settings tabs keep a consistent frame", async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 1600, height: 1000 });
  await page.goto("/settings");
  const frames = [];
  for (const section of ["General", "Storage", "Server"]) {
    await page.getByRole("link", { name: section, exact: true }).click();
    await expect(page.getByRole("link", { name: section, exact: true })).toHaveAttribute("aria-current", "page");
    const title = page.getByRole("heading", { name: "Settings", exact: true });
    await title.click();
    await title.scrollIntoViewIfNeeded();
    const heading = await title.boundingBox();
    const navigation = await page.getByRole("navigation", { name: "Settings sections" }).boundingBox();
    const panel = await page.getByRole("region").first().boundingBox();
    if (!heading || !navigation || !panel) throw new Error("Settings frame missing");
    frames.push({ heading, navigation, panel });
    await page.screenshot({ path: testInfo.outputPath(`settings-tab-${section.toLowerCase()}-desktop.png`) });
  }
  for (const frame of frames.slice(1)) {
    expect(frame.heading).toEqual(frames[0].heading);
    expect(frame.navigation).toEqual(frames[0].navigation);
    expect(frame.panel.x).toBe(frames[0].panel.x);
    expect(frame.panel.y).toBe(frames[0].panel.y);
    expect(frame.panel.width).toBe(frames[0].panel.width);
  }
});

test("settings layout on desktop and mobile", async ({ page }) => {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.setViewportSize({ width: 1600, height: 1000 });
  await page.goto("/settings");
  const container = page.getByRole("region", { name: "Container", exact: true });
  await expect(container).toBeVisible();
  await expect(page.getByRole("status", { name: "Settings save status" }).locator("svg")).toHaveCount(0);
  await container.getByRole("textbox", { name: "Docker image" }).focus();
  await container.getByRole("textbox", { name: "Docker image" }).press("Tab");
  await expect(page.getByRole("status", { name: "Settings save status" }).locator("svg")).toHaveCount(0);
  const cores = container.getByRole("spinbutton", { name: "CPU cores" });
  const architecture = container.getByRole("combobox", { name: "CPU architecture" });
  const purgeDelay = page.getByRole("textbox", { name: "Purge delay" });
  for (const [control, maximum] of [
    [cores, 160],
    [architecture, 300],
    [purgeDelay, 160],
  ] as const) {
    const bounds = await control.boundingBox();
    if (!bounds) throw new Error("Settings control missing");
    expect(bounds.width).toBeLessThanOrEqual(maximum);
  }
  await page.screenshot({ path: "test-results/settings-general-desktop.png", fullPage: true });
  for (const width of [320, 390, 768]) {
    await page.setViewportSize({ width, height: 844 });
    for (const [control, maximum] of [
      [cores, 160],
      [architecture, 300],
      [purgeDelay, 160],
    ] as const) {
      const bounds = await control.boundingBox();
      if (!bounds) throw new Error("Settings control missing");
      expect(bounds.x).toBeGreaterThanOrEqual(0);
      expect(bounds.x + bounds.width).toBeLessThanOrEqual(width);
      expect(bounds.width).toBeGreaterThanOrEqual(80);
      expect(bounds.width).toBeLessThanOrEqual(maximum);
    }
    await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    if (width === 390) {
      await page.screenshot({ path: "test-results/settings-general-mobile.png", fullPage: true });
    }
  }
  await page.setViewportSize({ width: 1600, height: 1000 });
  await page.getByRole("link", { name: "Storage", exact: true }).click();
  await expect(page).toHaveURL(/section=storage/);
  await expect(page.getByRole("navigation", { name: "Settings sections" }).locator("[aria-current=page]")).toHaveCount(
    1,
  );
  await expect(page.getByTestId("custom-mount-row")).toHaveCount(3);
  await expect(page.getByTestId("cache-mapping-row")).toHaveCount(3);
  const host = page.getByTestId("custom-mount-row").first().getByRole("textbox", { name: "Host path", exact: true });
  await host.focus();
  await host.press("Tab");
  await expect(page.getByRole("status", { name: "Settings save status" }).locator("svg")).toHaveCount(0);
  await page.screenshot({ path: "test-results/settings-desktop.png", fullPage: true });
  await page
    .getByRole("region", { name: "Custom mounts", exact: true })
    .screenshot({ path: "test-results/settings-mounts-desktop.png" });
  for (const width of [320, 390, 768]) {
    await page.setViewportSize({ width, height: 844 });
    for (const testID of ["cache-mapping-row", "custom-mount-row"]) {
      const rows = page.getByTestId(testID);
      for (const row of await rows.all()) {
        await row.scrollIntoViewIfNeeded();
        const bounds = await row.boundingBox();
        if (!bounds) throw new Error("Mapping row missing");
        for (const control of await row.getByRole("textbox").all()) {
          const box = await control.boundingBox();
          if (!box) throw new Error("Path input missing");
          expect(box.x).toBeGreaterThanOrEqual(bounds.x);
          expect(box.x + box.width).toBeLessThanOrEqual(bounds.x + bounds.width);
          expect(box.width).toBeGreaterThan(200);
        }
      }
    }
    if (width === 390) {
      await page
        .getByRole("region", { name: "Custom mounts", exact: true })
        .screenshot({ path: "test-results/settings-mounts-mobile.png" });
      await page.getByRole("heading", { name: "Settings", exact: true }).scrollIntoViewIfNeeded();
      await page.screenshot({ path: "test-results/settings-mobile.png", fullPage: true });
    }
  }
  await page.reload();
  await expect(page.getByRole("link", { name: "Storage", exact: true })).toHaveAttribute("aria-current", "page");
  await page.getByRole("link", { name: "Server", exact: true }).click();
  await expect(page.getByRole("region", { name: "Version", exact: true })).toBeVisible();
  await page.screenshot({ path: "test-results/settings-server-mobile.png", fullPage: true });
  await page.setViewportSize({ width: 1600, height: 1000 });
  await page.screenshot({ path: "test-results/settings-server-desktop.png", fullPage: true });
  await page.goBack();
  await expect(page.getByRole("link", { name: "Storage", exact: true })).toHaveAttribute("aria-current", "page");
  await page.goto("/settings?section=unknown");
  await expect(page.getByRole("region", { name: "Container", exact: true })).toBeVisible();
  expect(errors).toEqual([]);
});

test("destination sorting preserves mapping identity through edits and removal", async ({ page, api }) => {
  const original = await api.getPreferences();
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  try {
    await page.goto("/settings?section=storage");
    const knownCaches = page.getByRole("region", { name: "Well-known caches", exact: true }).getByRole("checkbox");
    await expect(knownCaches.nth(0)).toHaveAccessibleName("android-keys");
    await expect(knownCaches.nth(2)).toHaveAccessibleName("pip");
    await expect(knownCaches.nth(3)).toHaveAccessibleName("uv");
    await expect(knownCaches.last()).toHaveAccessibleName("go-mod");
    const mounts = page.getByTestId("custom-mount-row");
    await expect(mounts).toHaveCount(3);
    await expect(mounts.nth(0).getByRole("textbox", { name: "Host path", exact: true })).toHaveValue("~/.config/git");
    await expect(mounts.nth(1).getByRole("textbox", { name: "Host path", exact: true })).toHaveValue("~/datasets");
    await expect(mounts.nth(2).getByRole("textbox", { name: "Host path", exact: true })).toHaveValue(
      "~/Documents/reference",
    );
    const caches = page.getByTestId("cache-mapping-row");
    await expect(caches.nth(0).getByRole("textbox", { name: "Host path", exact: true })).toHaveValue(
      "~/.cache/huggingface",
    );
    await expect(caches.nth(1).getByRole("textbox", { name: "Host path", exact: true })).toHaveValue(
      "/srv/caic/cache/uv",
    );
    await expect(caches.nth(2).getByRole("textbox", { name: "Host path", exact: true })).toHaveValue("~/.gradle");

    const destination = mounts.nth(2).getByRole("textbox", { name: "Container path", exact: true });
    await destination.fill("/z/../a-reference");
    await expect(destination).toBeFocused();
    await destination.press("Tab");
    await expect(mounts.nth(2).getByRole("checkbox", { name: "Read only", exact: true })).toBeFocused();
    await page.getByRole("heading", { name: "Custom mounts", exact: true }).click();
    await expect(mounts.nth(0).getByRole("textbox", { name: "Host path", exact: true })).toHaveValue(
      "~/Documents/reference",
    );
    await expect
      .poll(
        async () =>
          (await api.getPreferences()).settings.customMounts?.find((m) => m.hostPath === "~/Documents/reference")
            ?.containerPath,
      )
      .toBe("/z/../a-reference");
    await mounts.nth(1).getByRole("button", { name: "Remove mount", exact: true }).click();
    await expect(mounts).toHaveCount(2);
    await expect
      .poll(async () => (await api.getPreferences()).settings.customMounts?.map((m) => m.hostPath))
      .toEqual(["~/Documents/reference", "~/datasets"]);
    await page.reload();
    await expect(mounts.nth(0).getByRole("textbox", { name: "Container path", exact: true })).toHaveValue(
      "/z/../a-reference",
    );
    await page.getByRole("button", { name: "Add mount", exact: true }).click();
    const added = mounts.nth(2);
    await expect(added.getByRole("textbox", { name: "Host path", exact: true })).toBeFocused();
    await added.getByRole("textbox", { name: "Host path", exact: true }).fill("~/new-data");
    await added.getByRole("textbox", { name: "Host path", exact: true }).press("Tab");
    await expect.poll(async () => (await api.getPreferences()).settings.customMounts?.length).toBe(3);
    await page.getByRole("link", { name: "General", exact: true }).click();
    await page.getByRole("link", { name: "Storage", exact: true }).click();
    await expect(mounts.nth(2).getByRole("textbox", { name: "Container path", exact: true })).toHaveAttribute(
      "placeholder",
      "~/new-data",
    );
    expect(errors).toEqual([]);
  } finally {
    await api.updatePreferences({ settings: original.settings });
  }
});

test("runtime CPU settings survive browser edits and reload", async ({ page, api }) => {
  const config = await api.getConfig();
  const runtimeName = config.runtimes?.[0]?.name;
  if (!runtimeName) throw new Error("No runtime configured");
  const original = await api.getPreferences();
  try {
    const seeded = await api.updatePreferences({
      settings: {
        ...original.settings,
        runtimeSettings: {
          [runtimeName]: { containerPlatform: "linux/amd64", maxCPUs: 4 },
          "unavailable-runtime": { containerPlatform: "linux/arm64", maxCPUs: 2 },
        },
      },
    });
    expect(seeded.settings.runtimeSettings?.[runtimeName]).toEqual({ containerPlatform: "linux/amd64", maxCPUs: 4 });
    await page.setViewportSize({ width: 320, height: 720 });
    await page.goto("/settings");
    const group = page.getByRole("group", { name: runtimeName, exact: true });
    const architecture = group.getByRole("combobox", { name: "CPU architecture" });
    const cores = group.getByRole("spinbutton", { name: "CPU cores" });
    await expect(architecture).toHaveValue("linux/amd64");
    await expect(cores).toHaveValue("4");
    await architecture.selectOption("linux/arm64");
    await cores.fill("6");
    await cores.press("Tab");
    await expect
      .poll(async () => (await api.getPreferences()).settings.runtimeSettings)
      .toEqual({
        [runtimeName]: { containerPlatform: "linux/arm64", maxCPUs: 6 },
        "unavailable-runtime": { containerPlatform: "linux/arm64", maxCPUs: 2 },
      });
    await page.reload();
    await expect(architecture).toHaveValue("linux/arm64");
    await expect(cores).toHaveValue("6");
    const bounds = await group.boundingBox();
    if (!bounds) throw new Error("Runtime settings are not visible");
    for (const control of [architecture, cores]) {
      const box = await control.boundingBox();
      if (!box) throw new Error("CPU control is not visible");
      expect(box.x).toBeGreaterThanOrEqual(bounds.x);
      expect(box.x + box.width).toBeLessThanOrEqual(bounds.x + bounds.width);
    }
    await page.screenshot({ path: "test-results/runtime-settings-mobile.png", fullPage: true });
  } finally {
    await api.updatePreferences({ settings: original.settings });
  }
});

test("server version check button sits in a button row and rechecks on demand", async ({ page }, testInfo) => {
  for (const viewport of [
    { name: "desktop", width: 1600, height: 1000 },
    { name: "mobile", width: 390, height: 800 },
  ]) {
    await page.setViewportSize({ width: viewport.width, height: viewport.height });
    await page.goto("/settings?section=server");
    const version = page.getByRole("region", { name: "Version", exact: true });
    const check = version.getByRole("button", { name: "Check for updates" });
    await expect(check).toBeVisible();
    const versionRequest = page.waitForRequest("**/server/version");
    await check.click();
    await versionRequest;
    await expect(check).toBeEnabled();
    const box = await check.boundingBox();
    expect(box?.width ?? Infinity).toBeLessThan(viewport.width / 2);
    await page.screenshot({ path: testInfo.outputPath(`server-version-${viewport.name}.png`) });
  }
});

test("settings tabs navigate in the same document", async ({ page }) => {
  await page.goto("/settings");
  await page.evaluate(() => {
    (window as unknown as { documentMarker: boolean }).documentMarker = true;
  });
  for (const [section, url] of [
    ["Storage", "/settings?section=storage"],
    ["Server", "/settings?section=server"],
    ["General", "/settings"],
  ]) {
    const tab = page.getByRole("link", { name: section, exact: true });
    await tab.click();
    await expect(tab).toHaveAttribute("aria-current", "page");
    await expect(page).toHaveURL(url);
    await expect(page.getByRole("link", { name: /^(General|Storage|Server)$/ })).toHaveCount(3);
    expect(await page.evaluate(() => (window as unknown as { documentMarker?: boolean }).documentMarker)).toBe(true);
  }
});

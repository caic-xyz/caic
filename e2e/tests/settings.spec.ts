// Browser coverage for native compaction budgets, saved thresholds, settings layout, destination ordering, and mapping edits.

import { ok } from "node:assert/strict";
import { expect, test } from "../helpers";
import type { ErrorResponse, VersionResp } from "../../sdk/caic/ts/v1/types.gen";

// Settings journeys exercise the server's saved preferences through browser reloads.
test.use({ taskPreferences: null });

// Settings exercise version presentation and requests without a live GitHub check.
test.beforeEach(async ({ page }) => {
  await page.route("**/server/version", (route) =>
    route.fulfill({
      json: {
        current: "1.0.0",
        latest: "1.0.0",
        updateAvailable: false,
        autoUpdateEnabled: false,
      } satisfies VersionResp,
    }),
  );
});

test("server model reload shows progress, success, and recoverable failure", async ({ page, api }) => {
  await page.setViewportSize({ width: 1600, height: 1000 });
  await page.goto("/settings?section=server");
  const models = page.getByRole("region", { name: "Reload models", exact: true });
  const codex = models.getByRole("button", { name: "codex", exact: true });
  const pi = models.getByRole("button", { name: "pi", exact: true });
  await expect(codex).toBeVisible();
  await expect(pi).toBeVisible();
  await expect(models.getByRole("button", { name: "opencode", exact: true })).toBeVisible();
  await expect(models.getByRole("button", { name: "antigravity", exact: true })).toBeVisible();
  await expect(models.getByRole("button", { name: "DeepSeek Harness", exact: true })).toBeVisible();
  await expect(models.getByRole("button")).toHaveCount(5);

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
  } finally {
    gate.resolve();
  }
  const status = models.getByRole("status", { name: "Model reload status" });
  await expect(status).toHaveText("codex models refreshed.");
  await expect(codex).toBeEnabled();
  await expect(pi).toBeEnabled();
  await expect
    .poll(async () => ((await api.listHarnesses()) ?? []).find((h) => h.name === "codex")?.models?.map((m) => m.id))
    .toContain("fake-model-fast");

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

  await page.setViewportSize({ width: 390, height: 844 });

  await pi.click();
  await expect(status).toHaveText("pi models refreshed.");
  await expect(error).toHaveCount(0);
  await expect(pi).toBeEnabled();
});

test("desktop settings tabs keep a consistent frame", async ({ page }) => {
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
  }
  const first = frames[0];
  ok(first);
  for (const frame of frames.slice(1)) {
    expect(frame.heading).toEqual(first.heading);
    expect(frame.navigation).toEqual(first.navigation);
    expect(frame.panel.x).toBe(first.panel.x);
    expect(frame.panel.y).toBe(first.panel.y);
    expect(frame.panel.width).toBe(first.panel.width);
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
  }
  await page.reload();
  await expect(page.getByRole("link", { name: "Storage", exact: true })).toHaveAttribute("aria-current", "page");
  await page.getByRole("link", { name: "Server", exact: true }).click();
  await expect(page.getByRole("region", { name: "Version", exact: true })).toBeVisible();

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
  } finally {
    await api.updatePreferences({ settings: original.settings });
  }
});

test("server version check button sits in a button row and rechecks on demand", async ({ page }) => {
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
  ] as const) {
    const tab = page.getByRole("link", { name: section, exact: true });
    await tab.click();
    await expect(tab).toHaveAttribute("aria-current", "page");
    await expect(page).toHaveURL(url);
    await expect(page.getByRole("link", { name: /^(General|Storage|Server)$/ })).toHaveCount(3);
    expect(await page.evaluate(() => (window as unknown as { documentMarker?: boolean }).documentMarker)).toBe(true);
  }
});

test("token thresholds save independently, validate, persist, and reset", async ({ page, api }) => {
  await page.goto("/settings");
  const claude = page.getByRole("spinbutton", { name: "claude threshold (tokens)" });
  const codex = page.getByRole("spinbutton", { name: "codex threshold (tokens)" });
  await expect(claude).toHaveAttribute("placeholder", "Harness default");
  await expect(codex).toHaveAttribute("placeholder", "Harness default");
  await claude.fill("120000");
  await claude.press("Tab");
  await expect
    .poll(async () => (await api.getPreferences()).settings.harnesses?.claude?.autoCompactTokenLimit)
    .toBe(120000);
  await codex.fill("90000");
  await codex.press("Tab");
  await expect
    .poll(async () => (await api.getPreferences()).settings.harnesses?.codex?.autoCompactTokenLimit)
    .toBe(90000);
  await page.reload();
  await expect(claude).toHaveValue("120000");
  await expect(codex).toHaveValue("90000");
  await claude.fill("-1");
  await claude.press("Tab");
  expect(await claude.evaluate((input: HTMLInputElement) => input.validity.valid)).toBe(false);
  expect((await api.getPreferences()).settings.harnesses?.claude?.autoCompactTokenLimit).toBe(120000);
  await claude.fill("");
  await claude.press("Tab");
  await expect
    .poll(async () => (await api.getPreferences()).settings.harnesses)
    .toEqual({
      codex: { autoCompactTokenLimit: 90000 },
    });
  await page.screenshot({ path: test.info().outputPath("compaction-desktop.png") });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.reload();
  await expect(claude).toHaveValue("");
  await expect(codex).toHaveValue("90000");
  const section = page.getByRole("region", { name: "Auto-compaction" });
  await section.scrollIntoViewIfNeeded();
  await page.screenshot({ path: test.info().outputPath("compaction-mobile.png") });
  const bounds = await section.boundingBox();
  expect(bounds).not.toBeNull();
  expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(390);
  await codex.fill("");
  await codex.press("Tab");
  await expect.poll(async () => (await api.getPreferences()).settings.harnesses).toBeUndefined();
});

test("native compaction budgets preserve zero, validate, persist, and reset independently", async ({ page, api }) => {
  const original = await api.getPreferences();
  try {
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    await page.setViewportSize({ width: 1600, height: 1200 });
    await page.goto("/settings");
    const section = page.getByRole("region", { name: "Auto-compaction", exact: true });
    const claude = section.getByRole("spinbutton", { name: "claude threshold (tokens)" });
    const codex = section.getByRole("spinbutton", { name: "codex threshold (tokens)" });
    const pi = section.getByRole("group", { name: "pi", exact: true });
    const opencode = section.getByRole("group", { name: "opencode", exact: true });
    const piReserve = pi.getByRole("spinbutton", { name: "pi compaction headroom (tokens)" });
    const piKeep = pi.getByRole("spinbutton", { name: "pi recent history (tokens)" });
    const openBuffer = opencode.getByRole("spinbutton", { name: "opencode compaction headroom (tokens)" });
    const openKeep = opencode.getByRole("spinbutton", { name: "opencode recent history (tokens)" });
    const fields = [piReserve, piKeep, openBuffer, openKeep];
    await test.step("visible names and shared guidance reach assistive technology", async () => {
      for (const [field, name] of [
        [claude, "claude threshold (tokens)"],
        [codex, "codex threshold (tokens)"],
        [piReserve, "pi compaction headroom (tokens)"],
        [piKeep, "pi recent history (tokens)"],
        [openBuffer, "opencode compaction headroom (tokens)"],
        [openKeep, "opencode recent history (tokens)"],
      ] as const) {
        await expect(field).toHaveAccessibleName(name);
        await expect(field).toHaveAccessibleDescription(
          /Changes apply to new tasks\..*Clear a field to use the harness default\..*do not enable disabled auto-compaction\./,
        );
      }
      for (const field of [claude, codex]) {
        await expect(field).toHaveAccessibleDescription(/Thresholds count used context tokens\./);
      }
      await expect(piReserve).toHaveAccessibleDescription(/Larger values trigger earlier compaction\./);
      await expect(openBuffer).toHaveAccessibleDescription(/native model, input, and output limits\./);
      for (const field of [piKeep, openKeep]) {
        await expect(field).toHaveAccessibleDescription(/Recent conversation kept after the summary/);
      }
    });
    await test.step("edit every native budget and preserve thresholds", async () => {
      for (const field of fields) {
        await expect(field).toHaveValue("");
        await expect(field).toHaveAttribute("placeholder", "Harness default");
      }
      for (const [field, value] of [
        [claude, "120000"],
        [codex, "90000"],
        [piReserve, "8192"],
        [piKeep, "12000"],
        [openBuffer, "16000"],
        [openKeep, "0"],
      ] as const) {
        await field.fill(value);
        await field.press("Tab");
        await expect(page.getByRole("status", { name: "Settings save status" })).toHaveText("Settings saved");
      }
      await expect
        .poll(async () => (await api.getPreferences()).settings.harnesses)
        .toEqual({
          claude: { autoCompactTokenLimit: 120000 },
          codex: { autoCompactTokenLimit: 90000 },
          pi: { reserveTokens: 8192, keepRecentTokens: 12000 },
          opencode: { bufferTokens: 16000, keepRecentTokens: 0 },
        });
      await page.reload();
      for (const [field, value] of [
        [piReserve, "8192"],
        [piKeep, "12000"],
        [openBuffer, "16000"],
        [openKeep, "0"],
      ] as const) {
        await expect(field).toHaveValue(value);
      }
    });
    await test.step("invalid drafts never save", async () => {
      let saves = 0;
      page.on("request", (request) => {
        if (request.method() === "POST" && request.url().endsWith("/preferences")) saves++;
      });
      const saved = (await api.getPreferences()).settings.harnesses;
      for (const field of fields) {
        for (const invalid of ["-1", "1.5", "2147483648"]) {
          await field.fill(invalid);
          await expect(field).toHaveValue(invalid);
          await field.press("Tab");
          expect(await field.evaluate((input: HTMLInputElement) => input.validity.valid)).toBe(false);
          expect((await api.getPreferences()).settings.harnesses).toEqual(saved);
          // Native validation keeps focus on the invalid field. Reload discards
          // that draft before trying another control, without saving a correction.
          await page.reload();
        }
      }
      expect(saves).toBe(0);
      await page.reload();
    });
    await test.step("reset one budget without clearing its neighbors", async () => {
      await piReserve.fill("");
      await piReserve.press("Tab");
      await expect
        .poll(async () => (await api.getPreferences()).settings.harnesses)
        .toEqual({
          claude: { autoCompactTokenLimit: 120000 },
          codex: { autoCompactTokenLimit: 90000 },
          pi: { reserveTokens: null, keepRecentTokens: 12000 },
          opencode: { bufferTokens: 16000, keepRecentTokens: 0 },
        });
      await page.reload();
      await expect(piReserve).toHaveValue("");
      await expect(openKeep).toHaveValue("0");
      await section.screenshot({ path: test.info().outputPath("native-compaction-desktop.png") });
      for (const width of [320, 390, 768]) {
        await page.setViewportSize({ width, height: 844 });
        for (const field of fields) {
          await field.scrollIntoViewIfNeeded();
          const box = await field.boundingBox();
          ok(box);
          expect(box.x).toBeGreaterThanOrEqual(0);
          expect(box.x + box.width).toBeLessThanOrEqual(width);
        }
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
      }
      await page.setViewportSize({ width: 390, height: 844 });
      await section.screenshot({ path: test.info().outputPath("native-compaction-mobile.png") });
      await openKeep.fill("");
      await openKeep.press("Tab");
      await expect
        .poll(async () => (await api.getPreferences()).settings.harnesses?.opencode)
        .toEqual({
          bufferTokens: 16000,
          keepRecentTokens: null,
        });
      await page.reload();
      await expect(openKeep).toHaveValue("");
      await expect(openBuffer).toHaveValue("16000");
      expect(errors).toEqual([]);
    });
  } finally {
    await api.updatePreferences({ settings: original.settings });
  }
});

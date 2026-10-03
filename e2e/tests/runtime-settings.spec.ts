// End-to-end tests for runtime CPU settings persistence and narrow-screen layout.

import { expect, test } from "../helpers";

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

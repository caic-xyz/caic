// Exercises production voice discovery and JS/CSS setup recovery through keyboard retry.

import { expect, test, createTaskAPI, waitForTaskState, type Page } from "../helpers";
import type { Config } from "../../sdk/caic/ts/v1/types.gen";

// Network failures must reach the production loader rather than a worker cache.
test.use({ serviceWorkers: "block" });

async function advertiseVoice(page: Page) {
  await page.route("**/api/caic/v1/server/config", async (route) => {
    const response = await route.fetch();
    const config: Config = await response.json();
    await route.fulfill({ response, json: { ...config, voiceGateway: { mode: "embedded" } } satisfies Config });
  });
  await page.route("**/.well-known/gomode.json", async (route) => {
    const response = await route.fetch();
    const manifest = await response.json();
    manifest.webShell.voiceGateway = { url: "/" };
    await route.fulfill({ response, json: manifest });
  });
  await page.route("**/api/voicegateway/v2/voice/choices", (route) =>
    route.fulfill({
      json: {
        choices: [{ id: "test", label: "Test voice", transports: ["webrtc"] }],
        defaults: [{ transport: "webrtc", id: "test" }],
      },
    }),
  );
}

for (const extension of ["js", "css"]) {
  test(`voice retries repeated production ${extension} failures without reloading`, async ({ page }) => {
    await advertiseVoice(page);
    let requests = 0;
    let failing = true;
    let asset = "";
    await page.route(new RegExp(`/assets/BrowserVoiceRuntime-[^/]+\\.${extension}$`), async (route) => {
      requests++;
      asset = route.request().url();
      if (failing) await route.abort("failed");
      else await route.continue();
    });
    await page.goto("/");
    const retry = page.getByRole("button", { name: "Retry", exact: true });
    await expect(retry).toBeVisible();
    const document = await page.evaluate(() => performance.timeOrigin);
    const initial = requests;
    expect(initial).toBeGreaterThan(0);
    await retry.focus();
    await page.keyboard.press("Enter");
    await expect.poll(() => requests).toBe(initial + 1);
    await expect(retry).toBeEnabled();
    await expect(page.getByTestId("voice-overlay")).toHaveCount(0);
    await retry.focus();
    failing = false;
    await page.keyboard.press("Enter");
    await expect(page.getByTestId("voice-overlay")).toBeVisible();
    expect(requests).toBe(initial + 2);
    expect(await page.evaluate(() => performance.timeOrigin)).toBe(document);
    await expect(page.getByRole("alert").filter({ hasText: "Voice assistant unavailable" })).toHaveCount(0);
    if (extension === "css")
      expect(
        await page
          .locator('link[rel="stylesheet"]')
          .evaluateAll(
            (links, url) =>
              links.some((link) => (link as HTMLLinkElement).href === url && (link as HTMLLinkElement).sheet),
            asset,
          ),
      ).toBe(true);
  });
}

test("voice discovery recovers after repeated setup failure at narrow width", async ({
  page,
  api,
  uniquePrompt,
}, info) => {
  const tasks = await Promise.all(
    Array.from({ length: 8 }, (_, index) =>
      createTaskAPI(api, uniquePrompt(`Review narrow voice setup recovery ${index}`)),
    ),
  );
  await Promise.all(tasks.map((task) => waitForTaskState(api, task, "waiting", 30_000)));
  await page.setViewportSize({ width: 320, height: 640 });
  await advertiseVoice(page);
  let requests = 0;
  const release = Promise.withResolvers<void>();
  await page.route("**/.well-known/gomode.json", async (route) => {
    requests++;
    if (requests === 2) await release.promise;
    if (requests <= 2) await route.fulfill({ status: 503, json: { error: "Voice unavailable" } });
    else await route.fallback();
  });
  try {
    await page.goto("/");
    const retry = page.getByRole("button", { name: "Retry", exact: true });
    await expect(retry).toBeVisible();
    const document = await page.evaluate(() => performance.timeOrigin);
    const notice = page.getByRole("alert").filter({ hasText: "Voice assistant unavailable" });
    const box = (await notice.boundingBox())!;
    expect(box.x).toBeGreaterThanOrEqual(0);
    expect(box.x + box.width).toBeLessThanOrEqual(321);
    await page.screenshot({ path: info.outputPath("setup-failed.png") });
    await retry.click();
    await expect.poll(() => requests).toBe(2);
    await expect(retry).toBeDisabled();
    await expect(retry).toBeFocused();
    await page.keyboard.press("Enter");
    await page.keyboard.press("Space");
    await expect
      .poll(() =>
        retry.evaluate((button) => {
          const box = button.getBoundingClientRect();
          return button.contains(globalThis.document.elementFromPoint(box.x + box.width / 2, box.y + box.height / 2));
        }),
      )
      .toBe(true);
    // Bypass only aria-disabled actionability; still dispatch a real mouse click.
    await retry.click({ force: true });
    expect(requests).toBe(2);
    await expect(retry).toBeFocused();
    await page.screenshot({ path: info.outputPath("setup-disabled.png") });
    release.resolve();
    await expect(retry).toBeEnabled();
    await retry.focus();
    await page.keyboard.press("Enter");
    await expect.poll(() => requests).toBe(3);
    await expect(page.getByTestId("voice-overlay")).toBeVisible();
    expect(await page.evaluate(() => performance.timeOrigin)).toBe(document);
    await expect(notice).toHaveCount(0);
    await page.screenshot({ path: info.outputPath("setup-success.png") });
  } finally {
    release.resolve();
  }
});

// Browser coverage for native page zoom, dense quota groups, task cards, and account-menu access.

import { ok } from "node:assert/strict";
import { createServer } from "node:http";
import { chromium } from "@playwright/test";
import { mkdir, writeFile } from "node:fs/promises";
import path from "node:path";
import type { TaskListEvent, UsageResp } from "../../sdk/caic/ts/v1/types.gen";
import { createTaskAPI, expect, test, waitForTaskState } from "../helpers";

test("page zoom keeps quotas, task cards, and the account menu within the viewport", async ({
  api,
  baseURL,
  uniquePrompt,
}, testInfo) => {
  const prompt = uniquePrompt("Investigate frontend width") + " with a long task description".repeat(20);
  const id = await createTaskAPI(api, prompt);
  const task = await waitForTaskState(api, id, "waiting");

  // tabs.setZoom changes Chromium's native page zoom, including CSS viewport
  // width and media queries. CSS zoom and deviceScaleFactor do not do that.
  const extension = testInfo.outputPath("zoom-extension");
  await mkdir(extension, { recursive: true });
  await writeFile(
    path.join(extension, "manifest.json"),
    JSON.stringify({
      manifest_version: 3,
      name: "Page zoom test",
      version: "1.0",
      background: { service_worker: "background.js" },
    }),
  );
  await writeFile(path.join(extension, "background.js"), "chrome.runtime.onInstalled.addListener(() => {});");

  const context = await chromium.launchPersistentContext(testInfo.outputPath("browser-profile"), {
    channel: "chromium",
    headless: true,
    baseURL,
    viewport: { width: 960, height: 1200 },
    args: [`--disable-extensions-except=${extension}`, `--load-extension=${extension}`],
  });
  // Other workers create and update tasks on the shared backend. Keep this
  // layout fixture fixed while native zoom and scrolling are being asserted.
  const snapshot: TaskListEvent = { kind: "snapshot", snapshot: [task], complete: true };
  const events = createServer((_request, response) => {
    response.writeHead(200, { "Content-Type": "text/event-stream", "Access-Control-Allow-Origin": "*" });
    response.write(`data: ${JSON.stringify(snapshot)}\n\n`);
  });
  try {
    await new Promise<void>((resolve) => events.listen(0, "127.0.0.1", resolve));
    const address = events.address();
    if (!address || typeof address === "string") throw new Error("Task event fixture did not start");
    const worker = context.serviceWorkers()[0] ?? (await context.waitForEvent("serviceworker"));
    const page = context.pages()[0];
    ok(page);
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    await page.route("**/api/caic/v1/tasks/events", (route) =>
      route.fulfill({ status: 307, headers: { location: `http://127.0.0.1:${address.port}/events` } }),
    );
    const usage: UsageResp = {
      local: { windows: [] },
      providers: [
        {
          provider: "antigravity",
          label: "Antigravity",
          logoUrl: "/logos/antigravity.svg",
          authKind: "oauth",
          usageUrl: "",
          fetchStatus: "fresh",
          rateLimits: [
            { label: "5h", window: "five-hour", utilization: 1 },
            { label: "7d", window: "weekly", utilization: 1 },
            { label: "3p-5h", window: "third-party-five-hour", utilization: 1 },
            { label: "3p-7d", window: "third-party-weekly", utilization: 1 },
          ],
        },
        ...(["deepseek", "groq", "openrouter", "xiaomi"] as const).map((provider) => ({
          provider,
          label: provider,
          logoUrl: `/logos/${provider}.svg`,
          authKind: "apikey" as const,
          usageUrl: "",
          fetchStatus: "fresh" as const,
          balance: { currency: "USD", total: 10.95 },
        })),
      ],
    };
    await page.route("**/api/caic/v1/usage", (route) => route.fulfill({ json: usage }));
    await page.route("**/api/caic/v1/usage/events", (route) =>
      route.fulfill({
        contentType: "text/event-stream",
        body: `data: ${JSON.stringify(usage)}\n\n`,
      }),
    );
    await page.goto("/");
    await expect(page.locator(`[data-task-id="${id}"]`).getByText(task.title, { exact: true })).toBeVisible();
    await expect(page.getByTestId("usage-badge")).toHaveCount(8);

    for (const zoom of [1.25, 1.5, 2, 4, 1]) {
      await test.step(`${zoom * 100}% zoom`, async () => {
        // With a single tab, omitting tabId targets the active page.
        await worker.evaluate(`chrome.tabs.setZoom(${zoom})`);
        await expect.poll(() => page.evaluate(() => window.devicePixelRatio)).toBe(zoom);
        await expect.poll(() => page.evaluate(() => window.innerWidth)).toBe(Math.round(960 / zoom));

        const avatar = page.getByRole("button", { name: "Menu" });
        await expect(avatar).toBeInViewport({ ratio: 1 });
        const elements = [
          page.locator("header"),
          page.getByTestId("new-task-form"),
          page.locator(`[data-task-id="${id}"]`),
          ...(await page.getByTestId("provider-usage").all()),
          ...(await page.getByTestId("usage-badge").all()),
        ];
        for (const element of elements) {
          const box = await element.boundingBox();
          if (!box) throw new Error("Zoomed element is not visible");
          expect(box.x).toBeGreaterThanOrEqual(0);
          expect(box.x + box.width).toBeLessThanOrEqual(960 / zoom);
        }

        const card = page.locator(`[data-task-id="${id}"]`);
        await card.scrollIntoViewIfNeeded();
        await expect(card).toBeInViewport();

        await avatar.focus();
        await avatar.press("ArrowDown");
        const settings = page.getByRole("menuitem", { name: "Settings" });
        await expect(settings).toBeFocused();
        await expect(settings).toBeInViewport({ ratio: 1 });

        await settings.press("Escape");
        await expect(avatar).toBeFocused();
      });
    }
    expect(errors).toEqual([]);
  } finally {
    await context.close();
    events.closeAllConnections();
    await new Promise<void>((resolve, reject) => events.close((error) => (error ? reject(error) : resolve())));
  }
});

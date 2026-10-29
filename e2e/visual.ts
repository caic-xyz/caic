// Deterministic browser setup and capture helpers for documentation screenshots.
import { expect, type Page } from "@playwright/test";
import { mkdirSync } from "node:fs";
import path from "path";
import { fileURLToPath } from "url";
import { validateHistoryEvent, validateTask } from "../sdk/caic/ts/v1/validate.gen";
import { createApiClient } from "../sdk/caic/ts/v1/api.gen";
import type { ImageRefreshStatus } from "../sdk/caic/ts/v1/types.gen";
import { resourceHistory, usageHistory } from "./visual-fixtures";
import { historyArray } from "../frontend/tests/history";

const visualTime = "2026-09-02T12:00:00.000Z";

export const screenshotRoot =
  process.env.CAIC_SCREENSHOT_DIR ?? path.join(path.dirname(fileURLToPath(import.meta.url)), "screenshots", "frontend");

export type FrontendScreenshotLayout = "desktop" | "mobile";

export function screenshotDir(layout: FrontendScreenshotLayout): string {
  return path.join(screenshotRoot, layout);
}

export async function prepareVisualPage(page: Page): Promise<void> {
  await page.clock.setFixedTime(visualTime);
  // Freeze demonstration inputs at the API seam. The product keeps its real
  // corners, icons, timing controls, and full native-activity content.
  await page.route("**/api/caic/v1/server/cache-sizes", (route) =>
    route.fulfill({
      json: {
        wellKnown: [
          { name: "go-mod", sizeBytes: 284000000 },
          { name: "npm", sizeBytes: 92000000 },
          { name: "pip", sizeBytes: 46000000 },
        ],
      },
    }),
  );
  await page.route("**/api/caic/v1/server/config", async (route) => {
    const client = createApiClient((url, init) => fetch(new URL(url, route.request().url()), init));
    const config = await client.getConfig();
    config.runtimes = [{ name: "md" }];
    await route.fulfill({ json: config });
  });
  await page.route("**/api/caic/v1/server/runtimes/*/image/refresh", (route) => {
    const status: ImageRefreshStatus = { state: "succeeded" };
    return route.fulfill({ json: status });
  });
  await page.route("**/api/caic/v1/usage/dashboard", (route) => route.fulfill({ json: usageHistory() }));
  await page.route("**/api/caic/v1/tasks/*/history/**/chunks/*/*", async (route) => {
    const response = await route.fetch();
    if (!response.ok()) throw new Error(`Visual history fetch failed: ${response.status()}`);
    const taskURL = route.request().url().split("/history/")[0];
    const taskResponse = await route.fetch({ url: taskURL });
    if (!taskResponse.ok()) throw new Error(`Visual task fetch failed: ${taskResponse.status()}`);
    const task = validateTask(await taskResponse.json());
    const start = Date.parse(task.startedAt ?? "");
    if (!Number.isFinite(start)) throw new Error("Visual task has no start timestamp");
    const values: unknown = await response.json();
    if (!Array.isArray(values)) throw new Error("Visual history must be an array");
    const events = values.map((value: unknown) => {
      const event = validateHistoryEvent(value);
      // Raw record 1 is the log header; the first projected record starts at
      // task creation rather than adding a fictitious header interval.
      event.message.ts = start + (event.recordID - 2) * 150 + event.index;
      return event;
    });
    await route.fulfill({ contentType: "application/json", body: historyArray(events) });
  });
  // Keep the real SSE connection, checkpoints, and failures. Only resource
  // samples are fixed; fulfilling a finite SSE body would manufacture EOF.
  await page.addInitScript((samples) => {
    const NativeEventSource = window.EventSource;
    window.EventSource = class extends NativeEventSource {
      constructor(url: string | URL, options?: EventSourceInit) {
        super(url, options);
        if (!/^\/api\/caic\/v1\/tasks\/[^/]+\/history\/events$/.test(new URL(url, location.href).pathname)) return;
        this.addEventListener("statsSnapshot", (event) => {
          Object.defineProperty(event, "data", { value: JSON.stringify({ samples }) });
        });
        this.addEventListener("stats", (event) => event.stopImmediatePropagation());
      }
    };
  }, resourceHistory());
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
}

export async function waitForVisualReadiness(page: Page): Promise<void> {
  const connection = page.getByTestId("new-task-button");
  if ((await connection.count()) > 0) {
    await expect(connection).toHaveAttribute("data-status", "connected");
  }
  const history = page.getByTestId("task-message-area");
  if ((await history.count()) > 0) {
    await expect(history).toHaveAttribute("aria-busy", "false");
    await expect(page.getByRole("button", { name: "Retry history", exact: true })).toHaveCount(0);
  }
  if ((await page.locator("#caic-visual-test-style").count()) === 0) {
    const style = await page.addStyleTag({
      content: `
        *, *::before, *::after {
          animation: none !important;
          transition: none !important;
        }
      `,
    });
    await style.evaluate((el) => {
      if (!(el instanceof HTMLStyleElement)) {
        throw new Error("Playwright did not create a style element");
      }
      el.id = "caic-visual-test-style";
    });
  }
  await page.evaluate(async () => {
    await document.fonts.ready;
    const deadline = performance.now() + 5000;
    const waitFor = (condition: () => boolean, target: EventTarget, events: string[], description: string) =>
      new Promise<void>((resolve, reject) => {
        if (condition()) {
          resolve();
          return;
        }
        const settle = () => {
          window.clearTimeout(timer);
          for (const event of events) target.removeEventListener(event, settle);
          if (condition()) resolve();
          else reject(new Error(`Timed out waiting for ${description}`));
        };
        const timer = window.setTimeout(settle, Math.max(0, deadline - performance.now()));
        for (const event of events) target.addEventListener(event, settle, { once: true });
      });
    await waitFor(
      () => document.readyState === "complete",
      document,
      ["readystatechange", "load"],
      "document.readyState to complete",
    );
    for (const image of Array.from(document.images)) {
      if (image.complete && image.naturalWidth === 0) {
        throw new Error(`Screenshot image failed to load: ${image.currentSrc || image.src}`);
      }
      await waitFor(
        () => image.complete && image.naturalWidth > 0,
        image,
        ["load", "error"],
        `screenshot image to load: ${image.currentSrc || image.src}`,
      );
    }
    // Repaint the settled page so resized SVGs do not reuse partially
    // invalidated raster caches. Restore the exact style without changing layout.
    const root = document.documentElement;
    const visibility = root.style.getPropertyValue("visibility");
    const priority = root.style.getPropertyPriority("visibility");
    try {
      root.style.setProperty("visibility", "hidden", "important");
      await new Promise<void>((resolve) => {
        requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
      });
    } finally {
      if (visibility) root.style.setProperty("visibility", visibility, priority);
      else root.style.removeProperty("visibility");
    }
    await new Promise<void>((resolve) => {
      requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
    });
  });
}

export async function captureScreenshot(page: Page, layout: FrontendScreenshotLayout, filename: string): Promise<void> {
  // Keep incidental hover controls from depending on the previous click or resize.
  await page.mouse.move(0, 0);
  await waitForVisualReadiness(page);
  const outputDir = screenshotDir(layout);
  mkdirSync(outputDir, { recursive: true });
  await page.screenshot({
    animations: "disabled",
    caret: "hide",
    path: path.join(outputDir, filename),
  });
}

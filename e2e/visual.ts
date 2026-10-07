// Deterministic browser setup and capture helpers for documentation screenshots.
import { expect, type Page } from "@playwright/test";
import { mkdirSync } from "node:fs";
import path from "path";
import { fileURLToPath } from "url";
import { validateEventMessage, validateTask } from "../sdk/caic/ts/v1/validate.gen";
import { createApiClient } from "../sdk/caic/ts/v1/api.gen";
import type { ImageRefreshStatus } from "../sdk/caic/ts/v1/types.gen";
import { resourceHistory, usageHistory } from "./visual-fixtures";

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
  await page.route("**/api/caic/v1/tasks/*/events*", async (route) => {
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 30000);
    try {
      // Replay the complete snapshot, including pending tools after the latest
      // narration. Browser pagination and reconnect cursors can omit that tail.
      const snapshotURL = new URL(route.request().url());
      snapshotURL.searchParams.delete("backward");
      snapshotURL.searchParams.delete("last-event-id");
      const response = await fetch(snapshotURL, { signal: controller.signal });
      if (!response.ok || !response.body) throw new Error(`Visual history fetch failed: ${response.status}`);
      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let body = "";
      try {
        while (!body.includes("event: ready\n")) {
          const chunk = await reader.read();
          if (chunk.done) throw new Error("Visual history ended before ready");
          body += decoder.decode(chunk.value, { stream: true });
          if (body.length > 2 * 1024 * 1024) throw new Error("Visual history exceeds 2 MiB");
        }
      } finally {
        await reader.cancel();
      }
      const taskResponse = await fetch(
        route
          .request()
          .url()
          .replace(/\/events.*$/, ""),
        { signal: controller.signal },
      );
      if (!taskResponse.ok) throw new Error(`Visual task fetch failed: ${taskResponse.status}`);
      const task = validateTask(await taskResponse.json());
      const start = Date.parse(task.startedAt ?? "");
      if (!Number.isFinite(start)) throw new Error("Visual task has no start timestamp");
      let index = 0;
      const frames = body
        .split("\n\n")
        .filter((frame) => frame.split("\n").some((line) => line.startsWith("data: ")) && !/^event:/m.test(frame));
      const events = frames.map((frame) => {
        const data = frame.split("\n").find((line) => line.startsWith("data: "));
        if (!data) throw new Error("Visual event has no data");
        const event = validateEventMessage(JSON.parse(data.slice(6)));
        event.ts = start + index++ * 150;
        return `data: ${JSON.stringify(event)}\n\n`;
      });
      events.push(...resourceHistory().map((event) => `data: ${JSON.stringify(event)}\n\n`));
      await route.fulfill({
        contentType: "text/event-stream",
        body: `retry: 600000\n\n${events.join("")}event: ready\ndata: {}\n\n`,
      });
    } finally {
      clearTimeout(timeout);
      controller.abort();
    }
  });
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
}

export async function waitForVisualReadiness(page: Page): Promise<void> {
  const connection = page.getByTestId("new-task-button");
  if ((await connection.count()) > 0) {
    await expect(connection).toHaveAttribute("data-status", "connected");
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

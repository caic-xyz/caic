// Deterministic browser setup and capture helpers for documentation screenshots.
import { expect, type Page, type Route } from "@playwright/test";
import { mkdirSync } from "node:fs";
import path from "path";
import { fileURLToPath } from "url";
import { validateHistoryEvent, validateHistoryManifest, validateTask } from "../sdk/caic/ts/v1/validate.gen";
import { createApiClient } from "../sdk/caic/ts/v1/api.gen";
import type { HistoryManifest, ImageRefreshStatus, ISOTimestamp } from "../sdk/caic/ts/v1/types.gen";
import {
  resourceHistory,
  serverHealth,
  systemStats,
  usageHistory,
  visualPositionTime,
  visualRecordStrideMs,
} from "./visual-fixtures";
import { historyArray } from "../frontend/tests/history";

const visualTime = "2026-09-02T12:00:00.000Z";

export const screenshotRoot =
  process.env.CAIC_SCREENSHOT_DIR ?? path.join(path.dirname(fileURLToPath(import.meta.url)), "screenshots", "frontend");

export type FrontendScreenshotLayout = "desktop" | "mobile";

export function screenshotDir(layout: FrontendScreenshotLayout): string {
  return path.join(screenshotRoot, layout);
}

/** Scene controls over the deterministic page that prepareVisualPage sets up. */
export interface VisualPage {
  /** Places a task's whole history as if the task started elapsedMs before the
   *  fixed clock, so its running tool calls read live times while completed
   *  durations keep their values. Call before the page first loads the task's
   *  history. */
  startHistoryBefore(id: string, elapsedMs: number): void;
}

export async function prepareVisualPage(page: Page): Promise<VisualPage> {
  await page.clock.setFixedTime(visualTime);
  // Histories are re-timed from the real task start, which is later than the
  // fixed clock: running work reads 0ms unless its scene moves the history.
  const historyStartByTask = new Map<string, number>();
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
  await page.route("**/api/caic/v1/server/system-stats", (route) => route.fulfill({ json: systemStats() }));
  const historyResponse = async (route: Route) => {
    const response = await route.fetch();
    if (!response.ok()) throw new Error(`Visual history fetch failed: ${response.status()}`);
    const taskURL = route.request().url().split("/history/")[0];
    const taskResponse = await route.fetch({ url: taskURL });
    if (!taskResponse.ok()) throw new Error(`Visual task fetch failed: ${taskResponse.status()}`);
    const task = validateTask(await taskResponse.json());
    const taskStart = Date.parse(task.startedAt ?? "");
    if (!Number.isFinite(taskStart)) throw new Error("Visual task has no start timestamp");
    const start = historyStartByTask.get(task.id) ?? taskStart;
    const values: unknown = await response.json();
    if (new URL(route.request().url()).pathname.endsWith("/manifest")) {
      const manifest = validateHistoryManifest(values);
      const events = manifest.bootstrap.through
        ? await route.fetch({
            url: `${new URL(manifest.bootstrap.resourceBase, taskURL)}/slice/1/0/${manifest.bootstrap.through}/end`,
          })
        : null;
      if (events && !events.ok()) throw new Error(`Visual timeline fetch failed: ${events.status()}`);
      const raw: unknown = events ? await events.json() : [];
      if (!Array.isArray(raw)) throw new Error("Visual timeline must be an array");
      const validated = raw.map((value: unknown) => validateHistoryEvent(value));
      const timestamps = validated.map((event): [number, number, number] => [
        event.recordID,
        event.index,
        event.message.ts,
      ]);
      // Background command times have no history position, so retime them by
      // the lifecycle records that reported them.
      const lifecycles = validated.flatMap((event): [number, number, string, string][] => {
        const command = event.message.backgroundCommand;
        return command ? [[event.recordID, event.index, command.id, command.status]] : [];
      });
      const normalized = await page.evaluate(
        ({ id, taskStart, start, timestamps, lifecycles, manifest }) => {
          const event = new CustomEvent("caic-visual-history", {
            detail: { id, taskStart, start, timestamps, lifecycles, manifest },
          });
          window.dispatchEvent(event);
          return event.detail.manifest;
        },
        {
          id: task.id,
          taskStart,
          start,
          timestamps,
          lifecycles,
          manifest,
        },
      );
      await route.fulfill({ json: normalized });
      return;
    }
    if (!Array.isArray(values)) throw new Error("Visual history must be an array");
    const events = values.map((value: unknown) => {
      const event = validateHistoryEvent(value);
      // Raw record 1 is the log header; projected records begin at creation.
      event.message.ts = visualPositionTime(start, event);
      return event;
    });
    await route.fulfill({ contentType: "application/json", body: historyArray(events) });
  };
  await page.route("**/api/caic/v1/tasks/*/history/manifest", historyResponse);
  await page.route("**/api/caic/v1/tasks/*/history/**/slice/*/*/*/*", historyResponse);
  await page.route("**/api/caic/v1/tasks/*/history/**/setup/*", historyResponse);
  await page.route("**/api/caic/v1/tasks/*/history/**/activity/*/*?*", historyResponse);
  // Preserve real SSE ordering and checkpoints while matching finite fixture
  // bodies and payload-free metadata to the same deterministic raw positions.
  // The health events are replaced too: the server samples the real host, so a
  // capture would record whichever CPU level the machine happened to report.
  const fixtures = { samples: resourceHistory(), health: serverHealth(), stride: visualRecordStrideMs };
  await page.addInitScript(({ samples, health, stride }) => {
    type Lifecycle = { recordID: number; index: number; id: string; status: string };
    type History = {
      taskStart: number;
      start: number;
      timestamps: Map<string, number>;
      lifecycles: Lifecycle[];
    };
    const histories = new Map<string, History>();
    function normalize(id: string, manifest: HistoryManifest) {
      const history = histories.get(id);
      if (!history) throw new Error(`Visual task has no timestamp map: ${id}`);
      const positionTime = (position: { recordID: number; index: number }) =>
        history.start + (position.recordID - 2) * stride + position.index;
      for (const section of manifest.sections ?? []) {
        for (const event of [section.boundary, section.endBoundary]) if (event) event.message.ts = positionTime(event);
      }
      const iso = (milliseconds: number) => new Date(milliseconds).toISOString() as ISOTimestamp;
      const metadata = manifest.metadata;
      if (metadata.initialInit) metadata.initialInit.message.ts = positionTime(metadata.initialInit);
      if (metadata.setupEndedAt) {
        const setupEnd = [metadata.initialInit, metadata.initialInput]
          .filter((position) => position !== undefined)
          .map(positionTime);
        if (setupEnd.length === 0) throw new Error("Visual setup has no end position");
        // Setup is timed from the task's API start, which a moved history keeps.
        metadata.setupEndedAt = iso(Math.min(...setupEnd) - history.start + history.taskStart);
      }
      for (const turn of metadata.turns ?? []) {
        turn.at = iso(positionTime(turn));
        if (turn.waitInput) turn.waitMs = positionTime(turn.waitInput) - positionTime(turn);
      }
      for (const activity of metadata.activities ?? []) {
        const original = history.timestamps.get(`${activity.first.recordID}:${activity.first.index}`);
        if (original === undefined) throw new Error("Visual activity has no first position timestamp");
        // Producer milliseconds can repeat. Anchor at the exact first position
        // and preserve the server's elapsed lifecycle times as one offset.
        const offset = positionTime(activity.first) - original;
        activity.anchorAt = iso(Date.parse(activity.anchorAt) + offset);
        if (activity.startedAt) {
          activity.startedAt = iso(Date.parse(activity.startedAt) + offset);
        }
        if (activity.endedAt) activity.endedAt = iso(Date.parse(activity.endedAt) + offset);
      }
      // A command starts at its first running record and ends at its first
      // terminal record, both anchored on the task start like every position.
      for (const command of metadata.backgroundCommands ?? []) {
        const records = history.lifecycles.filter((record) => record.id === command.id);
        const started = records.find((record) => record.status === "running");
        const ended = records.find((record) => record.status !== "running");
        if (command.startedAt) {
          if (!started) throw new Error(`Visual command has no running record: ${command.id}`);
          command.startedAt = iso(positionTime(started));
        }
        if (command.endedAt) {
          if (!ended) throw new Error(`Visual command has no terminal record: ${command.id}`);
          command.endedAt = iso(positionTime(ended));
        }
      }
    }
    window.addEventListener("caic-visual-history", (event) => {
      const { id, taskStart, start, timestamps, lifecycles, manifest } = (
        event as CustomEvent<{
          id: string;
          taskStart: number;
          start: number;
          timestamps: [number, number, number][];
          lifecycles: [number, number, string, string][];
          manifest: HistoryManifest;
        }>
      ).detail;
      const map = new Map<string, number>();
      for (const [recordID, index, original] of timestamps) map.set(`${recordID}:${index}`, original);
      histories.set(id, {
        taskStart,
        start,
        timestamps: map,
        lifecycles: lifecycles.map(([recordID, index, commandID, status]) => ({
          recordID,
          index,
          id: commandID,
          status,
        })),
      });
      normalize(id, manifest);
    });
    const NativeEventSource = window.EventSource;
    window.EventSource = class extends NativeEventSource {
      constructor(url: string | URL, options?: EventSourceInit) {
        super(url, options);
        const pathname = new URL(url, location.href).pathname;
        if (pathname === "/api/caic/v1/tasks/events") {
          this.addEventListener("message", (event) => {
            const value = JSON.parse(event.data) as { kind?: string; health?: unknown };
            if (value.kind !== "health") return;
            value.health = health;
            Object.defineProperty(event, "data", { value: JSON.stringify(value) });
          });
          return;
        }
        const task = /^\/api\/caic\/v1\/tasks\/([^/]+)\/history\/events$/.exec(pathname);
        const id = task?.[1];
        if (!id) return;
        this.addEventListener("message", (event) => {
          const value = JSON.parse(event.data) as {
            recordID: number;
            index: number;
            message: { ts: number; backgroundCommand?: { id: string; status: string } };
          };
          const history = histories.get(id);
          if (!history) throw new Error("Visual live history has no timestamp map");
          const ts = history.start + (value.recordID - 2) * stride + value.index;
          history.timestamps.set(`${value.recordID}:${value.index}`, value.message.ts);
          const command = value.message.backgroundCommand;
          if (command)
            history.lifecycles.push({
              recordID: value.recordID,
              index: value.index,
              id: command.id,
              status: command.status,
            });
          value.message.ts = ts;
          Object.defineProperty(event, "data", { value: JSON.stringify(value) });
        });
        this.addEventListener("manifest", (event) => {
          const value = JSON.parse(event.data) as HistoryManifest;
          normalize(id, value);
          Object.defineProperty(event, "data", { value: JSON.stringify(value) });
        });
        this.addEventListener("statsSnapshot", (event) => {
          Object.defineProperty(event, "data", { value: JSON.stringify({ samples }) });
        });
        this.addEventListener("stats", (event) => event.stopImmediatePropagation());
      }
    };
  }, fixtures);
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  return {
    startHistoryBefore(id, elapsedMs) {
      historyStartByTask.set(id, Date.parse(visualTime) - elapsedMs);
    },
  };
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
    // Observable Plot measures its container while rendering, so a chart caught
    // mid-layout disagrees with the next pass. Wait for its geometry to hold
    // still, and fail instead of capturing a chart mid-layout.
    const chartBoxes = () =>
      Array.from(document.querySelectorAll("svg.data-chart"))
        .map((element) => {
          const box = element.getBoundingClientRect();
          return `${Math.round(box.x)},${Math.round(box.y)},${Math.round(box.width)},${Math.round(box.height)}`;
        })
        .join("|");
    const settledFrames = 2;
    let previous = chartBoxes();
    let stableFrames = 0;
    for (let attempt = 0; attempt < 60 && stableFrames < settledFrames; attempt++) {
      await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
      const current = chartBoxes();
      stableFrames = current === previous ? stableFrames + 1 : 0;
      previous = current;
    }
    if (stableFrames < settledFrames) {
      throw new Error(`Chart geometry did not settle: ${previous}`);
    }
  });
}

export async function captureScreenshot(page: Page, layout: FrontendScreenshotLayout, filename: string): Promise<void> {
  // Keep incidental hover controls and focus rings from depending on the
  // previous click or resize.
  await page.mouse.move(0, 0);
  await page.evaluate(() => {
    if (document.activeElement instanceof HTMLElement) document.activeElement.blur();
  });
  await waitForVisualReadiness(page);
  const outputDir = screenshotDir(layout);
  mkdirSync(outputDir, { recursive: true });
  await page.screenshot({
    animations: "disabled",
    caret: "hide",
    path: path.join(outputDir, filename),
  });
}

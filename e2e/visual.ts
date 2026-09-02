// Deterministic browser setup and capture helpers for documentation screenshots.
import { expect, type Page } from "@playwright/test";
import path from "path";
import { fileURLToPath } from "url";

const visualTime = "2026-09-02T12:00:00.000Z";

export const screenshotDir =
  process.env.CAIC_SCREENSHOT_DIR ??
  path.join(
    path.dirname(fileURLToPath(import.meta.url)),
    "screenshots",
    "frontend",
  );

export async function prepareVisualPage(page: Page): Promise<void> {
  await page.clock.setFixedTime(visualTime);
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
}

export async function waitForVisualReadiness(page: Page): Promise<void> {
  const connection = page.getByTestId("connection-dot");
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
        [data-testid="task-message-area"] {
          border-radius: 0 !important;
        }
        [data-testid="task-detail-form"] {
          border-radius: 0 !important;
        }
        [data-testid="task-detail-form"] button {
          border-radius: 0 !important;
        }
        [data-task-id] {
          border-radius: 0 !important;
        }
        [data-testid="usage-badge"] {
          border-radius: 0 !important;
        }
        [data-testid="provider-usage"] {
          border-color: transparent !important;
          border-radius: 0 !important;
        }
        [data-testid="cache-size"] {
          visibility: hidden !important;
          width: 4rem !important;
        }
        button[title="Back to task"] svg,
        button[title="Collapse sidebar"] svg {
          shape-rendering: crispEdges;
        }
        [data-testid="timing-duration"] > span {
          display: none !important;
        }
        [data-testid="timing-duration"]::after {
          content: "150ms";
          font-size: 0.72rem;
          font-variant-numeric: tabular-nums;
          opacity: 0.72;
          white-space: nowrap;
        }
        [data-testid="task-setup"] [data-testid="timing-duration"]::after {
          content: "42ms";
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
    await new Promise<void>((resolve) => {
      requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
    });
  });
}

export async function captureScreenshot(
  page: Page,
  filename: string,
): Promise<void> {
  await waitForVisualReadiness(page);
  await page.screenshot({
    caret: "hide",
    path: path.join(screenshotDir, filename),
  });
}

// Shared e2e test helpers: typed API client and utilities.
import { test as base, expect, type APIRequestContext } from "@playwright/test";
import { createHash } from "node:crypto";
import { createApiClient, APIError, type FetchFn } from "../sdk/caic/ts/v1/api.gen";
import type { Task } from "../sdk/caic/ts/v1/types.gen";

// ---------------------------------------------------------------------------
// Adapt Playwright's APIRequestContext to the SDK's FetchFn interface.
// ---------------------------------------------------------------------------

function playwrightFetch(request: APIRequestContext): FetchFn {
  return async (url: string, init?: RequestInit): Promise<Response> => {
    const method = init?.method ?? "GET";
    const data = init?.body != null ? JSON.parse(init.body as string) : undefined;
    const pwRes = await request.fetch(url, { method, data });
    const body = await pwRes.body();
    // Copy into a plain Uint8Array: Node's Buffer is not a DOM BodyInit.
    return new Response(new Uint8Array(body), {
      status: pwRes.status(),
      headers: new Headers(pwRes.headers()),
    });
  };
}

// ---------------------------------------------------------------------------
// APIClient: generated SDK client bound to Playwright's request fixture.
// ---------------------------------------------------------------------------

function createClient(request: APIRequestContext) {
  return createApiClient(playwrightFetch(request));
}

export type APIClient = ReturnType<typeof createClient>;

// ---------------------------------------------------------------------------
// Fixtures: extends Playwright's base test with an `api` client.
// ---------------------------------------------------------------------------

type UniquePrompt = (prefix: string) => string;

export const test = base.extend<{ api: APIClient; uniquePrompt: UniquePrompt }>({
  api: async ({ request }, use) => {
    await use(createClient(request));
  },
  // eslint-disable-next-line no-empty-pattern -- this fixture intentionally has no fixture dependencies
  uniquePrompt: async ({}, use, testInfo) => {
    const seed = process.env.CAIC_E2E_SEED;
    if (!seed) throw new Error("CAIC_E2E_SEED is not configured");
    const testTag = createHash("sha256")
      .update(`${seed}\0${testInfo.testId}\0${testInfo.repeatEachIndex}\0${testInfo.retry}`)
      .digest("hex")
      .slice(0, 12);
    let sequence = 0;
    await use((prefix) => `${prefix} ${testTag}-${sequence++}`);
  },
});

export { expect, APIError };
export type { Page } from "@playwright/test";

// ---------------------------------------------------------------------------
// Utility: fill a contenteditable element (Playwright's fill() doesn't
// reliably fire input events on contenteditable divs).
// ---------------------------------------------------------------------------

export async function fillContentEditable(
  locator: import("@playwright/test").Locator,
  text: string,
): Promise<void> {
  await locator.click();
  await locator.evaluate((el) => {
    el.textContent = "";
    el.dispatchEvent(new Event("input", { bubbles: true }));
  });
  await locator.pressSequentially(text, { delay: 0 });
}

// ---------------------------------------------------------------------------
// Utility: create a task via API and return its ID.
// ---------------------------------------------------------------------------

export async function createTaskAPI(
  api: APIClient,
  prompt: string,
): Promise<string> {
  const repos = await api.listRepos();
  expect(repos.length).toBeGreaterThan(0);
  const harnesses = await api.listHarnesses();
  expect(harnesses.length).toBeGreaterThan(0);
  const resp = await api.createTask({
    initialPrompt: { text: prompt },
    repos: [{ name: repos[0].path }],
    harness: harnesses[0].name,
  });
  expect(resp.id).toBeTruthy();
  return resp.id;
}

// ---------------------------------------------------------------------------
// Utility: poll until a task reaches the expected state.
// ---------------------------------------------------------------------------

export async function waitForTaskState(
  api: APIClient,
  taskId: string,
  state: string,
  timeoutMs = 15_000,
): Promise<Task> {
  let task!: Task;
  await expect(async () => {
    task = await api.getTask(taskId);
    expect(task.state).toBe(state);
  }).toPass({ timeout: timeoutMs, intervals: [500] });
  return task;
}

// ---------------------------------------------------------------------------
// Utility: convert all PNGs in a directory to lossless WebP, removing originals.
// ---------------------------------------------------------------------------

export async function convertPngsToWebp(dir: string): Promise<void> {
  const fs = await import("fs");
  const { execFileSync } = await import("child_process");
  const path = await import("path");

  const pngs = fs.readdirSync(dir).filter((f: string) => f.endsWith(".png"));
  for (const png of pngs) {
    const src = path.join(dir, png);
    const dst = path.join(dir, png.replace(/\.png$/, ".webp"));
    execFileSync("ffmpeg", ["-y", "-i", src, "-lossless", "1", dst], {
      stdio: "pipe",
      timeout: 60_000,
    });
    fs.unlinkSync(src);
  }
}

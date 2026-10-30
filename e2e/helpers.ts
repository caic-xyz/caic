// Shared e2e fixtures: isolated browser task choices, typed API client, and utilities.
import { ok } from "node:assert/strict";
import { test as base, expect, type APIRequestContext, type Page } from "@playwright/test";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { createHash } from "node:crypto";
import { readdirSync, unlinkSync } from "node:fs";
import path from "node:path";
import { createApiClient, type FetchFn } from "../sdk/caic/ts/v1/api.gen";
import type { StepLoadTimingReport } from "../frontend/src/stepTimings";
import type { HistoryBootstrap, PreferencesResp, Task } from "../sdk/caic/ts/v1/types.gen";

// Observe diagnostic entries without exposing a test-only hook in the viewer.
export async function collectStepTimings(page: Page): Promise<StepLoadTimingReport[]> {
  const reports: StepLoadTimingReport[] = [];
  page.on("console", async (message) => {
    if (message.type() === "debug" && message.text().startsWith("Observed STEP timing")) {
      const detail = message.args()[1];
      ok(detail);
      reports.push((await detail.jsonValue()) as StepLoadTimingReport);
    }
  });
  await page.addInitScript(() => {
    new PerformanceObserver((list) => {
      for (const entry of list.getEntries())
        if (entry.name === "caic.step.load")
          console.debug("Observed STEP timing", (entry as PerformanceMeasure).detail);
    }).observe({ type: "measure" });
  });
  return reports;
}

// Exercise the public asynchronous native bridge, including its page/identity/scope binding.
export async function installNativeVoiceBridge(page: Page, connected: boolean) {
  await page.addInitScript((connected) => {
    type Binding = { document: string; token: string; scope: string; ready: boolean; tools: Array<{ name: string }> };
    const messages: Binding[] = [];
    const pending = new Map<string, (result: Record<string, unknown>) => void>();
    let invocation = 0;
    const bridge: { onmessage?: ((event: { data: string }) => void) | null; postMessage(message: string): void } = {
      postMessage(message) {
        const auth = JSON.parse(message) as Record<string, unknown>;
        if (auth.authChanged === true && auth.nativeAccess === true)
          bridge.onmessage?.({ data: JSON.stringify({ ...auth, frontendToken: "native-identity" }) });
      },
    };
    Object.assign(window, {
      goModeHost: { isVoiceConnected: () => connected },
      goModeFrontendDocument: Promise.resolve("native-document"),
      gomodeAuth: bridge,
      gomodeFrontendReadiness: { postMessage: (message: string) => messages.push(JSON.parse(message)) },
      preparationMessages: messages,
      gomodeFrontendToolResult: {
        postMessage(message: string) {
          const reply = JSON.parse(message) as { invocation: string; result: Record<string, unknown> };
          pending.get(reply.invocation)?.(reply.result);
          pending.delete(reply.invocation);
        },
      },
      invokeFrontendTool(name: string, args: Record<string, unknown>) {
        const binding = messages.at(-1);
        const dispatch = (window as unknown as { dispatchGoModeVoiceTool?: (request: unknown) => void })
          .dispatchGoModeVoiceTool;
        if (!binding?.ready || !dispatch) throw new Error("Frontend is not prepared");
        return new Promise<Record<string, unknown>>((resolve) => {
          const id = String(++invocation);
          pending.set(id, resolve);
          dispatch({
            document: binding.document,
            token: binding.token,
            scope: binding.scope,
            invocation: id,
            name,
            args,
          });
        });
      },
    });
  }, connected);
}

export async function invokeFrontendTool(page: Page, name: string, args: Record<string, unknown>) {
  return page.evaluate(
    ({ name, args }) =>
      (
        window as unknown as {
          invokeFrontendTool(name: string, args: Record<string, unknown>): Promise<Record<string, unknown>>;
        }
      ).invokeFrontendTool(name, args),
    { name, args },
  );
}

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
// Task creation saves shared server preferences. Each browser starts with explicit
// task choices, while real settings and all writes still reach the server. Tests
// of saved task choices opt out with taskPreferences: null; API-only tests keep
// using the real preferences endpoint without constructing a browser page.
// ---------------------------------------------------------------------------

type UniquePrompt = (prefix: string) => string;
type TaskPreferences = Pick<PreferencesResp, "repositories" | "harness" | "models" | "efforts">;

export const test = base.extend<{
  api: APIClient;
  uniquePrompt: UniquePrompt;
  taskPreferences: TaskPreferences | null;
}>({
  taskPreferences: [{ repositories: [], harness: "claude", models: {}, efforts: {} }, { option: true }],
  page: async ({ page, taskPreferences }, use) => {
    if (taskPreferences) {
      await page.route("**/api/caic/v1/server/preferences", async (route) => {
        if (route.request().method() !== "GET") {
          await route.continue();
          return;
        }
        const response = await route.fetch();
        if (!response.ok()) {
          await route.fulfill({ response });
          return;
        }
        const preferences: PreferencesResp = await response.json();
        await route.fulfill({ response, json: { ...preferences, ...taskPreferences } });
      });
    }
    await use(page);
    // A task-stream open can start a config refresh after the last assertion.
    // Drain route handlers before context teardown disposes route.fetch bodies.
    await page.unrouteAll({ behavior: "wait" });
  },
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

export { expect };
export type { Page } from "@playwright/test";

// ---------------------------------------------------------------------------
// Utility: create a task via API and return its ID.
// ---------------------------------------------------------------------------

export async function createTaskAPI(api: APIClient, prompt: string): Promise<string> {
  const repos = await api.listRepos();
  const firstRepo = repos[0];
  ok(firstRepo);
  const resp = await api.createTask({
    initialPrompt: { text: prompt },
    repos: [{ name: firstRepo.path }],
    harness: "claude",
  });
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
  }).toPass({ timeout: timeoutMs, intervals: [100, 250, 500] });
  return task;
}

// ---------------------------------------------------------------------------
// Utility: poll until the fake CI finishes walking a task's checks. The fake
// transitions one check per second after the task settles, so captures and
// assertions that run during the walk are nondeterministic.
// ---------------------------------------------------------------------------

export async function waitForCISettle(api: APIClient, taskIds: string[], timeoutMs = 15_000): Promise<void> {
  await expect(async () => {
    const tasks = await Promise.all(taskIds.map((id) => api.getTask(id)));
    for (const task of tasks) {
      expect(task.ciStatus).toBe("success");
    }
  }).toPass({ timeout: timeoutMs, intervals: [500] });
}

// ---------------------------------------------------------------------------
// Utility: convert all PNGs in a directory to lossless WebP, removing originals.
// ---------------------------------------------------------------------------

export async function convertPngsToWebp(dir: string): Promise<void> {
  const pngs: string[] = [];
  for (const entry of readdirSync(dir, { recursive: true, withFileTypes: true })) {
    if (entry.isFile() && entry.name.endsWith(".png")) {
      pngs.push(path.join(entry.parentPath, entry.name));
    }
  }
  pngs.sort();
  const encode = promisify(execFile);
  const queue = [...pngs];
  await Promise.all(
    Array.from({ length: Math.min(2, queue.length) }, async () => {
      for (let src = queue.shift(); src !== undefined; src = queue.shift()) {
        const dst = src.replace(/\.png$/, ".webp");
        await encode(
          "ffmpeg",
          ["-v", "error", "-y", "-threads", "1", "-i", src, "-threads", "1", "-lossless", "1", dst],
          {
            timeout: 60_000,
          },
        );
        unlinkSync(src);
      }
    }),
  );
}

export function recentHistoryPage(bootstrap: HistoryBootstrap) {
  const first = Math.floor((bootstrap.through - 1) / bootstrap.recordsPerChunk) * bootstrap.recordsPerChunk + 1;
  return { first, last: bootstrap.through, url: `${bootstrap.resourceBase}/slice/${first}/0/${bootstrap.through}/end` };
}

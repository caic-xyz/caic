// Shared e2e fixtures: browser diagnostics, isolated task choices and cleanup, typed API client, and utilities.
import { ok } from "node:assert/strict";
import { test as base, expect, type APIRequestContext, type BrowserContext, type Page } from "@playwright/test";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { createHash } from "node:crypto";
import { readdirSync, unlinkSync } from "node:fs";
import path from "node:path";
import { type FetchFn } from "../sdk/caic/ts/v1/api.gen";
import { createPromptClient } from "../frontend/src/api";
import type { HistoryBootstrap, PreferencesResp, Task } from "../sdk/caic/ts/v1/types.gen";

// Record the host auth bridge used by device-login journeys and acknowledge native access.
export async function installDeviceLoginAuthBridge(context: BrowserContext, eventKey: string) {
  await context.addInitScript((storageKey) => {
    Object.defineProperty(window, "goModeFrontendDocument", { value: Promise.resolve("device-document") });
    const bridge: {
      onmessage?: ((event: { data: string }) => void) | null;
      postMessage(message: string): void;
    } = {
      postMessage(message: string) {
        const events = JSON.parse(sessionStorage.getItem(storageKey) ?? "[]") as Record<string, unknown>[];
        const { frontendDocument, frontendRequest, ...auth } = JSON.parse(message) as Record<string, unknown>;
        if (frontendDocument !== "device-document" || typeof frontendRequest !== "string")
          throw new Error("Invalid bridge owner");
        events.push(auth);
        if (auth.authChanged === true && auth.nativeAccess === true) {
          bridge.onmessage?.({
            data: JSON.stringify({ frontendDocument, frontendRequest, frontendToken: "device-token" }),
          });
        }
        sessionStorage.setItem(storageKey, JSON.stringify(events));
      },
    };
    Object.defineProperty(window, "gomodeAuth", { value: bridge });
  }, eventKey);
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
    const encoded =
      init?.body instanceof FormData ? new Request("https://caic.test", { method, body: init.body }) : null;
    const data = encoded
      ? Buffer.from(await encoded.arrayBuffer())
      : init?.body != null
        ? JSON.parse(init.body as string)
        : undefined;
    const headers = encoded ? { "Content-Type": encoded.headers.get("Content-Type")! } : undefined;
    const pwRes = await request.fetch(url, { method, data, headers });
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
  return createPromptClient(playwrightFetch(request));
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
  browser: async ({ browser }, use) => {
    process.stdout.write(`E2E browser version: ${browser.version()}\n`);
    await use(browser);
  },
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
  uniquePrompt: async ({ api }, use, testInfo) => {
    const seed = process.env.CAIC_E2E_SEED;
    if (!seed) throw new Error("CAIC_E2E_SEED is not configured");
    const testTag = createHash("sha256")
      .update(`${seed}\0${testInfo.testId}\0${testInfo.repeatEachIndex}\0${testInfo.retry}`)
      .digest("hex")
      .slice(0, 12);
    let sequence = 0;
    try {
      await use((prefix) => `${prefix} ${testTag}-${sequence++}`);
    } finally {
      // The fake backend is shared across workers. Retire only this test's
      // tasks so later pages and trace snapshots do not accumulate every
      // earlier conversation's active sidebar card and resource updates.
      const tasks = ((await api.listTasks()) ?? []).filter((task) => task.initialPrompt.includes(testTag));
      await Promise.all(
        tasks.map(async (task) => {
          // Purge rejects transient startup and sync states; let them settle.
          // Failed and purged tasks need no further cleanup.
          await expect
            .poll(
              async () => {
                task = await api.getTask(task.id);
                return ["branching", "pending", "provisioning", "pulling", "pushing", "starting"].includes(task.state);
              },
              { timeout: 30_000 },
            )
            .toBe(false);
          if (task.state === "failed" || task.state === "purged") return;
          if (task.state !== "purging") await api.purgeTask(task.id);
          await waitForTaskState(api, task.id, "purged");
        }),
      );
    }
  },
});

export { expect };
export type { Page } from "@playwright/test";

// ---------------------------------------------------------------------------
// Every viewer open imports a STEP file in a fresh worker that instantiates the
// 7.6 MB OCCT WebAssembly module. A shared 4-worker CI runner spends seconds on
// that: a recorded import pulled the worker script in 393 ms and the compressed
// wasm in 747 ms over loopback, then spent about 1.5 s compiling and parsing a
// 12-triangle cube and finished just past Playwright's five-second default.
// Budget cold STEP imports well above it.
// ---------------------------------------------------------------------------

export const STEP_IMPORT_TIMEOUT = 30_000;

// ---------------------------------------------------------------------------
// Utility: create a task via API and return its ID.
// ---------------------------------------------------------------------------

export async function createTaskAPI(api: APIClient, prompt: string): Promise<string> {
  const repos = (await api.listRepos()) ?? [];
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

// Shared e2e test helpers: typed API client and utilities.
import { test as base, expect, type APIRequestContext } from "@playwright/test";
import { createApiClient, APIError, type FetchFn } from "../sdk/ts/v1/api.gen";
import type { Task } from "../sdk/ts/v1/types.gen";

// ---------------------------------------------------------------------------
// Adapt Playwright's APIRequestContext to the SDK's FetchFn interface.
// ---------------------------------------------------------------------------

function playwrightFetch(request: APIRequestContext): FetchFn {
  return async (url: string, init?: RequestInit): Promise<Response> => {
    const method = init?.method ?? "GET";
    const data = init?.body != null ? JSON.parse(init.body as string) : undefined;
    const pwRes = await request.fetch(url, { method, data });
    const body = await pwRes.body();
    return new Response(body, {
      status: pwRes.status(),
      headers: new Headers(pwRes.headers()),
    });
  };
}

// ---------------------------------------------------------------------------
// APIClient: SDK client extended with a getTask convenience method.
// ---------------------------------------------------------------------------

function createClient(request: APIRequestContext) {
  const sdk = createApiClient(playwrightFetch(request));
  return {
    ...sdk,
    getTask: async (id: string): Promise<Task | undefined> => {
      const tasks = await sdk.listTasks();
      return tasks.find((t) => t.id === id);
    },
  };
}

export type APIClient = ReturnType<typeof createClient>;

// ---------------------------------------------------------------------------
// Fixtures: extends Playwright's base test with an `api` client.
// ---------------------------------------------------------------------------

export const test = base.extend<{ api: APIClient }>({
  api: async ({ request }, use) => {
    await use(createClient(request));
  },
});

export { expect, APIError };

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
  let task: Task | undefined;
  await expect(async () => {
    task = await api.getTask(taskId);
    expect(task).toBeTruthy();
    expect(task!.state).toBe(state);
  }).toPass({ timeout: timeoutMs, intervals: [500] });
  return task!;
}

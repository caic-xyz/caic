// Error handling and edge case tests.
import { test, expect, createTaskAPI, waitForTaskState, APIError } from "../helpers";
import type { Harness } from "../../sdk/caic/ts/v1/types.gen";

test("POST /api/caic/v1/tasks with missing prompt returns 400", async ({ api }) => {
  const err = await api
    .createTask({ harness: "claude" } as unknown as Parameters<typeof api.createTask>[0])
    .catch((e: unknown) => e);
  expect(err).toBeInstanceOf(APIError);
  expect((err as APIError).status).toBe(400);
  expect((err as APIError).code).toBeTruthy();
});

test("POST /api/caic/v1/tasks with unknown repo returns 400", async ({ api }) => {
  const err = await api
    .createTask({ initialPrompt: { text: "hello" }, repos: [{ name: "nonexistent" }], harness: "claude" })
    .catch((e: unknown) => e);
  expect(err).toBeInstanceOf(APIError);
  expect((err as APIError).status).toBe(400);
});

test("POST /api/caic/v1/tasks with unknown harness returns 400", async ({ api }) => {
  const err = await api
    // Deliberately outside the Harness enum: the server must reject it.
    .createTask({ initialPrompt: { text: "hello" }, harness: "does-not-exist" as Harness })
    .catch((e: unknown) => e);
  expect(err).toBeInstanceOf(APIError);
  expect((err as APIError).status).toBe(400);
});

test("purge nonexistent task returns 404", async ({ api }) => {
  const err = await api.purgeTask("nonexistent-id").catch((e: unknown) => e);
  expect(err).toBeInstanceOf(APIError);
  expect((err as APIError).status).toBe(404);
});

test("send input to nonexistent task returns 404", async ({ api }) => {
  const err = await api
    .sendInput("nonexistent-id", { prompt: { text: "hello" } })
    .catch((e: unknown) => e);
  expect(err).toBeInstanceOf(APIError);
  expect((err as APIError).status).toBe(404);
});

test("navigating to a nonexistent task redirects home", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first()).toBeVisible();

  // The detail route resolves the task as a REST resource; a 404 is
  // authoritative and sends us home (no dependence on the list snapshot).
  await page.goto("/task/@nonexistent-id+bogus");
  await expect(page).toHaveURL(/\/$/, { timeout: 10_000 });
  await expect(page.getByTestId("prompt-input")).toBeVisible();
});

test("network failure shows reconnect banner", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first()).toBeVisible();

  // Intercept all API requests to simulate network failure. This must close
  // the existing SSE connection too, so we abort any in-flight requests and
  // close the page's EventSource by navigating through a network-blocked state.
  await page.route("**/api/**", (route) => route.abort("failed"));

  // Force the existing SSE connection to break by evaluating a close on it,
  // then reload the page so it tries to reconnect through the blocked route.
  await page.reload();

  // The connection dot should turn red (dotDisconnected class).
  const dot = page.getByTestId("connection-dot");
  await expect(dot).toHaveClass(/dotDisconnected/, { timeout: 15_000 });

  // Restore network and reload to verify recovery.
  await page.unrouteAll({ behavior: "ignoreErrors" });
  await page.reload();
  await expect(dot).toHaveClass(/dotConnected/, { timeout: 15_000 });
});

test("creating a task with special characters in prompt", async ({ api }) => {
  const prompt = '<script>alert("xss")</script> & "quotes" & émojis 🎉';
  const id = await createTaskAPI(api, prompt);
  await waitForTaskState(api, id, "waiting");

  const task = await api.getTask(id);
  expect(task).toBeTruthy();
  expect(task!.initialPrompt).toBe(prompt);
});

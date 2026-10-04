// Error handling and edge case tests.
import { test, expect, createTaskAPI, waitForTaskState, APIError } from "../helpers";
import type { Harness, UserResp, Warning } from "../../sdk/caic/ts/v1/types.gen";
import { validateTaskListEvent } from "../../sdk/caic/ts/v1/validate.gen";

test("runtime restoration warning shows the exact count and can be dismissed", async ({ page }) => {
  const warning: Warning = {
    id: "restore-outage-1",
    category: "runtime_restore_failed",
    message: "2 tasks could not be restored.",
    details: [],
  };
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.route("**/auth/me", (route) =>
    route.fulfill({
      json: { id: "restore-test", provider: "github", username: "restore-test" } satisfies UserResp,
    }),
  );
  await page.route("**/api/caic/v1/tasks/events", (route) =>
    route.fulfill({
      contentType: "text/event-stream",
      body: `data: ${JSON.stringify({ kind: "status", status: { loading: false, error: "" } })}\n\ndata: ${JSON.stringify({ kind: "snapshot", snapshot: [] })}\n\ndata: ${JSON.stringify({ kind: "warning", warning })}\n\n`,
    }),
  );
  await page.goto("/");
  await expect(page.getByText(warning.message)).toBeVisible();
  await page.getByRole("button", { name: "Dismiss warning" }).click();
  await expect(page.getByText(warning.message)).toHaveCount(0);
  expect(errors).toEqual([]);
});

test("POST /api/caic/v1/tasks with missing prompt returns 400", async ({ api }) => {
  const err = await api
    .createTask({ harness: "claude" } as unknown as Parameters<typeof api.createTask>[0])
    .catch((e: unknown) => e);
  expect(err).toBeInstanceOf(APIError);
  expect((err as APIError).status).toBe(400);
  expect((err as APIError).code).toBeTruthy();
});

test("categorized CI alerts stay quiet on replay and expose details on mobile", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  const warning: Warning = {
    id: "outage-1",
    category: "ci_poll_failed",
    message: "Échec de la récupération CI.",
    details: [
      { repo: "repos/a", error: "rate limit exceeded" },
      { repo: "repos/b", error: "connection refused" },
    ],
  };
  let requests = 0;
  let episode = warning;
  // Keep the session probe valid so finite SSE fixtures exercise reconnects.
  await page.route("**/auth/me", (route) =>
    route.fulfill({
      json: { id: "warning-test", provider: "github", username: "warning-test" } satisfies UserResp,
    }),
  );
  await page.route("**/api/caic/v1/tasks/events", async (route) => {
    requests++;
    const event = validateTaskListEvent({ kind: "warning", warning: episode });
    await route.fulfill({
      contentType: "text/event-stream",
      body: `data: ${JSON.stringify(event)}\n\n`,
    });
  });
  await page.goto("/");
  await expect(page.getByRole("button", { name: "Dismiss warning" })).toHaveCount(1);
  await expect(page.getByText(warning.message)).toBeVisible();
  await page.getByText("Details", { exact: true }).click();
  await expect(page.getByText("repos/a", { exact: true })).toBeVisible();
  await expect(page.getByText("connection refused", { exact: false })).toBeVisible();
  const bounds = await page.getByText(warning.message).boundingBox();
  if (!bounds) throw new Error("Warning message is not rendered");
  expect(bounds.x).toBeGreaterThanOrEqual(0);
  expect(bounds.x + bounds.width).toBeLessThanOrEqual(390);
  await page.screenshot({ path: test.info().outputPath("ci-warning-mobile.png") });
  await page.getByRole("button", { name: "Dismiss warning" }).click();
  const priorRequests = requests;
  await expect.poll(() => requests).toBeGreaterThan(priorRequests);
  await expect(page.getByRole("button", { name: "Dismiss warning" })).toHaveCount(0);
  episode = { ...warning, id: "outage-2" };
  await expect(page.getByRole("button", { name: "Dismiss warning" })).toHaveCount(1);
  expect(errors).toEqual([]);
});

test("POST /api/caic/v1/tasks with unknown repo returns 400", async ({ api }) => {
  const err = await api
    .createTask({
      initialPrompt: { text: "hello" },
      repos: [{ name: "nonexistent" }],
      harness: "claude",
    })
    .catch((e: unknown) => e);
  expect(err).toBeInstanceOf(APIError);
  expect((err as APIError).status).toBe(400);
  expect((err as APIError).code).toBe("UNKNOWN_REPOSITORY");
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
  const err = await api.sendInput("nonexistent-id", { prompt: { text: "hello" } }).catch((e: unknown) => e);
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

  // The caic wordmark should turn red when disconnected.
  const word = page.getByTestId("new-task-button");
  await expect(word).toHaveAttribute("data-status", "disconnected", { timeout: 15_000 });
  await expect(word).toHaveCSS("color", "rgb(220, 53, 69)");
  await expect(word).toHaveCSS("animation-name", /connection-wave/);
  await expect(word).toHaveCSS("transform", "none");
  await page.emulateMedia({ reducedMotion: "reduce" });
  await expect(word).toHaveCSS("animation-name", "none");

  // Restore network and reload to verify recovery.
  await page.unrouteAll({ behavior: "ignoreErrors" });
  await page.reload();
  await expect(word).toHaveAttribute("data-status", "connected", { timeout: 15_000 });
  await expect(word).toHaveCSS("color", "rgb(0, 0, 0)");
});

test("creating a task with special characters in prompt", async ({ api }) => {
  const prompt = '<script>alert("xss")</script> & "quotes" & émojis 🎉';
  const id = await createTaskAPI(api, prompt);
  await waitForTaskState(api, id, "waiting");

  const task = await api.getTask(id);
  expect(task).toBeTruthy();
  expect(task!.initialPrompt).toBe(prompt);
});

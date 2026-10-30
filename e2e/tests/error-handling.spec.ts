// Browser coverage for mobile warning details, missing-task navigation, and connection recovery.
import { test, expect } from "../helpers";
import type { UserResp, Warning } from "../../sdk/caic/ts/v1/types.gen";
import { validateTaskListEvent } from "../../sdk/caic/ts/v1/validate.gen";

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
      json: {
        id: "warning-test",
        sessionID: "fixture-session",
        provider: "github",
        username: "warning-test",
      } satisfies UserResp,
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

  await page.getByRole("button", { name: "Dismiss warning" }).click();
  const priorRequests = requests;
  await expect.poll(() => requests).toBeGreaterThan(priorRequests);
  await expect(page.getByRole("button", { name: "Dismiss warning" })).toHaveCount(0);
  episode = { ...warning, id: "outage-2" };
  await expect(page.getByRole("button", { name: "Dismiss warning" })).toHaveCount(1);
  expect(errors).toEqual([]);
});

test("navigating to a nonexistent task redirects home", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first()).toBeVisible();

  // After the initial complete list snapshot, selected-detail recovery
  // resolves the REST resource; a confirmed 404 sends us home.
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

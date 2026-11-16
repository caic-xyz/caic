// Browser coverage for the push timeline entry: live output, failure, cancel, reload, and narrow layout.
import { ok } from "node:assert/strict";
import { writeFileSync } from "node:fs";
import path from "node:path";
import { test, expect, createTaskAPI, waitForTaskState, type APIClient, type Page } from "../helpers";

// The fake runtime reads its pre-push hook mode from a file named after the
// pushed branch: "hold" streams output until the file changes, "fail" fails.
function setHook(branch: string, mode: "hold" | "ok" | "fail") {
  const dir = process.env.CAIC_E2E_HOOK_DIR;
  ok(dir, "CAIC_E2E_HOOK_DIR is not configured");
  writeFileSync(path.join(dir, branch.replaceAll("/", "-")), mode);
}

async function openTask(page: Page, api: APIClient, prompt: string, mode: "hold" | "fail" | "ok") {
  const id = await createTaskAPI(api, prompt);
  const task = await waitForTaskState(api, id, "waiting");
  const branch = task.repos?.[0]?.branch;
  ok(branch);
  setHook(branch, mode);
  await page.goto(`/task/@${id}`);
  await expect(page.getByTestId("task-message-area")).toContainText("Lifecycle", { timeout: 15_000 });
  return { id, branch };
}

async function startPush(page: Page) {
  await page.getByRole("button", { name: "Task actions", exact: true }).click();
  await page.getByRole("menuitem", { name: "Push", exact: true }).click();
}

test("a push streams into the timeline, survives a reload, and succeeds", async ({ page, api, uniquePrompt }) => {
  const { branch } = await openTask(page, api, uniquePrompt("FAKE_LIFECYCLE push success"), "hold");
  const entry = page.getByTestId("sync-activity");

  await test.step("the sync button starts one running entry with live output", async () => {
    await startPush(page);
    await expect(entry).toHaveCount(1);
    await expect(entry).toHaveAttribute("data-state", "running");
    await expect(entry).toContainText(branch);
    await expect(entry.getByTestId("sync-output")).toContainText("step 2");
    await expect(entry.getByRole("button", { name: "Cancel push" })).toBeVisible();
  });

  await test.step("a reload during the push rebuilds the running entry", async () => {
    await page.reload();
    await expect(entry).toHaveCount(1);
    await expect(entry).toHaveAttribute("data-state", "running");
    await expect(entry.getByTestId("sync-output")).toContainText("pre-push: checking");
  });

  await test.step("the push finishes live", async () => {
    setHook(branch, "ok");
    await expect(entry).toHaveAttribute("data-state", "pushed", { timeout: 15_000 });
    await expect(entry.getByRole("button", { name: "Cancel push" })).toHaveCount(0);
    await expect(entry.getByTestId("sync-duration")).toBeVisible();
  });
});

test("a failed push stays in the timeline across a reload", async ({ page, api, uniquePrompt }) => {
  await openTask(page, api, uniquePrompt("FAKE_LIFECYCLE push failure"), "fail");
  await startPush(page);
  const entry = page.getByTestId("sync-activity");
  await expect(entry).toHaveAttribute("data-state", "failed");
  await expect(entry.getByTestId("sync-error")).toContainText("lint failed");
  await expect(page.getByText(/^sync failed:/)).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Task actions", exact: true })).toBeEnabled();
  await page.reload();
  await expect(entry).toHaveAttribute("data-state", "failed");
  await expect(entry.getByTestId("sync-error")).toContainText("lint failed");
});

test("cancelling a running push ends it as cancelled, not failed", async ({ page, api, uniquePrompt }) => {
  await openTask(page, api, uniquePrompt("FAKE_LIFECYCLE push cancel"), "hold");
  await startPush(page);
  const entry = page.getByTestId("sync-activity");
  await expect(entry).toHaveAttribute("data-state", "running");
  await entry.getByRole("button", { name: "Cancel push" }).click();
  await expect(entry).toHaveAttribute("data-state", "cancelled");
  await expect(entry.getByTestId("sync-error")).toHaveCount(0);
  await expect(page.getByText(/^sync failed:/)).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Task actions", exact: true })).toBeEnabled();
});

test("the push entry fits a narrow viewport and keeps reduced motion calm", async ({ page, api, uniquePrompt }) => {
  await page.emulateMedia({ reducedMotion: "reduce" });
  await page.setViewportSize({ width: 390, height: 844 });
  const { branch } = await openTask(page, api, uniquePrompt("FAKE_LIFECYCLE push narrow"), "hold");
  await startPush(page);
  const entry = page.getByTestId("sync-activity");
  await expect(entry).toHaveAttribute("data-state", "running");
  await expect(entry.getByTestId("sync-output")).toContainText("step 2");
  const cancel = entry.getByRole("button", { name: "Cancel push" });
  await expect(cancel).toBeInViewport({ ratio: 1 });
  const box = await cancel.boundingBox();
  ok(box);
  expect(box.width).toBeGreaterThanOrEqual(28);
  expect(box.height).toBeGreaterThanOrEqual(28);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  const entryBox = await entry.boundingBox();
  ok(entryBox);
  expect(entryBox.x + entryBox.width).toBeLessThanOrEqual(390);
  const animation = await entry
    .locator("svg")
    .first()
    .evaluate((svg) => getComputedStyle(svg).animationName);
  expect(animation).toBe("none");
  setHook(branch, "ok");
  await expect(entry).toHaveAttribute("data-state", "pushed", { timeout: 15_000 });
});

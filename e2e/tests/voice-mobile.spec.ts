// Browser coverage for voice choice compatibility, shared errors, native focus, and mobile task navigation.

import {
  createTaskAPI,
  expect,
  test,
  waitForTaskState,
  installNativeVoiceBridge,
  invokeFrontendTool,
} from "../helpers";
import type { Config } from "../../sdk/caic/ts/v1/types.gen";

test("browser voice selects a text choice on mobile and displays a failed choice once", async ({ page }) => {
  await page.setViewportSize({ width: 393, height: 851 });
  await page.addInitScript(() => {
    localStorage.setItem(`gomode.voiceChoice.${location.origin}`, "gemini");
  });
  await page.route("**/api/caic/v1/server/config", async (route) => {
    const response = await route.fetch();
    const config = (await response.json()) as Config;
    await route.fulfill({ json: { ...config, voiceGateway: { mode: "embedded" } } });
  });
  await page.route("**/.well-known/gomode.json", (route) =>
    route.fulfill({
      json: {
        apiVersion: 1,
        service: "caic",
        webShell: { toolGroups: [{ name: "tasks", endpoint: "/api/caic/v1/mcp" }], voiceGateway: { url: "/" } },
      },
    }),
  );
  let includeTextChoice = true;
  await page.route("**/api/voicegateway/v1/voice/choices", (route) =>
    route.fulfill({
      json: {
        choices: [
          { id: "gemini", label: "Gemini Live", transports: ["webrtc"] },
          ...(includeTextChoice ? [{ id: "text", label: "Text model", transports: ["text"] }] : []),
        ],
        defaultChoiceID: "gemini",
      },
    }),
  );
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.goto("/");
  const overlay = page.getByTestId("voice-overlay");
  await overlay.getByRole("button", { name: "Voice settings", exact: true }).click();
  const choice = overlay.getByRole("combobox", { name: "Voice choice", exact: true });
  await expect(choice).toHaveValue("gemini");
  await overlay.getByRole("radio", { name: "Browser speech", exact: true }).check();
  await expect(choice).toBeEnabled();
  await expect(choice).toHaveValue("text");
  await expect(overlay.getByRole("alert")).toBeEmpty();
  await overlay.getByRole("button", { name: "Close voice settings", exact: true }).click();
  includeTextChoice = false;
  await overlay.getByRole("button", { name: "Connect voice assistant", exact: true }).click();
  const failure = overlay.getByText("The selected voice choice is unavailable. Choose another voice choice.", {
    exact: true,
  });
  await expect(failure.filter({ visible: true })).toHaveCount(1);
  await overlay.getByRole("button", { name: "Voice settings", exact: true }).click();
  await expect(failure.filter({ visible: true })).toHaveCount(1);
  expect(errors).toEqual([]);
});

test("mobile voice task links open details and browser back restores the voice list", async ({ page, api }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await installNativeVoiceBridge(page, true);
  const id = await createTaskAPI(api, "Review mobile voice task view");
  const task = await waitForTaskState(api, id, "waiting", 30_000);

  await page.goto("/");
  const voiceView = page.getByTestId("mobile-voice-tasks");
  await expect(voiceView).toBeVisible();
  const taskLink = voiceView.locator(`[data-task-id="${id}"]`);
  await expect(taskLink).toContainText(task.title);
  await expect(taskLink).toContainText(/#\d+/);
  await expect(taskLink).toHaveAttribute("href", `/task/@${id}`);
  await expect(page.getByTestId("new-task-button")).toBeHidden();
  await expect(page.getByTestId("new-task-form")).toBeHidden();
  await expect(page.getByTestId("task-list")).toBeHidden();
  await expect.poll(() => voiceView.evaluate((el) => el.scrollWidth <= el.clientWidth)).toBe(true);
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);

  const result = await invokeFrontendTool(page, "focus_task", { task_number: id });
  expect(result).toEqual({ task_id: id });
  await expect(page).toHaveURL(/\/$/);
  await expect(taskLink).toHaveAttribute("aria-current", "true");
  await taskLink.click();
  await expect(page).toHaveURL(new RegExp(`/task/@${id}$`));
  await expect(voiceView).not.toBeVisible();
  await expect(page.getByTestId("detail-pane")).toBeVisible();

  await page.goBack();
  await expect(voiceView).toBeVisible();

  await page.setViewportSize({ width: 900, height: 844 });
  await expect(voiceView).not.toBeVisible();
  await expect(page.getByTestId("task-list")).toBeVisible();
  await invokeFrontendTool(page, "focus_task", { task_number: id });
  await expect(page).toHaveURL(new RegExp(`/task/@${id}(?:\\+[^/]+)?$`));
  await expect(page.getByTestId("detail-pane")).toBeVisible();
});

test("six long task titles fit above voice controls on a Pixel 6", async ({ page, api }) => {
  await page.setViewportSize({ width: 393, height: 851 });
  await installNativeVoiceBridge(page, true);

  const ids = await Promise.all(
    Array.from({ length: 6 }, (_, index) =>
      createTaskAPI(
        api,
        `Review the complete implementation and carefully explain the next steps for the mobile voice task layout ${index}`,
      ),
    ),
  );
  const tasks = await Promise.all(ids.map((id) => waitForTaskState(api, id, "waiting", 30_000)));

  await page.goto("/");
  const voiceView = page.getByTestId("mobile-voice-tasks");
  const rows = voiceView.locator("li");
  await expect.poll(() => rows.count()).toBeGreaterThanOrEqual(6);
  await expect.poll(() => rows.nth(5).evaluate((row) => row.getBoundingClientRect().bottom)).toBeLessThan(851 - 180);

  const name = voiceView.locator(`[data-task-id="${ids[0]}"]`).locator("span").nth(1);
  const titleMetrics = await name.evaluate((el) => ({
    height: el.getBoundingClientRect().height,
    lineHeight: Number.parseFloat(getComputedStyle(el).lineHeight),
    fullTitle: el.textContent,
  }));
  expect(titleMetrics.fullTitle).toBe(tasks[0]?.title);
  expect(titleMetrics.fullTitle).toHaveLength(80);
  expect(titleMetrics.height).toBeLessThanOrEqual(titleMetrics.lineHeight * 2 + 1);
});

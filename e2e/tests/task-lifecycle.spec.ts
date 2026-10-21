// End-to-end tests for the task lifecycle using a fake backend.
import { ok } from "node:assert/strict";
import { test, expect, waitForTaskState, fillContentEditable, createTaskAPI } from "../helpers";

test("create task, verify streaming text and result, then purge", async ({ page, api, uniquePrompt }) => {
  await page.goto("/");

  // Wait for repos to load (a chip appears in the strip).
  await expect(page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first()).toBeVisible();

  // Use a unique prompt to avoid collisions with parallel tests.
  const prompt = uniquePrompt("FAKE_LIFECYCLE e2e lifecycle");

  // Fill prompt and submit.
  await fillContentEditable(page.getByTestId("prompt-input"), prompt);
  await page.getByTestId("submit-task").click();

  // Task creation navigates directly to TaskDetail; the initial event stream
  // should render there without requiring a task-list click/reopen.
  await expect(page).toHaveURL(/\/task\//);

  // The dedicated scenario emits a stable text-delta marker and success result.
  // Scope both assertions to this task's detail stream.
  const messages = page.getByTestId("task-message-area");
  await expect(messages).toContainText("Lifecycle streaming marker", { timeout: 15_000 });
  await expect(messages).toContainText("Lifecycle completed", { timeout: 10_000 });

  // Resolve the task ID and wait for "waiting" state via API before clicking
  // stop. The UI may show the result message before the SSE delivers the
  // "waiting" state; clicking stop while still "running" triggers a
  // window.confirm() that Playwright auto-dismisses as false, skipping the
  // stop.
  const tasks = await api.listTasks();
  const task = tasks.find((t) => t.initialPrompt === prompt);
  expect(task).toBeTruthy();
  await waitForTaskState(api, task!.id, "waiting");

  // The Stop button (trash can) appears on hover over the task card in the sidebar.
  // Scope to the specific card to avoid strict-mode violations from parallel tests.
  const taskCard = page.locator("[data-task-id]", { hasText: prompt });
  await taskCard.hover();
  const stopBtn = taskCard.getByTestId("stop-task");
  await expect(stopBtn).toBeVisible({ timeout: 15_000 });

  // Wait for the SSE event to update the frontend state to "waiting".
  // If the UI still shows "running", the stop button triggers a
  // window.confirm() that Playwright auto-dismisses as false.
  await expect(taskCard.getByTestId("state-badge")).toHaveText("waiting", { timeout: 15_000 });

  await stopBtn.click();

  // Poll API until our task is "stopped".
  await waitForTaskState(api, task!.id, "stopped");

  // A stopped task must be selected before its destructive inline action appears.
  await taskCard.click();
  const purgeButton = taskCard.getByTestId("purge-task");
  await expect(purgeButton).toBeVisible();

  // Purging through the UI skips confirmation while the recovery delay is enabled.
  await purgeButton.click();
  await waitForTaskState(api, task!.id, "purged");
});

test("new-task effort stays compact and features follow the selectors", async ({ page, api }, testInfo) => {
  const harnesses = await api.listHarnesses();
  await page.route("**/server/harnesses", (route) =>
    route.fulfill({
      json: harnesses.map((harness) =>
        harness.name === "codex"
          ? { ...harness, models: [{ id: "fake-model", effortOptions: ["low", "medium", "high"] }] }
          : harness,
      ),
    }),
  );
  await page.setViewportSize({ width: 1280, height: 844 });
  await page.goto("/");
  await page.getByTestId("harness-select").selectOption("codex");
  await page.getByTestId("model-select").click();
  await page.getByRole("option", { name: "fake-model", exact: true }).click();
  const effort = page.getByTestId("effort-select");
  await effort.selectOption("medium");
  const disclosure = page.getByRole("button", { name: "Show features" });
  const desktopEffort = (await effort.boundingBox())!;
  const desktopDisclosure = (await disclosure.boundingBox())!;
  expect.soft(Math.abs(desktopDisclosure.y - desktopEffort.y)).toBeLessThan(1);
  expect.soft(desktopDisclosure.x - desktopEffort.x - desktopEffort.width).toBeLessThan(10);
  await page.screenshot({ path: testInfo.outputPath("compact-controls-desktop.png") });

  await page.setViewportSize({ width: 390, height: 844 });
  await expect(effort).toBeVisible();
  expect.soft((await effort.boundingBox())!.width).toBeLessThanOrEqual(desktopEffort.width + 1);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  await page.screenshot({ path: testInfo.outputPath("compact-controls-mobile.png") });
});

test("collapsed task features preserve opaque session IDs on desktop and mobile", async ({
  page,
  api,
  uniquePrompt,
}, testInfo) => {
  const config = await api.getConfig();
  const runtimes = config.runtimes;
  if (!runtimes?.length) throw new Error("Fake server must expose a runtime");
  const runtimeConfig = runtimes[0];
  ok(runtimeConfig);
  await page.route("**/server/config", (route) =>
    route.fulfill({
      json: { ...config, runtimes: [...runtimes, { name: "standby" }] },
    }),
  );
  await page.goto("/");
  await expect(page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first()).toBeVisible();
  const runtime = page.getByRole("combobox", { name: "Runtime" });
  const disclosure = page.getByRole("button", { name: /^(Show|Hide) features$/ });
  const input = page.getByRole("textbox", { name: "Session ID to resume" });
  await expect(disclosure).toHaveAttribute("aria-expanded", "false");
  await expect(input).toBeHidden();
  await expect(runtime).toBeHidden();
  const harness = page.getByTestId("harness-select");
  await expect
    .poll(async () => Math.abs((await disclosure.boundingBox())!.height - (await harness.boundingBox())!.height))
    .toBeLessThan(1);
  await page.screenshot({ path: testInfo.outputPath("features-collapsed-desktop.png") });
  await page.setViewportSize({ width: 390, height: 844 });
  await expect
    .poll(async () => Math.abs((await disclosure.boundingBox())!.height - (await harness.boundingBox())!.height))
    .toBeLessThan(1);
  await page.screenshot({ path: testInfo.outputPath("features-collapsed-mobile.png") });
  await page.setViewportSize({ width: 1280, height: 844 });
  await disclosure.focus();
  await page.keyboard.press("Tab");
  await expect(page.getByTestId("prompt-input")).toBeFocused();
  await page.keyboard.press("Shift+Tab");
  await expect(disclosure).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(disclosure).toHaveAttribute("aria-expanded", "true");
  await expect(input).toBeVisible();
  await expect(runtime).toBeVisible();
  await runtime.selectOption(runtimeConfig.name);
  await expect(input).toHaveAttribute("placeholder", "Session ID to resume");
  await expect(input).not.toHaveAttribute("maxlength");
  await disclosure.focus();
  await page.keyboard.press("Tab");
  await expect(runtime).toBeFocused();
  const sessionID = "harness session:';$(opaque) " + "opaque-value-".repeat(12);
  await input.fill(`  ${sessionID}  `);
  expect((await input.boundingBox())!.width).toBeLessThan(450);
  await page.screenshot({ path: testInfo.outputPath("resume-session-desktop.png") });
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(input).toHaveValue(`  ${sessionID}  `);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  const bounds = await input.boundingBox();
  expect(bounds!.width).toBeGreaterThan(250);
  expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(390);
  await page.screenshot({ path: testInfo.outputPath("resume-session-mobile.png") });
  const toggle = page.getByRole("checkbox", { name: "Enable CAIC MCP delegation for this task" });
  await toggle.focus();
  await page.keyboard.press("Space");
  await expect(toggle).not.toBeChecked();
  await page.getByRole("button", { name: "Hide features" }).click();
  await expect(input).toBeHidden();
  await expect(runtime).toBeHidden();
  await expect(page.getByRole("button", { name: "Show features" })).toBeFocused();
  await page.getByRole("button", { name: "Show features" }).click();
  await expect(input).toHaveValue(`  ${sessionID}  `);
  await expect(runtime).toHaveValue(runtimeConfig.name);
  await expect(toggle).not.toBeChecked();
  await page.getByRole("button", { name: "Hide features" }).click();

  const prompt = uniquePrompt("Continue the existing session");
  await fillContentEditable(page.getByTestId("prompt-input"), prompt);
  const request = page.waitForRequest((req) => req.method() === "POST" && req.url().endsWith("/api/caic/v1/tasks"));
  await page.getByTestId("submit-task").click();
  const body = (await request).postDataJSON();
  expect(body).toMatchObject({ resumeSessionID: sessionID, runtimeName: runtimeConfig.name });
  expect(body.caicMCP).toBeUndefined();
  await expect(page).toHaveURL(/\/task\//);
  const tasks = await api.listTasks();
  const task = tasks.find((t) => t.initialPrompt === prompt);
  expect(task).toBeTruthy();
  await waitForTaskState(api, task!.id, "waiting");
  await api.stopTask(task!.id);
});

test("mobile stopped task card exposes Revive without selection", async ({ page, api }, testInfo) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const id = await createTaskAPI(api, "Revive from the mobile task summary");
  await waitForTaskState(api, id, "waiting", 30_000);
  await api.stopTask(id);
  await waitForTaskState(api, id, "stopped");

  await page.goto("/");
  const stopped = page.getByRole("button", { name: /Stopped \(/ }).first();
  await expect(stopped).toBeVisible();
  const card = page.locator(`[data-task-id="${id}"]`);
  if (!(await card.isVisible())) await stopped.click();
  const revive = card.getByRole("button", { name: "Revive", exact: true });
  await expect(revive).toBeVisible();
  await expect.poll(() => revive.evaluate((button) => getComputedStyle(button.parentElement!).opacity)).toBe("1");
  await expect(card.getByTestId("purge-task")).toHaveCount(0);
  await page.setViewportSize({ width: 1280, height: 844 });
  await expect(revive).toBeHidden();
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(revive).toBeVisible();
  await card.screenshot({ path: testInfo.outputPath("mobile-revive-card.png") });

  await revive.click();
  await waitForTaskState(api, id, "waiting", 30_000);
  await expect(card.getByTestId("state-badge")).toHaveText("waiting");
  await expect(page).toHaveURL(/\/$/);
});

test("setup logs remain visible after task-detail replay", async ({ page, api }, testInfo) => {
  const id = await createTaskAPI(api, "Verify setup log replay");
  await waitForTaskState(api, id, "waiting", 30_000);

  await page.goto(`/task/@${id}`);
  const setup = page.getByTestId("task-setup");
  const setupLogs = page.getByTestId("task-setup-logs");
  const messageArea = page.getByTestId("task-message-area");
  await expect(setup).toBeVisible();
  await expect(messageArea.getByTestId("task-setup")).toBeVisible();
  await expect(setup).not.toHaveAttribute("open");
  await expect(setup.locator("summary")).toContainText(/(?:\d+ms|\d+\.\d+s)/);
  await expect(setupLogs).not.toBeVisible();

  await setup.getByText("Setup logs").click();
  await expect(setupLogs).toContainText("Fake runtime setup complete");

  // Reopening the detail stream preserves setup output and restores its
  // collapsed default instead of burying it in a historical turn.
  await page.reload();
  await expect(setup).toBeVisible();
  await expect(messageArea.getByTestId("task-setup")).toBeVisible();
  await expect(setup).not.toHaveAttribute("open");
  await page.screenshot({ path: testInfo.outputPath("task-setup-logs.png") });
});

test("task detail desktop layout wraps timed controls and avoids pane overflow", async ({ page, api }) => {
  await page.setViewportSize({ width: 950, height: 800 });
  const id = await createTaskAPI(api, "Fix a desktop overflow regression");
  await waitForTaskState(api, id, "waiting", 30_000);

  await page.goto(`/task/@${id}`);
  const messageArea = page.getByTestId("task-message-area");
  const detailPane = page.getByTestId("detail-pane");
  const taskList = page.getByTestId("task-list");
  await expect(messageArea).toBeVisible();

  await test.step("timing and Raw controls wrap message content", async () => {
    const rawButton = page.getByRole("button", { name: "raw", exact: true }).nth(1);
    await expect(rawButton).toBeAttached();
    await expect
      .poll(async () =>
        rawButton.evaluate((button) => {
          const toolbar = button.parentElement;
          const message = toolbar?.parentElement?.parentElement;
          return message ? getComputedStyle(message).paddingRight : null;
        }),
      )
      .toBe("0px");
    await expect
      .poll(async () =>
        rawButton.evaluate((button) => {
          const toolbar = button.parentElement;
          const content = toolbar?.parentElement?.parentElement?.parentElement;
          const timing = content?.querySelector<HTMLElement>("[data-testid='timing-duration']")?.parentElement
            ?.parentElement;
          return timing ? getComputedStyle(timing).float : null;
        }),
      )
      .toBe("right");

    await rawButton.hover();
    await expect
      .poll(async () =>
        rawButton.evaluate((button) => {
          const toolbar = button.parentElement;
          const content = toolbar?.parentElement?.parentElement?.parentElement;
          const timing = content?.querySelector<HTMLElement>("[data-testid='timing-duration']")?.parentElement
            ?.parentElement;
          return timing ? getComputedStyle(timing).opacity : null;
        }),
      )
      .toBe("0");
  });

  await test.step("detail pane has no extra gutters or horizontal overflow", async () => {
    await expect.poll(async () => messageArea.evaluate((el) => getComputedStyle(el).overflowX)).toBe("hidden");
    await expect
      .poll(async () =>
        taskList.evaluate((list) => {
          const detail = document.querySelector('[data-testid="detail-pane"]');
          if (!detail) return Number.POSITIVE_INFINITY;
          return detail.getBoundingClientRect().left - list.getBoundingClientRect().right;
        }),
      )
      .toBeLessThanOrEqual(8);

    await page.getByTitle("Collapse sidebar").click();
    const expandButton = page.getByTitle("Expand sidebar");
    await expect(expandButton).toBeVisible();
    await expect
      .poll(async () =>
        expandButton.evaluate((button) => {
          const detail = document.querySelector('[data-testid="detail-pane"]');
          if (!detail) return Number.POSITIVE_INFINITY;
          return detail.getBoundingClientRect().left - button.getBoundingClientRect().right;
        }),
      )
      .toBeLessThanOrEqual(8);
    await expect(detailPane).toBeVisible();
  });
});

test("add-repo dropdown is visible and not clipped by overflow", async ({ page }) => {
  await page.goto("/");

  // Wait for repos to load.
  await expect(page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first()).toBeVisible();

  // The add-repo button should be present (at least one repo is not yet selected).
  const addBtn = page.getByTestId("add-repo-button");
  await expect(addBtn).toBeVisible();

  // Open the dropdown.
  await addBtn.click();
  const dropdown = page.getByTestId("add-repo-dropdown");
  await expect(dropdown).toBeVisible();

  // The dropdown must not be clipped: its bounding box must be fully within the viewport.
  const box = await dropdown.boundingBox();
  expect(box).not.toBeNull();
  const viewportSize = page.viewportSize();
  expect(viewportSize).not.toBeNull();
  expect(box!.y).toBeGreaterThanOrEqual(0);
  expect(box!.y + box!.height).toBeLessThanOrEqual(viewportSize!.height);
});

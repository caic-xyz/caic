// Browser coverage for deferred optional pages, scoped loading, retained conversations, and failed-chunk recovery.

import { createTaskAPI, expect, test, waitForTaskState } from "../helpers";

// Chunk gates must exercise the production loader rather than a service-worker cache.
test.use({ serviceWorkers: "block" });

for (const width of [390, 1280]) {
  test(`optional task views load without replacing the conversation at ${width}px`, async ({
    page,
    api,
    uniquePrompt,
  }) => {
    await page.setViewportSize({ width, height: 844 });
    const id = await createTaskAPI(api, uniquePrompt("FAKE_LIFECYCLE Keep the conversation while opening task info"));
    await waitForTaskState(api, id, "waiting");
    const scripts: string[] = [];
    const errors: string[] = [];
    page.on("request", (request) => {
      if (request.resourceType() === "script") scripts.push(request.url());
    });
    page.on("pageerror", (error) => errors.push(error.message));
    const release = Promise.withResolvers<void>();
    let infoRequests = 0;
    await page.route(/\/assets\/TaskInfoPage-[^/]+\.js$/, async (route) => {
      infoRequests++;
      await release.promise;
      await route.continue();
    });
    try {
      await page.goto("/");
      await expect(page.locator("#connection-status")).toHaveText("Connected");
      await expect(page.getByTestId("prompt-input")).toBeVisible();
      expect(
        scripts.filter((url) =>
          /\/(?:Diff|FileViewer|Monitoring|Processes|Sessions|Settings|Stats|TaskInfo|Vnc)Page-[^/]+\.js$/.test(url),
        ),
      ).toEqual([]);

      await page.locator(`[data-task-id="${id}"]`).click();
      const conversation = page.getByTestId("conversation-pane");
      const transcript = conversation.getByText("Lifecycle streaming marker", { exact: true });
      await expect(transcript).toBeVisible();
      // The fake CI badge changes width when the last check passes.
      await expect(
        conversation.getByTestId("task-detail-header").getByText("CI: passed", { exact: true }),
      ).toBeVisible();
      const original = await conversation.elementHandle();
      const prompt = page.getByTestId("task-detail-prompt");
      await expect(prompt).toBeVisible();
      await prompt.fill("Keep this draft while the view loads");
      const taskURL = page.url();
      const infoLink = page.getByRole("link", { name: "Task info", exact: true });
      await infoLink.focus();
      const linkBounds = await infoLink.boundingBox();
      await page.keyboard.press("Enter");
      await expect.poll(() => infoRequests).toBe(1);
      // Solid's route transition retains the current view while its next
      // component loads; the shell reports that pending navigation.
      await expect(page.getByRole("progressbar", { name: "Loading page" })).toBeVisible();
      await expect(page.getByTestId("app-shell")).toBeVisible();
      expect(await original!.evaluate((node) => node.isConnected)).toBe(true);
      await expect(prompt).toHaveText("Keep this draft while the view loads");
      await expect(transcript).toBeVisible();
      expect(await infoLink.boundingBox()).toEqual(linkBounds);

      release.resolve();
      await expect(page.getByRole("heading", { name: "Overview", exact: true })).toBeVisible();
      await expect(page.getByRole("progressbar", { name: "Loading page" })).toHaveCount(0);
      if (width > 768) await expect(conversation).toBeVisible();
      else await expect(conversation).toBeHidden();
      await page.getByRole("button", { name: "Close view", exact: true }).click();
      await expect(page).toHaveURL(taskURL);
      await expect(prompt).toBeVisible();
      await expect(prompt).toHaveText("Keep this draft while the view loads");
      expect(await original!.evaluate((node) => node.isConnected)).toBe(true);
      await page.getByRole("link", { name: "Task info", exact: true }).click();
      await expect(page.getByRole("heading", { name: "Overview", exact: true })).toBeVisible();
      expect(infoRequests).toBe(1);
      expect(errors).toEqual([]);
    } finally {
      release.resolve();
    }
  });
}

for (const resource of ["script", "stylesheet", "evaluation"] as const) {
  test(`a failed route ${resource} uses ${resource === "script" ? "in-place retry" : "document reload"} recovery`, async ({
    page,
  }, testInfo) => {
    const width = resource === "script" ? 390 : 1280;
    await page.setViewportSize({ width, height: 844 });
    const release = Promise.withResolvers<void>();
    const asset =
      resource === "stylesheet" ? /\/assets\/SettingsPage-[^/]+\.css$/ : /\/assets\/SettingsPage-[^/]+\.js$/;
    let requests = 0;
    await page.route(asset, async (route) => {
      requests++;
      if (requests === 1) {
        await release.promise;
        if (resource === "evaluation") {
          await route.fulfill({
            contentType: "text/javascript",
            body: 'throw new TypeError("Settings module evaluation failed");',
          });
        } else await route.abort("failed");
      } else await route.continue();
    });
    try {
      // A held stylesheet delays window.load; wait for the mounted document instead.
      await page.goto("/settings", { waitUntil: "domcontentloaded" });
      await expect.poll(() => requests).toBe(1);
      await expect(page.getByRole("status")).toHaveText("Loading page…");
      const shell = page.getByTestId("app-shell");
      await expect(shell).toBeVisible();
      const original = await shell.elementHandle();
      await expect(page.getByTestId("new-task-button")).toBeVisible();
      const documentStartedAt = await page.evaluate(() => performance.timeOrigin);
      release.resolve();
      const failure = page.getByTestId("error-fallback");
      await expect(failure).toBeVisible();
      await expect(failure).toContainText("Page content could not be loaded.");
      const retry = failure.getByRole("button", { name: "Try again", exact: true });
      if (resource === "script") await expect(retry).toBeVisible();
      else await expect(retry).toHaveCount(0);
      await expect(failure.getByRole("button", { name: "Copy diagnostic details", exact: true })).toBeVisible();
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
      await page.screenshot({ path: testInfo.outputPath(`code-load-${resource}.png`) });
      await failure.getByText("Technical details", { exact: true }).click();
      await expect(failure).toContainText("Caused by:");
      if (resource === "evaluation") await expect(failure).toContainText("Settings module evaluation failed");
      expect(await original!.evaluate((node) => node.isConnected)).toBe(true);
      await expect(page.getByTestId("new-task-button")).toBeVisible();
      if (resource === "script") await retry.click();
      else await failure.getByRole("button", { name: "Reload page", exact: true }).click();
      await expect(page.getByRole("heading", { name: "Settings", exact: true })).toBeVisible();
      await expect(failure).toHaveCount(0);
      expect(requests).toBe(2);
      const recoveredDocument = await page.evaluate(() => performance.timeOrigin);
      if (resource === "script") {
        expect(recoveredDocument).toBe(documentStartedAt);
        expect(await original!.evaluate((node) => node.isConnected)).toBe(true);
      } else expect(recoveredDocument).not.toBe(documentStartedAt);
    } finally {
      release.resolve();
    }
  });
}

test("a failed nested chart download retries within the current document", async ({ page, api, uniquePrompt }) => {
  const id = await createTaskAPI(api, uniquePrompt("Inspect performance after chart loading recovers"));
  let requests = 0;
  await page.route(/\/assets\/StatsCharts-[^/]+\.js$/, async (route) => {
    if (++requests === 1) await route.abort("failed");
    else await route.continue();
  });
  await page.goto(`/task/@${id}/perf`);
  const failure = page.getByTestId("error-fallback");
  await expect(failure).toContainText("Page content could not be loaded.");
  const documentStartedAt = await page.evaluate(() => performance.timeOrigin);
  await failure.getByRole("button", { name: "Try again", exact: true }).click();
  await expect(page.getByTestId("resource-charts")).toBeVisible();
  await expect(failure).toHaveCount(0);
  expect(requests).toBe(2);
  expect(await page.evaluate(() => performance.timeOrigin)).toBe(documentStartedAt);
});

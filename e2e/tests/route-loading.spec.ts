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
    const id = await createTaskAPI(api, uniquePrompt("Keep the conversation while opening task info"));
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
      await expect(conversation).toBeVisible();
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

test("standalone page loading retains the shell and a failed chunk recovers on reload", async ({ page }) => {
  const release = Promise.withResolvers<void>();
  let requests = 0;
  await page.route(/\/assets\/SettingsPage-[^/]+\.js$/, async (route) => {
    requests++;
    if (requests === 1) {
      await release.promise;
      await route.abort("failed");
    } else await route.continue();
  });
  try {
    await page.goto("/settings");
    await expect.poll(() => requests).toBe(1);
    await expect(page.getByRole("status")).toHaveText("Loading page…");
    const shell = page.getByTestId("app-shell");
    await expect(shell).toBeVisible();
    const original = await shell.elementHandle();
    await expect(page.getByTestId("new-task-button")).toBeVisible();
    const document = await page.evaluate(() => performance.timeOrigin);
    release.resolve();
    await expect(page.getByTestId("error-fallback")).toBeVisible();
    expect(await original!.evaluate((node) => node.isConnected)).toBe(true);
    await expect(page.getByTestId("new-task-button")).toBeVisible();
    await page.getByRole("button", { name: "Reload page", exact: true }).click();
    await expect(page.getByRole("heading", { name: "Settings", exact: true })).toBeVisible();
    await expect(page.getByTestId("error-fallback")).toHaveCount(0);
    expect(requests).toBe(2);
    expect(await page.evaluate(() => performance.timeOrigin)).not.toBe(document);
  } finally {
    release.resolve();
  }
});

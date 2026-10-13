// Browser coverage for persistent task/document panes, viewport full screen, history, and CAD identity.
import { test, expect, createTaskAPI, waitForTaskState } from "../helpers";
import { createPaintedBambu3mfFixture } from "../../frontend/tests/cad-fixture";

for (const width of [390, 1000, 1440]) {
  test(`document workspace keeps task and viewer state at ${width}px`, async ({ page, api, uniquePrompt }) => {
    await page.setViewportSize({ width, height: 900 });
    const config = await api.getConfig();
    config.voiceGateway = { mode: "embedded", url: "/voice/" };
    await page.route("**/api/caic/v1/server/config", (route) => route.fulfill({ json: config }));
    await page.route("**/.well-known/gomode.json", (route) =>
      route.fulfill({ json: { service: "caic", webShell: { voiceGateway: { url: "/voice/" } } } }),
    );
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    let documentReads = 0;
    let streams = 0;
    page.on("request", (request) => {
      if (/\/tasks\/[^/]+\/events\?/.test(request.url())) streams++;
    });
    await page.route("**/raw?*", async (route) => {
      const path = new URL(route.request().url()).searchParams.get("path");
      if (path === "/guide.md") {
        documentReads++;
        await route.fulfill({
          contentType: "text/plain",
          body: "# Guide\n\n" + "A paragraph of realistic document text with **emphasis**.\n\n".repeat(80),
        });
      } else if (path === "/model.3mf") {
        await route.fulfill({ contentType: "model/3mf", body: Buffer.from(createPaintedBambu3mfFixture("8")) });
      } else await route.continue();
    });
    const id = await createTaskAPI(
      api,
      uniquePrompt(
        "Context paragraph.<br><br>".repeat(30) +
          "Inspect [Guide](/guide.md) and [Model](/model.3mf).<br><br>" +
          "More conversation context.<br><br>".repeat(60),
      ),
    );
    await waitForTaskState(api, id, "waiting");
    await page.goto(`/task/@${id}`);
    await expect(page.getByRole("link", { name: "Guide", exact: true })).toBeVisible();
    const conversation = await page.getByTestId("conversation-pane").elementHandle();
    await page.getByTestId("task-detail-prompt").fill("Keep this draft");
    const initialStreams = streams;
    await page.getByRole("link", { name: "Guide", exact: true }).scrollIntoViewIfNeeded();
    let taskScroll = await page.getByTestId("task-message-area").evaluate((element) => element.scrollTop);
    expect(taskScroll).toBeGreaterThan(0);
    await page.getByRole("link", { name: "Guide", exact: true }).click();
    await expect(page.getByRole("heading", { name: "Guide", exact: true })).toBeVisible();
    if (width > 768) {
      await expect(page.getByTestId("task-detail-prompt")).toBeVisible();
      const taskBounds = await page.getByTestId("conversation-pane").boundingBox();
      const docBounds = await page.getByTestId("document-pane").boundingBox();
      expect(taskBounds && docBounds && docBounds.x >= taskBounds.x + taskBounds.width).toBeTruthy();
    }
    expect(await conversation?.evaluate((node) => node.isConnected)).toBe(true);
    if (width === 1000) {
      await expect(page.getByTitle("Expand sidebar")).toBeVisible();
      await page.getByTestId("task-detail-prompt").focus();
      await page.keyboard.press("Shift+Tab");
      await expect(page.getByTestId("task-detail-prompt")).not.toBeFocused();
      await page.getByRole("link", { name: "Guide", exact: true }).scrollIntoViewIfNeeded();
      taskScroll = await page.getByTestId("task-message-area").evaluate((element) => element.scrollTop);
      await page.getByTitle("Expand sidebar").click();
      await expect(page.getByTitle("Collapse sidebar")).toBeVisible();
      await page.getByTitle("Collapse sidebar").click();
    } else if (width === 1440) {
      await expect(page.getByTitle("Collapse sidebar")).toBeVisible();
    } else {
      await expect(page.getByTestId("conversation-pane")).toBeHidden();
      await page.getByRole("button", { name: "Task", exact: true }).click();
      await expect(page).toHaveURL(/pane=task/);
      await expect(page.getByTestId("task-detail-prompt")).toHaveText("Keep this draft");
      await expect(page.getByTestId("document-pane")).toBeHidden();
      await page.getByTestId("task-detail-prompt").focus();
      await page.keyboard.press("Shift+Tab");
      await expect(page.getByTestId("task-detail-prompt")).not.toBeFocused();
      await page.getByRole("link", { name: "Guide", exact: true }).scrollIntoViewIfNeeded();
      taskScroll = await page.getByTestId("task-message-area").evaluate((element) => element.scrollTop);
      await page.getByRole("button", { name: "Document", exact: true }).click();
      await expect(page.getByRole("heading", { name: "Guide", exact: true })).toBeVisible();
    }
    await page.screenshot({ path: test.info().outputPath("docked-document.png") });
    await expect(page.getByTestId("voice-overlay")).toBeVisible();
    if (width > 768) {
      await page.getByRole("button", { name: "Context actions", exact: true }).click();
      await expect(page.getByRole("menu")).toBeVisible();
      await page.keyboard.press("Escape");
      await expect(page.getByRole("menu")).toBeHidden();
      await expect(page.getByTestId("document-pane")).toBeVisible();
    }
    await page.getByRole("button", { name: "Full screen", exact: true }).click();
    await expect(page).toHaveURL(/fullscreen=1/);
    await expect(page.getByTestId("new-task-button")).toBeHidden();
    await expect(page.getByTestId("summary-pane")).toBeHidden();
    await expect(page.getByTestId("conversation-pane")).toBeHidden();
    await expect(page.getByTestId("voice-overlay")).toBeHidden();
    await expect(page.getByRole("link", { name: "raw file", exact: true })).toBeVisible();
    await page.keyboard.press("PageDown");
    await page.keyboard.press("/");
    await expect(page).toHaveURL(new RegExp(`/view\\?[^#]*fullscreen=1`));
    await expect(page.getByTestId("task-detail-prompt")).not.toBeFocused();
    await page.keyboard.press("Tab");
    await expect(page.getByRole("link", { name: "raw file", exact: true })).toBeFocused();
    await page.keyboard.press("Shift+Tab");
    await expect(page.getByRole("button", { name: "Exit full screen", exact: true })).toBeFocused();
    await page.screenshot({ path: test.info().outputPath("fullscreen-document.png") });
    await page.getByRole("button", { name: "Exit full screen", exact: true }).click();
    await expect(page.getByTestId("new-task-button")).toBeVisible();
    await page.goBack();
    await expect(page.getByRole("button", { name: "Exit full screen", exact: true })).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("button", { name: "Full screen", exact: true })).toBeVisible();
    expect(documentReads).toBe(1);
    expect(streams).toBe(initialStreams);
    await page.setViewportSize({ width: 1440, height: 900 });
    await expect(page.getByTitle("Collapse sidebar")).toBeVisible();
    await expect(page.getByTestId("conversation-pane")).toBeVisible();
    await page.setViewportSize({ width: 1000, height: 900 });
    await expect(page.getByTitle("Expand sidebar")).toBeVisible();
    await page.setViewportSize({ width: 390, height: 900 });
    await expect(page.getByTestId("conversation-pane")).toBeHidden();
    await page.getByRole("button", { name: "Task", exact: true }).click();
    await page.setViewportSize({ width: 1440, height: 900 });
    await expect(page.getByTestId("document-pane")).toBeVisible();
    await page.setViewportSize({ width: 390, height: 900 });
    await expect(page.getByTestId("document-pane")).toBeHidden();
    await page.getByRole("button", { name: "Document", exact: true }).click();
    await page.setViewportSize({ width, height: 900 });
    expect(documentReads).toBe(1);
    expect(await conversation?.evaluate((node) => node.isConnected)).toBe(true);
    await page.getByRole("button", { name: "Close document", exact: true }).click();
    await expect(page.getByTestId("task-detail-prompt")).toHaveText("Keep this draft");
    await expect(page.getByRole("link", { name: "Guide", exact: true })).toBeFocused();
    expect(await page.getByTestId("task-message-area").evaluate((element) => element.scrollTop)).toBe(taskScroll);
    expect(await conversation?.evaluate((node) => node.isConnected)).toBe(true);
    await page.getByRole("link", { name: "Guide", exact: true }).click();
    await expect(page.getByRole("heading", { name: "Guide", exact: true })).toBeVisible();
    await page.goBack();
    await expect(page.getByTestId("document-pane")).toBeHidden();
    await expect(page.getByRole("link", { name: "Guide", exact: true })).toBeFocused();
    await page.getByRole("link", { name: "Model", exact: true }).click();
    await expect(page.getByText("4 triangles", { exact: true })).toBeVisible();
    const canvas = await page.locator("canvas").elementHandle();
    await page.getByRole("button", { name: "wireframe", exact: true }).click();
    await page.getByRole("button", { name: "Full screen", exact: true }).click();
    await page.getByRole("button", { name: "Exit full screen", exact: true }).click();
    expect(await canvas?.evaluate((node) => node.isConnected)).toBe(true);
    await expect(page.getByRole("button", { name: "wireframe", exact: true })).toHaveClass(/active/);
    await page.getByRole("button", { name: "Full screen", exact: true }).click();
    await page.reload();
    await expect(page.getByRole("button", { name: "Exit full screen", exact: true })).toBeVisible();
    await expect(page.getByTestId("new-task-button")).toBeHidden();
    await api.sendInput(id, { prompt: { text: "continue" } });
    await waitForTaskState(api, id, "waiting");
    await expect(page.getByTestId("conversation-pane").getByText(/A SQL query walks into a bar/)).toBeAttached();
    expect(errors).toEqual([]);
  });
}

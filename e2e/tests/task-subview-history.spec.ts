// Browser coverage for persistent task views, shared history, and full screen on mobile and desktop.
import { createTaskAPI, expect, test, waitForTaskState } from "../helpers";

for (const width of [390, 1280]) {
  test(`task details fill the workspace at ${width}px`, async ({ page, api, uniquePrompt }) => {
    await page.setViewportSize({ width, height: 844 });
    const id = await createTaskAPI(api, uniquePrompt("Check task details full screen"));
    const task = await waitForTaskState(api, id, "waiting");
    const taskURL = `/task/@${id}`;
    const fullScreenURL = `${taskURL}?fullscreen=1`;
    await page.goto(taskURL);
    const conversation = page.getByTestId("conversation-pane");
    const prompt = conversation.getByTestId("task-detail-prompt");
    const summary = page.getByTestId("summary-pane");
    const newTask = page.getByTestId("new-task-button");
    const header = conversation.getByTestId("task-detail-header");
    const enter = header.getByRole("button", { name: "Full screen", exact: true });
    const exit = header.getByRole("button", { name: "Exit full screen", exact: true });
    const close = header.getByRole("button", { name: "Close task details" });
    const pane = page.getByTestId("task-view-pane");
    const fullScreenConversation = new RegExp(`/task/@${id}(?:\\+[^/?]+)?\\?fullscreen=1$`);
    await prompt.fill("Keep this draft");
    const promptNode = await prompt.elementHandle();

    async function expectRightAlignedControls(toggle: typeof enter) {
      const headerBox = await header.boundingBox();
      const toggleBox = await toggle.boundingBox();
      if (!headerBox || !toggleBox) throw new Error("Task header and full-screen control must be visible");
      let lastControl = toggleBox;
      if (width <= 768) {
        await expect(close).toBeHidden();
      } else {
        const closeBox = await close.boundingBox();
        if (!closeBox) throw new Error("The desktop task header must have a close control");
        expect(toggleBox.x + toggleBox.width).toBeLessThan(closeBox.x);
        expect(toggleBox.y).toBeCloseTo(closeBox.y, 0);
        lastControl = closeBox;
      }
      expect(Math.abs(headerBox.x + headerBox.width - (lastControl.x + lastControl.width))).toBeLessThanOrEqual(12);
      expect(Math.abs(toggleBox.y - headerBox.y)).toBeLessThanOrEqual(4);
    }

    await test.step("Expand the conversation and keep the task title visible", async () => {
      await expectRightAlignedControls(enter);
      await enter.click();
      await expect(page).toHaveURL(fullScreenURL);
      await expect(exit).toBeFocused();
      await expect(summary).toBeHidden();
      await expect(newTask).toBeHidden();
      await expectRightAlignedControls(exit);
      const title = header.getByText(task.title, { exact: true });
      await expect(title).toBeVisible();
      await title.hover();
      await expect(page.getByRole("tooltip")).toHaveText(task.title);
      await exit.hover();
      await expect(conversation).toBeVisible();
      const bounds = await conversation.boundingBox();
      expect(bounds!.width).toBeGreaterThan(width - 24);
      await expect(prompt).toHaveText("Keep this draft");
    });

    await test.step("Let Escape dismiss task actions before exiting full screen", async () => {
      const actions = conversation.getByRole("button", { name: "Task actions" });
      const fork = conversation.getByRole("menuitem", { name: "Fork", exact: true });
      await actions.click();
      await expect(fork).toBeVisible();
      await page.keyboard.press("Escape");
      await expect(fork).toBeHidden();
      await expect(actions).toBeFocused();
      await expect(page).toHaveURL(fullScreenURL);
      await actions.click();
      await fork.click();
      const dialog = page.getByRole("dialog");
      await expect(dialog.getByRole("heading", { name: "Fork task" })).toBeVisible();
      await page.keyboard.press("Escape");
      await expect(dialog).toBeHidden();
      await expect(page).toHaveURL(fullScreenURL);
    });

    await test.step("Restore the workspace with Escape and keep the focused draft", async () => {
      await prompt.press("Escape");
      await expect(page).toHaveURL(taskURL);
      await expect(prompt).toBeFocused();
      await expect(newTask).toBeVisible();
      await expect(prompt).toHaveText("Keep this draft");
      expect(await promptNode?.evaluate((node) => node === document.activeElement)).toBe(true);
    });

    await test.step("Restore full screen through browser Back and subview closing", async () => {
      await page.goBack();
      await expect(page).toHaveURL(fullScreenURL);
      const info = conversation.getByRole("link", { name: "Task info" });
      await info.click();
      await expect(page).toHaveURL(`${taskURL}/info`);
      await expect(pane).toBeVisible();
      await page.goBack();
      await expect(page).toHaveURL(fullScreenURL);
      await expect(info).toBeFocused();
      await page.goForward();
      await expect(page).toHaveURL(`${taskURL}/info`);
      if (width <= 768) {
        const expandSubview = pane.getByRole("button", { name: "Full screen", exact: true });
        await expect(expandSubview).toBeFocused();
        await page.mouse.move(0, 843);
        await expect(page.getByRole("tooltip")).toBeHidden();
        await expandSubview.press("Escape");
      } else await pane.getByRole("button", { name: "Close view" }).click();
      await expect(page).toHaveURL(fullScreenConversation);
      await expect(exit).toBeVisible();
      await expect(summary).toBeHidden();
      await expect(prompt).toHaveText("Keep this draft");
    });

    await test.step("Return from a nested subview after toggling its full screen and reloading", async () => {
      await header.getByRole("link", { name: "Task performance" }).click();
      await pane
        .getByTestId("turn-strip")
        .getByRole("link", { name: /^Turn 1,/ })
        .click();
      await expect(page).toHaveURL(/\/perf\/turns\/1$/);
      await pane.getByRole("button", { name: "Full screen", exact: true }).click();
      await page.reload();
      await expect(pane.getByRole("button", { name: "Exit full screen", exact: true })).toBeVisible();
      await page.keyboard.press("Escape");
      await expect(page).toHaveURL(/\/perf\/turns\/1$/);
      if (width <= 768) await page.keyboard.press("Escape");
      else await pane.getByRole("button", { name: "Close view" }).click();
      await expect(page).toHaveURL(fullScreenConversation);
    });

    await test.step("Reload the full-screen conversation and exit with the header control", async () => {
      const conversationPath = new URL(page.url()).pathname;
      await page.reload();
      await expect(exit).toBeVisible();
      await expect(newTask).toBeHidden();
      await exit.click();
      await expect(page).toHaveURL(conversationPath);
      await expect(enter).toBeFocused();
    });
  });

  for (const subview of ["diff", "info", "perf", "processes", "vnc"]) {
    test(`${subview} preserves the task workspace at ${width}px`, async ({ page, api, uniquePrompt }) => {
      await page.setViewportSize({ width, height: 844 });
      const errors: string[] = [];
      page.on("pageerror", (error) => errors.push(error.message));
      const id = await createTaskAPI(api, uniquePrompt("Check task subview navigation"));
      const task = await waitForTaskState(api, id, "waiting");
      if (subview === "vnc") {
        await page.route(`**/api/caic/v1/tasks/${id}`, (route) =>
          route.fulfill({ json: { ...task, runtime: { ...task.runtime, vncPort: 5900 } } }),
        );
      }
      let streams = 0;
      let historyReads = 0;
      page.on("request", (request) => {
        if (/\/tasks\/[^/]+\/history\/events\?/.test(request.url())) streams++;
        if (/\/history\/(?:bootstrap$|.*\/chunks\/)/.test(request.url())) historyReads++;
      });
      await page.goto(`/task/@${id}`);
      const conversation = page.getByTestId("conversation-pane");
      const prompt = page.getByTestId("task-detail-prompt");
      await expect(prompt).toBeVisible();
      await expect(page.getByTestId("task-message-area")).toHaveAttribute("aria-busy", "false");
      await prompt.fill("Keep this draft");
      const taskURL = page.url();
      const taskNode = await conversation.elementHandle();
      const initialStreams = streams;
      const initialReads = historyReads;
      const link = conversation.locator(`a[href$="/${subview}"]`).first();
      await link.click();
      await expect(page).toHaveURL(new RegExp(`/${subview}$`));
      const subviewURL = page.url();
      expect(await taskNode?.evaluate((node) => node.isConnected)).toBe(true);
      const pane = page.getByTestId("task-view-pane");
      const mobile = width <= 768;
      await expect(pane).toBeVisible();
      if (subview === "perf") {
        await expect(pane.getByText("Performance", { exact: true })).toBeVisible();
        await expect(pane.getByRole("heading", { name: "Resources" })).toBeVisible();
      }
      if (mobile) await expect(conversation).toBeHidden();
      else {
        await expect(conversation).toBeVisible();
        const taskBox = await conversation.boundingBox();
        const paneBox = await pane.boundingBox();
        expect(paneBox!.x).toBeGreaterThanOrEqual(taskBox!.x + taskBox!.width);
        await expect(conversation.locator('a[href$="/perf"]')).toHaveAttribute(
          "href",
          `${new URL(taskURL).pathname}/perf`,
        );
      }
      const fullScreen = pane.getByRole("button", { name: "Full screen", exact: true });
      const closeView = pane.getByRole("button", { name: "Close view", exact: true });
      await expect(fullScreen).toBeVisible();
      const paneNode = await pane.locator('[data-testid="detail-pane"] > div').first().elementHandle();
      await fullScreen.click();
      await expect(page).toHaveURL(/fullscreen=1/);
      await expect(conversation).toBeHidden();
      await expect(page.getByTestId("summary-pane")).toBeHidden();
      await expect(page.getByTestId("new-task-button")).toBeHidden();
      await expect(page.getByRole("button", { name: "Exit full screen", exact: true })).toBeFocused();
      await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
      if (subview === "vnc") {
        const canvas = pane.locator("canvas");
        await expect
          .poll(() =>
            canvas.evaluate((element: HTMLCanvasElement) => {
              const context = element.getContext("2d");
              if (!context || element.width === 0 || element.height === 0) return false;
              return context
                .getImageData(0, 0, element.width, element.height)
                .data.some((value, index) => index % 4 !== 3 && value !== 0);
            }),
          )
          .toBe(true);
        await canvas.click({ position: { x: 10, y: 10 } });
        await page.keyboard.press("Escape");
        await expect(page).toHaveURL(subviewURL);
        await expect(page.getByRole("button", { name: "Full screen", exact: true })).toBeFocused();
        await page.goBack();
        await expect(page).toHaveURL(/fullscreen=1/);
      }
      await page.goBack();
      await expect(page).toHaveURL(subviewURL);
      expect(await paneNode?.evaluate((node) => node.isConnected)).toBe(true);

      // Escape closes the mobile subview and leaves it in browser history.
      if (mobile) await page.keyboard.press("Escape");
      else await closeView.click();
      await expect(page).toHaveURL(new RegExp(`/task/@${id}(?:\\+[^/]+)?$`));
      await expect(prompt).toHaveText("Keep this draft");
      await expect(link).toBeFocused();
      await page.goBack();
      await expect(page).toHaveURL(subviewURL);
      await expect(fullScreen).toBeVisible();
      if (mobile) await expect(closeView).toBeHidden();
      else await expect(closeView).toBeVisible();
      await page.goBack();
      await expect(page).toHaveURL(taskURL);
      expect(await taskNode?.evaluate((node) => node.isConnected)).toBe(true);
      // Subviews reuse the conversation owner, including its decoded history.
      expect(streams).toBe(initialStreams);
      expect(historyReads).toBe(initialReads);
      if (subview === "perf" && width === 1280) {
        await link.click();
        await page.getByRole("button", { name: "Full screen", exact: true }).click();
        await page.reload();
        await expect(page).toHaveURL(/\/perf\?fullscreen=1$/);
        await expect(page.getByRole("button", { name: "Exit full screen", exact: true })).toBeFocused();
        await expect(conversation).toBeHidden();
        await page.keyboard.press("Escape");
        await expect(page).toHaveURL(subviewURL);
        await expect(conversation).toBeVisible();
        await page.keyboard.press("Escape");
        await expect(page).toHaveURL(new RegExp(`/task/@${id}(?:\\+[^/]+)?$`));
      }
      expect(errors).toEqual([]);
    });
  }
}

// Browser coverage for persistent task subviews, shared history, and full screen on mobile and desktop.
import { createTaskAPI, expect, test, waitForTaskState } from "../helpers";

for (const width of [390, 1280]) {
  for (const subview of ["diff", "info", "processes", "stats", "vnc"]) {
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
      page.on("request", (request) => {
        if (/\/tasks\/[^/]+\/events\?/.test(request.url())) streams++;
      });
      await page.goto(`/task/@${id}`);
      const conversation = page.getByTestId("conversation-pane");
      const prompt = page.getByTestId("task-detail-prompt");
      await expect(prompt).toBeVisible();
      await prompt.fill("Keep this draft");
      const taskURL = page.url();
      const taskNode = await conversation.elementHandle();
      const initialStreams = streams;
      const link = conversation.locator(`a[href$="/${subview}"]`).first();
      await link.click();
      await expect(page).toHaveURL(new RegExp(`/${subview}$`));
      const subviewURL = page.url();
      expect(await taskNode?.evaluate((node) => node.isConnected)).toBe(true);
      const pane = page.getByTestId("task-view-pane");
      await expect(pane).toBeVisible();
      if (width <= 768) await expect(conversation).toBeHidden();
      else {
        await expect(conversation).toBeVisible();
        const taskBox = await conversation.boundingBox();
        const paneBox = await pane.boundingBox();
        expect(paneBox!.x).toBeGreaterThanOrEqual(taskBox!.x + taskBox!.width);
        await expect(conversation.locator('a[href$="/stats"]')).toHaveAttribute(
          "href",
          `${new URL(taskURL).pathname}/stats`,
        );
      }
      await expect(page.getByRole("button", { name: "Full screen", exact: true })).toBeVisible();
      const paneNode = await pane.locator('[data-testid="detail-pane"] > div').first().elementHandle();
      await page.getByRole("button", { name: "Full screen", exact: true }).click();
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
      await page.screenshot({ path: test.info().outputPath(`${subview}-workspace.png`) });
      await page.getByRole("button", { name: "Close view", exact: true }).click();
      await expect(page).toHaveURL(new RegExp(`/task/@${id}(?:\\+[^/]+)?$`));
      await expect(prompt).toHaveText("Keep this draft");
      await expect(link).toBeFocused();
      await page.goBack();
      await expect(page).toHaveURL(subviewURL);
      await expect(page.getByRole("button", { name: "Close view", exact: true })).toBeVisible();
      await page.goBack();
      await expect(page).toHaveURL(taskURL);
      expect(await taskNode?.evaluate((node) => node.isConnected)).toBe(true);
      expect(streams).toBe(initialStreams);
      if (subview === "stats" && width === 1280) {
        await link.click();
        await page.getByRole("button", { name: "Full screen", exact: true }).click();
        await page.reload();
        await expect(page).toHaveURL(/\/stats\?fullscreen=1$/);
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

// Browser coverage for one pending prompt, attachment restoration, editing, steering, and delivery.
import { test, expect, createTaskAPI, waitForTaskState } from "../helpers";

const QUEUED_PNG = "iVBORw0KGgoAAAANSUhEUgAAAAIAAAABCAIAAAB7QOjdAAAADUlEQVR4nGP4zwAE/wEHAAH/4iOeWQAAAABJRU5ErkJggg==";

for (const mobile of [false, true]) {
  test(`queued prompts survive reload and send after the current turn (${mobile ? "mobile" : "desktop"})`, async ({
    page,
    api,
  }, testInfo) => {
    const pageErrors: string[] = [];
    page.on("pageerror", (error) => pageErrors.push(error.message));
    if (mobile) await page.setViewportSize({ width: 390, height: 844 });
    await page.goto("/");
    await page.getByTestId("prompt-input").fill("Investigate the failing authentication test");
    await expect(page.getByTestId("submit-task")).toBeEnabled();
    await page.screenshot({ path: testInfo.outputPath("new-task.png") });
    const id = await createTaskAPI(api, "FAKE_DEMO_GATED queue journey");
    await waitForTaskState(api, id, "running");
    await api.sendInput(id, {
      prompt: {
        text: "send this after the answer",
        images: [
          { mediaType: "image/png", data: QUEUED_PNG },
          { mediaType: "image/png", data: QUEUED_PNG },
        ],
      },
      queue: true,
    });
    await page.goto(`/task/@${id}`);
    const queue = page.getByRole("region", { name: "Pending prompt" });
    await expect(queue.getByTestId("queued-input")).toHaveCount(1);
    await page.reload();
    await expect(queue.getByTestId("queued-input")).toHaveCount(1);
    const thumbnails = queue.getByRole("img");
    await expect(thumbnails).toHaveCount(2);
    await expect
      .poll(() =>
        thumbnails.evaluateAll((images) =>
          images.every((image) => image instanceof HTMLImageElement && image.complete && image.naturalWidth === 2),
        ),
      )
      .toBe(true);
    await page.screenshot({ path: testInfo.outputPath("queue.png") });
    const prompt = page.getByTestId("task-detail-prompt");
    await prompt.fill("keep this draft");
    await page.getByRole("button", { name: "Send options", exact: true }).click();
    await expect(page.getByRole("menuitem", { name: /Send now/ })).toBeEnabled();
    await page.screenshot({ path: testInfo.outputPath("send-options.png") });
    await page.keyboard.press("Escape");
    await expect(page.getByRole("button", { name: "Send options", exact: true })).toBeFocused();
    await expect(queue.getByRole("button", { name: "Edit pending prompt" })).toBeDisabled();
    await expect(page.getByTestId("send-input")).toBeDisabled();
    await prompt.press("Enter");
    await expect(prompt).toHaveText("keep this draft");
    await prompt.fill("");
    const cancelPath = `**/tasks/${id}/input/cancel`;
    await page.route(cancelPath, (route) =>
      route.fulfill({
        status: 409,
        contentType: "application/json",
        body: JSON.stringify({ error: "Try again", code: "conflict" }),
      }),
    );
    await queue.getByRole("button", { name: "Edit pending prompt" }).click();
    await expect(page.getByText(/Edit failed:/)).toBeVisible();
    await expect(queue).toContainText("send this after the answer");
    await expect(prompt).toHaveText("");
    await page.unroute(cancelPath);
    await queue.getByRole("button", { name: "Edit pending prompt" }).click();
    await expect(queue).toHaveCount(0);
    await expect(prompt).toBeFocused();
    await expect(prompt).toHaveText("send this after the answer");
    const composer = page.getByTestId("task-detail-form");
    await expect(composer.getByRole("img", { name: "attached" })).toHaveCount(2);
    await expect(page.getByText("Let me write a test first, then fix it.")).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath("editing.png") });
    await prompt.fill("edited pending prompt");
    await prompt.press("Enter");
    await expect(queue).toContainText("edited pending prompt");
    await expect(prompt).toHaveText("");
    await expect(queue.getByRole("img")).toHaveCount(2);
    await expect(api.sendInput(id, { prompt: { text: "second pending prompt" }, queue: true })).rejects.toThrow();
    await page.reload();
    await expect(queue).toContainText("edited pending prompt");
    await prompt.fill("answer");
    await prompt.press("Shift+Enter");
    await prompt.pressSequentially("immediately");
    expect(await prompt.innerText()).toBe("answer\nimmediately");
    // The fake agent treats each input line as a separate invocation.
    await prompt.fill("FAKE_DEMO_FINISH");
    const sent = page.waitForRequest(
      (request) => request.method() === "POST" && request.url().endsWith(`/tasks/${id}/input`),
    );
    if (mobile) {
      await page.getByRole("button", { name: "Send options", exact: true }).focus();
      await page.keyboard.press("ArrowDown");
      await expect(page.getByRole("menuitem", { name: /Send now/ })).toBeFocused();
      await page.getByRole("menuitem", { name: /Send now/ }).click();
    } else await prompt.press("Control+Enter");
    expect((await sent).postDataJSON()).toMatchObject({ queue: false });
    await expect(queue).toHaveCount(0);
    await expect.poll(async () => (await api.getTask(id)).numTurns).toBe(3);
    await waitForTaskState(api, id, "waiting");
    const task = await api.getTask(id);
    expect(task.pendingInput).toBeNull();
    expect(pageErrors).toEqual([]);
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  });
}

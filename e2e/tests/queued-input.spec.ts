// Browser coverage for one pending prompt, attachment restoration, editing, steering, and delivery.
import { test, expect, createTaskAPI, waitForTaskState } from "../helpers";

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
    const png = await page.evaluate(() => {
      const canvas = document.createElement("canvas");
      canvas.width = 320;
      canvas.height = 1600;
      const context = canvas.getContext("2d");
      if (!context) throw new Error("2d canvas unavailable");
      const gradient = context.createLinearGradient(0, 0, 320, 1600);
      gradient.addColorStop(0, "#203c61");
      gradient.addColorStop(1, "#84bdc8");
      context.fillStyle = gradient;
      context.fillRect(0, 0, 320, 1600);
      context.fillStyle = "#fff";
      context.font = "32px sans-serif";
      context.fillText("Portrait preview", 40, 800);
      const data = canvas.toDataURL("image/png");
      return data.slice(data.indexOf(",") + 1);
    });
    await api.sendInput(
      id,
      {
        prompt: {
          text: "send this after the answer",
        },
        queue: true,
      },
      {
        images: [0, 1].map(() => new Blob([Buffer.from(png, "base64")], { type: "image/png" })),
        signal: new AbortController().signal,
      },
    );
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
          images.every(
            (image) =>
              image instanceof HTMLImageElement &&
              image.complete &&
              image.naturalWidth === 19 &&
              image.naturalHeight === 96,
          ),
        ),
      )
      .toBe(true);
    await page.screenshot({ path: testInfo.outputPath("queue.png") });
    await queue.getByRole("link", { name: "Open queued attachment 1" }).click();
    await expect(page).toHaveURL(new RegExp(`/task/@${id}/view\\?image=`));
    const full = page.getByRole("img", { name: "attachment", exact: true });
    await expect(full).toBeVisible();
    expect(await full.evaluate((el: HTMLImageElement) => el.naturalWidth)).toBe(320);
    await page.screenshot({ path: testInfo.outputPath("queued-image.png") });
    await page.reload();
    await expect(page.getByRole("img", { name: "attachment", exact: true })).toBeVisible();
    await page.getByRole("button", { name: "Close view" }).click();
    await expect(queue).toBeVisible();
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

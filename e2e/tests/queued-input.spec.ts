// Browser coverage for queued prompt appending, keyboard recall, attachment restoration, steering, and delivery.
import { test, expect, createTaskAPI, waitForTaskState, recentHistoryPage } from "../helpers";
import type { ErrorResponse } from "../../sdk/caic/ts/v1/types.gen";

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
    const composer = page.getByTestId("task-detail-form");
    await prompt.fill("keep this draft");
    await composer
      .locator('input[type="file"]')
      .setInputFiles({ name: "follow-up.png", mimeType: "image/png", buffer: Buffer.from(png, "base64") });
    await expect(composer.getByRole("img", { name: "attached" })).toHaveCount(1);
    await page.getByRole("button", { name: "Send options", exact: true }).click();
    await expect(page.getByRole("menuitem", { name: /Send now/ })).toBeEnabled();
    await page.screenshot({ path: testInfo.outputPath("send-options.png") });
    await page.keyboard.press("Escape");
    await expect(page.getByRole("button", { name: "Send options", exact: true })).toBeFocused();
    await expect(queue.getByRole("button", { name: "Edit pending prompt" })).toBeDisabled();
    await expect(page.getByTestId("send-input")).toBeEnabled();
    await prompt.press("ArrowUp");
    await expect(prompt).toHaveText("keep this draft");
    await expect(queue).toBeVisible();
    await prompt.press("Enter");
    await expect(prompt).toHaveText("");
    const combinedText = "send this after the answer\nkeep this draft";
    await expect.poll(async () => (await api.getTask(id)).pendingInput?.prompt.text).toBe(combinedText);
    await expect(queue.getByRole("img")).toHaveCount(3);
    for (const thumbnail of await queue.getByRole("img").all()) {
      await expect(thumbnail).toHaveJSProperty("naturalHeight", 96);
    }
    await page.screenshot({ path: testInfo.outputPath("appended.png") });
    const cancelPath = `**/tasks/${id}/input/cancel`;
    await page.route(cancelPath, (route) =>
      route.fulfill({
        status: 409,
        contentType: "application/json",
        body: JSON.stringify({ error: { code: "CONFLICT", message: "Try again" } } satisfies ErrorResponse),
      }),
    );
    if (mobile) await queue.getByRole("button", { name: "Edit pending prompt" }).click();
    else await prompt.press("ArrowUp");
    await expect(page.getByText("Edit failed: Try again")).toBeVisible();
    await expect(queue).toContainText("send this after the answer");
    await expect(prompt).toHaveText("");
    await page.unroute(cancelPath);
    if (mobile) await queue.getByRole("button", { name: "Edit pending prompt" }).click();
    else await prompt.press("ArrowUp");
    await expect(queue).toHaveCount(0);
    await expect(prompt).toBeFocused();
    await expect(prompt).toHaveText(combinedText, { useInnerText: true });
    await prompt.pressSequentially(" updated");
    await expect(prompt).toHaveText(`${combinedText} updated`, { useInnerText: true });
    await expect(composer.getByRole("img", { name: "attached" })).toHaveCount(3);
    await expect(page.getByText("Let me write a test first, then fix it.")).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath("editing.png") });
    await prompt.fill("edited pending prompt");
    await prompt.press("Enter");
    await expect(queue).toContainText("edited pending prompt");
    await expect(prompt).toHaveText("");
    await expect(queue.getByRole("img")).toHaveCount(3);
    await api.sendInput(id, { prompt: { text: "second pending prompt" }, queue: true });
    await expect
      .poll(async () => (await api.getTask(id)).pendingInput?.prompt.text)
      .toBe("edited pending prompt\nsecond pending prompt");
    await page.reload();
    await expect(queue).toContainText("edited pending prompt");
    await prompt.fill("answer");
    await prompt.press("Shift+Enter");
    await prompt.pressSequentially("immediately");
    expect(await prompt.innerText()).toBe("answer\nimmediately");
    // Release the current turn so the combined pending prompt can follow it.
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
    // Each whole prompt produces one result; the fake reports cumulative 1 + 2.
    await expect.poll(async () => (await api.getTask(id)).numTurns).toBe(3);
    await waitForTaskState(api, id, "waiting");
    const task = await api.getTask(id);
    expect(task.pendingInput).toBeNull();
    const bootstrap = (await api.getHistoryManifest(id)).bootstrap;
    const recent = recentHistoryPage(bootstrap);
    const events = await api.getHistorySlice(
      id,
      bootstrap.scope,
      bootstrap.projectionVersion,
      bootstrap.generation,
      String(recent.first),
      "0",
      String(recent.last),
      "end",
    );
    const inputs = events.filter((event) => event.message.kind === "userInput").map((event) => event.message.userInput);
    expect(inputs.map((input) => input?.text)).toEqual([
      "FAKE_DEMO_GATED queue journey",
      "FAKE_DEMO_FINISH",
      "edited pending prompt\nsecond pending prompt",
    ]);
    expect(inputs.at(-1)?.images).toHaveLength(3);
    expect(
      events.filter((event) => event.message.kind === "result").map((event) => event.message.result?.numTurns),
    ).toEqual([1, 2]);
    expect(pageErrors).toEqual([]);
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  });
}

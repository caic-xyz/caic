// Browser regressions for long prompts staying within the task form and conversation viewport.

import { test, expect, fillContentEditable, createTaskAPI, waitForTaskState } from "../helpers";

for (const width of [390, 1280]) {
  test(`long recorded prompt stays in the conversation viewport at ${width}px`, async ({ page, api, uniquePrompt }) => {
    await page.setViewportSize({ width, height: 844 });
    const prompt = uniquePrompt("Long prompt") + "\n\nDescribe the change and its expected behavior.".repeat(250);
    const id = await createTaskAPI(api, prompt);
    await waitForTaskState(api, id, "waiting");
    const eventsURL = `**/api/caic/v1/tasks/${id}/events*`;
    // Keep the recorded prompt visible before conversation history arrives.
    await page.route(eventsURL, (route) =>
      route.fulfill({ contentType: "text/event-stream", body: "event: ready\ndata: {}\n\n" }),
    );
    await page.goto(`/task/@${id}`);
    await expect(page.getByRole("heading", { name: "Prompt", exact: true })).toBeVisible();

    for (const phase of ["recorded", "replayed"]) {
      const messages = page.getByTestId("task-message-area");
      await expect.poll(() => messages.evaluate((el) => el.clientHeight)).toBeGreaterThan(150);
      const input = page.getByTestId("task-detail-prompt");
      await expect(input).toBeInViewport();
      expect(await input.evaluate((el) => el.getBoundingClientRect().bottom)).toBeLessThanOrEqual(844);
      expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
      expect(await messages.evaluate((el) => el.scrollHeight)).toBeGreaterThan(
        await messages.evaluate((el) => el.clientHeight),
      );
      await page.screenshot({ path: test.info().outputPath(`long-prompt-${phase}.png`) });
      if (phase === "recorded") {
        await page.unroute(eventsURL);
        await page.reload();
        await expect(page.getByRole("heading", { name: "Prompt", exact: true })).toHaveCount(0);
      }
    }
  });
}

test("long new-task prompt keeps the task list below the editor", async ({ page }) => {
  await page.setViewportSize({ width: 769, height: 800 });
  await page.route("**/api/caic/v1/server/repos", async (route) => {
    const response = await route.fetch();
    const repos = (await response.json()) as { path: string }[];
    const extra = Array.from({ length: 7 }, (_, i) => ({ ...repos[0], path: `/repos/project-${i}` }));
    await route.fulfill({ response, json: [...repos, ...extra] });
  });
  await page.goto("/");
  await expect(page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first()).toBeVisible();

  for (let i = 0; i < 7; i++) {
    await page.getByTestId("add-repo-button").click();
    await page
      .getByRole("listbox", { name: "Manage repositories" })
      .getByRole("option", { name: `project-${i}` })
      .click();
  }

  const prompt = page.getByTestId("prompt-input");
  await fillContentEditable(prompt, "Describe the change and its expected behavior. ".repeat(35));
  await expect(prompt).toContainText("expected behavior");

  const form = page.getByTestId("new-task-form");
  const list = page.getByTestId("task-list");
  const formBottom = await form.evaluate((el) => el.getBoundingClientRect().bottom);
  const promptBottom = await prompt.evaluate((el) => el.getBoundingClientRect().bottom);
  const listTop = await list.evaluate((el) => el.getBoundingClientRect().top);
  expect(promptBottom).toBeLessThanOrEqual(formBottom);
  expect(formBottom).toBeLessThan(listTop);
});

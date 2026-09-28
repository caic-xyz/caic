// Browser regression for long new-task prompts staying above the task list.

import { test, expect, fillContentEditable } from "../helpers";

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

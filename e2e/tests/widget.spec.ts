// E2E tests for generative UI widget rendering.
import { test, expect, fillContentEditable } from "../helpers";

test("FAKE_WIDGET renders a widget card with iframe", async ({ page, uniquePrompt }) => {
  await page.goto("/");

  // Wait for repos to load.
  await expect(page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first()).toBeVisible();

  const prompt = uniquePrompt("FAKE_WIDGET");

  // Create task.
  await fillContentEditable(page.getByTestId("prompt-input"), prompt);
  await page.getByTestId("submit-task").click();
  await expect(page).toHaveURL(/\/task\//);

  const messages = page.getByTestId("task-message-area");

  // The widget card should appear with the title.
  await expect(messages.getByText("light_refraction_in_water", { exact: true })).toBeVisible({ timeout: 15_000 });

  // A sandboxed iframe should be present (the widget renderer).
  const iframe = messages.locator("iframe[title='light_refraction_in_water']");
  await expect(iframe).toBeVisible({ timeout: 10_000 });

  // The completion checkmark should appear once the widget finishes.
  await expect(messages.getByText("\u2713", { exact: true })).toBeVisible({ timeout: 10_000 });

  // Wait for the result message.
  await expect(messages.locator("strong", { hasText: "Done" })).toBeVisible({
    timeout: 10_000,
  });
});

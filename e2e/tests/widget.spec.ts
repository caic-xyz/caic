// E2E tests for generative UI widget rendering.
import { test, expect } from "../helpers";

test("FAKE_WIDGET renders a widget card with iframe", async ({ page }) => {
  await page.goto("/");

  // Wait for repos to load.
  await expect(page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first()).toBeVisible();

  const prompt = `FAKE_WIDGET ${Date.now()}`;

  // Create task.
  await page.getByTestId("prompt-input").fill(prompt);
  await page.getByTestId("submit-task").click();

  // Open task detail.
  await page.getByText(prompt).first().click();

  // The widget card should appear with the title.
  await expect(page.getByText("light_refraction_in_water").first()).toBeVisible({ timeout: 15_000 });

  // A sandboxed iframe should be present (the widget renderer).
  const iframe = page.locator("iframe[title='light_refraction_in_water']");
  await expect(iframe).toBeVisible({ timeout: 10_000 });

  // The completion checkmark should appear once the widget finishes.
  await expect(page.getByText("\u2713").first()).toBeVisible({ timeout: 10_000 });

  // Wait for the result message.
  await expect(page.locator("strong", { hasText: "Done" })).toBeVisible({
    timeout: 10_000,
  });
});

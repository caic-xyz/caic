// E2E tests for generative UI widget rendering.
import { test, expect } from "../helpers";

test("FAKE_WIDGET renders a widget card with iframe", async ({ page, uniquePrompt }) => {
  await page.emulateMedia({ colorScheme: "light" });
  await page.goto("/");

  // Wait for repos to load.
  await expect(page.getByTestId("repo-chips").locator("[data-testid^='chip-label-']").first()).toBeVisible();

  const prompt = uniquePrompt("FAKE_WIDGET");

  // Create task.
  await page.getByTestId("prompt-input").fill(prompt);
  await page.getByTestId("submit-task").click();
  await expect(page).toHaveURL(/\/task\//);

  const messages = page.getByTestId("task-message-area");

  // The widget card should appear with the title.
  await expect(messages.getByText("light_refraction_in_water", { exact: true })).toBeVisible({
    timeout: 15_000,
  });

  // A sandboxed iframe should be present (the widget renderer).
  const iframe = messages.locator("iframe[title='light_refraction_in_water']");
  await expect(iframe).toBeVisible({ timeout: 10_000 });

  // The completion checkmark should appear once the widget finishes.
  await expect(messages.getByText("\u2713", { exact: true })).toBeVisible({ timeout: 10_000 });

  // Wait for the result message.
  await expect(messages.locator("strong", { hasText: "Done" })).toBeVisible({
    timeout: 10_000,
  });

  const widgetFrame = page.frameLocator("iframe[title='light_refraction_in_water']");
  const widgetBody = widgetFrame.locator("body");
  const heading = widgetFrame.getByRole("heading", { name: "Light Refraction in Water" });
  const slider = widgetFrame.getByRole("slider");
  await slider.press("ArrowRight");
  await expect(slider).toHaveValue("41");

  // The same widget responds to the preference change without losing user input.
  const lightCanvas = await page.locator("body").evaluate((el) => getComputedStyle(el).backgroundColor);
  const lightWidgetCanvas = await widgetBody.evaluate((el) => getComputedStyle(el).backgroundColor);
  const lightWidgetHeading = await heading.evaluate((el) => getComputedStyle(el).color);
  await page.emulateMedia({ colorScheme: "dark" });
  await expect(page.locator("body")).not.toHaveCSS("background-color", lightCanvas);
  await expect(widgetBody).not.toHaveCSS("background-color", lightWidgetCanvas);
  await expect(heading).not.toHaveCSS("color", lightWidgetHeading);
  await expect(slider).toHaveValue("41");
  await page.emulateMedia({ colorScheme: "light" });
  await expect(page.locator("body")).toHaveCSS("background-color", lightCanvas);
  await expect(widgetBody).toHaveCSS("background-color", lightWidgetCanvas);
  await expect(heading).toHaveCSS("color", lightWidgetHeading);

  // Observe both messages after the app's listener has processed them, in
  // sender order, so the assertion cannot race the sandbox's resize events.
  await page.evaluate(() => {
    const state = window as Window & { widgetHeights?: Promise<string[]> };
    state.widgetHeights = new Promise<string[]>((resolve) => {
      const observed: string[] = [];
      const frame = document.querySelector<HTMLIFrameElement>("iframe[title='light_refraction_in_water']");
      if (!frame) throw new Error("Missing widget frame");
      function onMessage(event: MessageEvent<unknown>) {
        if (event.source !== frame?.contentWindow || typeof event.data !== "object" || event.data === null) return;
        if (!("testResize" in event.data)) return;
        observed.push(frame.style.getPropertyValue("--iframe-height"));
        if (observed.length === 2) {
          window.removeEventListener("message", onMessage);
          resolve(observed);
        }
      }
      window.addEventListener("message", onMessage);
    });
  });
  await widgetBody.evaluate(() => {
    window.parent.postMessage({ type: "resize", height: 123, testResize: true }, "*");
    window.parent.postMessage({ type: "resize", height: "bad", testResize: true }, "*");
  });
  const [validHeight, invalidHeight] = await page.evaluate(() => {
    const state = window as Window & { widgetHeights?: Promise<string[]> };
    if (!state.widgetHeights) throw new Error("Missing widget listener");
    return state.widgetHeights;
  });
  expect(validHeight).toBe("143px");
  expect(invalidHeight).toBe(validHeight);
});

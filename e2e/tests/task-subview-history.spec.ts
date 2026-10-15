// Browser coverage for history-backed task subview arrows on mobile and desktop.
import { createTaskAPI, expect, test, waitForTaskState } from "../helpers";

for (const width of [390, 1280]) {
  for (const subview of ["diff", "info", "processes", "stats", "vnc"]) {
    test(`${subview} back arrow traverses history at ${width}px`, async ({ page, api, uniquePrompt }) => {
      await page.setViewportSize({ width, height: 844 });
      const id = await createTaskAPI(api, uniquePrompt("Check task subview navigation"));
      await waitForTaskState(api, id, "waiting");
      await page.goto(`/task/@${id}`);
      await expect(page.getByTestId("task-detail-prompt")).toBeVisible();
      const taskURL = page.url();
      await page.goto(`${taskURL}/${subview}`);
      await page.getByRole("button", { name: "Back", exact: true }).click();
      await expect(page).toHaveURL(taskURL);
      await expect(page.getByTestId("task-detail-prompt")).toBeVisible();
      await page.goForward();
      await expect(page).toHaveURL(`${taskURL}/${subview}`);
      await expect(page.getByRole("button", { name: "Back", exact: true })).toBeVisible();
      await page.goBack();
      await expect(page).toHaveURL(taskURL);
    });
  }
}

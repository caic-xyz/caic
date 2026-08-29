// E2E tests for mobile fork-task navigation and layout.
import {
  test,
  expect,
  createTaskAPI,
  waitForTaskState,
  fillContentEditable,
} from "../helpers";

test("forking from mobile opens only the forked task detail", async ({ page, api, uniquePrompt }, testInfo) => {
  await page.setViewportSize({ width: 390, height: 844 });

  const sourcePrompt = uniquePrompt("e2e fork mobile source");
  const sourceId = await createTaskAPI(api, sourcePrompt);
  await waitForTaskState(api, sourceId, "waiting", 30_000);

  await page.goto(`/task/@${sourceId}`);
  await expect(page.getByTestId("detail-pane")).toBeVisible();
  await expect(page.getByTestId("task-list")).toBeHidden();

  await page.getByRole("button", { name: "Context actions" }).click();
  await page.getByRole("button", { name: "Fork" }).click();

  await expect(page.getByTestId("fork-dialog")).toBeVisible();
  const forkPrompt = uniquePrompt("e2e fork mobile child");
  await fillContentEditable(page.getByTestId("fork-prompt-input"), forkPrompt);
  await page.getByTestId("fork-submit").click();

  let forkedId = "";
  await expect(async () => {
    const tasks = await api.listTasks();
    const forked = tasks.find((task) => task.initialPrompt === forkPrompt);
    expect(forked).toBeTruthy();
    forkedId = forked!.id;
  }).toPass({ timeout: 10_000, intervals: [500] });

  const screenshotPath = testInfo.outputPath("fork-mobile-layout.png");
  await page.screenshot({ path: screenshotPath, fullPage: true });
  await testInfo.attach("fork-mobile-layout", {
    path: screenshotPath,
    contentType: "image/png",
  });

  await expect(page).toHaveURL(new RegExp(`/task/@${forkedId}`));
  await expect(page.getByTestId("detail-pane")).toBeVisible();
  await expect(page.getByTestId("task-list")).toBeHidden();
});

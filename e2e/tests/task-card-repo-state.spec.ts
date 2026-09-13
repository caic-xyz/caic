// E2E tests task-card repository-state summaries for single and mapped repositories.

import { expect, test, waitForTaskState } from "../helpers";

test("task cards keep single- and multi-repository states coherent", async ({ page, api, uniquePrompt }, testInfo) => {
  const repos = await api.listRepos();
  const harnesses = await api.listHarnesses();
  expect(repos.length).toBeGreaterThanOrEqual(2);
  expect(harnesses.length).toBeGreaterThan(0);

  const singleTask = await api.createTask({
    initialPrompt: { text: uniquePrompt("single repository state") },
    repos: [{ name: repos[0].path }],
    harness: harnesses[0].name,
  });
  const multiTask = await api.createTask({
    initialPrompt: { text: uniquePrompt("mapped repository state") },
    repos: [{ name: repos[0].path }, { name: repos[1].path }],
    harness: harnesses[0].name,
  });
  await waitForTaskState(api, singleTask.id, "waiting");
  await waitForTaskState(api, multiTask.id, "waiting");
  const singleRepoStatus = await api.getTaskRepoStatus(singleTask.id);
  const multiRepoStatus = await api.getTaskRepoStatus(multiTask.id);

  await page.goto(`/task/@${multiTask.id}`);

  const singleCard = page.locator(`[data-task-id="${singleTask.id}"]`);
  const multiCard = page.locator(`[data-task-id="${multiTask.id}"]`);
  await expect(singleCard.getByRole("img", { name: "2 changed files, 12 additions, 2 deletions, 1 uncommitted file, 1 commit ahead of upstream" })).toBeVisible();
  await expect(multiCard.getByRole("img", { name: "2 changed files, 12 additions, 2 deletions, 1 uncommitted file, 1 commit ahead of upstream" })).toBeVisible();
  await expect(multiCard.getByRole("img", { name: "2 changed files, 9 additions, 5 deletions, 2 uncommitted files, 1 commit behind upstream" })).toBeVisible();
  await expect(multiCard).toContainText(repos[0].path);
  await expect(multiCard).toContainText(repos[1].path);
  await expect(singleCard.getByTestId("task-card-repo-state")).toHaveCount(1);
  await expect(multiCard.getByTestId("task-card-repo-state")).toHaveCount(2);

  const multiStateRows = multiCard.getByTestId("task-card-repo-state");
  await expect(singleCard.getByTestId("task-card-repo-state")).toContainText(singleRepoStatus.repositories[0].branch);
  await expect(multiStateRows.nth(0)).toContainText(`${multiTask.repos![0].name} · ${multiRepoStatus.repositories[0].branch}`);
  await expect(multiStateRows.nth(1)).toContainText(`${multiTask.repos![1].name} · ${multiRepoStatus.repositories[1].branch}`);
  await expect.poll(async () => {
    const row = await singleCard.getByTestId("task-card-repo-state").boundingBox();
    const stats = await singleCard.getByTestId("task-card-repo-state").getByRole("img").boundingBox();
    if (!row || !stats) return Number.POSITIVE_INFINITY;
    return Math.abs(row.x + row.width - stats.x - stats.width);
  }).toBeLessThan(1);
  await expect.poll(async () => {
    const badge = await singleCard.getByTestId("state-badge").boundingBox();
    const stats = await singleCard.getByTestId("task-card-repo-state").getByRole("img").boundingBox();
    if (!badge || !stats) return Number.POSITIVE_INFINITY;
    return Math.abs(badge.x + badge.width - stats.x - stats.width);
  }).toBeLessThan(1);
  await expect.poll(async () => {
    const first = await multiStateRows.nth(0).getByRole("img").boundingBox();
    const second = await multiStateRows.nth(1).getByRole("img").boundingBox();
    if (!first || !second) return Number.POSITIVE_INFINITY;
    return Math.abs(first.x + first.width - second.x - second.width);
  }).toBeLessThan(1);

  await expect.poll(async () => Promise.all([singleCard, multiCard].map((card) =>
    card.evaluate((el) => el.scrollWidth <= el.clientWidth),
  ))).toEqual([true, true]);

  await page.getByTestId("task-list").screenshot({
    path: testInfo.outputPath("task-card-repository-state.png"),
  });
});

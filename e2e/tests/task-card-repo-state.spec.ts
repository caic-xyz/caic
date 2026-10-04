// E2E tests task-card repository-state summaries for single and mapped repositories.

import { expect, test, waitForTaskState } from "../helpers";

test("task-card cost tooltip stays open after the first touch tap", async ({ browser, baseURL, api, uniquePrompt }) => {
  const repos = await api.listRepos();
  const task = await api.createTask({
    initialPrompt: { text: uniquePrompt("FAKE_DEMO touch tooltip") },
    repos: [{ name: repos[0].path }],
    harness: "claude",
  });
  await waitForTaskState(api, task.id, "waiting");
  // Keep the sidebar visible while exercising Chromium's real touch sequence.
  const context = await browser.newContext({ baseURL, hasTouch: true, viewport: { width: 1280, height: 900 } });
  try {
    const page = await context.newPage();
    await page.goto(`/task/@${task.id}`);
    const card = page.locator(`[data-task-id="${task.id}"]`);
    // Card activation normalizes a bare task URL. Finish that navigation before
    // opening the tooltip, which correctly dismisses on navigation or scrolling.
    await card.press("Enter");
    await expect(page).toHaveURL(new RegExp(`/task/@${task.id}\\+[^/]+$`));
    const cost = card.getByTestId("task-card-cost").getByRole("button");
    await cost.tap();
    await expect(page.getByText(/API-equivalent cost/)).toBeVisible();
    await cost.tap();
    await expect(page.getByText(/API-equivalent cost/)).toHaveCount(0);
  } finally {
    await context.close();
  }
});

test("task cards keep single- and multi-repository states coherent", async ({ page, api, uniquePrompt }, testInfo) => {
  const repos = await api.listRepos();
  const harnesses = await api.listHarnesses();
  expect(repos.length).toBeGreaterThanOrEqual(2);
  expect(harnesses.length).toBeGreaterThan(0);

  // FAKE_DEMO makes the fake agent run a tool-using turn, which drives the
  // backend's post-tool repository-state probe the cards render.
  const singleTask = await api.createTask({
    initialPrompt: { text: uniquePrompt("FAKE_DEMO single repository state") },
    repos: [{ name: repos[0].path }],
    harness: harnesses[0].name,
  });
  const multiTask = await api.createTask({
    initialPrompt: { text: uniquePrompt("FAKE_DEMO mapped repository state") },
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
  await expect(
    singleCard.getByRole("img", {
      name: "2 changed files, 12 additions, 2 deletions, 1 uncommitted file, 1 commit ahead of upstream",
    }),
  ).toBeVisible();
  await expect(
    multiCard.getByRole("img", {
      name: "2 changed files, 12 additions, 2 deletions, 1 uncommitted file, 1 commit ahead of upstream",
    }),
  ).toBeVisible();
  await expect(
    multiCard.getByRole("img", {
      name: "2 changed files, 9 additions, 5 deletions, 2 uncommitted files, 1 commit behind upstream",
    }),
  ).toBeVisible();
  await expect(multiCard).toContainText(repos[0].path);
  await expect(multiCard).toContainText(repos[1].path);
  await expect(singleCard.getByTestId("task-card-repo-state")).toHaveCount(1);
  await expect(multiCard.getByTestId("task-card-repo-state")).toHaveCount(2);

  const multiStateRows = multiCard.getByTestId("task-card-repo-state");
  await expect(singleCard.getByTestId("task-card-repo-state")).toContainText(singleRepoStatus.repositories[0].branch);
  await expect(multiStateRows.nth(0)).toContainText(
    `${multiTask.repos![0].name} · ${multiRepoStatus.repositories[0].branch}`,
  );
  await expect(multiStateRows.nth(1)).toContainText(
    `${multiTask.repos![1].name} · ${multiRepoStatus.repositories[1].branch}`,
  );
  await expect
    .poll(async () => {
      const row = await singleCard.getByTestId("task-card-repo-state").boundingBox();
      const stats = await singleCard.getByTestId("task-card-repo-state").getByRole("img").boundingBox();
      if (!row || !stats) return Number.POSITIVE_INFINITY;
      return Math.abs(row.x + row.width - stats.x - stats.width);
    })
    .toBeLessThan(1);
  await expect
    .poll(async () => {
      const badge = await singleCard.getByTestId("state-badge").boundingBox();
      const stats = await singleCard.getByTestId("task-card-repo-state").getByRole("img").boundingBox();
      if (!badge || !stats) return Number.POSITIVE_INFINITY;
      return Math.abs(badge.x + badge.width - stats.x - stats.width);
    })
    .toBeLessThan(1);
  await expect
    .poll(async () => {
      const first = await multiStateRows.nth(0).getByRole("img").boundingBox();
      const second = await multiStateRows.nth(1).getByRole("img").boundingBox();
      if (!first || !second) return Number.POSITIVE_INFINITY;
      return Math.abs(first.x + first.width - second.x - second.width);
    })
    .toBeLessThan(1);

  await expect
    .poll(async () =>
      Promise.all([singleCard, multiCard].map((card) => card.evaluate((el) => el.scrollWidth <= el.clientWidth))),
    )
    .toEqual([true, true]);

  for (const width of [1280, 390]) {
    await page.setViewportSize({ width, height: 900 });
    // Return to the list on mobile, where task detail replaces the sidebar.
    await page.goto("/");
    await expect(multiStateRows.nth(1)).toBeVisible();
    const cost = await multiCard.getByTestId("task-card-cost").boundingBox();
    const badge = await multiCard.getByTestId("state-badge").boundingBox();
    expect(Math.abs(cost!.y + cost!.height / 2 - badge!.y - badge!.height / 2)).toBeLessThan(1);
    const rhythm = await multiCard.evaluate((card) => {
      const rows = Array.from(card.querySelectorAll('[data-testid="task-card-repo-state"]'));
      const group = card.querySelector('[data-testid="task-card-repo-states"]')!.parentElement!;
      const metadata = group.previousElementSibling!;
      const first = rows[0].getBoundingClientRect();
      const second = rows[1].getBoundingClientRect();
      return {
        heights: rows.map((row) => row.getBoundingClientRect().height),
        textSizes: [
          metadata.firstElementChild!,
          card.querySelector('[data-testid="task-card-cost"]')!,
          card.querySelector('[data-testid="task-card-cost"]')!.previousElementSibling!,
          ...rows.map((row) => row.firstElementChild!),
        ].map((el) => getComputedStyle(el).fontSize),
        lineHeight: Number.parseFloat(getComputedStyle(metadata).lineHeight),
        groupGap: first.top - metadata.getBoundingClientRect().bottom,
        rowGap: second.top - first.bottom,
        contained: card.scrollWidth <= card.clientWidth,
      };
    });
    expect(new Set(rhythm.textSizes).size).toBe(1);
    expect(Math.abs(rhythm.heights[0] - rhythm.lineHeight)).toBeLessThan(1);
    expect(Math.abs(rhythm.heights[1] - rhythm.heights[0])).toBeLessThan(1);
    expect(Math.abs(rhythm.groupGap - rhythm.rowGap)).toBeLessThan(1);
    expect(rhythm.rowGap).toBeGreaterThan(0);
    expect(rhythm.rowGap).toBeLessThanOrEqual(3);
    expect(rhythm.contained).toBe(true);
    await page.getByTestId("task-list").screenshot({
      path: testInfo.outputPath(`task-card-repository-state-${width}.png`),
    });
  }

  // Purged tasks keep repository labels but no runtime Git markers.
  await api.purgeTask(multiTask.id);
  await waitForTaskState(api, multiTask.id, "purged");
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(`/task/@${multiTask.id}`);
  await expect(multiStateRows.nth(1)).toBeVisible();
  await expect(multiStateRows.nth(1).getByRole("img")).toHaveCount(0);
  const emptyRowHeight = (await multiStateRows.nth(1).boundingBox())!.height;
  const summaryRowHeight = (await singleCard.getByTestId("task-card-repo-state").boundingBox())!.height;
  expect(Math.abs(emptyRowHeight - summaryRowHeight)).toBeLessThan(1);
});

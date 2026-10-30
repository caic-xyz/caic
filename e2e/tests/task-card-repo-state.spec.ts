// Browser coverage for task-card tooltips, repository layout, and live diff refreshes.

import { ok } from "node:assert/strict";
import { createTaskAPI, expect, test, waitForTaskState } from "../helpers";
import type { TaskListEvent } from "../../sdk/caic/ts/v1/types.gen";

test("task-card cost tooltip stays open after the first touch tap", async ({ browser, baseURL, api, uniquePrompt }) => {
  const repos = await api.listRepos();
  const firstRepo = repos[0];
  ok(firstRepo);
  const secondRepo = repos[1];
  ok(secondRepo);
  const task = await api.createTask({
    initialPrompt: { text: uniquePrompt("FAKE_DEMO touch tooltip") },
    repos: [{ name: firstRepo.path }],
    harness: "claude",
  });
  await waitForTaskState(api, task.id, "waiting");
  // Keep the sidebar visible while exercising Chromium's real touch sequence.
  const context = await browser.newContext({ baseURL, hasTouch: true, viewport: { width: 1280, height: 900 } });
  try {
    const page = await context.newPage();
    // Keep other workers' task insertions from moving the touch target between
    // Playwright's hit testing and dispatch, which can retry and toggle twice.
    const snapshot = { kind: "snapshot", snapshot: [await api.getTask(task.id)] } satisfies TaskListEvent;
    await page.route(
      (url) => url.pathname === "/api/caic/v1/tasks/events",
      (route) => route.fulfill({ contentType: "text/event-stream", body: `data: ${JSON.stringify(snapshot)}\n\n` }),
    );
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

test("task cards keep single- and multi-repository states coherent", async ({ page, api, uniquePrompt }) => {
  const repos = await api.listRepos();
  const firstRepo = repos[0];
  ok(firstRepo);
  const secondRepo = repos[1];
  ok(secondRepo);
  const harnesses = await api.listHarnesses();
  const harness = harnesses[0];
  ok(harness);
  expect(repos.length).toBeGreaterThanOrEqual(2);
  expect(harnesses.length).toBeGreaterThan(0);

  // FAKE_DEMO makes the fake agent run a tool-using turn, which drives the
  // backend's post-tool repository-state probe the cards render.
  const singleTask = await api.createTask({
    initialPrompt: { text: uniquePrompt("FAKE_DEMO single repository state") },
    repos: [{ name: firstRepo.path }],
    harness: harness.name,
  });
  const multiTask = await api.createTask({
    initialPrompt: { text: uniquePrompt("FAKE_DEMO mapped repository state") },
    repos: [{ name: firstRepo.path }, { name: secondRepo.path }],
    harness: harness.name,
  });
  await waitForTaskState(api, singleTask.id, "waiting");
  await waitForTaskState(api, multiTask.id, "waiting");
  const singleRepoStatus = await api.getTaskRepoStatus(singleTask.id);
  const multiRepoStatus = await api.getTaskRepoStatus(multiTask.id);
  const singleStatus = singleRepoStatus.repositories[0];
  const firstStatus = multiRepoStatus.repositories[0];
  const secondStatus = multiRepoStatus.repositories[1];
  const firstTaskRepo = multiTask.repos?.[0];
  const secondTaskRepo = multiTask.repos?.[1];
  ok(singleStatus);
  ok(firstStatus);
  ok(secondStatus);
  ok(firstTaskRepo);
  ok(secondTaskRepo);

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
  await expect(multiCard).toContainText(firstRepo.path);
  await expect(multiCard).toContainText(secondRepo.path);
  await expect(singleCard.getByTestId("task-card-repo-state")).toHaveCount(1);
  await expect(multiCard.getByTestId("task-card-repo-state")).toHaveCount(2);

  const multiStateRows = multiCard.getByTestId("task-card-repo-state");
  await expect(singleCard.getByTestId("task-card-repo-state")).toContainText(singleStatus.branch);
  await expect(multiStateRows.nth(0)).toContainText(`${firstTaskRepo.name} · ${firstStatus.branch}`);
  await expect(multiStateRows.nth(1)).toContainText(`${secondTaskRepo.name} · ${secondStatus.branch}`);
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
    // Other workers can insert tasks and move this card between separate reads.
    // Measure both elements in one frame and allow the layout to settle.
    await expect
      .poll(() =>
        multiCard.evaluate((card) => {
          const cost = card.querySelector('[data-testid="task-card-cost"]')!.getBoundingClientRect();
          const badge = card.querySelector('[data-testid="state-badge"]')!.getBoundingClientRect();
          return Math.abs(cost.y + cost.height / 2 - badge.y - badge.height / 2);
        }),
      )
      .toBeLessThan(1);
    const rhythm = await multiCard.evaluate((card) => {
      const rows = Array.from(card.querySelectorAll('[data-testid="task-card-repo-state"]'));
      const group = card.querySelector('[data-testid="task-card-repo-states"]')!.parentElement!;
      const metadata = group.previousElementSibling!;
      const firstRow = rows[0];
      const secondRow = rows[1];
      if (!firstRow || !secondRow) throw new Error("Repository rows are missing");
      const first = firstRow.getBoundingClientRect();
      const second = secondRow.getBoundingClientRect();
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
    const firstHeight = rhythm.heights[0];
    const secondHeight = rhythm.heights[1];
    ok(firstHeight !== undefined);
    ok(secondHeight !== undefined);
    expect(Math.abs(firstHeight - rhythm.lineHeight)).toBeLessThan(1);
    expect(Math.abs(secondHeight - firstHeight)).toBeLessThan(1);
    expect(Math.abs(rhythm.groupGap - rhythm.rowGap)).toBeLessThan(1);
    expect(rhythm.rowGap).toBeGreaterThan(0);
    expect(rhythm.rowGap).toBeLessThanOrEqual(3);
    expect(rhythm.contained).toBe(true);
  }

  // Purged tasks keep repository labels but no runtime Git markers.
  await api.purgeTask(multiTask.id);
  await waitForTaskState(api, multiTask.id, "purged");
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(`/task/@${multiTask.id}`);
  await expect(multiStateRows.nth(1)).toBeVisible();
  await expect(multiStateRows.nth(1).getByRole("img")).toHaveCount(0);
  // Live task updates can replace repository rows. Read both heights together
  // and retry missing or unsettled rows instead of keeping stale element handles.
  await expect
    .poll(() =>
      page.getByTestId("task-list").evaluate(
        (list, { single, multi }) => {
          const emptyRow = list
            .querySelector(`[data-task-id="${multi}"]`)
            ?.querySelectorAll('[data-testid="task-card-repo-state"]')
            .item(1);
          const summaryRow = list.querySelector(`[data-task-id="${single}"] [data-testid="task-card-repo-state"]`);
          const emptyHeight = emptyRow?.getBoundingClientRect().height ?? 0;
          const summaryHeight = summaryRow?.getBoundingClientRect().height ?? 0;
          return emptyHeight > 0 && summaryHeight > 0
            ? Math.abs(emptyHeight - summaryHeight)
            : Number.POSITIVE_INFINITY;
        },
        { single: singleTask.id, multi: multiTask.id },
      ),
    )
    .toBeLessThan(1);
});

test("turn completion refreshes the diff and preserves task-card agreement", async ({ page, api, uniquePrompt }) => {
  const id = await createTaskAPI(api, uniquePrompt("Repository summary"));
  await waitForTaskState(api, id, "waiting");
  await page.goto(`/task/@${id}/diff`);
  const card = page.locator(`[data-task-id="${id}"]`);
  const diff = page.getByTestId("task-view-pane");
  const summary = card.getByRole("img", {
    name: "2 changed files, 12 additions, 2 deletions, 1 uncommitted file, 1 commit ahead of upstream",
  });
  const assertAgreement = async () => {
    await expect(summary).toBeVisible();
    await expect(diff.getByRole("heading", { name: "Commits ahead (1)" })).toBeVisible();
    await expect(diff.getByRole("heading", { name: "Uncommitted changes (1)" })).toBeVisible();
    const committed = diff.getByRole("button", { name: "cmd/caic/main.go", exact: true });
    const uncommitted = diff.getByRole("button", { name: "frontend/src/App.tsx", exact: true });
    await expect(committed).toContainText("+8");
    await expect(uncommitted).toContainText("+4");
    await expect(uncommitted).toContainText("−2");
  };
  await assertAgreement();
  await card.getByTestId("task-card-cost").getByRole("button").hover();
  await expect(page.getByText(/API-equivalent cost/)).toBeVisible();
  await page.mouse.move(0, 0);
  await expect(page.getByText(/API-equivalent cost/)).toHaveCount(0);

  const indexResponse = () =>
    page.waitForResponse((response) => new URL(response.url()).pathname === `/api/caic/v1/tasks/${id}/diff/index`);
  try {
    await test.step("running turn revalidates the open diff", async () => {
      const refreshed = indexResponse();
      await page.getByTestId("task-detail-prompt").fill("FAKE_DEMO_GATED");
      await page.getByTestId("send-input").click();
      await expect(card.getByTestId("state-badge")).toHaveText("running");
      expect((await refreshed).ok()).toBe(true);
      await expect(diff.getByText("Updating diff...", { exact: true })).toHaveCount(0);
    });

    await test.step("completion revalidates again and renders the matching file counts", async () => {
      const refreshed = indexResponse();
      await api.sendInput(id, { prompt: { text: "FAKE_DEMO_FINISH" } });
      expect((await refreshed).ok()).toBe(true);
      await expect(card.getByTestId("state-badge")).toHaveText("waiting");
      await assertAgreement();
    });
  } finally {
    await api.purgeTask(id);
  }
});

test("failed refreshes retain counts and show a stale marker at desktop and mobile widths", async ({
  page,
  api,
  uniquePrompt,
}) => {
  const repos = await api.listRepos();
  const firstRepo = repos[0];
  ok(firstRepo);
  const secondRepo = repos[1];
  ok(secondRepo);
  const task = await api.createTask({
    initialPrompt: { text: uniquePrompt("FAKE_DEMO stale summary") },
    repos: [{ name: firstRepo.path }],
    harness: "claude",
  });
  await waitForTaskState(api, task.id, "waiting");
  const snapshot = await api.getTask(task.id);
  const primary = snapshot.repos?.[0];
  if (!primary) throw new Error("Task has no repository");
  snapshot.repoStates = [
    {
      name: primary.name,
      branch: primary.branch,
      stale: true,
      ahead: 1,
      behind: 0,
      changedFiles: 2,
      linesAdded: 9,
      linesDeleted: 3,
      uncommittedFiles: 0,
      conflicts: 0,
    },
  ];
  const event = { kind: "snapshot", snapshot: [snapshot] } satisfies TaskListEvent;
  await page.route(
    (url) => url.pathname === "/api/caic/v1/tasks/events",
    (route) => route.fulfill({ contentType: "text/event-stream", body: `data: ${JSON.stringify(event)}\n\n` }),
  );
  for (const width of [1280, 390]) {
    await page.setViewportSize({ width, height: 900 });
    await page.goto("/");
    const card = page.locator(`[data-task-id="${task.id}"]`);
    await expect(card.getByTestId("repo-state-stale")).toBeVisible();
    await expect(card.getByRole("img", { name: /Refresh failed; showing last known changes/ })).toBeVisible();
    await expect(card.getByTestId("repo-state-diff-stats")).toContainText("+9");
    expect(await card.evaluate((el) => el.scrollWidth <= el.clientWidth)).toBe(true);
  }
});

// End-to-end tests for repository-diff missing commits, mobile layout, and patch retry focus.
import { createTaskAPI, expect, test, waitForTaskState } from "../helpers";
import type { ErrorResponse, FileDiffResp, TaskDiffIndexResp } from "../../sdk/caic/ts/v1/types.gen";

for (const width of [390, 1280]) {
  test(`missing upstream commits can be inspected at ${width}px`, async ({ page, api }, testInfo) => {
    await page.setViewportSize({ width, height: 844 });
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    page.on("console", (message) => {
      if (message.type() === "error") errors.push(message.text());
    });
    const id = await createTaskAPI(api, "Inspect missing upstream commits");
    const sha = "abcdef1234567890abcdef1234567890abcdef12";
    const path = "backend/internal/service/upstream.go";
    let patchRequests = 0;
    await page.route(
      (url) => url.pathname === `/api/caic/v1/tasks/${id}/diff/index`,
      async (route) =>
        route.fulfill({
          json: {
            repositories: [
              {
                name: "caic-xyz/caic",
                branch: "feature",
                upstream: "origin/main",
                ahead: 0,
                behind: 1,
                commits: [
                  {
                    sha,
                    authoredDate: "2026-10-04",
                    subject: "Missing upstream change",
                    behind: true,
                    stat: [{ path, linesAdded: 1, linesDeleted: 0, oldSize: -1, newSize: -1 }],
                  },
                ],
                uncommitted: [],
              },
            ],
          } satisfies TaskDiffIndexResp,
        }),
    );
    await page.route(
      (url) => url.pathname === `/api/caic/v1/tasks/${id}/diff/file`,
      async (route) => {
        patchRequests++;
        const url = new URL(route.request().url());
        expect(url.searchParams.get("commit")).toBe(sha);
        expect(url.searchParams.get("path")).toBe(path);
        await route.fulfill({ json: { diff: "@@ -0,0 +1 @@\n+missing upstream line" } satisfies FileDiffResp });
      },
    );
    await page.goto(`/task/@${id}/diff`);
    await expect(page.getByRole("heading", { name: "Commits ahead (0)" })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Commits behind (1)" })).toBeVisible();
    await expect(page.getByText("Missing upstream change")).toBeVisible();
    expect(patchRequests).toBe(0);
    const row = page.getByRole("button", { name: path });
    await row.focus();
    await row.press("Enter");
    await expect(page.getByText("+missing upstream line")).toBeVisible();
    expect(patchRequests).toBe(1);
    await expect.poll(async () => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    await page.screenshot({ path: testInfo.outputPath("behind-commits.png") });
    expect(errors).toEqual([]);
  });
}

test("long diff paths use middle elision on mobile", async ({ page, api }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const id = await createTaskAPI(api, "Check mobile diff path elision");
  const path = "backend/internal/server/apiconv/oauth_handlers_test.go";
  const additionOnlyPath = "oauth/AGENTS.md";

  await page.route(
    (url) => url.pathname === `/api/caic/v1/tasks/${id}/diff/index`,
    async (route) => {
      await route.fulfill({
        json: {
          repositories: [
            {
              name: "caic-xyz/caic",
              branch: "caic-26",
              upstream: "origin/main",
              ahead: 0,
              behind: 0,
              commits: [],
              uncommitted: [
                {
                  path,
                  worktreeStatus: "M",
                  linesAdded: 27,
                  linesDeleted: 5,
                  oldSize: -1,
                  newSize: -1,
                },
                {
                  path: additionOnlyPath,
                  worktreeStatus: "M",
                  linesAdded: 9,
                  linesDeleted: 0,
                  oldSize: -1,
                  newSize: -1,
                },
              ],
            },
          ],
        } satisfies TaskDiffIndexResp,
      });
    },
  );

  await page.goto(`/task/@${id}/diff`);

  const row = page.getByTitle(path);
  const displayedPath = row.getByTestId("diff-file-path");
  const added = row.getByText("+27");
  await expect(displayedPath).toBeVisible();
  await expect(displayedPath).toHaveText("backend/…/oauth_handlers_test.go");
  await expect(row).toHaveAttribute("title", path);

  await expect.poll(async () => displayedPath.evaluate((el) => getComputedStyle(el).whiteSpace)).toBe("nowrap");
  await expect
    .poll(async () =>
      row.evaluate((button) => {
        const pathEl = button.querySelector<HTMLElement>('[data-testid="diff-file-path"]');
        const addedEl = Array.from(button.querySelectorAll("span")).find((span) => span.textContent === "+27");
        if (!pathEl || !addedEl) return Number.POSITIVE_INFINITY;
        const pathBox = pathEl.getBoundingClientRect();
        const addedBox = addedEl.getBoundingClientRect();
        return Math.abs(pathBox.top + pathBox.height / 2 - (addedBox.top + addedBox.height / 2));
      }),
    )
    .toBeLessThan(2);
  await expect(added).toBeVisible();
  const additionOnly = page.getByTitle(additionOnlyPath).getByText("+9");
  const deleted = row.getByText("−5");
  await expect(additionOnly).toBeVisible();
  await expect
    .poll(async () => {
      const additionBox = await additionOnly.boundingBox();
      const deletionBox = await deleted.boundingBox();
      if (!additionBox || !deletionBox) return Number.POSITIVE_INFINITY;
      return Math.abs(additionBox.x + additionBox.width - (deletionBox.x + deletionBox.width));
    })
    .toBeLessThan(1);
  await expect.poll(async () => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
});

test("file patch retry preserves keyboard focus", async ({ page, api }) => {
  const id = await createTaskAPI(api, "Retry a file diff");
  await waitForTaskState(api, id, "waiting");
  const path = "frontend/src/retry.ts";
  let holdIndex = false;
  const indexGate = Promise.withResolvers<void>();
  let attempt = 0;
  const retryGate = Promise.withResolvers<void>();

  await page.route(
    (url) => url.pathname === `/api/caic/v1/tasks/${id}/diff/index`,
    async (route) => {
      if (holdIndex) await indexGate.promise;
      await route.fulfill({
        json: {
          repositories: [
            {
              name: "caic-xyz/caic",
              branch: "caic-27",
              upstream: "origin/main",
              ahead: 0,
              behind: 0,
              commits: [],
              uncommitted: [
                {
                  path,
                  worktreeStatus: "M",
                  linesAdded: 1,
                  linesDeleted: 1,
                  oldSize: -1,
                  newSize: -1,
                },
              ],
            },
          ],
        } satisfies TaskDiffIndexResp,
      });
    },
  );
  await page.route(
    (url) => url.pathname === `/api/caic/v1/tasks/${id}/diff/file`,
    async (route) => {
      attempt += 1;
      if (attempt === 1) {
        await route.fulfill({
          status: 500,
          json: {
            error: {
              code: "INTERNAL_ERROR",
              message: "patch unavailable",
            },
          } satisfies ErrorResponse,
        });
        return;
      }

      await retryGate.promise;
      await route.fulfill({
        json: { diff: "@@ -1 +1 @@\n-old\n+retried" } satisfies FileDiffResp,
      });
    },
  );

  try {
    await page.goto(`/task/@${id}/diff`);
    const row = page.getByRole("button", { name: path });
    await row.click();
    const retry = page.getByRole("button", { name: `Retry diff for ${path}` });
    await expect(retry).toBeVisible();
    // Hold a real background refresh during the retry to exercise both statuses.
    holdIndex = true;
    await api.sendInput(id, { prompt: { text: "continue" } });
    await expect(page.getByRole("status").filter({ hasText: "Updating diff..." })).toBeVisible();
    await retry.focus();
    await retry.press("Enter");

    await expect(retry).toHaveAttribute("aria-disabled", "true");
    await expect(retry).toBeFocused();
    await expect(page.getByRole("status", { name: `Diff status for ${path}` })).toHaveText("Retrying file diff...");

    retryGate.resolve();
    await expect(page.getByText("+retried")).toBeVisible();
    await expect(row).toBeFocused();
    expect(attempt).toBe(2);
  } finally {
    retryGate.resolve();
    indexGate.resolve();
  }
});

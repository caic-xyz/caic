// End-to-end tests for mobile repository-diff path layout.
import { createTaskAPI, expect, test } from "../helpers";

test("long diff paths use middle elision on mobile", async ({ page, api }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const id = await createTaskAPI(api, "Check mobile diff path elision");
  const path =
    "backend/internal/server/apiconv/oauth_handlers_test.go";
  const additionOnlyPath = "oauth/AGENTS.md";

  await page.route(
    (url) =>
      url.pathname === `/api/caic/v1/tasks/${id}/diff` &&
      url.searchParams.has("path"),
    async (route) => {
      await route.fulfill({
        json: {
          diff: "",
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
                  added: 27,
                  deleted: 5,
                  binary: false,
                  diff: "@@ -1 +1 @@\n-old\n+new",
                },
                {
                  path: additionOnlyPath,
                  worktreeStatus: "M",
                  added: 9,
                  deleted: 0,
                  binary: false,
                  diff: "@@ -0,0 +1 @@\n+new",
                },
              ],
            },
          ],
        },
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

  await expect
    .poll(async () =>
      displayedPath.evaluate((el) => getComputedStyle(el).whiteSpace),
    )
    .toBe("nowrap");
  await expect
    .poll(async () =>
      row.evaluate((button) => {
        const pathEl = button.querySelector<HTMLElement>(
          '[data-testid="diff-file-path"]',
        );
        const addedEl = Array.from(button.querySelectorAll("span")).find(
          (span) => span.textContent === "+27",
        );
        if (!pathEl || !addedEl) return Number.POSITIVE_INFINITY;
        const pathBox = pathEl.getBoundingClientRect();
        const addedBox = addedEl.getBoundingClientRect();
        return Math.abs(
          pathBox.top + pathBox.height / 2 -
            (addedBox.top + addedBox.height / 2),
        );
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
      return Math.abs(
        additionBox.x + additionBox.width -
          (deletionBox.x + deletionBox.width),
      );
    })
    .toBeLessThan(1);
  await expect
    .poll(async () =>
      page.evaluate(() => document.documentElement.scrollWidth <= innerWidth),
    )
    .toBe(true);
});

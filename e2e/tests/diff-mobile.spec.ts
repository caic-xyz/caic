// End-to-end tests for repository-diff mobile layout and patch retry focus.
import { createTaskAPI, expect, test } from "../helpers";

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
                  added: 27,
                  deleted: 5,
                  binary: false,
                },
                {
                  path: additionOnlyPath,
                  worktreeStatus: "M",
                  added: 9,
                  deleted: 0,
                  binary: false,
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
    .poll(async () => displayedPath.evaluate((el) => getComputedStyle(el).whiteSpace))
    .toBe("nowrap");
  await expect
    .poll(async () =>
      row.evaluate((button) => {
        const pathEl = button.querySelector<HTMLElement>('[data-testid="diff-file-path"]');
        const addedEl = Array.from(button.querySelectorAll("span")).find(
          (span) => span.textContent === "+27",
        );
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
  await expect
    .poll(async () => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth))
    .toBe(true);
});

test("file patch retry preserves keyboard focus", async ({ page, api }) => {
  const id = await createTaskAPI(api, "Retry a file diff");
  const path = "frontend/src/retry.ts";
  let attempt = 0;
  let finishRetry: () => void = () => undefined;
  const retryGate = new Promise<void>((resolve) => {
    finishRetry = resolve;
  });

  await page.route(
    (url) => url.pathname === `/api/caic/v1/tasks/${id}/diff/index`,
    async (route) => {
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
                  added: 1,
                  deleted: 1,
                  binary: false,
                },
              ],
            },
          ],
        },
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
          },
        });
        return;
      }

      await retryGate;
      await route.fulfill({
        json: { diff: "@@ -1 +1 @@\n-old\n+retried" },
      });
    },
  );

  await page.goto(`/task/@${id}/diff`);
  const row = page.getByRole("button", { name: path });
  await row.click();
  const retry = page.getByRole("button", { name: `Retry diff for ${path}` });
  await expect(retry).toBeVisible();
  await retry.focus();
  await retry.press("Enter");

  await expect(retry).toHaveAttribute("aria-disabled", "true");
  await expect(retry).toBeFocused();
  await expect(page.getByRole("status")).toHaveText("Retrying file diff...");

  finishRetry();
  await expect(page.getByText("+retried")).toBeVisible();
  await expect(row).toBeFocused();
  expect(attempt).toBe(2);
});

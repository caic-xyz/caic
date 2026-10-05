// Browser coverage and opt-in heap measurements for collapsed and cached diff payloads.
// Heap measurements: CAIC_DIFF_HEAP=1 pnpm exec playwright test --config e2e/playwright.config.ts e2e/tests/diff-retention.spec.ts --grep 'heap workload' --workers=1.
// Run against each production build separately; reported CDP usedSize is measured heap, not the cache's conservative payload estimate.

import { createTaskAPI, expect, test, waitForTaskState } from "../helpers";
import type { FileDiffResp, TaskDiffIndexResp } from "../../sdk/caic/ts/v1/types.gen";

function patch(seed: number, size: number): string {
  let state = seed + 1;
  const lines = ["@@ -0,0 +1 @@", `+retention-marker-${seed}`];
  let length = lines.join("\n").length;
  while (length < size) {
    let line = "+";
    for (let i = 0; i < 1024; i++) {
      state = (Math.imul(state, 1664525) + 1013904223) >>> 0;
      line += String.fromCharCode(33 + (state % 90));
    }
    lines.push(line);
    length += line.length + 1;
  }
  return lines.join("\n");
}

for (const workload of [
  { name: "giant", files: 1, size: 2_000_000 },
  { name: "cache", files: 32, size: 750_000 },
]) {
  test(`diff retained heap workload ${workload.name}`, async ({ page, api }, testInfo) => {
    test.skip(process.env.CAIC_DIFF_HEAP !== "1", "opt-in browser heap measurement");
    const id = await createTaskAPI(api, `Diff retention ${workload.name}`);
    await waitForTaskState(api, id, "waiting");
    const paths = Array.from({ length: workload.files }, (_, index) => `retention-${index}.txt`);
    await page.route(
      (url) => url.pathname === `/api/caic/v1/tasks/${id}/diff/index`,
      async (route) => {
        await route.fulfill({
          json: {
            repositories: [
              {
                name: "repo",
                branch: "feature",
                ahead: 1,
                behind: 0,
                uncommitted: [],
                commits: [
                  {
                    sha: "a".repeat(40),
                    subject: "Retained payload fixture",
                    authoredDate: "2026-10-04",
                    stat: paths.map((path) => ({ path, linesAdded: 1, linesDeleted: 0, oldSize: -1, newSize: -1 })),
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
        const path = new URL(route.request().url()).searchParams.get("path");
        const index = paths.indexOf(path ?? "");
        expect(index).toBeGreaterThanOrEqual(0);
        await route.fulfill({ json: { diff: patch(index, workload.size) } satisfies FileDiffResp });
      },
    );
    await page.goto(`/task/@${id}/diff`);
    await expect(page.getByRole("button", { name: paths[0], exact: true })).toBeVisible();
    const cdp = await page.context().newCDPSession(page);
    await cdp.send("HeapProfiler.enable");
    await cdp.send("HeapProfiler.collectGarbage");
    const baseline = (await cdp.send("Runtime.getHeapUsage")).usedSize;
    for (let index = 0; index < paths.length; index++) {
      const row = page.getByRole("button", { name: paths[index], exact: true });
      await row.press("Enter");
      await expect(page.getByText(`+retention-marker-${index}`, { exact: true })).toBeAttached();
      await row.press("Enter");
      await expect(page.getByText(`+retention-marker-${index}`, { exact: true })).toHaveCount(0);
    }
    await cdp.send("HeapProfiler.collectGarbage");
    const collapsed = (await cdp.send("Runtime.getHeapUsage")).usedSize;
    await page.getByRole("button", { name: "Back to task", exact: true }).click();
    await expect(page).toHaveURL(new RegExp(`/task/@${id}(?:\\+[^/]+)?$`));
    await cdp.send("HeapProfiler.collectGarbage");
    const departed = (await cdp.send("Runtime.getHeapUsage")).usedSize;
    const result = { workload: workload.name, baseline, collapsed, departed, collapsedDelta: collapsed - baseline };
    console.log(JSON.stringify(result));
    await testInfo.attach("heap.json", { body: JSON.stringify(result), contentType: "application/json" });
    await cdp.detach();
  });
}

for (const width of [390, 1280]) {
  test(`collapsed oversized patches reload with keyboard focus at ${width}px`, async ({ page, api }) => {
    await page.setViewportSize({ width, height: 844 });
    const id = await createTaskAPI(api, "Inspect a large patch");
    await waitForTaskState(api, id, "waiting");
    const path = "large-patch.txt";
    let requests = 0;
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    await page.route(
      (url) => url.pathname === `/api/caic/v1/tasks/${id}/diff/index`,
      async (route) => {
        await route.fulfill({
          json: {
            repositories: [
              {
                name: "repo",
                branch: "feature",
                ahead: 1,
                behind: 0,
                uncommitted: [],
                commits: [
                  {
                    sha: "b".repeat(40),
                    subject: "Large patch",
                    authoredDate: "2026-10-04",
                    stat: [{ path, linesAdded: 1, linesDeleted: 0, oldSize: -1, newSize: -1 }],
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
        requests++;
        await route.fulfill({ json: { diff: patch(requests, 1_100_000) } satisfies FileDiffResp });
      },
    );
    await page.goto(`/task/@${id}/diff`);
    const row = page.getByRole("button", { name: path, exact: true });
    await row.press("Enter");
    await expect(page.getByText("+retention-marker-1", { exact: true })).toBeAttached();
    await row.press("Enter");
    await expect(row).toBeFocused();
    await expect(page.getByText("+retention-marker-1", { exact: true })).toHaveCount(0);
    await row.press("Enter");
    await expect(page.getByText("+retention-marker-2", { exact: true })).toBeAttached();
    expect(requests).toBe(2);
    await expect(row).toBeFocused();
    await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    expect(errors).toEqual([]);
  });
}

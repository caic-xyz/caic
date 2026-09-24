// End-to-end coverage for reading resource samples from the task statistics charts.

import { createTaskAPI, expect, test } from "../helpers";

test("resource charts read samples through the crosshair", async ({ page, api }) => {
  const id = await createTaskAPI(api, "Seed the resource charts");

  await page.goto(`/task/@${id}`);
  await page.getByRole("link", { name: "Task statistics" }).click();
  await expect(page).toHaveURL(new RegExp(`/task/@${id}/stats$`));

  const resources = page.getByTestId("resource-charts");
  await expect(resources).toBeVisible();
  // The fake runtime streams a paced sample history; wait until the chart holds
  // more than the first sample before reading values from it.
  await expect
    .poll(async () => Number(/(\d+) samples/u.exec((await resources.textContent()) ?? "")?.[1] ?? 0))
    .toBeGreaterThanOrEqual(2);

  const cpu = page.getByLabel("CPU utilization over time");
  const box = await cpu.boundingBox();
  if (!box) throw new Error("CPU utilization chart has no layout box");

  // The first sample sits on the left frame edge and every later sample reports
  // the same CPU reading, so both edges stay readable however the paced stream
  // was interrupted and restarted.
  await page.mouse.move(box.x + 44, box.y + box.height / 2);
  await expect(cpu).toContainText("10.0%");
  await page.mouse.move(box.x + box.width - 6, box.y + box.height / 2);
  await expect(cpu).toContainText("50.0%");
  // The readout is centered on the focused sample; at the right edge it parks
  // inside the frame instead of being clipped by the viewport.
  const readout = cpu.locator('[aria-label="text"] text');
  await expect(readout).toHaveCount(1);
  await expect
    .poll(async () => {
      const transform = (await readout.getAttribute("transform")) ?? "";
      return Number(/translate\(([-\d.]+)/u.exec(transform)?.[1] ?? Number.NaN);
    })
    .toBeLessThanOrEqual(box.width - 3 - 34);
});

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
  // PlotHost replaces the SVG as samples arrive. Read its box in one browser
  // evaluation so a redraw cannot detach it between lookup and measurement.
  let box = { x: 0, y: 0, width: 0, height: 0 };
  await expect
    .poll(async () => {
      box = await cpu.evaluate((svg) => {
        const { x, y, width, height } = svg.getBoundingClientRect();
        return { x, y, width, height };
      });
      return box.width > 0 && box.height > 0;
    })
    .toBe(true);

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
  // Plot and DOM layout use slightly different floating-point rounding.
  await expect
    .poll(async () => {
      const transform = (await readout.getAttribute("transform")) ?? "";
      return Number(/translate\(([-\d.]+)/u.exec(transform)?.[1] ?? Number.NaN);
    })
    .toBeLessThanOrEqual(box.width - 3 - 34 + 0.5);
});

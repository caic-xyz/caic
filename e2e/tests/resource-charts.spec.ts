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

  // New samples replace the SVG and its pointer listener. Enter the current
  // SVG again on each poll and compare the readout with the table in the same
  // browser snapshot. Other tasks can restart the fake stream, so table order
  // alone does not identify the timestamps at the chart edges.
  async function readAt(edge: "left" | "right"): Promise<string> {
    // Aim at the rendered edge dots instead of a fixed offset: the retained
    // sample count grows, so the sample spacing shrinks and a fixed offset
    // drifts onto a neighbouring sample.
    const aim = await cpu.evaluate((svg, which) => {
      const rect = svg.getBoundingClientRect();
      const dots = Array.from(svg.querySelectorAll('[aria-label="dot"] circle'), (circle) =>
        circle.getBoundingClientRect(),
      );
      if (dots.length === 0) return null;
      const xs = dots.map((r) => r.x + r.width / 2);
      return {
        x: which === "left" ? Math.min(...xs) : Math.max(...xs),
        y: rect.y + rect.height / 2,
        box: { x: rect.x, y: rect.y, width: rect.width, height: rect.height },
      };
    }, edge);
    if (!aim) return "missing sample dots";
    box = aim.box;
    await page.mouse.move(box.x - 2, box.y - 2);
    await page.mouse.move(aim.x, aim.y);
    const snapshot = await cpu.evaluate((svg) => {
      const text = svg.querySelector('[aria-label="text"] text');
      const rows = Array.from(svg.closest('[data-testid="resource-charts"]')?.querySelectorAll("table tbody tr") ?? []);
      const samples = rows.map((row) => {
        const cells = row.querySelectorAll("td");
        return { ts: Date.parse(cells[0]?.textContent ?? ""), cpu: Number.parseFloat(cells[1]?.textContent ?? "") };
      });
      return { samples, value: text?.textContent ?? "", transform: text?.getAttribute("transform") ?? "" };
    });
    if (snapshot.samples.length < 2) return "missing samples";
    const timestamps = snapshot.samples.map((sample) => sample.ts);
    const targetTs = edge === "left" ? Math.min(...timestamps) : Math.max(...timestamps);
    const oppositeTs = edge === "left" ? Math.max(...timestamps) : Math.min(...timestamps);
    const expected = snapshot.samples.filter((sample) => sample.ts === targetTs).map((sample) => sample.cpu);
    const opposite = snapshot.samples.filter((sample) => sample.ts === oppositeTs).map((sample) => sample.cpu);
    if (expected.some((value) => opposite.includes(value))) return "waiting for distinct edge samples";
    const actual = Number.parseFloat(snapshot.value);
    if (!expected.includes(actual)) return `${actual} is not a ${edge} edge sample (${expected.join(", ")})`;
    if (edge === "right") {
      // Plot and DOM layout use slightly different floating-point rounding.
      const x = Number(/translate\(([-\d.]+)/u.exec(snapshot.transform)?.[1] ?? Number.NaN);
      if (!(x <= box.width - 3 - 34 + 0.5)) return `readout at ${x} clips the right edge`;
    }
    return "matched";
  }

  await expect.poll(() => readAt("left"), { timeout: 15_000 }).toBe("matched");
  // The readout is centered on the focused sample; at the right edge it parks
  // inside the frame instead of being clipped by the viewport.
  await expect.poll(() => readAt("right"), { timeout: 15_000 }).toBe("matched");
});

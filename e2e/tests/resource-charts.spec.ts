// End-to-end coverage for reading resource samples from the task statistics charts.

import { createTaskAPI, expect, test } from "../helpers";
import type { EventStats } from "../../sdk/caic/ts/v1/types.gen";

test("task CPU configured ceilings and metadata failures stay distinct", async ({ page, api }, testInfo) => {
  const id = await createTaskAPI(api, "Inspect fractional CPU constraints");
  const ts = Date.UTC(2026, 9, 4);
  const event = {
    kind: "stats",
    ts,
    stats: {
      ts,
      cpuPerc: 150,
      cpuCeiling: 1.5,
      cpuQuota: 1.5 as number | null,
      runtimeCPUs: 8,
      cpuError: "",
      memUsed: 512,
      memLimit: 1024,
      memPerc: 50,
      netRx: 0,
      netTx: 0,
      blockRead: 0,
      blockWrite: 0,
      diskUsed: -1,
    },
  };
  await page.route(`**/api/caic/v1/tasks/${id}/history/events?*`, (route) =>
    route.fulfill({
      contentType: "text/event-stream",
      body: `event: statsSnapshot\ndata: ${JSON.stringify({ samples: [event.stats] })}\n\nevent: checkpoint\ndata: {"cursor":"fixture"}\n\n`,
    }),
  );
  await page.goto(`/task/@${id}/stats`);
  const resources = page.getByTestId("resource-charts");
  for (const width of [1280, 390]) {
    await page.setViewportSize({ width, height: 900 });
    await expect(resources).toContainText("150.0% · 1.50 CPUs");
    await expect(resources).toContainText("effective capacity unknown · configured ceiling ≤ 1.5 CPUs");
    await expect(resources).toContainText("Runtime: 8 CPUs · quota 1.5 CPUs");
    await expect(resources).toContainText("Inherited CPU limits and process affinity are unreported.");
    await expect(resources).not.toContainText("of capacity");
    await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    await resources.screenshot({ path: testInfo.outputPath(`cpu-capacity-${width}.png`) });
  }
  await resources.getByText("Exact samples").click();
  await expect(resources.getByRole("table")).toContainText("Configured CPU ceiling");
  await expect(resources.getByRole("table")).toContainText("CPU quota");
  await expect(resources.getByRole("table")).toContainText("1.5");
  event.stats.cpuQuota = null;
  event.stats.cpuCeiling = 8;
  event.stats.cpuError = "inspect unavailable";
  await page.reload();
  await expect(resources).toContainText("quota unknown");
  await expect(resources).toContainText("configured ceiling ≤ 8 CPUs");
  await resources.getByText("CPU metadata warning").click();
  await expect(resources.getByText("inspect unavailable")).toBeVisible();
});

for (const history of ["clustered", "live"] as const) {
  test(`resource charts read ${history} samples through the crosshair`, async ({ page, api }) => {
    const id = await createTaskAPI(api, "Seed the resource charts");

    if (history === "clustered") {
      // Reproduce stream restarts: the first two samples are only 1 ms apart
      // in a 20-second window, making their dots less than one pixel apart.
      const start = Date.UTC(2026, 9, 4);
      const events: { stats: EventStats }[] = [0, 1, 20_000].map((offset, index) => ({
        stats: {
          ts: start + offset,
          cpuPerc: 118 + index,
          cpuCeiling: 4,
          cpuQuota: 0,
          runtimeCPUs: 4,
          memUsed: 512,
          memLimit: 1024,
          memPerc: 50,
          netRx: index * 12000,
          netTx: index * 8000,
          blockRead: 0,
          blockWrite: 0,
          diskUsed: -1,
        },
      }));
      await page.route(`**/api/caic/v1/tasks/${id}/history/events?*`, (route) =>
        route.fulfill({
          contentType: "text/event-stream",
          body: `event: statsSnapshot\ndata: ${JSON.stringify({ samples: events.map((event) => event.stats) })}\n\nevent: checkpoint\ndata: {"cursor":"fixture"}\n\n`,
        }),
      );
    }

    await page.goto(`/task/@${id}`);
    await page.getByRole("link", { name: "Task statistics" }).click();
    await expect(page).toHaveURL(new RegExp(`/task/@${id}/stats$`));

    const resources = page.getByTestId("resource-charts");
    await expect(resources).toBeVisible();
    await expect(resources).toContainText("configured ceiling ≤ 4 CPUs");
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
      // drifts onto a neighbouring sample. Plot shifts rendered dots by half a
      // pixel; aim 1 px outward so tightly clustered samples cannot put the
      // pointer closer to the next timestamp than the edge timestamp.
      const aim = await cpu.evaluate((svg, which) => {
        const rect = svg.getBoundingClientRect();
        const dots = Array.from(svg.querySelectorAll('[aria-label="dot"] circle'), (circle) =>
          circle.getBoundingClientRect(),
        );
        if (dots.length === 0) return null;
        const xs = dots.map((r) => r.x + r.width / 2);
        return {
          x: which === "left" ? Math.min(...xs) - 1 : Math.max(...xs) + 1,
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
        const rows = Array.from(
          svg.closest('[data-testid="resource-charts"]')?.querySelectorAll("table tbody tr") ?? [],
        );
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

    // Percentage and throughput labels have different widths. Every tick must
    // fit the actual SVG at both responsive layouts, including its leading digits.
    for (const viewport of [
      { width: 1280, height: 800 },
      { width: 390, height: 844 },
    ]) {
      await page.setViewportSize(viewport);
      await resources.scrollIntoViewIfNeeded();
      for (const label of [
        "CPU utilization over time",
        "Network receive throughput over time",
        "Network transmit throughput over time",
      ]) {
        await expect
          .poll(() =>
            page.getByLabel(label, { exact: true }).evaluate((svg) => {
              const bounds = svg.getBoundingClientRect();
              const ticks = Array.from(svg.querySelectorAll('[aria-label="y-axis tick label"] text'));
              return (
                ticks.length === 2 &&
                ticks.every((tick) => {
                  const box = tick.getBoundingClientRect();
                  return box.left >= bounds.left - 0.5 && box.right <= bounds.right + 0.5;
                })
              );
            }),
          )
          .toBe(true);
      }
    }
  });
}

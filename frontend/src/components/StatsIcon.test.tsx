// Tests for the task statistics glyph that links from a task header.

import { afterEach, describe, it } from "node:test";
import { render } from "@solidjs/testing-library";
import { MemoryRouter, Route } from "@solidjs/router";
import { expect, vi } from "@tests/expect";

import type { EventStats } from "@sdk/types.gen";

import type { TaskUsageSummary } from "./StatsDetail";
import StatsIcon from "./StatsIcon";

const usage: TaskUsageSummary = {
  cacheReadInputTokens: 7_000,
  cacheWriteInputTokens: 2_000,
  costUSD: 0.125,
  inputTokens: 1_000,
  outputTokens: 500,
};

const stats: EventStats[] = [
  {
    ts: 1_000,
    cpuPerc: 10,
    memUsed: 1_024,
    memLimit: 4_096,
    memPerc: 25,
    netRx: 0,
    netTx: 0,
    blockRead: 0,
    blockWrite: 0,
    diskUsed: -1,
  },
];

afterEach(() => vi.restoreAllMocks());

function renderIcon(history: EventStats[]) {
  return render(() => (
    <MemoryRouter>
      <Route path="*" component={() => <StatsIcon href="/task/@task/stats" stats={history} usage={usage} />} />
    </MemoryRouter>
  ));
}

// The glyph draws four bars in order: CPU, memory, network, disk.
function barHeights(history: EventStats[]): (string | null | undefined)[] {
  const { getByRole } = renderIcon(history);
  const bars = getByRole("link", { name: "Task statistics" }).querySelectorAll("rect");
  return Array.from(bars, (bar) => bar.getAttribute("height"));
}

function barClasses(history: EventStats[]): string[] {
  const { getByRole } = renderIcon(history);
  const bars = getByRole("link", { name: "Task statistics" }).querySelectorAll("rect");
  return Array.from(bars, (bar) => bar.getAttribute("class") ?? "");
}

describe("StatsIcon", () => {
  it("surfaces task token volume and cost before opening details", () => {
    const { getByRole } = renderIcon([]);

    const trigger = getByRole("link", { name: "Task statistics" });
    expect(trigger).toHaveAttribute("href", "/task/@task/stats");
    expect(trigger).toHaveTextContent("11kt");
    expect(trigger).toHaveTextContent("$0.13");
  });

  it("states the sampled resource figures for assistive technology", () => {
    const { getByRole } = renderIcon([
      { ...stats[0], cpuPerc: 50, memPerc: 75, netRx: 3_072, netTx: 1_024, diskUsed: -1 },
    ]);

    const trigger = getByRole("link", { name: "Task statistics" });
    expect(trigger).toHaveTextContent("50.0% CPU");
    expect(trigger).toHaveTextContent("75.0% memory");
    expect(trigger).toHaveTextContent("4.0 KiB transferred");
    expect(trigger).toHaveTextContent("unavailable disk used");
  });

  it("scales every bar against its own fixed ceiling", () => {
    const [{ ...sample }] = stats;
    const heights = barHeights([
      {
        ...sample,
        cpuPerc: 90,
        memPerc: 40,
        netRx: 850_000_000,
        netTx: 0,
        diskUsed: 5_000_000_000,
      },
    ]);

    // Eight pixels per full bar: 90% CPU, 40% memory, 85% of the 1 GB transfer
    // ceiling, and half of the 10 GB disk ceiling.
    expect(heights).toEqual(["7", "3", "7", "4"]);
  });

  it("keeps a bar's height and color on the same scale", () => {
    const [{ ...sample }] = stats;
    const at = (diskUsed: number) => {
      const history = [{ ...sample, diskUsed }];
      return { classes: barClasses(history), heights: barHeights(history) };
    };

    // Disk turns amber at half of its 10 GB ceiling and red at 85% of it, which
    // are the same fractions that decide the bar's height.
    const below = at(4_000_000_000);
    expect(below.heights[3]).toBe("3");
    expect(below.classes[3]).toContain("Success");
    const warning = at(5_000_000_000);
    expect(warning.heights[3]).toBe("4");
    expect(warning.classes[3]).toContain("Warning");
    const danger = at(8_500_000_000);
    expect(danger.heights[3]).toBe("7");
    expect(danger.classes[3]).toContain("Danger");
  });

  it("keeps bar heights stable as a task's history grows", () => {
    const [{ ...sample }] = stats;
    const latest = { ...sample, cpuPerc: 50, memPerc: 50, netRx: 100_000_000, netTx: 0, diskUsed: 1_000_000_000 };
    const small = barHeights([latest]);

    // A larger historical peak must not shrink the current reading: the bar
    // measures the sample against a fixed ceiling, not against the maximum the
    // task happens to have reached.
    const withEarlierPeak = barHeights([{ ...latest, ts: 500, netRx: 900_000_000, diskUsed: 9_000_000_000 }, latest]);

    expect(withEarlierPeak).toEqual(small);
  });

  it("renders a day-scale resource history without spreading it into Math.max", () => {
    // V8 limits the number of arguments passed to a function. Long-running
    // tasks can retain more samples than Math.max(...samples) accepts.
    const longHistory = Array.from({ length: 150_000 }, (_, i) => ({
      ...stats[0],
      diskUsed: i,
      netRx: i,
      netTx: i,
      ts: i,
    }));

    expect(() => renderIcon(longHistory)).not.toThrow();
  });
});

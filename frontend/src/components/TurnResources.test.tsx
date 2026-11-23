// Tests the turn's resource lanes: samples within the window on explicit domains, exact values, and nothing drawn without samples in the window.

import { describe, it } from "node:test";
import { render, screen, within } from "@solidjs/testing-library";
import { expect } from "@tests/expect";

import type { EventStats } from "@sdk/types.gen";
import TurnResources from "./TurnResources";

const start = Date.parse("2026-01-01T00:00:00Z");

function sample(atMs: number, cpuPerc: number, memPerc: number): EventStats {
  return {
    ts: start + atMs,
    cpuPerc,
    cpuCeiling: 4,
    cpuQuota: 0,
    runtimeCPUs: 8,
    memUsed: 512 * 2 ** 20,
    memLimit: 2 ** 30,
    memPerc,
    netRx: 0,
    netTx: 0,
    blockRead: 0,
    blockWrite: 0,
    diskUsed: 0,
  };
}

function renderResources(stats: readonly EventStats[]) {
  return render(() => <TurnResources stats={stats} startMs={start} durationMs={10_000} tickShare={0.2} />);
}

function rows(): string[][] {
  const table = within(screen.getByRole("region", { name: "Resource samples in this turn" }));
  return table
    .getAllByRole("row")
    .slice(1)
    .map((row) => [...row.querySelectorAll("td")].map((cell) => cell.textContent ?? ""));
}

function dots(lane: HTMLElement | undefined): { x: string; y: string }[] {
  return [...(lane?.querySelectorAll<HTMLElement>("span[style]") ?? [])].map((dot) => ({
    x: dot.style.getPropertyValue("--x"),
    y: dot.style.getPropertyValue("--y"),
  }));
}

describe("TurnResources", () => {
  it("places the samples within the turn on the timeline's axis with exact values", () => {
    renderResources([sample(-2_000, 50, 10), sample(0, 100, 25), sample(5_000, 300, 50), sample(12_000, 10, 10)]);

    const [cpu, memory] = screen.getAllByTestId("turn-resource-lane");
    // CPU reaches the 4-CPU ceiling: 100% per CPU.
    expect(cpu).toHaveTextContent("CPU peak 300.0% · scale 0–400%, 100% = 1 CPU");
    expect(dots(cpu)).toEqual([
      { x: "0", y: "0.25" },
      { x: "0.5", y: "0.75" },
    ]);
    expect(memory).toHaveTextContent("Memory peak 50.0% of limit · scale 0–100%");
    expect(dots(memory)).toEqual([
      { x: "0", y: "0.25" },
      { x: "0.5", y: "0.5" },
    ]);
    expect(rows()).toEqual([
      ["0.000 s", "100.0%", "25.0%", "512.0 MiB"],
      ["5.000 s", "300.0%", "50.0%", "512.0 MiB"],
    ]);
  });

  it("draws nothing when no sample falls within the turn", () => {
    for (const stats of [[], [sample(-5_000, 50, 10), sample(15_000, 50, 10)]]) {
      const { unmount } = renderResources(stats);
      expect(screen.queryByTestId("turn-resources")).not.toBeInTheDocument();
      unmount();
    }
  });
});

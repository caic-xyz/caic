// Tests the turn's resource lanes: samples within the window on explicit domains, exact values, and the coverage note for missing, older, dropped, and late-starting samples.

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

// retained is the server's ring capacity; the component reads it, not a
// constant of its own.
const retained = 8;

function renderResources(stats: readonly EventStats[]) {
  return render(() => (
    <TurnResources stats={stats} retained={retained} startMs={start} durationMs={10_000} tickShare={0.2} />
  ));
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

    expect(screen.queryByTestId("turn-resources-note")).not.toBeInTheDocument();
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

  // A full ring of retained samples has dropped older ones.
  const ring = (fromMs: number) => Array.from({ length: retained }, (_, i) => sample(fromMs + i * 100, 50, 10));

  it("draws nothing for a turn older than the retained samples", () => {
    renderResources(ring(60_000));

    expect(screen.queryByTestId("turn-resources-note")).not.toBeInTheDocument();
    expect(screen.queryByTestId("turn-resource-lane")).not.toBeInTheDocument();
    expect(screen.queryByRole("region")).not.toBeInTheDocument();
  });

  it("says when retained samples cover only the end of the turn", () => {
    renderResources(ring(4_000));

    expect(screen.getByTestId("turn-resources-note")).toHaveTextContent(
      "Samples start 0:04 into the turn; earlier ones are no longer retained.",
    );
    expect(rows()).toHaveLength(retained);
  });

  it("adds no note when sampling began after the turn started", () => {
    renderResources([sample(4_000, 50, 10), sample(8_000, 50, 10)]);

    expect(screen.queryByTestId("turn-resources-note")).not.toBeInTheDocument();
    expect(rows()).toHaveLength(2);
  });

  it("says when the task has no samples, or none within the turn", () => {
    const { unmount } = renderResources([]);
    expect(screen.getByTestId("turn-resources-note")).toHaveTextContent(
      "No resource samples are retained for this task.",
    );
    unmount();

    renderResources([sample(-5_000, 50, 10), sample(15_000, 50, 10)]);
    expect(screen.getByTestId("turn-resources-note")).toHaveTextContent("No resource sample falls within this turn.");
  });
});

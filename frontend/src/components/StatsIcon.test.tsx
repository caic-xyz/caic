// Tests for compact task token totals and detailed per-invocation usage statistics.

import { render, within } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { EventStats } from "@sdk/types.gen";

import type { TurnTiming } from "../timing";
import StatsIcon, { type TaskUsageSummary } from "./StatsIcon";

const usage: TaskUsageSummary = {
  cacheReadInputTokens: 7_000,
  cacheWriteInputTokens: 2_000,
  costUSD: 0.125,
  inputTokens: 1_000,
  outputTokens: 500,
};

const turns: TurnTiming[] = [{
  event: { kind: "result", ts: 2_000 },
  result: {
    subtype: "success",
    isError: false,
    result: "done",
    totalCostUSD: 0.125,
    duration: 5,
    durationAPI: 4,
    numTurns: 1,
    usage: {
      inputTokens: 1_000,
      outputTokens: 500,
      cacheCreationInputTokens: 2_000,
      cacheReadInputTokens: 7_000,
      reasoningOutputTokens: 200,
      model: "test-model",
    },
  },
  waitMs: 3_000,
}];

const stats: EventStats[] = [
  { ts: 1_000, cpuPerc: 10, memUsed: 1_024, memLimit: 4_096, memPerc: 25, netRx: 0, netTx: 0, blockRead: 0, blockWrite: 0, diskUsed: -1 },
  { ts: 2_000, cpuPerc: 140.25, memUsed: 2_048, memLimit: 4_096, memPerc: 50, netRx: 2_048, netTx: 1_024, blockRead: 0, blockWrite: 0, diskUsed: 1_048_576 },
  { ts: 3_000, cpuPerc: 50, memUsed: 3_072, memLimit: 4_096, memPerc: 75, netRx: 3_072, netTx: 1_024, blockRead: 0, blockWrite: 0, diskUsed: -1 },
];

afterEach(() => vi.restoreAllMocks());

describe("StatsIcon", () => {
  it("surfaces task token volume and cost before opening details", () => {
    const { getByRole } = render(() => <StatsIcon events={[]} stats={[]} turns={turns} usage={usage} />);

    const trigger = getByRole("button", { name: "Task statistics" });
    expect(trigger).toHaveTextContent("11kt");
    expect(trigger).toHaveTextContent("$0.13");
  });

  it("separates token categories and reports cache efficiency", async () => {
    const user = userEvent.setup();
    const events = [
      { kind: "toolUse", ts: 1_000, toolUse: { toolUseID: "tool-1", name: "Bash", input: {} } },
      { kind: "toolResult", ts: 2_000, toolResult: { toolUseID: "tool-1", duration: 1 } },
    ] as const;
    const { findByTestId, getByLabelText, getByRole, getByTestId } = render(() => (
      <StatsIcon events={events} stats={[]} turns={turns} usage={usage} />
    ));

    await user.click(getByRole("button", { name: "Task statistics" }));

    const summary = getByTestId("task-usage-summary");
    expect(summary).toHaveTextContent("New input1.0kt");
    expect(summary).toHaveTextContent("Cache write2.0kt");
    expect(summary).toHaveTextContent("Cache read7.0kt");
    expect(summary).toHaveTextContent("Output500t");
    expect(summary).toHaveTextContent("Thinking200t");
    expect(summary).toHaveTextContent("Cache hit 70%");

    const invocations = getByTestId("invocation-usage");
    expect(invocations).toHaveTextContent("test-model");
    expect(invocations).toHaveTextContent("1.0kt");
    expect(invocations).toHaveTextContent("2.0kt");
    expect(invocations).toHaveTextContent("7.0kt");
    expect(invocations).toHaveTextContent("200t");
    expect(getByLabelText("Turn 1 used 1.0kt of uncached input")).toBeInTheDocument();
    expect(await findByTestId("turn-token-chart", undefined, { timeout: 5_000 })).toBeInTheDocument();
    expect(await findByTestId("tool-time-chart", undefined, { timeout: 5_000 })).toBeInTheDocument();
  });

  it("keeps double-digit turns in chronological chart order without Plot warnings", async () => {
    const user = userEvent.setup();
    const warn = vi.spyOn(console, "warn").mockImplementation(() => undefined);
    const manyTurns = Array.from({ length: 12 }, () => turns[0]);
    const { findByTestId, getByRole } = render(() => (
      <StatsIcon events={[]} stats={[]} turns={manyTurns} usage={usage} />
    ));

    await user.click(getByRole("button", { name: "Task statistics" }));
    const chart = await findByTestId("turn-token-chart");
    const labels = Array.from(chart.querySelectorAll('[aria-label="x-axis tick label"] text'), (label) => label.textContent);
    expect(labels).toEqual(Array.from({ length: 12 }, (_, i) => String(i + 1)));
    expect(warn).not.toHaveBeenCalled();
  });

  it("shows aligned resource history and network throughput", async () => {
    const user = userEvent.setup();
    const { findByLabelText, findByTestId, getByRole } = render(() => (
      <StatsIcon events={[]} stats={stats} turns={[]} usage={usage} />
    ));

    await user.click(getByRole("button", { name: "Task statistics" }));
    const resources = await findByTestId("resource-charts");
    expect(resources).toHaveTextContent("CPU50.0% · max 140.25%");
    expect(resources).toHaveTextContent("Memory3.0 KB / 4.0 KB");
    expect(resources).toHaveTextContent("NetworkRX 3.0 KB · TX 1.0 KB");
    expect(resources).toHaveTextContent("DiskUnavailable");
    expect(resources).toHaveTextContent("3 samples");
    const cpuChart = await findByLabelText("CPU utilization over time");
    expect(cpuChart).toBeInTheDocument();
    const cpuDots = Array.from(cpuChart.querySelectorAll("circle"));
    expect(cpuDots.every((dot) => Number(dot.getAttribute("cy")) >= Number(dot.getAttribute("r")))).toBe(true);
    const cpuScaleLabels = Array.from(
      cpuChart.querySelectorAll('[aria-label="y-axis tick label"] text'),
      (label) => label.textContent,
    );
    expect(cpuScaleLabels).toEqual(["0%", "141%"]);
    expect(await findByLabelText("Memory utilization over time")).toBeInTheDocument();
    const rxChart = await findByLabelText("Network receive throughput over time");
    const txChart = await findByLabelText("Network transmit throughput over time");
    expect(resources).toHaveTextContent("RX/s1.0 KB/s");
    expect(resources).toHaveTextContent("TX/s0 B/s");
    expect(resources).not.toHaveTextContent("network chart shows throughput");
    expect(
      Array.from(rxChart.querySelectorAll('[aria-label="y-axis tick label"] text'), (label) => label.textContent),
    ).toEqual(["0 B/s", "2.0 KB/s"]);
    expect(
      Array.from(txChart.querySelectorAll('[aria-label="y-axis tick label"] text'), (label) => label.textContent),
    ).toEqual(["0 B/s", "1.0 KB/s"]);
    expect(rxChart.querySelector('[aria-label="y-axis tick label"]')).toHaveAttribute("text-anchor", "end");
    expect(txChart.querySelector('[aria-label="y-axis tick label"]')).toHaveAttribute("text-anchor", "start");
    expect(await findByLabelText("Writable disk usage over time")).toBeInTheDocument();
    const titles = Array.from(resources.querySelectorAll("title"), (title) => title.textContent ?? "");
    expect(titles.some((title) => title.includes("RX 2.0 KB/s"))).toBe(true);
    expect(titles.some((title) => title.includes("TX 1.0 KB/s"))).toBe(true);
    const summary = within(resources).getByText("Exact samples (3)");
    await user.click(summary);
    const scroller = within(resources).getByRole("region", { name: "Exact resource samples" });
    expect(scroller).toHaveAttribute("tabindex", "0");
    scroller.focus();
    expect(scroller).toHaveFocus();
    const table = within(resources).getByRole("table");
    expect(table).toHaveTextContent("CPU");
    expect(table).toHaveTextContent("RX/s");
    expect(table).toHaveTextContent("2.0 KB/s (2048 B/s)");
    expect(table).toHaveTextContent("1.0 MB (1048576 B)");
  });

  it("preserves irregular exact sample values in the accessible table", async () => {
    const user = userEvent.setup();
    const irregularStats: EventStats[] = [
      { ...stats[0], ts: 1_234, cpuPerc: 12.34567, memPerc: 23.45678, netRx: 100, netTx: 200 },
      { ...stats[1], ts: 3_234, cpuPerc: 87.65432, memPerc: 76.54321, netRx: 1_601, netTx: 201, diskUsed: 1_537 },
    ];
    const { findByTestId, getByRole } = render(() => (
      <StatsIcon events={[]} stats={irregularStats} turns={[]} usage={usage} />
    ));

    await user.click(getByRole("button", { name: "Task statistics" }));
    const resources = await findByTestId("resource-charts");
    await user.click(within(resources).getByText("Exact samples (2)"));
    const table = within(resources).getByRole("table");
    expect(table).toHaveTextContent("1970-01-01T00:00:03.234Z");
    expect(table).toHaveTextContent("87.65432%");
    expect(table).toHaveTextContent("76.54321%");
    expect(table).toHaveTextContent("751 B/s (750.5 B/s)");
    expect(table).toHaveTextContent("0.5 B/s (0.5 B/s)");
    expect(table).toHaveTextContent("1.5 KB (1537 B)");
  });

  it("does not infer network throughput from one sample", async () => {
    const user = userEvent.setup();
    const warn = vi.spyOn(console, "warn").mockImplementation(() => undefined);
    const { findByTestId, getByRole } = render(() => (
      <StatsIcon events={[]} stats={[stats[0]]} turns={[]} usage={usage} />
    ));

    await user.click(getByRole("button", { name: "Task statistics" }));
    const resources = await findByTestId("resource-charts");
    expect(resources).toHaveTextContent("Waiting for another sample");
    expect(resources).toHaveTextContent("No disk history available");
    expect(warn).not.toHaveBeenCalled();
  });
});

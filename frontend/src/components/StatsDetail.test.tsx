// Tests for the task statistics charts, resource samples, and their text alternatives.

import { afterEach, describe, it } from "node:test";
import { render, within } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";
import { expect, vi } from "@tests/expect";

import type { EventStats } from "@sdk/types.gen";

import type { TurnTiming } from "../timing";
import { StatsContent, type TaskUsageSummary } from "./StatsDetail";

const usage: TaskUsageSummary = {
  cacheReadInputTokens: 7_000,
  cacheWriteInputTokens: 2_000,
  costUSD: 0.125,
  inputTokens: 1_000,
  outputTokens: 500,
};

const turns: TurnTiming[] = [
  {
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
        reportedModel: "test-model",
      },
    },
    changeStat: null,
    waitMs: 3_000,
  },
];

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
  {
    ts: 2_000,
    cpuPerc: 140.25,
    memUsed: 2_048,
    memLimit: 4_096,
    memPerc: 50,
    netRx: 2_048,
    netTx: 1_024,
    blockRead: 0,
    blockWrite: 0,
    diskUsed: 1_048_576,
  },
  {
    ts: 3_000,
    cpuPerc: 50,
    memUsed: 3_072,
    memLimit: 4_096,
    memPerc: 75,
    netRx: 3_072,
    netTx: 1_024,
    blockRead: 0,
    blockWrite: 0,
    diskUsed: -1,
  },
];

afterEach(() => vi.restoreAllMocks());

// Half the width of the longest pointer readout, mirroring the constant the
// resource charts use to keep the readout inside the frame.
const readoutHalfWidth = 34;

// Observable Plot reads pointer coordinates from the event and maps them over
// its own pixel geometry, so a synthetic move is enough to focus a sample. The
// resource charts are a few dozen pixels tall and pointerX ignores the vertical
// distance, so one fixed vertical position serves every chart.
function hover(chart: Element, clientX: number): void {
  chart.dispatchEvent(new MouseEvent("pointermove", { clientX, clientY: 29, bubbles: true }));
}

// Plot positions text marks with a translate() transform.
function readoutCenter(chart: Element): number {
  const transform = chart.querySelector('[aria-label="text"] text')?.getAttribute("transform") ?? "";
  return Number(/translate\(([-\d.]+)/u.exec(transform)?.[1] ?? Number.NaN);
}

describe("StatsContent", () => {
  it("separates token categories and reports cache efficiency", async () => {
    const events = [
      {
        kind: "toolUse",
        ts: 1_000,
        toolUse: { toolUseID: "tool-1", name: "Bash", input: {} },
      },
      {
        kind: "toolResult",
        ts: 2_000,
        toolResult: { toolUseID: "tool-1", duration: 1 },
      },
    ] as const;
    const { findByTestId, getByTestId } = render(() => (
      <StatsContent events={events} stats={[]} turns={turns} usage={usage} />
    ));

    const summary = getByTestId("task-usage-summary");
    expect(summary).toHaveTextContent("New input1.0kt");
    expect(summary).toHaveTextContent("Cache write2.0kt");
    expect(summary).toHaveTextContent("Cache read7.0kt");
    expect(summary).toHaveTextContent("Output500t");
    expect(summary).toHaveTextContent("Thinking200t");
    expect(summary).toHaveTextContent("Cache hit 70%");

    expect(await findByTestId("turn-token-chart", undefined, { timeout: 5_000 })).toBeInTheDocument();
    expect(await findByTestId("tool-time-chart", undefined, { timeout: 5_000 })).toBeInTheDocument();
  });

  it("keeps double-digit turns in chronological chart order without Plot warnings", async () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => undefined);
    const manyTurns = Array.from({ length: 12 }, () => turns[0]);
    const { findByTestId } = render(() => <StatsContent events={[]} stats={[]} turns={manyTurns} usage={usage} />);
    const chart = await findByTestId("turn-token-chart");
    const labels = Array.from(
      chart.querySelectorAll('[aria-label="x-axis tick label"] text'),
      (label) => label.textContent,
    );
    expect(labels).toEqual(Array.from({ length: 12 }, (_, i) => String(i + 1)));
    expect(warn).not.toHaveBeenCalled();
  });

  it("shows aligned resource history and network throughput", async () => {
    const user = userEvent.setup();
    const { findByLabelText, findByTestId } = render(() => (
      <StatsContent events={[]} stats={stats} turns={[]} usage={usage} />
    ));
    const resources = await findByTestId("resource-charts");
    expect(resources).toHaveTextContent("CPU50.0% · max 140.25%");
    expect(resources).toHaveTextContent("Memory3.0 KiB / 4.0 KiB");
    expect(resources).toHaveTextContent("NetworkRX 3.0 KiB · TX 1.0 KiB");
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
    expect(resources).toHaveTextContent("RX/s1.0 KiB/s");
    expect(resources).toHaveTextContent("TX/s0 B/s");
    expect(resources).not.toHaveTextContent("network chart shows throughput");
    expect(
      Array.from(rxChart.querySelectorAll('[aria-label="y-axis tick label"] text'), (label) => label.textContent),
    ).toEqual(["0 B/s", "2.0 KiB/s"]);
    expect(
      Array.from(txChart.querySelectorAll('[aria-label="y-axis tick label"] text'), (label) => label.textContent),
    ).toEqual(["0 B/s", "1.0 KiB/s"]);
    expect(rxChart.querySelector('[aria-label="y-axis tick label"]')).toHaveAttribute("text-anchor", "end");
    expect(txChart.querySelector('[aria-label="y-axis tick label"]')).toHaveAttribute("text-anchor", "start");
    expect(await findByLabelText("Writable disk usage over time")).toBeInTheDocument();
    // Both charts sample the same instants, so one pointer position reads both.
    hover(rxChart, 140);
    hover(txChart, 140);
    expect(rxChart).toHaveTextContent("RX 2.0 KiB/s");
    expect(txChart).toHaveTextContent("TX 1.0 KiB/s");
    const summary = within(resources).getByText("Exact samples (3)");
    await user.click(summary);
    const scroller = within(resources).getByRole("region", {
      name: "Exact resource samples",
    });
    expect(scroller).toHaveAttribute("tabindex", "0");
    scroller.focus();
    expect(scroller).toHaveFocus();
    const table = within(resources).getByRole("table");
    expect(table).toHaveTextContent("CPU");
    expect(table).toHaveTextContent("RX/s");
    expect(table).toHaveTextContent("2.0 KiB/s (2048 B/s)");
    expect(table).toHaveTextContent("1.0 MiB (1048576 B)");
  });

  it("reads resource samples through the crosshair without clipping the readout", async () => {
    const { findByLabelText } = render(() => <StatsContent events={[]} stats={stats} turns={[]} usage={usage} />);
    const cpuChart = await findByLabelText("CPU utilization over time");
    const width = Number(cpuChart.getAttribute("width"));
    const frameLeft = 40; // the CPU chart reserves room for its left axis
    const frameRight = width - 3;

    expect(readoutCenter(cpuChart)).toBeNaN();
    // The outermost samples sit on the frame edge, where a centered readout
    // would be clipped; it parks inside the frame instead.
    hover(cpuChart, frameLeft + 4);
    expect(cpuChart).toHaveTextContent("10.0%");
    expect(readoutCenter(cpuChart)).toBeGreaterThanOrEqual(frameLeft + readoutHalfWidth - 0.5);
    hover(cpuChart, frameRight - 4);
    expect(cpuChart).toHaveTextContent("50.0%");
    expect(readoutCenter(cpuChart)).toBeLessThanOrEqual(frameRight - readoutHalfWidth + 0.5);

    // Only the middle disk sample carries a measurement, and a sample without
    // one must not produce a readout.
    const diskChart = await findByLabelText("Writable disk usage over time");
    hover(diskChart, 6);
    expect(diskChart).not.toHaveTextContent("MB");
    hover(diskChart, 140);
    expect(diskChart).toHaveTextContent("1.0 MiB");
  });

  it("preserves irregular exact sample values in the accessible table", async () => {
    const user = userEvent.setup();
    const irregularStats: EventStats[] = [
      {
        ...stats[0],
        ts: 1_234,
        cpuPerc: 12.34567,
        memPerc: 23.45678,
        netRx: 100,
        netTx: 200,
      },
      {
        ...stats[1],
        ts: 3_234,
        cpuPerc: 87.65432,
        memPerc: 76.54321,
        netRx: 1_601,
        netTx: 201,
        diskUsed: 1_537,
      },
    ];
    const { findByTestId } = render(() => <StatsContent events={[]} stats={irregularStats} turns={[]} usage={usage} />);
    const resources = await findByTestId("resource-charts");
    await user.click(within(resources).getByText("Exact samples (2)"));
    const table = within(resources).getByRole("table");
    expect(table).toHaveTextContent("1970-01-01T00:00:03.234Z");
    expect(table).toHaveTextContent("87.65432%");
    expect(table).toHaveTextContent("76.54321%");
    expect(table).toHaveTextContent("751 B/s (750.5 B/s)");
    expect(table).toHaveTextContent("0.5 B/s (0.5 B/s)");
    expect(table).toHaveTextContent("1.5 KiB (1537 B)");
  });

  it("does not infer network throughput from one sample", async () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => undefined);
    const { findByTestId } = render(() => <StatsContent events={[]} stats={[stats[0]]} turns={[]} usage={usage} />);
    const resources = await findByTestId("resource-charts");
    expect(resources).toHaveTextContent("Waiting for another sample");
    expect(resources).toHaveTextContent("No disk history available");
    expect(warn).not.toHaveBeenCalled();
  });

  it("names each chart for assistive technology", async () => {
    const { findByRole } = render(() => <StatsContent events={[]} stats={stats} turns={turns} usage={usage} />);

    expect(await findByRole("img", { name: "CPU utilization over time" })).toBeInTheDocument();
    expect(await findByRole("img", { name: "Token volume by turn" })).toBeInTheDocument();
  });

  it("qualifies what the token chart aggregates", async () => {
    const { findByRole } = render(() => <StatsContent events={[]} stats={[]} turns={turns} usage={usage} />);

    expect(await findByRole("img", { name: "Token volume by turn" })).toHaveAttribute(
      "aria-description",
      expect.stringContaining("cache reads usually dominate"),
    );
  });

  it("switches the token chart between volume and each turn's share", async () => {
    const user = userEvent.setup();
    const { findByRole, findByTestId } = render(() => (
      <StatsContent events={[]} stats={[]} turns={turns} usage={usage} />
    ));
    const figure = await findByTestId("turn-token-chart");
    expect(await findByRole("img", { name: "Token volume by turn" })).toBeInTheDocument();

    await user.click(within(figure).getByLabelText("Show each turn as a share of its own tokens"));

    expect(await findByRole("img", { name: "Token share by turn" })).toBeInTheDocument();
    expect(
      Array.from(figure.querySelectorAll('[aria-label="y-axis tick label"] text'), (label) => label.textContent),
    ).toEqual(["0%", "50%", "100%"]);
  });

  it("lists the exact turn token counts as the chart's text alternative", async () => {
    const user = userEvent.setup();
    const { findByTestId } = render(() => <StatsContent events={[]} stats={[]} turns={turns} usage={usage} />);
    const figure = await findByTestId("turn-token-chart");

    await user.click(within(figure).getByText("Turn tokens (1)"));

    const table = within(figure).getByRole("table");
    expect(table).toHaveTextContent("Turn 1");
    expect(table).toHaveTextContent("7kt");
    expect(table).toHaveTextContent("11kt");
  });

  it("lists cumulative tool time as the chart's text alternative", async () => {
    const user = userEvent.setup();
    const events = [
      { kind: "toolUse", ts: 1_000, toolUse: { toolUseID: "tool-1", name: "Bash", input: {} } },
      { kind: "toolResult", ts: 2_000, toolResult: { toolUseID: "tool-1", duration: 1 } },
    ] as const;
    const { findByTestId } = render(() => <StatsContent events={events} stats={[]} turns={[]} usage={usage} />);
    const figure = await findByTestId("tool-time-chart");

    await user.click(within(figure).getByText("Tool timings (1)"));

    const table = within(figure).getByRole("table");
    expect(table).toHaveTextContent("Bash");
  });
});

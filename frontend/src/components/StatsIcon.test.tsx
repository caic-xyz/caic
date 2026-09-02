// Tests for compact task token totals and detailed per-invocation usage statistics.

import { render } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

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
    expect(await findByTestId("turn-token-chart")).toBeInTheDocument();
    expect(await findByTestId("tool-time-chart")).toBeInTheDocument();
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
});

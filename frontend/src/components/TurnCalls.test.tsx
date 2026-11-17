// Tests the per-call chart's common domains, exact-value table, cost marking, subagent actors, label spacing, omitted and unattributed notes, and per-turn-only line.

import { describe, it } from "node:test";
import { render, screen, within } from "@solidjs/testing-library";
import { expect } from "@tests/expect";
import { defined } from "@tests/history";

import type { HistoryTurnCall, HistoryTurnTimeline, ISOTimestamp } from "@sdk/types.gen";
import TurnCalls from "./TurnCalls";

function call(number: number, fields: Partial<HistoryTurnCall>): HistoryTurnCall {
  return {
    number,
    atMs: number * 1_000,
    inputTokens: 0,
    cacheCreationInputTokens: 0,
    cacheReadInputTokens: 0,
    outputTokens: 0,
    reasoningOutputTokens: 0,
    ...fields,
  };
}

function timeline(calls: HistoryTurnCall[], fields: Partial<HistoryTurnTimeline>): HistoryTurnTimeline {
  return {
    number: 1,
    startedAt: "2026-01-01T00:00:00Z" as ISOTimestamp,
    endedAt: "2026-01-01T00:01:00Z" as ISOTimestamp,
    durationMs: 60_000,
    modelMs: 0,
    spans: [],
    omitted: [],
    calls,
    tools: [],
    ...fields,
  };
}

function rows(): string[][] {
  const table = within(screen.getByRole("region", { name: "Call tokens" }));
  return table
    .getAllByRole("row")
    .slice(1)
    .map((row) => [...row.querySelectorAll("td")].map((cell) => cell.textContent ?? ""));
}

function shares(column: HTMLElement): string[] {
  return [...column.querySelectorAll<HTMLElement>("span")]
    .map((el) => el.style.getPropertyValue("--share"))
    .filter(Boolean);
}

describe("TurnCalls", () => {
  it("lists every call's exact tokens, context, and cost, and marks estimates", () => {
    render(() => (
      <TurnCalls
        costUpdating={false}
        timeline={timeline(
          [
            call(1, {
              atMs: 4_000,
              inputTokens: 10,
              cacheCreationInputTokens: 18_000,
              outputTokens: 400,
              contextWindow: 200_000,
              cost: { usd: 0.0132, source: "estimated" },
            }),
            call(2, {
              atMs: 9_000,
              source: { agentID: "agent-1" },
              activityID: "agent-1",
              inputTokens: 3_000,
              cacheReadInputTokens: 5,
              outputTokens: 60,
            }),
            call(3, {
              atMs: 15_000,
              inputTokens: 28,
              cacheCreationInputTokens: 400,
              cacheReadInputTokens: 18_000,
              outputTokens: 1_000,
              reasoningOutputTokens: 250,
              contextWindow: 200_000,
              cost: { usd: 0.25, source: "reported" },
            }),
          ],
          {
            spans: [{ kind: "subagent", startMs: 5_000, endMs: 10_000, label: "explore", activityID: "agent-1" }],
            // A cumulative report covering no single call still counts in the
            // summary's totals.
            unattributed: {
              inputTokens: 1_500,
              cacheCreationInputTokens: 0,
              cacheReadInputTokens: 0,
              outputTokens: 40,
              reasoningOutputTokens: 0,
            },
          },
        )}
      />
    ));

    expect(rows()).toEqual([
      ["1", "Agent", "4.000 s", "10", "18,000", "0", "400", "0", "18,010 / 200,000", "~$0.0132"],
      ["2", "explore", "9.000 s", "3,000", "0", "5", "60", "0", "3,005", "—"],
      ["3", "Agent", "15.000 s", "28", "400", "18,000", "1,000", "250", "18,428 / 200,000", "$0.2500"],
    ]);
    // The actor is text among numbers, so it reads from the left.
    const actorCells = screen.getAllByRole("cell", { name: "Agent" });
    expect(actorCells.map((cell) => cell.dataset.align)).toEqual(["text", "text"]);
    expect(screen.getByText("Call tokens, ~ marks estimated costs (3)")).toBeInTheDocument();
    expect(screen.getByTestId("turn-calls-summary")).toHaveTextContent(
      "cache read 44% of input · peak context 18.4kt of 200kt (9%) · 1.5kt output",
    );
    expect(screen.getByRole("img")).toHaveAccessibleName(/^Tokens per API call, 3 calls: cache read 44% of input/);
    expect(screen.getByTestId("turn-calls-unattributed")).toHaveTextContent(
      "Not attributed to a call: 1.5kt tokens from usage reports that cover no single call.",
    );

    // Input classes and windows share one domain, the 200,000-token window;
    // output has its own, the largest output.
    const columns = screen.getAllByTestId("turn-call");
    expect(columns).toHaveLength(3);
    const first = defined(columns[0]);
    const child = defined(columns[1]);
    const last = defined(columns[2]);
    expect(shares(first)).toEqual(["0", "0.09", "0.00005", "1", "0", "0.4"]);
    expect(shares(last)).toEqual(["0.09", "0.002", "0.00014", "1", "0.25", "0.75"]);
    expect(child.className).toMatch(/_child_/);
    expect(first.className).not.toMatch(/_child_/);
  });

  it("marks the call costs while estimates refresh", () => {
    render(() => (
      <TurnCalls
        costUpdating
        timeline={timeline(
          [call(1, { inputTokens: 10, outputTokens: 4, cost: { usd: 0.25, source: "estimated" } })],
          {},
        )}
      />
    ));

    const cells = within(screen.getByRole("region", { name: "Call tokens" })).getAllByRole("cell");
    expect(cells.filter((cell) => cell.getAttribute("aria-busy") === "true").map((cell) => cell.textContent)).toEqual([
      "~$0.2500",
    ]);
  });

  it("explains per-turn-only usage in one line instead of an empty chart", () => {
    render(() => <TurnCalls timeline={timeline([], {})} costUpdating={false} />);

    expect(screen.getByTestId("turn-calls-empty")).toHaveTextContent(
      "This harness reports token usage per turn, not per API call; the turn totals are in the details below.",
    );
    expect(screen.queryByRole("img")).not.toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Call tokens" })).not.toBeInTheDocument();
  });

  it("labels round positions of a dense turn and reports the calls the cap left out", () => {
    const calls = Array.from({ length: 120 }, (_, i) => call(i + 1, { inputTokens: 100, outputTokens: 1 }));
    render(() => (
      <TurnCalls
        costUpdating={false}
        timeline={timeline(calls, {
          omittedCalls: {
            count: 2,
            inputTokens: 1_500,
            cacheCreationInputTokens: 0,
            cacheReadInputTokens: 0,
            outputTokens: 2,
            reasoningOutputTokens: 0,
          },
        })}
      />
    ));

    const labels = screen
      .getAllByTestId("turn-call")
      .map((column) => column.lastElementChild?.textContent)
      .filter(Boolean);
    expect(labels).toEqual(["1", "25", "50", "75", "100", "120"]);
    expect(screen.getByTestId("turn-calls-omitted")).toHaveTextContent(
      "Not shown: 2 smaller calls (1.5kt tokens in total).",
    );
    expect(rows()).toHaveLength(120);
    expect(screen.getByTestId("turn-calls-summary")).toHaveTextContent(
      "cache read 0% of input · peak context 100t, window unknown · 122t output",
    );
  });
});

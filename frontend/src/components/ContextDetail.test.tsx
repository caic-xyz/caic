// Tests a context's end reason for compacted, cleared, and current sections, its turns on the task's strip scale, growth chart domain, exact values, totals, and stepping.

import { describe, it } from "node:test";
import { Show } from "solid-js";
import { render, screen, waitFor, within } from "@solidjs/testing-library";
import { Route, Router, useParams } from "@solidjs/router";
import userEvent from "@testing-library/user-event";
import { expect } from "@tests/expect";

import type { HistoryRunningTurn, HistorySection, HistoryTurn, ISOTimestamp } from "@sdk/types.gen";
import ContextDetail from "./ContextDetail";

function turn(number: number, sectionID: string, durationMs: number, contextTokens: number): HistoryTurn {
  return {
    recordID: number,
    index: 0,
    number,
    sectionID,
    startedAt: `2026-01-01T00:0${number}:00Z` as ISOTimestamp,
    at: `2026-01-01T00:0${number}:30Z` as ISOTimestamp,
    model: "test-model",
    durationMs,
    durationAPIMs: durationMs / 2,
    usage: {
      inputTokens: 1_000,
      outputTokens: 500,
      cacheCreationInputTokens: 2_000,
      cacheReadInputTokens: 10_000,
      reportedModel: "test-model",
      contextWindow: 200_000,
    },
    cost: { usd: 0.5, source: "reported" },
    waitMs: 1_000,
    contextTokens,
  };
}

function section(id: string, end: HistorySection["endBoundary"]): HistorySection {
  return { id, endBoundary: end, turnCount: 0, textCount: 4, toolCount: 7, durationMs: 0, skills: [] };
}

const sections: readonly HistorySection[] = [
  section("1", {
    recordID: 10,
    index: 0,
    message: {
      kind: "system",
      ts: Date.parse("2026-01-01T00:03:00Z"),
      system: { subtype: "compact_boundary", contextTokensBefore: 150_000, contextTokensAfter: 20_000 },
    },
  }),
  section("2", {
    recordID: 20,
    index: 0,
    message: { kind: "system", ts: Date.parse("2026-01-01T00:04:00Z"), system: { subtype: "context_cleared" } },
  }),
  section("3", undefined),
];

const turns: readonly HistoryTurn[] = [
  turn(1, "1", 20_000, 60_000),
  turn(2, "1", 40_000, 140_000),
  { ...turn(3, "2", 80_000, 0), cost: undefined },
];

const href = (id: string) => `/task/@abc/perf/contexts/${id}`;
const turnHref = (n: number) => `/task/@abc/perf/turns/${n}`;

function ContextRoute(props: { running: HistoryRunningTurn | null }) {
  const params = useParams();
  const current = () => sections.find((candidate) => candidate.id === params.section);
  return (
    <Show when={current()}>
      {(value) => (
        <ContextDetail
          section={value()}
          sections={sections}
          turns={turns}
          running={props.running}
          href={href}
          turnHref={turnHref}
          costUpdating={false}
        />
      )}
    </Show>
  );
}

function renderContext(id: string, running: HistoryRunningTurn | null) {
  window.history.replaceState(null, "", href(id));
  return render(() => (
    <Router>
      <Route path="/task/:taskId/perf/contexts/:section" component={() => <ContextRoute running={running} />} />
    </Router>
  ));
}

function detail(label: string): string {
  const row = screen.getByRole("rowheader", { name: label }).closest("tr");
  return row?.querySelector("td")?.textContent ?? "";
}

function cells(): string[][] {
  const table = within(screen.getByRole("region", { name: "Turns in this context" }));
  return table
    .getAllByRole("row")
    .slice(1)
    .map((row) => [...row.querySelectorAll("td")].map((cell) => cell.textContent ?? ""));
}

describe("ContextDetail", () => {
  it("explains a compacted context and plots its growth against the window and the compaction level", () => {
    renderContext("1", null);

    expect(screen.getByTestId("context-end")).toHaveTextContent("Ended by compaction from 150,000 to 20,000 tokens");
    expect(screen.getByTestId("context-summary-line")).toHaveTextContent("2 turns · 1:00 turn time · $1 · 27kt tokens");

    // The strip holds this context's turns on the task's scale.
    const strip = within(screen.getByTestId("turn-strip"));
    const bars = strip.getAllByRole("link");
    expect(bars.map((bar) => bar.getAttribute("aria-label"))).toEqual(["Turn 1, 20.000 s", "Turn 2, 40.000 s"]);
    expect(bars[1]?.querySelector<HTMLElement>("span")?.style.getPropertyValue("--turn-share")).toBe("0.5");

    const columns = screen.getAllByTestId("context-growth-turn");
    expect(columns).toHaveLength(2);
    const share = (column: HTMLElement | undefined, kind: RegExp) =>
      [...(column?.querySelectorAll<HTMLElement>("span") ?? [])]
        .find((span) => kind.test(span.className))
        ?.style.getPropertyValue("--share");
    // The domain reaches the window, the largest value.
    expect(share(columns[0], /_fill_/)).toBe("0.3");
    expect(share(columns[1], /_fill_/)).toBe("0.7");
    expect(share(columns[1], /_window_/)).toBe("1");
    expect(share(columns[1], /_compacted_/)).toBe("0.75");
    expect(screen.getByTestId("context-growth")).toHaveTextContent(
      "60kt after turn 1 → 140kt after turn 2 · peak 140kt",
    );

    expect(cells()).toEqual([
      ["1", "20.000 s", "60,000", "200,000", "1,000", "2,000", "10,000", "500", "$0.5000"],
      ["2", "40.000 s", "140,000", "200,000", "1,000", "2,000", "10,000", "500", "$0.5000"],
    ]);
    expect(detail("Model")).toBe("test-model");
    expect(detail("Turn time")).toBe("60.000 s");
    expect(detail("Time awaiting user")).toBe("2.000 s");
    expect(detail("Messages")).toBe("4");
    expect(detail("Tool calls")).toBe("7");
    expect(detail("Cache read tokens")).toBe("20,000");
    expect(detail("Cost")).toBe("$1");
  });

  it("explains a cleared context without claiming an unknown size or a partial cost", () => {
    renderContext("2", null);

    expect(screen.getByTestId("context-end")).toHaveTextContent("Ended by a context clear");
    expect(document.querySelector('[class*="_compacted_"]')).toBeNull();
    expect(screen.getByTestId("context-growth")).toHaveTextContent("Context sizes are unknown for these turns.");
    expect(cells()[0]?.[2]).toBe("Unknown");
    expect(screen.getByTestId("context-summary-line")).not.toHaveTextContent("$");
    expect(detail("Cost")).toBe("Unavailable");
    expect(screen.getByRole("rowheader", { name: "Ended" })).toBeInTheDocument();
  });

  it("shows the current context's running turn and no end", () => {
    const running: HistoryRunningTurn = {
      number: 4,
      sectionID: "3",
      startedAt: new Date(Date.now() - 10_000).toISOString() as ISOTimestamp,
    };
    renderContext("3", running);

    expect(screen.getByTestId("context-end")).toHaveTextContent("Current context");
    expect(screen.queryByRole("rowheader", { name: "Ended" })).not.toBeInTheDocument();
    expect(
      within(screen.getByTestId("turn-strip")).getByRole("link", { name: /^Turn 4, running, \d+\.\d{3} s so far$/ }),
    ).toHaveAttribute("href", turnHref(4));
    expect(screen.queryByTestId("context-growth")).not.toBeInTheDocument();
  });

  it("steps between contexts", async () => {
    const user = userEvent.setup();
    renderContext("1", null);

    expect(screen.getByRole("button", { name: "Previous context" })).toBeDisabled();
    await user.click(screen.getByRole("button", { name: "Next context" }));
    await waitFor(() => expect(window.location.pathname).toBe(href("2")));
    expect(screen.getByTestId("context-end")).toHaveTextContent("context clear");
    document.body.focus();
    await user.keyboard("{ArrowLeft}");
    await waitFor(() => expect(window.location.pathname).toBe(href("1")));
  });
});

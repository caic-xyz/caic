// Tests the turn strip's round-tick axis, running bar, turn labels, second lane, current-turn marking, links, single Tab stop, empty, single, and dense cases, and linear rendering of dense strips.

import { describe, it } from "node:test";
import { fireEvent, render, screen } from "@solidjs/testing-library";
import { Route, Router } from "@solidjs/router";
import { expect } from "@tests/expect";

import type { HistoryRunningTurn, HistoryTurn, ISOTimestamp } from "@sdk/types.gen";
import TurnStrip, { type TurnStripLane } from "./TurnStrip";

function turn(number: number, durationMs: number): HistoryTurn {
  return {
    recordID: number,
    index: 0,
    number,
    sectionID: "1",
    startedAt: "2026-01-01T00:00:00Z" as ISOTimestamp,
    at: "2026-01-01T00:00:00Z" as ISOTimestamp,
    model: "model",
    durationMs,
    durationAPIMs: 0,
    usage: { inputTokens: 0, outputTokens: 0, cacheCreationInputTokens: 0, cacheReadInputTokens: 0, reportedModel: "" },
  };
}

const href = (n: number) => `/task/@abc/perf/turns/${n}`;

function renderStrip(
  turns: readonly HistoryTurn[],
  current: number | null,
  running: HistoryRunningTurn | null,
  runningMs: number | null,
  lane?: TurnStripLane,
) {
  window.history.replaceState(null, "", current === null ? "/task/@abc/perf" : href(current));
  return render(() => (
    <Router>
      <Route
        path="*"
        component={() => (
          <TurnStrip turns={turns} running={running} runningMs={runningMs} lane={lane} current={current} href={href} />
        )}
      />
    </Router>
  ));
}

// axisLabels lists each axis's tick labels from the baseline up.
function axisLabels(): string[][] {
  const scale = screen.getByTestId("turn-strip").parentElement?.firstElementChild;
  return [...(scale?.children ?? [])].map((axis) => [...axis.children].map((tick) => tick.textContent ?? ""));
}

function columnLabels(): string[] {
  return [...screen.getByTestId("turn-strip").children].map((item) => item.lastElementChild?.textContent ?? "");
}

function share(link: HTMLElement): string {
  return link.querySelector<HTMLElement>("span")?.style.getPropertyValue("--turn-share") ?? "";
}

describe("TurnStrip", () => {
  it("renders a dense strip in time linear in its turns", () => {
    const count = 1000;
    let reads = 0;
    // Counts reads of the turns, so a scan per bar shows as count² reads.
    const turns = new Proxy(
      Array.from({ length: count }, (_, i) => turn(i + 1, 1_000)),
      {
        get(target, key, receiver) {
          if (typeof key === "string" && /^\d+$/.test(key)) reads++;
          return Reflect.get(target, key, receiver);
        },
      },
    );
    renderStrip(turns, 900, null, null);
    expect(screen.getAllByRole("link")).toHaveLength(count);
    expect(reads).toBeLessThan(20 * count);
  });

  it("scales every bar to a round axis over the longest turn and marks the current one", () => {
    renderStrip([turn(1, 15_000), turn(2, 60_000), turn(3, 0)], 2, null, null);

    const first = screen.getByRole("link", { name: "Turn 1, 15.000 s" });
    const current = screen.getByRole("link", { name: "Turn 2, 60.000 s" });
    const empty = screen.getByRole("link", { name: "Turn 3, 0.000 s" });
    expect(first).toHaveAttribute("href", href(1));
    expect(share(first)).toBe("0.25");
    expect(share(current)).toBe("1");
    expect(share(empty)).toBe("0");
    expect(current).toHaveAttribute("aria-current", "page");
    expect(current.className).toMatch(/_current_/);
    expect(first).not.toHaveAttribute("aria-current");
    expect(first.className).not.toMatch(/_current_/);
    // At most three round intervals reach the longest turn.
    expect(axisLabels()).toEqual([["0", "0:30", "1:00"]]);
  });

  it("marks no bar in the overview", () => {
    renderStrip([turn(1, 1_000), turn(2, 2_000)], null, null, null);
    for (const link of screen.getAllByRole("link")) {
      expect(link).not.toHaveAttribute("aria-current");
      expect(link.className).not.toMatch(/_current_/);
    }
  });

  it("is one Tab stop at the current turn", () => {
    renderStrip([turn(1, 1_000), turn(2, 2_000), turn(3, 3_000)], 2, null, null);
    const tabbable = screen.getAllByRole("link").filter((link) => link.tabIndex === 0);
    expect(tabbable).toHaveLength(1);
    expect(tabbable[0]).toHaveAccessibleName("Turn 2, 2.000 s");
    expect(screen.getByRole("link", { name: "Turn 1, 1.000 s" })).toHaveAttribute("tabindex", "-1");
  });

  it("moves the overview's Tab stop with the arrow, Home, and End keys", () => {
    renderStrip([turn(1, 1_000), turn(2, 2_000), turn(3, 3_000)], null, null, null);
    const tabStops = () =>
      screen
        .getAllByRole("link")
        .filter((link) => link.tabIndex === 0)
        .map((link) => link.getAttribute("href"));
    expect(tabStops()).toEqual([href(1)]);

    const first = screen.getByRole("link", { name: "Turn 1, 1.000 s" });
    first.focus();
    fireEvent.keyDown(first, { key: "ArrowRight" });
    expect(document.activeElement).toHaveAccessibleName("Turn 2, 2.000 s");
    expect(tabStops()).toEqual([href(2)]);

    fireEvent.keyDown(document.activeElement as Element, { key: "End" });
    expect(document.activeElement).toHaveAccessibleName("Turn 3, 3.000 s");
    fireEvent.keyDown(document.activeElement as Element, { key: "ArrowRight" });
    expect(document.activeElement).toHaveAccessibleName("Turn 3, 3.000 s");
    fireEvent.keyDown(document.activeElement as Element, { key: "Home" });
    expect(document.activeElement).toHaveAccessibleName("Turn 1, 1.000 s");
    expect(tabStops()).toEqual([href(1)]);
  });

  it("grows the running turn's bar after the completed turns", () => {
    const running: HistoryRunningTurn = {
      number: 3,
      sectionID: "1",
      startedAt: "2026-01-01T00:00:00Z" as ISOTimestamp,
    };
    renderStrip([turn(1, 60_000), turn(2, 20_000)], 3, running, 30_000);

    const bar = screen.getByRole("link", { name: "Turn 3, running, 30.000 s so far" });
    expect(bar).toHaveAttribute("href", href(3));
    expect(bar).toHaveAttribute("aria-current", "page");
    expect(bar.className).toMatch(/_running_/);
    // The bar is as long as the caller's clock says, on the shared scale.
    expect(share(bar)).toBe("0.5");
    expect(bar).toHaveAttribute("title", "Turn 3 · running · 0:30");
    expect(screen.getAllByRole("link").at(-1)).toBe(bar);
  });

  it("labels every column of a short part of the task and round turns of a long one", () => {
    const short = renderStrip(
      [61, 62, 63].map((n) => turn(n, 1_000)),
      null,
      null,
      null,
    );
    expect(columnLabels()).toEqual(["61", "62", "63"]);
    short.unmount();
    renderStrip(
      Array.from({ length: 40 }, (_, i) => turn(i + 1, 1_000)),
      null,
      null,
      null,
    );
    expect(columnLabels().filter(Boolean)).toEqual(["10", "20", "30", "40"]);
  });

  it("draws a second lane in the same columns on its own axis, empty for the running turn", () => {
    const running: HistoryRunningTurn = {
      number: 3,
      sectionID: "1",
      startedAt: "2026-01-01T00:00:00Z" as ISOTimestamp,
    };
    renderStrip([turn(1, 1_000), turn(2, 2_000)], null, running, 1_000, {
      top: 400,
      step: 200,
      format: (value) => `${value}u`,
      cell: (item) => <span data-testid="lane-cell">{item.number}</span>,
    });
    expect(screen.getAllByTestId("lane-cell").map((cell) => cell.textContent)).toEqual(["1", "2"]);
    expect(axisLabels()).toEqual([
      ["0", "0:01", "0:02"],
      ["0", "200u", "400u"],
    ]);
  });

  it("explains an empty task instead of drawing an axis", () => {
    renderStrip([], null, null, null);
    expect(screen.getByText("No completed turns yet.")).toBeInTheDocument();
    expect(screen.queryByTestId("turn-strip")).not.toBeInTheDocument();
  });

  it("draws a single turn at full height when its duration is round", () => {
    renderStrip([turn(1, 6_000)], 1, null, null);
    expect(share(screen.getByRole("link", { name: "Turn 1, 6.000 s" }))).toBe("1");
  });

  it("keeps every turn of a long task reachable in order", () => {
    const turns = Array.from({ length: 200 }, (_, i) => turn(i + 1, (i % 7) * 1_000));
    renderStrip(turns, 150, null, null);
    const links = screen.getAllByRole("link");
    expect(links).toHaveLength(200);
    expect(links[9]).toHaveAccessibleName("Turn 10, 2.000 s");
    expect(links[149]).toHaveAttribute("aria-current", "page");
  });
});

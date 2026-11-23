// Tests the tool-time breakdown's exact rows, shared bar scale, task totals without a longest call, and empty line.

import { describe, it } from "node:test";
import { render, screen, within } from "@solidjs/testing-library";
import { expect } from "@tests/expect";

import TurnTools from "./TurnTools";

describe("TurnTools", () => {
  it("lists each tool's calls, total, and longest time on one scale", () => {
    render(() => (
      <TurnTools
        tools={[
          { name: "Bash", calls: 3, durationMs: 80_000, longestMs: 60_000 },
          { name: "Read", calls: 12, durationMs: 20_000, longestMs: 1_437 },
        ]}
      />
    ));

    const rows = within(screen.getByRole("region", { name: "Tool time" })).getAllByTestId("turn-tool");
    expect(rows.map((row) => [...row.children].map((cell) => cell.textContent))).toEqual([
      ["Bash", "3", "80.000 s", "60.000 s"],
      ["Read", "12", "20.000 s", "1.437 s"],
    ]);
    const share = (row: HTMLElement) => row.querySelector<HTMLElement>("span")?.style.getPropertyValue("--share");
    expect(rows.map(share)).toEqual(["1", "0.25"]);
  });

  it("omits the longest column for task totals, which report none", () => {
    render(() => <TurnTools tools={[{ name: "Bash", calls: 3, durationMs: 80_000 }]} />);

    const table = screen.getByRole("region", { name: "Tool time" });
    expect(within(table).queryByRole("columnheader", { name: "Longest" })).not.toBeInTheDocument();
    expect([...within(table).getByTestId("turn-tool").children].map((cell) => cell.textContent)).toEqual([
      "Bash",
      "3",
      "80.000 s",
    ]);
  });

  it("states that there are no tool calls", () => {
    render(() => <TurnTools tools={[]} />);

    expect(screen.getByTestId("turn-tools-empty")).toHaveTextContent("No tool calls.");
    expect(screen.queryByRole("region", { name: "Tool time" })).not.toBeInTheDocument();
  });
});

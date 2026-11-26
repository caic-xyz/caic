// Tests tooltip scroll dismissal scope and bounded agent-supplied text.

import { it } from "node:test";
import { fireEvent, render, screen } from "@solidjs/testing-library";
import { expect } from "@tests/expect";

import Tooltip from "./Tooltip";

it("stays open while unrelated content scrolls and closes when its trigger scrolls", () => {
  render(() => (
    <>
      <div data-testid="list">
        <Tooltip text="Exact value">
          <span>value</span>
        </Tooltip>
      </div>
      <div data-testid="transcript" />
    </>
  ));
  const trigger = screen.getByRole("button", { name: "value" });
  fireEvent.focus(trigger);
  expect(screen.getByRole("tooltip")).toHaveTextContent("Exact value");

  // A streaming transcript elsewhere on the page keeps scrolling to its tail.
  fireEvent.scroll(screen.getByTestId("transcript"));
  expect(screen.getByRole("tooltip")).toBeInTheDocument();

  fireEvent.scroll(screen.getByTestId("list"));
  expect(screen.queryByRole("tooltip")).not.toBeInTheDocument();
  fireEvent.focus(trigger);
  fireEvent.scroll(document);
  expect(screen.queryByRole("tooltip")).not.toBeInTheDocument();
});

it("bounds an agent-supplied multi-line label", () => {
  const script = Array.from({ length: 40 }, (_, i) => `step ${i}: ${"x".repeat(300)}`).join("\n");
  render(() => (
    <Tooltip text={script}>
      <span>command</span>
    </Tooltip>
  ));
  fireEvent.focus(screen.getByRole("button", { name: "command" }));
  const lines = (screen.getByRole("tooltip").textContent ?? "").split("\n");
  expect(lines).toHaveLength(16);
  expect(Math.max(...lines.map((line) => line.length))).toBeLessThanOrEqual(140);
});

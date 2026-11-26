// Tests tooltip text bounds for long lines, many lines, and multi-line labels.

import { it } from "node:test";
import { expect } from "@tests/expect";

import { boundTooltipText, firstLine } from "./tooltipText";

it("keeps short text and bounds long lines and line counts", () => {
  expect(boundTooltipText("CPU: 1.00\nRAM: 2 GiB")).toBe("CPU: 1.00\nRAM: 2 GiB");
  const long = boundTooltipText("x".repeat(500));
  expect(long).toHaveLength(140);
  expect(long.endsWith("…")).toBe(true);
  const lines = boundTooltipText(Array.from({ length: 40 }, (_, i) => `line ${i}`).join("\n")).split("\n");
  expect(lines).toHaveLength(16);
  expect(lines[14]).toBe("line 14");
  expect(lines[15]).toBe("…");
});

it("names a multi-line command by its first line", () => {
  expect(firstLine("make test")).toBe("make test");
  expect(firstLine("git add a && git commit  \nLOG=$(mktemp)\nmake verify")).toBe("git add a && git commit …");
});

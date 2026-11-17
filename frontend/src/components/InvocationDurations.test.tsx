// Tests transcript duration links to turn and context Performance views.

import { describe, it } from "node:test";
import { render, screen } from "@solidjs/testing-library";
import { Route, Router } from "@solidjs/router";
import { expect } from "@tests/expect";

import type { HistorySection, HistoryTurn, ISOTimestamp } from "@sdk/types.gen";
import { ContextDurationLink, TurnDurationLink } from "./InvocationDurations";

const turn: HistoryTurn = {
  recordID: 1,
  index: 0,
  number: 4,
  sectionID: "1",
  startedAt: "2025-12-31T23:59:55Z" as ISOTimestamp,
  at: "2026-01-01T00:00:00Z" as ISOTimestamp,
  model: "test-model",
  cost: { usd: 0.125, source: "reported" },
  durationMs: 5000,
  durationAPIMs: 4000,
  usage: {
    inputTokens: 1000,
    outputTokens: 500,
    cacheCreationInputTokens: 2000,
    cacheReadInputTokens: 7000,
    reasoningOutputTokens: 200,
    reportedModel: "test-model",
  },
  changeStat: { files: 3, linesAdded: 14, linesDeleted: 2, binaryFiles: 1 },
  waitMs: 3000,
};

describe("TurnDurationLink", () => {
  it("links the turn time to the turn's Performance view", () => {
    window.history.replaceState(null, "", "/task/@abc+slug/diff");
    render(() => (
      <Router>
        <Route path="*" component={() => <TurnDurationLink turn={turn} />} />
      </Router>
    ));

    const link = screen.getByRole("link", { name: "Turn 4 performance, 0:05" });
    expect(link).toHaveAttribute("href", "/task/@abc+slug/perf/turns/4");
    expect(link).toHaveTextContent("0:05");
  });
});

describe("ContextDurationLink", () => {
  it("links an archived context's time to its Performance view", () => {
    window.history.replaceState(null, "", "/task/@abc+slug");
    const section: HistorySection = {
      id: "2",
      turnCount: 3,
      textCount: 4,
      toolCount: 5,
      durationMs: 75_000,
      skills: [],
    };
    render(() => (
      <Router>
        <Route path="*" component={() => <ContextDurationLink section={section} />} />
      </Router>
    ));

    const link = screen.getByRole("link", { name: "Context 2 performance, 1:15" });
    expect(link).toHaveAttribute("href", "/task/@abc+slug/perf/contexts/2");
  });
});

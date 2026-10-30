// Tests per-turn invocation details surfaced from a completed result card.

import { describe, it } from "node:test";
import { render, screen } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";
import { expect } from "@tests/expect";

import type { HistoryTurn, ISOTimestamp } from "@sdk/types.gen";
import TurnInvocationIcon, { SessionInvocationIcon } from "./TurnInvocationIcon";

const turn: HistoryTurn = {
  recordID: 1,
  index: 0,
  sectionID: "1",
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

describe("TurnInvocationIcon", () => {
  it("opens the completed turn's timing, cost, and token details", async () => {
    const user = userEvent.setup();
    render(() => <TurnInvocationIcon turn={turn} model={null} />);

    await user.click(screen.getByRole("button", { name: "Turn invocation details" }));

    const dialog = screen.getByTestId("turn-invocation-dialog");
    expect(dialog).toHaveTextContent("test-model");
    expect(dialog).toHaveTextContent("Turn time0:05");
    expect(dialog).toHaveTextContent("API time0:04");
    expect(dialog).toHaveTextContent("User wait0:03");
    expect(dialog).toHaveTextContent("Generated change3 files · +14 −2 · 1 binary");
    expect(dialog).toHaveTextContent("Cost$0.13");
    expect(dialog).toHaveTextContent("New input1.0kt");
    expect(dialog).toHaveTextContent("Cache write2.0kt");
    expect(dialog).toHaveTextContent("Cache read7.0kt");
    expect(dialog).toHaveTextContent("Output500t");
    expect(dialog).toHaveTextContent("Thinking200t");

    await user.click(screen.getByRole("button", { name: "Close turn details" }));
    expect(screen.queryByTestId("turn-invocation-dialog")).not.toBeInTheDocument();
  });

  it("uses the configured model and omits metrics the runtime did not report", async () => {
    const user = userEvent.setup();
    const incomplete = {
      ...turn,
      model: "session-model",
      cost: undefined,
      durationAPIMs: 0,
      usage: { ...turn.usage, reportedModel: "" },
      waitMs: undefined,
    } satisfies HistoryTurn;
    render(() => <TurnInvocationIcon turn={incomplete} model="configured-model" />);

    await user.click(screen.getByRole("button", { name: "Turn invocation details" }));

    const dialog = screen.getByTestId("turn-invocation-dialog");
    expect(dialog).toHaveTextContent("session-model");
    expect(dialog).not.toHaveTextContent("API time");
    expect(dialog).not.toHaveTextContent("User wait");
    expect(dialog).not.toHaveTextContent("Cost");
    expect(dialog).not.toHaveTextContent("Unavailable");
  });
});

describe("SessionInvocationIcon", () => {
  it("sums turn costs and identifies an estimate", async () => {
    const user = userEvent.setup();
    const estimated = {
      ...turn,
      cost: { usd: 0.25, source: "estimated" },
    } satisfies HistoryTurn;
    render(() => <SessionInvocationIcon turns={[turn, estimated]} model={null} compactionSummary="" />);

    await user.click(screen.getByRole("button", { name: "Session invocation details" }));
    expect(screen.getByTestId("session-invocation-dialog")).toHaveTextContent("Estimated cost$0.38");
  });

  it("does not present a partial total as a session cost", async () => {
    const user = userEvent.setup();
    const unknown = {
      ...turn,
      cost: undefined,
    } satisfies HistoryTurn;
    render(() => <SessionInvocationIcon turns={[turn, unknown]} model={null} compactionSummary="" />);

    await user.click(screen.getByRole("button", { name: "Session invocation details" }));
    expect(screen.getByTestId("session-invocation-dialog")).not.toHaveTextContent("Cost");
  });
});

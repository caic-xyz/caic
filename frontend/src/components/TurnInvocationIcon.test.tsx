// Tests per-turn invocation details surfaced from a completed result card.

import { render, screen } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import type { TurnTiming } from "../timing";
import TurnInvocationIcon from "./TurnInvocationIcon";

const turn: TurnTiming = {
  event: { kind: "result", ts: 5_000 },
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
  changeStat: { files: 3, added: 14, deleted: 2, binaryFiles: 1 },
  waitMs: 3_000,
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
      reportedModel: "session-model",
      result: {
        ...turn.result,
        totalCostUSD: 0,
        durationAPI: 0,
        usage: { ...turn.result.usage, reportedModel: "" },
      },
      waitMs: null,
    } satisfies TurnTiming;
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

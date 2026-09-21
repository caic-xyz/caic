// Tests native activity card accessibility, explicit batch scope, background marking, and stale running observations.

import { afterEach, describe, it } from "node:test";
import { render, screen, cleanup } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";
import { expect } from "@tests/expect";
import NativeAgents, { NativeAgentCard } from "./NativeSubagents";
import type { NativeActivity } from "../nativeSubagents";

afterEach(cleanup);

const activities: NativeActivity[] = [
  {
    id: "joker-1",
    toolUseID: "spawn",
    groupID: "g",
    label: "Joker",
    prompt: "Tell a README joke",
    status: "completed",
    result: "Read me before you judge me.",
    startedAt: 1000,
    endedAt: 3000,
    anchorTs: 3000,
  },
  {
    id: "b",
    status: "running",
    startedAt: 2000,
    endedAt: null,
    background: true,
    anchorTs: 2000,
  },
  {
    id: "batch",
    scope: "batch",
    label: "Parallel review",
    status: "running",
    startedAt: 2000,
    endedAt: null,
    anchorTs: 2000,
  },
  { id: "unknown", status: "unknown", startedAt: null, endedAt: null, anchorTs: 0 },
  {
    id: "paused",
    scope: "batch",
    label: "Paused workflow",
    status: "paused",
    startedAt: 1000,
    endedAt: null,
    anchorTs: 2000,
  },
  {
    id: "failed",
    label: "Failure",
    status: "failed",
    result: "Permission denied",
    startedAt: 1000,
    endedAt: 2000,
    anchorTs: 2000,
  },
];

describe("NativeAgents", () => {
  it("exposes expandable cards with their identity and result", async () => {
    render(() => <NativeAgents activities={activities} settled={false} />);
    const card = screen.getByText("Subagent: Joker").closest("details");
    const summary = card?.querySelector("summary");
    if (!card || !summary) throw new Error("Subagent card must have a details summary");
    const user = userEvent.setup();
    await user.click(summary);
    expect(screen.getByText("Read me before you judge me.")).toBeVisible();
    expect(screen.getByText("Tell a README joke")).toBeVisible();
    expect(card).toContainElement(screen.getByText("joker-1"));
    expect(screen.getByText("Batch: Paused workflow")).toBeInTheDocument();
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
  });

  it("marks a detached run and keeps its data attribute", () => {
    render(() => <NativeAgents activities={activities} settled={false} />);
    expect(screen.getByText("background")).toBeInTheDocument();
    const detached = screen.getByTestId("native-subagents").querySelector('[data-background="true"]');
    expect(detached).not.toBeNull();
    expect(detached?.getAttribute("data-native-id")).toBe("b");
  });

  it("renders nothing when no activity is anchored", () => {
    render(() => <NativeAgents activities={[]} settled={false} />);
    expect(screen.queryByTestId("native-subagents")).not.toBeInTheDocument();
  });

  it("settled replay does not claim active work or fabricate an interruption", () => {
    render(() => (
      <>
        <NativeAgentCard activity={activities[1]} settled={true} />
        <NativeAgentCard activity={activities[3]} settled={true} />
        <NativeAgentCard activity={activities[4]} settled={true} />
        <NativeAgentCard activity={activities[5]} settled={true} />
      </>
    ));
    expect(screen.getByText("Last observed running · outcome unknown")).toBeInTheDocument();
    expect(screen.getByText("Status unknown")).toBeInTheDocument();
    expect(screen.getByText("Failed")).toBeInTheDocument();
    // A paused resumable run is reported as paused, never as an interruption.
    expect(screen.getByText("Paused")).toBeInTheDocument();
    expect(screen.queryByText("Interrupted")).not.toBeInTheDocument();
  });
});

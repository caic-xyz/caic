// Tests native activity accessibility, explicit batch scope, counts, results, and stale running observations.

import { render, screen, cleanup } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";
import NativeSubagents from "./NativeSubagents";
import type { NativeActivity } from "../nativeSubagents";

afterEach(cleanup);

const activities: NativeActivity[] = [
  {
    id: "a",
    toolUseID: "spawn",
    groupID: "g",
    label: "Joker",
    prompt: "Tell a README joke",
    status: "completed",
    result: "Read me before you judge me.",
    startedAt: 1000,
    endedAt: 3000,
  },
  { id: "b", status: "running", startedAt: 2000, endedAt: null },
  {
    id: "batch",
    scope: "batch",
    label: "Parallel review",
    status: "running",
    startedAt: 2000,
    endedAt: null,
  },
  { id: "unknown", status: "unknown", startedAt: null, endedAt: null },
  {
    id: "paused",
    scope: "batch",
    label: "Paused workflow",
    status: "paused",
    startedAt: 1000,
    endedAt: null,
  },
  {
    id: "failed",
    label: "Failure",
    status: "failed",
    result: "Permission denied",
    startedAt: 1000,
    endedAt: 2000,
  },
];

describe("NativeSubagents", () => {
  it("exposes expandable cards and separate agent/batch active counts", async () => {
    render(() => <NativeSubagents activities={activities} settled={false} />);
    expect(screen.getByRole("region", { name: "Native subagent activity" })).toBeInTheDocument();
    expect(screen.getByText("1 agents active · 1 batches active")).toBeInTheDocument();
    expect(screen.getByText("Native batch: Parallel review")).toBeInTheDocument();
    const user = userEvent.setup();
    const card = screen.getByText("Native agent: Joker").closest("details");
    const summary = card?.querySelector("summary");
    if (!card || !summary) throw new Error("Native agent card must have a details summary");
    await user.click(summary);
    expect(screen.getByText("Read me before you judge me.")).toBeVisible();
    expect(screen.getByText("Tell a README joke")).toBeVisible();
    expect(card).toContainElement(screen.getByText("spawn"));
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
  });

  it("settled replay does not claim active work or fabricate an interruption", () => {
    render(() => <NativeSubagents activities={activities} settled={true} />);
    expect(screen.getByText("0 agents active · 0 batches active")).toBeInTheDocument();
    expect(screen.getAllByText("Last observed running · outcome unknown")).toHaveLength(2);
    expect(screen.getByText("Status unknown")).toBeInTheDocument();
    expect(screen.getByText("Failed")).toBeInTheDocument();
    // A paused resumable run is reported as paused, never as an interruption.
    expect(screen.getByText("Paused")).toBeInTheDocument();
    expect(screen.queryByText("Interrupted")).not.toBeInTheDocument();
  });
});

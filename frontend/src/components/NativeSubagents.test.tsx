// Tests native activity and background-command card accessibility, scope and exit-code reporting, background marking, and stale running observations.

import { afterEach, describe, it } from "node:test";
import { render, screen, cleanup } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";
import { expect } from "@tests/expect";
import { BackgroundCommandCard, BackgroundCommandStatusChip, BackgroundCommands } from "./NativeSubagents";
import NativeAgents, { NativeAgentCard } from "./NativeSubagents";
import type { BackgroundCommandActivity, NativeActivity } from "../nativeSubagents";

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

afterEach(cleanup);

const commands: BackgroundCommandActivity[] = [
  {
    id: "claude:shell:b3xwrrd6m",
    toolUseID: "toolu_lint",
    label: "Install and run new lint in pitchpal",
    status: "completed",
    result: 'Background command "Install and run new lint in pitchpal" completed (exit code 0)',
    exitCode: 0,
    outputRef: "/tmp/claude-1000/tasks/b3xwrrd6m.output",
    startedAt: 1000,
    endedAt: 2600,
    anchorTs: 2600,
  },
  {
    id: "claude:shell:bhlpwfs1v",
    label: "Wait for pitchpal lint to finish",
    status: "running",
    startedAt: 2000,
    endedAt: null,
    anchorTs: 2000,
  },
  {
    id: "claude:shell:nocode",
    status: "failed",
    result: "Background command died (exit code 137)",
    exitCode: 137,
    startedAt: 3000,
    endedAt: 3100,
    anchorTs: 3100,
  },
];

describe("BackgroundCommands", () => {
  it("exposes expandable cards with identity, output, and result", async () => {
    render(() => <BackgroundCommands commands={commands} settled={false} />);
    const card = screen.getByText("Background command: Install and run new lint in pitchpal").closest("details");
    const summary = card?.querySelector("summary");
    if (!card || !summary) throw new Error("Background-command card must have a details summary");
    const user = userEvent.setup();
    await user.click(summary);
    expect(screen.getByText("claude:shell:b3xwrrd6m")).toBeVisible();
    expect(screen.getByText("/tmp/claude-1000/tasks/b3xwrrd6m.output")).toBeVisible();
    expect(screen.getByText(/completed \(exit code 0\)/)).toBeVisible();
    expect(card?.textContent).toContain("exit 0");
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
  });

  it("reports the exit code on the summary without claiming a failed command succeeded", () => {
    render(() => <BackgroundCommandCard command={commands[2]} settled={false} />);
    expect(screen.getByText("exit 137")).toBeInTheDocument();
    expect(screen.getByText("Failed")).toBeInTheDocument();
    expect(screen.getByTestId("background-command-card").getAttribute("data-command-id")).toBe("claude:shell:nocode");
  });

  it("renders nothing when no command is anchored", () => {
    render(() => <BackgroundCommands commands={[]} settled={false} />);
    expect(screen.queryByTestId("background-commands")).not.toBeInTheDocument();
  });

  it("a settled parent never promotes a running command to an outcome", () => {
    render(() => <BackgroundCommandCard command={commands[1]} settled={true} />);
    expect(screen.getByText("Last observed running · outcome unknown")).toBeInTheDocument();
    expect(screen.queryByText("Completed")).not.toBeInTheDocument();
  });

  it("the tool-row chip resolves from running in background to the exit code", () => {
    render(() => <BackgroundCommandStatusChip command={commands[1]} settled={false} />);
    expect(screen.getByTestId("background-command-chip")).toHaveTextContent("running in background");
    cleanup();
    render(() => <BackgroundCommandStatusChip command={commands[0]} settled={false} />);
    expect(screen.getByTestId("background-command-chip")).toHaveTextContent("exit 0");
    expect(screen.getByTestId("background-command-chip").getAttribute("data-state")).toBe("completed");
  });
});

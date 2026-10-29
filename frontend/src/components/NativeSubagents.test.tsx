// Tests lazy native disclosures, child updates and release, lifecycle results, and keyboard-accessible background cards.

import { afterEach, describe, it } from "node:test";
import { render, screen, cleanup } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";
import { createSignal, onCleanup } from "solid-js";
import { expect, vi } from "@tests/expect";
import { BackgroundCommandCard, BackgroundCommandStatusChip, BackgroundCommands } from "./NativeSubagents";
import NativeAgents, { NativeAgentCard } from "./NativeSubagents";
import type { BackgroundCommandActivity, NativeActivity } from "../nativeSubagents";

afterEach(cleanup);

const activities: [NativeActivity, NativeActivity, NativeActivity, NativeActivity, NativeActivity, NativeActivity] = [
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
  it("mounts child details only while open and reveals updates received while closed", async () => {
    const [activity, setActivity] = createSignal<NativeActivity>({
      ...activities[0],
      messages: [{ kind: "text", ts: 1, text: { text: "First response" } }],
    });
    const mounted = vi.fn();
    const released = vi.fn();
    render(() => (
      <NativeAgentCard
        activity={activity()}
        settled={true}
        renderTranscript={(groups) => {
          mounted();
          onCleanup(released);
          return (
            <p data-testid="child-response">
              {groups()
                .map((group) => group.content?.text ?? group.events[0]?.text?.text)
                .join("")}
            </p>
          );
        }}
      />
    ));
    const card = screen.getByTestId("native-subagent-card");
    const summary = card.querySelector("summary");
    if (!summary) throw new Error("Missing subagent summary");
    expect(mounted).not.toHaveBeenCalled();
    expect(screen.queryByTestId("child-response")).not.toBeInTheDocument();
    const user = userEvent.setup();
    await user.click(summary);
    expect(screen.getByTestId("child-response")).toHaveTextContent("First response");
    expect(mounted).toHaveBeenCalledTimes(1);
    await user.click(summary);
    expect(screen.queryByTestId("child-response")).not.toBeInTheDocument();
    expect(released).toHaveBeenCalledTimes(1);
    setActivity((previous) => ({
      ...previous,
      messages: [{ kind: "text", ts: 2, text: { text: "Latest response" } }],
    }));
    expect(mounted).toHaveBeenCalledTimes(1);
    await user.click(summary);
    expect(screen.getByTestId("child-response")).toHaveTextContent("Latest response");
    expect(mounted).toHaveBeenCalledTimes(2);
    expect(screen.getByTestId("native-subagent-card")).toBe(card);
  });

  it("preserves a distinct lifecycle result alongside partial child output", async () => {
    render(() => (
      <NativeAgentCard
        activity={{
          ...activities[0],
          result: "Finished research.",
          messages: [{ kind: "textDelta", ts: 1, textDelta: { text: "Partial research" } }],
        }}
        settled={true}
        renderTranscript={() => <p>Partial research</p>}
      />
    ));
    const summary = screen.getByTestId("native-subagent-card").querySelector("summary");
    if (!summary) throw new Error("Missing subagent summary");
    await userEvent.setup().click(summary);
    expect(screen.getByText("Partial research")).toBeInTheDocument();
    expect(screen.getByText("Finished research.")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Result" })).toBeInTheDocument();
  });

  it("omits only a lifecycle echo already represented by completed child text", async () => {
    render(() => (
      <NativeAgentCard
        activity={{
          ...activities[0],
          result: " Finished research. ",
          messages: [{ kind: "text", ts: 1, text: { text: "Finished research." } }],
        }}
        settled={true}
        renderTranscript={() => <p>Finished research.</p>}
      />
    ));
    const summary = screen.getByTestId("native-subagent-card").querySelector("summary");
    if (!summary) throw new Error("Missing subagent summary");
    await userEvent.setup().click(summary);
    expect(screen.getAllByText("Finished research.")).toHaveLength(1);
    expect(screen.queryByRole("heading", { name: "Result" })).not.toBeInTheDocument();
  });

  it("does not show a missing-result placeholder when child output exists", async () => {
    render(() => (
      <NativeAgentCard
        activity={{
          ...activities[0],
          result: "",
          messages: [{ kind: "textDelta", ts: 1, textDelta: { text: "Partial research" } }],
        }}
        settled={true}
        renderTranscript={() => <p>Partial research</p>}
      />
    ));
    const summary = screen.getByTestId("native-subagent-card").querySelector("summary");
    if (!summary) throw new Error("Missing subagent summary");
    await userEvent.setup().click(summary);
    expect(screen.queryByText("No result reported by the harness.")).not.toBeInTheDocument();
    expect(screen.getByText("Partial research")).toBeInTheDocument();
  });
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

const commands: [BackgroundCommandActivity, BackgroundCommandActivity, BackgroundCommandActivity] = [
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
  it("ticks a running command, uses its final duration, and hides stale elapsed time", () => {
    vi.useFakeTimers();
    vi.setSystemTime(5_000);
    const [command, setCommand] = createSignal(commands[1]);
    const [settled, setSettled] = createSignal(false);
    try {
      const view = render(() => <BackgroundCommandCard command={command()} settled={settled()} />);
      expect(screen.getByTestId("background-command-duration")).toHaveTextContent("0:03");
      vi.advanceTimersByTime(2_000);
      expect(screen.getByTestId("background-command-duration")).toHaveTextContent("0:05");
      setSettled(true);
      expect(screen.queryByTestId("background-command-duration")).not.toBeInTheDocument();
      setCommand({ ...commands[1], status: "completed", endedAt: 8_000 });
      vi.advanceTimersByTime(10_000);
      expect(screen.getByTestId("background-command-duration")).toHaveTextContent("0:06");
      view.unmount();
    } finally {
      vi.useRealTimers();
    }
  });

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
    render(() => <BackgroundCommandStatusChip command={commands[1]} settled={true} />);
    expect(screen.getByTestId("background-command-chip")).toHaveTextContent("Last observed running · outcome unknown");
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

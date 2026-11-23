// Tests timeline lanes and exact text, short-turn gridlines, a running turn's provisional time and in-place growth, keyboard span activation, conversation reveal, and transcript highlighting.

import { afterEach, beforeEach, describe, it } from "node:test";
import { render, screen, waitFor, within } from "@solidjs/testing-library";
import { Route, Router } from "@solidjs/router";
import { createSignal } from "solid-js";
import userEvent from "@testing-library/user-event";
import { expect, vi } from "@tests/expect";
import { defined, historyManifest } from "@tests/history";

import type { HistoryTurnTimeline, ISOTimestamp } from "@sdk/types.gen";
import { api } from "../api";
import { toolIdentity } from "../grouping";
import { TaskTimelineProvider, useTranscriptLinks } from "../TaskTimelineContext";
import { toolLinkKey, type TranscriptLinks, type TranscriptTarget } from "../transcriptLinks";
import TurnTimeline from "./TurnTimeline";

const startedAt = "2026-01-01T00:00:00Z" as ISOTimestamp;
const start = Date.parse(startedAt);
const child = { agentID: "agent-1" };

const timeline: HistoryTurnTimeline = {
  number: 2,
  startedAt,
  endedAt: "2026-01-01T00:01:00Z" as ISOTimestamp,
  durationMs: 60_000,
  modelMs: 20_000,
  spans: [
    { kind: "model", startMs: 0, endMs: 10_000 },
    {
      kind: "tool",
      startMs: 10_000,
      endMs: 30_000,
      label: "Bash",
      detail: "go test",
      status: "failed",
      toolUseID: "bash",
    },
    { kind: "subagent", startMs: 12_000, endMs: 28_000, label: "explore", status: "completed", activityID: "agent-1" },
    {
      kind: "tool",
      startMs: 13_000,
      endMs: 14_000,
      label: "Read",
      status: "completed",
      source: child,
      toolUseID: "read",
      activityID: "agent-1",
    },
    { kind: "model", startMs: 30_000, endMs: 40_000 },
    {
      kind: "background",
      startMs: 35_000,
      endMs: 3_660_000,
      label: "npm run dev",
      status: "completed",
      toolUseID: "serve",
      commandID: "cmd",
    },
    { kind: "tool", startMs: 35_000, endMs: 36_000, label: "Bash", status: "completed", toolUseID: "serve" },
    { kind: "tool", startMs: 40_000, endMs: 60_000, label: "Edit", status: "completed", toolUseID: "edit" },
  ],
  omitted: [{ kind: "tool", count: 3, durationMs: 1_500 }],
  calls: [],
  tools: [],
};

beforeEach(() => {
  vi.spyOn(api, "historyEvents").mockImplementation(() => ({ close: vi.fn() }) as unknown as EventSource);
  vi.spyOn(globalThis, "fetch").mockImplementation(async () => Response.json(historyManifest([], "")));
});

afterEach(() => vi.restoreAllMocks());

interface Revealed {
  target: TranscriptTarget;
  focus: boolean;
}

function renderTimeline(visible: boolean, initial: HistoryTurnTimeline, runningMs: number | null) {
  const [shown, setShown] = createSignal(initial);
  const revealed: Revealed[] = [];
  let links: TranscriptLinks | undefined;
  window.history.replaceState(null, "", "/task/@abc/perf/turns/2");
  const Probe = () => {
    links = useTranscriptLinks();
    links.setRevealer({ visible: () => visible, reveal: (target, focus) => revealed.push({ target, focus }) });
    return null;
  };
  render(() => (
    <Router>
      <Route
        path="/task/*"
        component={() => (
          <TaskTimelineProvider taskId="abc">
            <Probe />
            <TurnTimeline timeline={shown()} runningMs={runningMs} conversationHref="/task/@abc">
              <p>Resources</p>
            </TurnTimeline>
          </TaskTimelineProvider>
        )}
      />
    </Router>
  ));
  return { revealed, links: () => defined(links), setShown };
}

function span(name: RegExp): HTMLElement {
  return screen.getByRole("button", { name });
}

describe("TurnTimeline", () => {
  it("draws labelled lanes with exact values in a table", () => {
    renderTimeline(true, timeline, null);

    const lanes = screen.getAllByTestId("turn-timeline-lane").map((lane) => lane.firstElementChild?.textContent);
    expect(lanes).toEqual([
      "Model 0:20",
      "Tools 3 calls · 0:40",
      "Subagents 1",
      "Subagent tools 1 call",
      "Background 1",
    ]);
    expect(span(/^Tools: Bash go test, at 10\.000 s, lasts 20\.000 s, failed$/)).toBeInTheDocument();
    const outliving = span(/^Background: npm run dev, at 35\.000 s, ends 3,600\.000 s after the turn$/);
    expect(within(outliving).getByTestId("turn-timeline-beyond")).toBeInTheDocument();
    expect(screen.getByTestId("turn-timeline-omitted")).toHaveTextContent(
      "Not shown: 3 shorter tool calls (0:02 in total).",
    );
    const table = within(screen.getByRole("region", { name: "Timeline spans" }));
    const row = table.getByRole("button", { name: "npm run dev" }).closest("tr");
    expect(row).toHaveTextContent("Backgroundnpm run dev35.000 s3,660.000 s (after the turn)");
  });

  it("marks a running turn's latest model time provisional and grows in place", async () => {
    const user = userEvent.setup();
    const running: HistoryTurnTimeline = {
      number: 3,
      startedAt,
      durationMs: 12_000,
      modelMs: 7_000,
      spans: [
        { kind: "model", startMs: 0, endMs: 4_000 },
        { kind: "tool", startMs: 4_000, endMs: 9_000, label: "Bash", status: "completed", toolUseID: "a" },
        { kind: "model", startMs: 9_000 },
      ],
      omitted: [],
      calls: [],
      tools: [],
    };
    const { setShown } = renderTimeline(true, running, null);
    await waitFor(() => expect(screen.getAllByTestId("turn-timeline-span")).toHaveLength(3));

    expect(screen.getByText(/^now/)).toHaveTextContent("now 0:12");
    const latest = span(/^Model: Model time, at 9\.000 s, so far 3\.000 s/);
    expect(latest.className).toMatch(/_provisional_/);
    expect(screen.getByText("Resources")).toBeInTheDocument();

    await user.click(span(/^Tools: Bash/));
    const focused = document.activeElement;
    expect(focused).toBe(span(/^Tools: Bash/));
    // The next refresh ends the provisional span and adds a tool call; the
    // focused span keeps its element.
    setShown({
      ...running,
      durationMs: 20_000,
      spans: [
        { kind: "model", startMs: 0, endMs: 4_000 },
        { kind: "tool", startMs: 4_000, endMs: 9_000, label: "Bash", status: "completed", toolUseID: "a" },
        { kind: "model", startMs: 9_000, endMs: 15_000 },
        { kind: "tool", startMs: 15_000, label: "Edit", status: "running", toolUseID: "b" },
      ],
    });
    await waitFor(() => expect(screen.getAllByTestId("turn-timeline-span")).toHaveLength(4));
    expect(document.activeElement).toBe(focused);
    expect(span(/^Tools: Edit, at 15\.000 s, still running$/)).toBeInTheDocument();
    expect(screen.getByText(/^now/)).toHaveTextContent("now 0:20");
  });

  it("labels the gridlines of a short turn apart", async () => {
    renderTimeline(
      true,
      {
        ...timeline,
        durationMs: 2_000,
        spans: [{ kind: "model", startMs: 0, endMs: 2_000 }],
        omitted: [],
      },
      null,
    );
    await waitFor(() => expect(screen.getAllByTestId("turn-timeline-span")).toHaveLength(1));
    const axis = screen.getByTestId("turn-timeline-plot").firstElementChild;
    const labels = [...(axis?.children ?? [])].map((tick) => tick.textContent);
    expect(labels).toEqual(["0s", "0.5s", "1.0s", "1.5s", "end 0:02"]);
    // The plot's name states the axis end as shown, not the turn's duration.
    expect(screen.getByRole("group", { name: /^Turn 2 timeline, axis ends at 0:02; / })).toBe(
      screen.getByTestId("turn-timeline-plot"),
    );
  });

  it("extends a running turn's axis to the view's clock between refreshes", async () => {
    renderTimeline(
      true,
      {
        ...timeline,
        endedAt: undefined,
        durationMs: 12_000,
        spans: [{ kind: "model", startMs: 0 }],
        omitted: [],
      },
      15_400,
    );
    await waitFor(() => expect(screen.getAllByTestId("turn-timeline-span")).toHaveLength(1));
    expect(screen.getByText(/^now/)).toHaveTextContent("now 0:15");
    expect(screen.getByTestId("turn-timeline-plot")).toHaveAccessibleName(/^Turn 2 timeline, axis ends now, at 0:15; /);
    expect(span(/^Model: .*so far 15\.400 s$/)).toBeInTheDocument();
  });

  it("is one Tab stop whose arrow keys move between spans and whose Enter and Space reveal them", async () => {
    const user = userEvent.setup();
    const { revealed } = renderTimeline(true, timeline, null);
    await waitFor(() => expect(screen.getAllByTestId("turn-timeline-span")).toHaveLength(8));

    await user.tab();
    expect(document.activeElement).toBe(span(/^Model: Model time, from the turn start/));
    expect(screen.getAllByTestId("turn-timeline-span").filter((el) => el.tabIndex === 0)).toEqual([
      document.activeElement,
    ]);
    await user.keyboard("{ArrowRight}");
    expect(document.activeElement).toBe(span(/^Model: Model time, at 30\.000 s/));
    await user.keyboard("{ArrowDown}");
    expect(document.activeElement).toBe(span(/^Tools: Bash, at 35\.000 s/));
    await user.keyboard("{Enter}");
    expect(revealed).toEqual([
      {
        target: { toolKey: toolIdentity(undefined, "serve"), nativeID: null, ownerAgentID: null, at: start + 35_000 },
        focus: false,
      },
    ]);
    await user.keyboard("{ArrowDown}{ArrowDown}");
    expect(document.activeElement).toBe(span(/^Subagent tools: Read, at 13\.000 s/));
    await user.keyboard(" ");
    expect(revealed.at(-1)).toEqual({
      target: { toolKey: toolIdentity(child, "read"), nativeID: null, ownerAgentID: "agent-1", at: start + 13_000 },
      focus: false,
    });
    await user.keyboard("{ArrowUp}{Enter}");
    expect(revealed.at(-1)?.target).toEqual({
      toolKey: null,
      nativeID: "agent-1",
      ownerAgentID: null,
      at: start + 12_000,
    });
    // Inferred model time reveals the row holding the gap's last moment.
    await user.keyboard("{ArrowUp}{ArrowUp}{Home}{Enter}");
    expect(revealed.at(-1)?.target).toEqual({ toolKey: null, nativeID: null, ownerAgentID: null, at: start + 9_999 });
    expect(window.location.pathname).toBe("/task/@abc/perf/turns/2");
  });

  it("shows a hidden conversation, then reveals the span there with focus", async () => {
    const user = userEvent.setup();
    const { revealed } = renderTimeline(false, timeline, null);
    await waitFor(() => expect(screen.getAllByTestId("turn-timeline-span")).toHaveLength(8));

    await user.click(span(/^Tools: Edit/));
    expect(revealed).toEqual([
      {
        target: { toolKey: toolIdentity(undefined, "edit"), nativeID: null, ownerAgentID: null, at: start + 40_000 },
        focus: true,
      },
    ]);
    await waitFor(() => expect(window.location.pathname).toBe("/task/@abc"));
  });

  it("marks every span of the hovered transcript item", async () => {
    const { links } = renderTimeline(true, timeline, null);
    await waitFor(() => expect(screen.getAllByTestId("turn-timeline-span")).toHaveLength(8));
    const linked = () =>
      screen
        .getAllByTestId("turn-timeline-span")
        .filter((el) => /_linked_/.test(el.className))
        .map((el) => el.getAttribute("aria-label")?.split(",")[0]);

    links().setHovered(toolLinkKey(toolIdentity(undefined, "serve")));
    // The call and the command it launched share the transcript row.
    await waitFor(() => expect(linked()).toEqual(["Tools: Bash", "Background: npm run dev"]));
    links().setHovered(null);
    await waitFor(() => expect(linked()).toEqual([]));
  });
});

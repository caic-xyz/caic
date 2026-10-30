// Tests disclosure-only actor fetches, suffix updates, cancellation, and payload-free lifecycle summaries.
import { defined } from "@tests/history";
import assert from "node:assert/strict";
import { afterEach, describe, it } from "node:test";
import { render, screen, cleanup, waitFor } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";
import { createSignal } from "solid-js";
import { expect, vi } from "@tests/expect";
import { fixtureHistoryResponse, historyManifest, historyResponse } from "@tests/history";
import type { HistoryEvent, HistoryActivity, ISOTimestamp } from "@sdk/types.gen";
import type { HistoryEventsHandlers } from "@sdk/api.gen";
import { TaskTimelineProvider } from "../TaskTimelineContext";
import { api } from "../api";
import {
  BackgroundCommandCard,
  BackgroundCommandStatusChip,
  BackgroundCommands,
  NativeAgentCard,
} from "./NativeSubagents";
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.useRealTimers();
});
const lifecycle = (recordID: number, status: "running" | "completed", result: string): HistoryEvent => ({
  recordID,
  index: 0,
  message: {
    kind: "nativeSubagent",
    ts: recordID * 1000,
    nativeSubagent: { id: "child", label: "Reviewer", status, prompt: "Review the change", result },
  },
});
function actorFixture(events: HistoryEvent[]) {
  let handlers: HistoryEventsHandlers | null = null;
  const requests: string[] = [];
  vi.spyOn(api, "historyEvents").mockImplementation((_id, _cursor, value) => {
    handlers = value;
    return { close: vi.fn() } as unknown as EventSource;
  });
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    requests.push(url);
    return fixtureHistoryResponse(url, events, "");
  });
  const initial = defined(historyManifest(events, "").metadata.activities[0]);
  const [activity, setActivity] = createSignal(initial);
  const view = render(() => (
    <TaskTimelineProvider taskId="task">
      <NativeAgentCard
        activity={activity()}
        settled={false}
        renderTranscript={(groups) => (
          <p data-testid="child-transcript">
            <a href="https://example.com/response" rel="external">
              {groups()
                .map((group) => group.content?.text ?? group.events.findLast((event) => event.text)?.text?.text ?? "")
                .join("")}
            </a>
          </p>
        )}
      />
    </TaskTimelineProvider>
  ));
  const checkpoint = (event: HistoryEvent) => {
    events.push(event);
    const manifest = historyManifest(events, "");
    setActivity(defined(manifest.metadata.activities[0]));
    if (!handlers) throw new Error("Missing task stream");
    handlers.onMessage(event);
    handlers.onCheckpoint?.({ cursor: manifest.bootstrap.cursor, through: event.recordID });
    return manifest;
  };
  const publish = (manifest: ReturnType<typeof historyManifest>) => handlers?.onManifest?.(manifest);
  const advance = (event: HistoryEvent) => publish(checkpoint(event));
  return { view, requests, advance, checkpoint, publish };
}
describe("native disclosures", () => {
  it("keeps an open transcript while own output and unrelated routing grow in one manifest", async () => {
    const fixture = actorFixture([
      lifecycle(1, "running", ""),
      {
        recordID: 2,
        index: 0,
        message: { kind: "text", ts: 2000, source: { agentID: "child" }, text: { text: "First response" } },
      },
    ]);
    const user = userEvent.setup();
    await user.click(defined(screen.getByTestId("native-subagent-card").querySelector(":scope > summary")));
    const transcript = await screen.findByTestId("child-transcript");
    const link = defined(transcript.querySelector("a"));
    await user.tab();
    expect(link).toHaveFocus();
    fixture.checkpoint({
      recordID: 3,
      index: 0,
      message: { kind: "text", ts: 3000, source: { agentID: "child" }, text: { text: "Second response" } },
    });
    const manifest = fixture.checkpoint({
      recordID: 4,
      index: 0,
      message: {
        kind: "nativeSubagent",
        ts: 4000,
        nativeSubagent: { id: "other", status: "running", toolUseID: "other-spawn" },
      },
    });
    fixture.publish(manifest);
    await waitFor(() => expect(screen.getByTestId("child-transcript")).toHaveTextContent("Second response"));
    const prefixes = fixture.requests.filter((url) => url.includes("/activity/") && !url.includes("afterRecordID"));
    assert.deepEqual(prefixes, ["/history/owner/3/test/activity/nativeSubagent/child/2"]);
    assert.equal(screen.getByTestId("child-transcript"), transcript);
    expect(link).toHaveFocus();
    assert.ok(fixture.requests.some((url) => url.includes("afterRecordID=2&afterIndex=4294967295")));
    fixture.view.unmount();
  });
  it("reuses its finite prefix after unrelated root and actor routing activity", async () => {
    const fixture = actorFixture([
      lifecycle(1, "running", ""),
      {
        recordID: 2,
        index: 0,
        message: { kind: "text", ts: 2000, source: { agentID: "child" }, text: { text: "Cached response" } },
      },
    ]);
    const summary = defined(screen.getByTestId("native-subagent-card").querySelector(":scope > summary"));
    const user = userEvent.setup();
    await user.click(summary);
    await screen.findByText("Cached response");
    await user.click(summary);
    fixture.advance({ recordID: 3, index: 0, message: { kind: "text", ts: 3000, text: { text: "Root update" } } });
    await user.click(summary);
    await screen.findByText("Cached response");
    fixture.advance({
      recordID: 4,
      index: 0,
      message: {
        kind: "nativeSubagent",
        ts: 4000,
        nativeSubagent: { id: "other", status: "running", toolUseID: "other-spawn" },
      },
    });
    await waitFor(() => {
      const prefixes = fixture.requests.filter((url) => url.includes("/activity/") && !url.includes("afterRecordID"));
      assert.deepEqual(prefixes, [
        "/history/owner/3/test/activity/nativeSubagent/child/2",
        "/history/owner/3/test/activity/nativeSubagent/child/2",
      ]);
    });
    expect(screen.getByText("Cached response")).toBeInTheDocument();
    fixture.view.unmount();
  });
  it("fetches only while open, consumes suffixes, and reopens current content without retaining the body", async () => {
    const events = [
      lifecycle(1, "running", ""),
      {
        recordID: 2,
        index: 0,
        message: { kind: "text", ts: 2000, source: { agentID: "child" }, text: { text: "First response" } },
      } satisfies HistoryEvent,
    ];
    const fixture = actorFixture(events);
    await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
    assert.equal(fixture.requests.filter((url) => url.includes("/activity/")).length, 0);
    const card = screen.getByTestId("native-subagent-card");
    const summary = defined(card.querySelector("summary"));
    const user = userEvent.setup();
    await user.click(summary);
    await screen.findByText("First response");
    fixture.advance({
      recordID: 3,
      index: 0,
      message: { kind: "text", ts: 3000, source: { agentID: "child" }, text: { text: "Second response" } },
    });
    await waitFor(() => expect(screen.getByTestId("child-transcript")).toHaveTextContent("Second response"));
    assert.ok(
      fixture.requests.some((url) =>
        url.includes("/activity/nativeSubagent/child/3?afterRecordID=2&afterIndex=4294967295"),
      ),
    );
    await user.click(summary);
    expect(screen.queryByTestId("child-transcript")).not.toBeInTheDocument();
    fixture.advance(lifecycle(4, "completed", "Finished research"));
    await user.click(summary);
    await screen.findByText("Finished research");
    assert.equal(screen.getByTestId("native-subagent-card"), card);
    fixture.view.unmount();
  });
  it("keeps distinct lifecycle results and suppresses only a completed transcript echo", async () => {
    const fixture = actorFixture([
      lifecycle(1, "completed", "Final answer"),
      {
        recordID: 2,
        index: 0,
        message: { kind: "text", ts: 2000, source: { agentID: "child" }, text: { text: "Final answer" } },
      },
    ]);
    await userEvent.setup().click(defined(screen.getByTestId("native-subagent-card").querySelector("summary")));
    await screen.findByText("Final answer");
    expect(screen.getAllByText("Final answer")).toHaveLength(1);
    expect(screen.queryByRole("heading", { name: "Result" })).not.toBeInTheDocument();
    fixture.view.unmount();
  });
  it("cancels a gated actor read when closed and ignores its late body", async () => {
    const fixture = actorFixture([lifecycle(1, "running", "")]);
    await waitFor(() => assert.ok(fixture.requests.some((url) => url.endsWith("/manifest"))));
    const original = defined(vi.mocked(globalThis.fetch).getMockImplementation());
    let signal: AbortSignal | null = null;
    let release: (value: Response) => void = () => {};
    vi.mocked(globalThis.fetch).mockImplementation((input, init) => {
      if (!String(input).includes("/activity/")) return original(input, init);
      signal = init?.signal ?? null;
      return new Promise((resolve) => {
        release = resolve;
      });
    });
    const summary = defined(screen.getByTestId("native-subagent-card").querySelector("summary"));
    const user = userEvent.setup();
    await user.click(summary);
    await waitFor(() => assert.ok(signal));
    await user.click(summary);
    assert.equal((signal as AbortSignal | null)?.aborted, true);
    release(
      historyResponse([
        { recordID: 1, index: 0, message: { kind: "text", ts: 1, text: { text: "Late private body" } } },
      ]),
    );
    expect(screen.queryByText("Late private body")).not.toBeInTheDocument();
    fixture.view.unmount();
  });
});
const command: HistoryActivity = {
  sectionID: "1",
  kind: "backgroundCommand",
  id: "shell",
  status: "running",
  first: { recordID: 1, index: 0 },
  bodyThrough: 1,
  ownershipThrough: 0,
  startedAt: new Date(2000).toISOString() as ISOTimestamp,
  anchorAt: new Date(2000).toISOString() as ISOTimestamp,
};
describe("background summaries", () => {
  it("ticks running work, freezes a completed duration, and reports unknown outcomes honestly", () => {
    vi.useFakeTimers();
    vi.setSystemTime(5000);
    const [activity, setActivity] = createSignal(command);
    const [settled, setSettled] = createSignal(false);
    render(() => <BackgroundCommandCard command={activity()} settled={settled()} />);
    expect(screen.getByTestId("background-command-duration")).toHaveTextContent("0:03");
    vi.advanceTimersByTime(2000);
    expect(screen.getByTestId("background-command-duration")).toHaveTextContent("0:05");
    setSettled(true);
    expect(screen.queryByTestId("background-command-duration")).not.toBeInTheDocument();
    expect(screen.getByText("Last observed running · outcome unknown")).toBeInTheDocument();
    setActivity({ ...command, status: "failed", exitCode: 137, endedAt: new Date(8000).toISOString() as ISOTimestamp });
    expect(screen.getByText("exit 137")).toBeInTheDocument();
    expect(screen.getByTestId("background-command-duration")).toHaveTextContent("0:06");
  });
  it("keeps a running chip distinct from terminal exit evidence", () => {
    const [activity, setActivity] = createSignal(command);
    render(() => <BackgroundCommandStatusChip command={activity()} settled={false} />);
    expect(screen.getByTestId("background-command-chip")).toHaveTextContent("running in background");
    setActivity({ ...command, status: "completed", exitCode: 0 });
    expect(screen.getByTestId("background-command-chip")).toHaveTextContent("exit 0");
  });
});
it("reloads the finite actor once when a delayed manifest resolves earlier delegation ownership", async () => {
  const start = lifecycle(1, "running", "");
  const earlier: HistoryEvent = {
    recordID: 2,
    index: 0,
    message: {
      kind: "text",
      ts: 2000,
      source: { delegationToolUseID: "spawn" },
      text: { text: "Recovered earlier delegation" },
    },
  };
  const fixture = actorFixture([start, earlier]);
  const card = screen.getByTestId("native-subagent-card");
  await userEvent.setup().click(defined(card.querySelector("summary")));
  await waitFor(() =>
    expect(screen.queryByRole("status", { name: "Loading subagent history" })).not.toBeInTheDocument(),
  );
  expect(screen.queryByText("Recovered earlier delegation")).not.toBeInTheDocument();
  const original = defined(vi.mocked(globalThis.fetch).getMockImplementation());
  let release: (value: Response) => void = () => {};
  let obsolete: AbortSignal | null = null;
  vi.mocked(globalThis.fetch).mockImplementation((input, init) => {
    if (!String(input).includes("afterRecordID=2")) return original(input, init);
    obsolete = init?.signal ?? null;
    return new Promise((resolve) => {
      release = resolve;
    });
  });
  const relation: HistoryEvent = {
    recordID: 3,
    index: 0,
    message: {
      kind: "nativeSubagent",
      ts: 3000,
      nativeSubagent: { id: "child", status: "running", toolUseID: "spawn" },
    },
  };
  const manifest = fixture.checkpoint(relation);
  await waitFor(() => assert.ok(obsolete));
  fixture.publish(manifest);
  await screen.findByText("Recovered earlier delegation");
  assert.equal((obsolete as AbortSignal | null)?.aborted, true);
  release(historyResponse([relation]));
  expect(screen.getAllByText("Recovered earlier delegation")).toHaveLength(1);
  expect(screen.getByTestId("native-subagent-card")).toBe(card);
  fixture.view.unmount();
});

it("keeps a disclosed background card mounted across equivalent manifest copies", async () => {
  const events: HistoryEvent[] = [
    {
      recordID: 1,
      index: 0,
      message: {
        kind: "backgroundCommand",
        ts: 1000,
        backgroundCommand: { id: "shell", status: "completed", result: "Lint finished", exitCode: 0 },
      },
    },
  ];
  let stream: HistoryEventsHandlers | null = null;
  vi.spyOn(api, "historyEvents").mockImplementation((_id, _cursor, handlers) => {
    stream = handlers;
    return { close: vi.fn() } as unknown as EventSource;
  });
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => fixtureHistoryResponse(String(input), events, ""));
  const first = historyManifest(events, "").metadata.activities;
  const [commands, setCommands] = createSignal(first);
  render(() => (
    <TaskTimelineProvider taskId="background-identity">
      <BackgroundCommands commands={commands()} settled={true} />
    </TaskTimelineProvider>
  ));
  const card = screen.getByTestId("background-command-card");
  await userEvent.setup().click(defined(card.querySelector("summary")));
  await screen.findByText("Lint finished");
  const summary = defined(card.querySelector("summary"));
  summary.focus();
  const reads = vi.mocked(globalThis.fetch).mock.calls.filter(([input]) => String(input).includes("/activity/")).length;
  defined(stream as HistoryEventsHandlers | null).onManifest?.(historyManifest(events, ""));
  setCommands(first.map((value) => ({ ...value })));
  expect(screen.getByTestId("background-command-card")).toBe(card);
  expect(card).toHaveAttribute("open");
  expect(summary).toHaveFocus();
  assert.equal(
    vi.mocked(globalThis.fetch).mock.calls.filter(([input]) => String(input).includes("/activity/")).length,
    reads,
  );
  expect(screen.getByText("Lint finished")).toBeVisible();
});

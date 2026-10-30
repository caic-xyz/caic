// Tests coherent history publication, stable context disclosures, task-owned plans, scoped tools, and SSE lifecycle.

import assert from "node:assert/strict";
import { historyResponse, historyManifest, fixtureHistoryResponse } from "@tests/history";
import { afterEach, beforeEach, describe, it } from "node:test";
import { expect, vi } from "@tests/expect";
import { Route, Router } from "@solidjs/router";
import { fireEvent, render, screen, waitFor, within } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";

import { createSignal } from "solid-js";
import type { EventMessage, HistoryEvent, ISOTimestamp, TaskPlan } from "@sdk/types.gen";
import type { HistoryEventsHandlers } from "@sdk/api.gen";

import TaskDetail from "./TaskDetail";
import { TaskTimelineProvider, useTaskTimeline } from "../TaskTimelineContext";
import { api } from "../api";
import { HostModeProvider } from "@maruel/gomode/web/HostMode";

// Spies on the real api singleton replace the former module mocks.
let taskEventStreamMock = vi.spyOn(api, "historyEvents");
const clearContextMock = vi.spyOn(api, "clearContext");
const compactContextMock = vi.spyOn(api, "compactContext");
const getTaskDiffMock = vi.spyOn(api, "getTaskDiff");
const getTaskDiffIndexMock = vi.spyOn(api, "getTaskDiffIndex");
const getTaskFileDiffMock = vi.spyOn(api, "getTaskFileDiff");

beforeEach(() => {
  taskEventStreamMock = vi
    .spyOn(api, "historyEvents")
    .mockImplementation(() => ({ close: vi.fn(), onerror: null }) as unknown as EventSource);
  fixtureEvents = [];
  fixtureRecord = 0;
  fixturePrompt = "";
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) =>
    fixtureHistoryResponse(String(input), fixtureEvents, fixturePrompt),
  );
});
let fixtureRecord = 0;
let fixtureEvents: HistoryEvent[] = [];
let fixturePrompt = "";
function emit(handlers: HistoryEventsHandlers, message: EventMessage) {
  if (message.result)
    message = {
      ...message,
      result: {
        ...message.result,
        duration: message.result.duration ?? 0,
        durationAPI: message.result.durationAPI ?? 0,
        usage: message.result.usage ?? {
          inputTokens: 0,
          outputTokens: 0,
          cacheCreationInputTokens: 0,
          cacheReadInputTokens: 0,
          reportedModel: "",
        },
      },
    };
  const event = { recordID: ++fixtureRecord, index: 0, message };
  fixtureEvents.push(event);
  handlers.onMessage(event);
  handlers.onCheckpoint?.({ cursor: `3.test.${fixtureRecord}.end`, through: fixtureRecord });
  if (
    message.init ||
    message.result ||
    message.toolResult ||
    message.nativeSubagent ||
    message.backgroundCommand ||
    message.userInput ||
    message.skillLoad ||
    message.todo ||
    message.log ||
    message.commitSnapshot ||
    message.system ||
    message.source?.agentID ||
    message.source?.delegationToolUseID
  )
    handlers.onManifest?.(historyManifest(fixtureEvents, fixturePrompt));
}

clearContextMock.mockImplementation(() => Promise.resolve({ status: "cleared" }) as never);
compactContextMock.mockImplementation(() => Promise.resolve({ status: "compacting" }) as never);
getTaskDiffMock.mockImplementation(() => Promise.resolve({} as never));
getTaskDiffIndexMock.mockImplementation(() => Promise.resolve({ repositories: [] }));
getTaskFileDiffMock.mockImplementation(() => Promise.resolve({ diff: "" }));

const pushedRepoStates = [
  {
    name: "my-repo",
    branch: "task-branch",
    ahead: 1,
    behind: 0,
    changedFiles: 2,
    linesAdded: 15,
    linesDeleted: 3,
    uncommittedFiles: 1,
    conflicts: 0,
  },
];

const baseProps = {
  taskId: "abc",
  taskState: "running",
  autoFocusPrompt: true,
  repo: "my-repo",
  remoteURL: "https://github.com/org/my-repo",
  branch: "feature-branch",
  baseBranch: "main",
  harness: "claude",
  stoppedDiskUsedBytes: -1,
  now: Date.parse("2026-07-08T12:00:00Z"),
  repoStates: pushedRepoStates,
  onClose: () => {},
  onStop: () => {},
  onPurge: () => {},
  onRevive: () => {},
  childTasks: [],
  currentPlan: null,
  inputDraft: "",
  onInputDraft: () => {},
  inputImages: [],
  imageConstraints: null,
  imageGeneration: 0,
  onAddImages: () => {},
  onRemoveImage: () => {},
  onSendInput: async () => {},
  onCancelInputConversion: () => {},
  onError: () => {},
};

async function renderTaskDetail(props: Partial<Parameters<typeof TaskDetail>[0]> = {}) {
  fixturePrompt = props.initialPrompt ?? "";
  window.history.replaceState(null, "", "/task/@abc+test-task");
  const view = render(() => (
    <Router>
      <Route
        path="/*"
        component={() => (
          <HostModeProvider>
            <TaskTimelineProvider taskId={props.taskId ?? baseProps.taskId}>
              <TaskDetail {...baseProps} {...props} />
            </TaskTimelineProvider>
          </HostModeProvider>
        )}
      />
    </Router>
  ));
  await waitFor(() => expect(screen.queryByRole("status", { name: "Loading history" })).not.toBeInTheDocument());
  return view;
}

it("never mounts completed context content while canonical history is published", async () => {
  const init = (sessionID: string): EventMessage => ({
    kind: "init",
    ts: 1,
    init: { reportedModel: "model", agentVersion: "v", sessionID, tools: [], cwd: "/workspace", harness: "claude" },
  });
  const events: EventMessage[] = [
    init("past"),
    { kind: "text", ts: 2, text: { text: "![Historical render probe](/historical-image.png)" } },
    resultEvent(3),
    { kind: "system", ts: 4, system: { subtype: "compact_boundary" } },
    init("current"),
    { kind: "text", ts: 6, text: { text: "Current context response" } },
    resultEvent(7),
  ];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) =>
    fixtureHistoryResponse(
      String(input),
      events.map((message, index) => ({ recordID: index + 1, index: 0, message })),
      "",
    ),
  );
  const rendered: string[] = [];
  const innerHTML = Object.getOwnPropertyDescriptor(Element.prototype, "innerHTML");
  assert.ok(innerHTML?.set);
  const original = innerHTML.set;
  const renderedHTML = vi.spyOn(Element.prototype, "innerHTML", "set").mockImplementation(function (
    this: Element,
    value: string,
  ) {
    if (value.includes("Historical render probe")) rendered.push(value);
    original.call(this, value);
  });
  let view: Awaited<ReturnType<typeof renderTaskDetail>> | null = null;
  try {
    view = await renderTaskDetail({ taskId: "canonical-publication" });
    expect(screen.getByText("Current context response")).toBeInTheDocument();
    expect(rendered).toHaveLength(0);
    await userEvent.setup().click(screen.getByRole("button", { name: /Compacted session/ }));
    await userEvent.setup().click(await screen.findByRole("button", { name: /^1 message/ }));
    expect(screen.getByRole("img", { name: "Historical render probe" })).toBeInTheDocument();
    expect(rendered.length).toBeGreaterThan(0);
  } finally {
    view?.unmount();
    renderedHTML.mockRestore();
  }
});

it("updates only the task-owned root plan while preserving canonical history and transport", async () => {
  const events: EventMessage[] = [
    { kind: "toolUse", ts: 1000, toolUse: { toolUseID: "owned", name: "ExitPlanMode", input: {} } },
    { kind: "toolUse", ts: 1100, toolUse: { toolUseID: "newer", name: "ExitPlanMode", input: {} } },
    { kind: "nativeSubagent", ts: 1200, nativeSubagent: { id: "child", label: "Child reviewer", status: "completed" } },
    {
      kind: "toolUse",
      ts: 1300,
      source: { agentID: "child" },
      toolUse: { toolUseID: "owned", name: "ExitPlanMode", input: {} },
    },
  ];
  const stream = vi
    .spyOn(api, "historyEvents")
    .mockImplementation(() => ({ close: vi.fn() }) as unknown as EventSource);
  const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) =>
    fixtureHistoryResponse(
      String(input),
      events.map((message, index) => ({ recordID: index + 1, index: 0, message })),
      "",
    ),
  );
  const [plan, setPlan] = createSignal<TaskPlan | null>({ toolUseID: "owned", content: "Original root plan" });
  let timeline: ReturnType<typeof useTaskTimeline> | undefined;
  function Probe() {
    timeline = useTaskTimeline();
    return null;
  }
  window.history.replaceState(null, "", "/task/@abc+test-task");
  const view = render(() => (
    <Router>
      <Route
        path="/*"
        component={() => (
          <HostModeProvider>
            <TaskTimelineProvider taskId="abc">
              <Probe />
              <TaskDetail {...baseProps} taskState="has_plan" currentPlan={plan()} />
            </TaskTimelineProvider>
          </HostModeProvider>
        )}
      />
    </Router>
  ));
  try {
    await waitFor(() => expect(screen.getByTestId("plan-content")).toHaveTextContent("Original root plan"));
    assert.ok(timeline);
    const messages = timeline.messages();
    const epoch = timeline.epoch();
    const requests = fetchMock.mock.calls.length;
    const streams = stream.mock.calls.length;
    const card = screen.getByTestId("native-subagent-card");
    const summary = card.querySelector("summary");
    assert.ok(summary);
    await userEvent.setup().click(summary);
    expect(screen.getAllByTestId("plan-content")).toHaveLength(1);
    setPlan({ toolUseID: "owned", content: "Revised root plan" });
    await waitFor(() => expect(screen.getByTestId("plan-content")).toHaveTextContent("Revised root plan"));
    setPlan({ toolUseID: "absent", content: "Unowned plan" });
    expect(screen.queryByTestId("plan-content")).not.toBeInTheDocument();
    setPlan(null);
    expect(screen.queryByTestId("plan-content")).not.toBeInTheDocument();
    assert.equal(timeline.messages(), messages);
    assert.equal(timeline.epoch(), epoch);
    assert.equal(fetchMock.mock.calls.length, requests + 1);
    assert.equal(stream.mock.calls.length, streams);
  } finally {
    view.unmount();
    fetchMock.mockRestore();
    stream.mockRestore();
  }
});

it("keeps a single-call plan dormant inside a collapsed past turn", async () => {
  taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
    for (const event of [
      { kind: "toolUse", ts: 1000, toolUse: { toolUseID: "past-plan", name: "ExitPlanMode", input: {} } },
      resultEvent(2000),
      { kind: "userInput", ts: 3000, userInput: { text: "Continue" } },
      { kind: "text", ts: 4000, text: { text: "Current response" } },
      resultEvent(5000),
    ] satisfies EventMessage[])
      emit(handlers, event);
    return { addEventListener: vi.fn(), close: vi.fn(), onerror: null } as unknown as EventSource;
  });
  await renderTaskDetail({
    taskId: "plan-disclosure",
    currentPlan: { toolUseID: "past-plan", content: "Dormant plan" },
  });
  expect(screen.queryByTestId("plan-content")).not.toBeInTheDocument();
  await userEvent.setup().click(screen.getByRole("button", { name: /1 tool call/ }));
  expect(screen.getByTestId("plan-content")).toHaveTextContent("Dormant plan");
  expect(screen.getByTestId("tool-call")).not.toHaveAttribute("open");
});

it("retries live history and automatically loads the full tool output", async () => {
  const startedAt = Date.now() - 2_000;
  let chunkRequests = 0;
  const requests: string[] = [];
  const historyStream = vi
    .spyOn(api, "historyEvents")
    .mockImplementation(() => ({ close: vi.fn() }) as unknown as EventSource);
  const events: HistoryEvent[] = [
    {
      recordID: 2,
      index: 0,
      message: {
        kind: "toolUse",
        ts: startedAt,
        toolUse: { toolUseID: "tool", name: "Bash", input: { command: "echo old" } },
      },
    },
    {
      recordID: 3,
      index: 0,
      message: { kind: "toolOutputDelta", ts: 3, toolOutputDelta: { toolUseID: "tool", delta: "old output" } },
    },
    {
      recordID: 520,
      index: 0,
      message: { kind: "toolOutputDelta", ts: 520, toolOutputDelta: { toolUseID: "tool", delta: "tail output" } },
    },
  ];
  const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    requests.push(url);
    if (url.includes("/slice/") && ++chunkRequests === 1)
      return new Response("temporarily unavailable", { status: 503 });
    return fixtureHistoryResponse(url, events, "");
  });
  const view = await renderTaskDetail({});
  try {
    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("History request failed (503)"));
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Retry history" }));
    await waitFor(() => expect(screen.getByTestId("task-message-area")).toHaveAttribute("aria-busy", "false"));
    expect(screen.getAllByTestId("tool-call")).toHaveLength(1);
    expect(screen.getByTestId("tool-duration")).toBeInTheDocument();
    expect(requests.some((url) => url.endsWith("/slice/2/0/256/end"))).toBe(true);
    expect(screen.getByTestId("tool-call")).toHaveTextContent("old outputtail output");
  } finally {
    view.unmount();
    fetchMock.mockRestore();
    historyStream.mockRestore();
  }
});

it("automatically restores completed activity timing while keeping live observations ticking", async () => {
  vi.useFakeTimers();
  vi.setSystemTime(10_000);
  const starts: EventMessage[] = [
    {
      kind: "nativeSubagent",
      ts: 1_000,
      nativeSubagent: { id: "worker", label: "Historical worker", status: "running" },
    },
    { kind: "toolUse", ts: 1_000, toolUse: { toolUseID: "shell-tool", name: "Bash", input: {}, background: true } },
    {
      kind: "backgroundCommand",
      ts: 1_000,
      backgroundCommand: { id: "shell", toolUseID: "shell-tool", status: "running" },
    },
  ];
  const finishes: EventMessage[] = [
    { kind: "nativeSubagent", ts: 2_000, nativeSubagent: { id: "worker", status: "completed" } },
    { kind: "backgroundCommand", ts: 2_000, backgroundCommand: { id: "shell", status: "completed" } },
  ];
  const seeds = starts.map((message, index) => ({ recordID: index + 1, index: 0, message }));
  const streams: HistoryEventsHandlers[] = [];
  const historyStream = vi.spyOn(api, "historyEvents").mockImplementation((_id, _cursor, handlers) => {
    streams.push(handlers);
    return { close: vi.fn() } as unknown as EventSource;
  });
  const requests: string[] = [];
  const historyEvents = [...seeds, ...finishes.map((message, index) => ({ recordID: 257 + index, index: 0, message }))];
  const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    requests.push(url);
    return fixtureHistoryResponse(url, historyEvents, "");
  });
  const view = await renderTaskDetail({});
  try {
    await vi.waitFor(() => expect(screen.getByTestId("native-subagent-duration")).toHaveTextContent("0:01"));
    await vi.waitFor(() => expect(screen.getByTestId("task-message-area")).toHaveAttribute("aria-busy", "false"));
    const beforeTick = requests.length;
    expect(screen.getByTestId("native-subagent-duration")).toHaveTextContent("0:01");
    expect(screen.getByTestId("background-command-duration")).toHaveTextContent("0:01");
    await vi.advanceTimersByTimeAsync(3_000);
    expect(screen.getByTestId("native-subagent-duration")).toHaveTextContent("0:01");
    expect(screen.getByTestId("background-command-duration")).toHaveTextContent("0:01");
    expect(requests).toHaveLength(beforeTick);
    const stream = streams[0];
    assert.ok(stream);
    stream.onMessage({
      recordID: 259,
      index: 0,
      message: { kind: "nativeSubagent", ts: Date.now() - 1_000, nativeSubagent: { id: "ongoing", status: "running" } },
    });
    stream.onMessage({
      recordID: 260,
      index: 0,
      message: {
        kind: "backgroundCommand",
        ts: Date.now() - 1_000,
        backgroundCommand: { id: "ongoing-shell", status: "running" },
      },
    });
    historyEvents.push(
      {
        recordID: 259,
        index: 0,
        message: {
          kind: "nativeSubagent",
          ts: Date.now() - 1000,
          nativeSubagent: { id: "ongoing", status: "running" },
        },
      },
      {
        recordID: 260,
        index: 0,
        message: {
          kind: "backgroundCommand",
          ts: Date.now() - 1000,
          backgroundCommand: { id: "ongoing-shell", status: "running" },
        },
      },
    );
    stream.onManifest?.(historyManifest(historyEvents, ""));
    await vi.advanceTimersByTimeAsync(100);
    const native = view.container.querySelector('[data-native-id="ongoing"]');
    const background = view.container.querySelector('[data-command-id="ongoing-shell"]');
    assert.ok(native instanceof HTMLElement && background instanceof HTMLElement);
    expect(within(native).getByTestId("native-subagent-duration")).toHaveTextContent("0:01");
    expect(within(background).getByTestId("background-command-duration")).toHaveTextContent("0:01");
    await vi.advanceTimersByTimeAsync(2_000);
    expect(within(native).getByTestId("native-subagent-duration")).toHaveTextContent("0:03");
    expect(within(background).getByTestId("background-command-duration")).toHaveTextContent("0:03");
  } finally {
    view.unmount();
    fetchMock.mockRestore();
    historyStream.mockRestore();
    vi.useRealTimers();
  }
});

it("links a sent image to its document view at the conversation width", async () => {
  const hash = "a".repeat(64);
  taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
    emit(handlers, {
      kind: "userInput",
      ts: 1000,
      userInput: {
        text: "look",
        images: [{ mediaType: "image/png", data: "aW1n", hash }],
      },
    });
    return { addEventListener: vi.fn(), close: vi.fn(), onerror: null } as unknown as EventSource;
  });
  await renderTaskDetail();
  const image = await screen.findByRole("img", { name: "attached" });
  expect(image.getAttribute("src")).toBe(`/api/caic/v1/tasks/abc/attachments/${hash}?w=480`);
  const link = screen.getByRole("link", { name: "Open attached image" });
  expect(link.getAttribute("href")).toBe(`/task/@abc/view?attachment=${hash}`);
});

it("falls back to the inline image when the attachment cannot load", async () => {
  const hash = "a".repeat(64);
  taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
    emit(handlers, {
      kind: "userInput",
      ts: 1000,
      userInput: { text: "look", images: [{ mediaType: "image/png", data: "aW1n", hash }] },
    });
    return { addEventListener: vi.fn(), close: vi.fn(), onerror: null } as unknown as EventSource;
  });
  await renderTaskDetail();
  const image = await screen.findByRole("img", { name: "attached" });
  const descriptor = Object.getOwnPropertyDescriptor(Object.getPrototypeOf(image) as object, "src");
  if (!descriptor?.get || !descriptor.set) throw new Error("Missing HTMLImageElement.src accessors");
  let assignments = 0;
  Object.defineProperty(image, "src", {
    configurable: true,
    get: () => descriptor.get?.call(image),
    set: (value: string) => {
      assignments++;
      descriptor.set?.call(image, value);
    },
  });
  fireEvent.error(image);
  expect(image.getAttribute("src")).toBe("data:image/png;base64,aW1n");
  expect(assignments).toBe(1);
  fireEvent.error(image);
  expect(assignments).toBe(1);
});

it("keeps rendering an image inline when it has no digest", async () => {
  taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
    emit(handlers, {
      kind: "userInput",
      ts: 1000,
      userInput: { text: "look", images: [{ mediaType: "image/png", data: "aW1n" }] },
    });
    return { addEventListener: vi.fn(), close: vi.fn(), onerror: null } as unknown as EventSource;
  });
  await renderTaskDetail();
  const image = await screen.findByRole("img", { name: "attached" });
  expect(image.getAttribute("src")).toBe("data:image/png;base64,aW1n");
  expect(screen.queryByRole("link", { name: "Open attached image" })).not.toBeInTheDocument();
});

it("keeps a child-first card mounted and open as parent content and lifecycle metadata arrive", async () => {
  const source = {
    agentID: "codex:thread:child",
    threadID: "child",
    rootThreadID: "parent",
    turnID: "turn",
    itemID: "answer",
  };
  let send: ((message: EventMessage) => void) | undefined;
  taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
    send = (message) => emit(handlers, message);
    emit(handlers, { kind: "textDelta", ts: 1000, source, textDelta: { text: "Child first" } });
    return { addEventListener: vi.fn(), close: vi.fn(), onerror: null } as unknown as EventSource;
  });
  await renderTaskDetail();
  const card = screen.getByTestId("native-subagent-card");
  const summary = card.querySelector("summary");
  if (!summary) throw new Error("Missing native summary");
  await userEvent.setup().click(summary);
  const block = within(card).getByTestId("stream-message");
  send?.({
    kind: "textDelta",
    ts: 2000,
    source: { ...source, agentID: undefined, threadID: "parent" },
    textDelta: { text: "Parent later" },
  });
  send?.({
    kind: "nativeSubagent",
    ts: 2100,
    nativeSubagent: { id: "codex:thread:child", label: "Research", status: "running" },
  });
  await waitFor(() => expect(screen.getByText("Parent later")).toBeInTheDocument());
  expect(screen.getByTestId("native-subagent-card")).toBe(card);
  expect(card).toHaveAttribute("open");
  expect(within(card).getByTestId("stream-message")).toBe(block);
  expect(
    card.compareDocumentPosition(screen.getByText("Parent later")) & Node.DOCUMENT_POSITION_FOLLOWING,
  ).toBeTruthy();
});

it("keeps concurrent child Markdown and parent thinking stable through item and lifecycle completion", async () => {
  const parent = { threadID: "parent", rootThreadID: "parent", turnID: "turn", itemID: "reason" };
  const child = { ...parent, agentID: "codex:thread:child", threadID: "child", itemID: "answer" };
  let send: ((message: EventMessage) => void) | undefined;
  taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
    send = (message) => emit(handlers, message);
    const initial: EventMessage[] = [
      { kind: "thinkingDelta", ts: 1000, source: parent, thinkingDelta: { text: "Consider " } },
      {
        kind: "nativeSubagent",
        ts: 1100,
        nativeSubagent: { id: "codex:thread:child", label: "Research", status: "running" },
      },
      { kind: "textDelta", ts: 1200, source: child, textDelta: { text: "- A **bow" } },
    ];
    initial.forEach((message) => emit(handlers, message));
    return { addEventListener: vi.fn(), close: vi.fn(), onerror: null } as unknown as EventSource;
  });
  await renderTaskDetail();
  const card = screen.getByTestId("native-subagent-card");
  const summary = card.querySelector("summary");
  if (!summary) throw new Error("Missing native summary");
  await userEvent.setup().click(summary);
  const childBlock = within(card).getByTestId("stream-message");
  const parentBlock = screen.getAllByTestId("stream-message").find((element) => !card.contains(element));
  const thinking = parentBlock?.querySelector("details");
  expect(thinking).not.toHaveAttribute("open");
  send?.({ kind: "thinkingDelta", ts: 1300, source: parent, thinkingDelta: { text: "triangulation." } });
  send?.({ kind: "userInput", ts: 1400, userInput: { text: "Continue" } });
  send?.({ kind: "textDelta", ts: 1500, source: child, textDelta: { text: "tie** example." } });
  send?.({ kind: "text", ts: 1600, source: child, text: { text: "- A **bowtie** example." } });
  send?.({ kind: "thinking", ts: 1700, source: parent, thinking: { text: "Consider triangulation." } });
  send?.({
    kind: "nativeSubagent",
    ts: 1800,
    nativeSubagent: {
      id: "codex:thread:child",
      label: "Research",
      status: "completed",
      result: "- A **bowtie** example.",
    },
  });
  await waitFor(() => expect(within(card).getByText("Completed")).toBeInTheDocument());
  expect(screen.getByTestId("native-subagent-card")).toBe(card);
  expect(card).toHaveAttribute("open");
  expect(within(card).getByTestId("stream-message")).toBe(childBlock);
  await waitFor(() =>
    expect(within(childBlock).getByRole("listitem").querySelector("strong")?.textContent).toBe("bowtie"),
  );
  expect(parentBlock?.querySelector("details")).toBe(thinking);
  expect(parentBlock?.textContent).toContain("Consider triangulation.");
  send?.({ kind: "text", ts: 1900, source: child, text: { text: "" } });
  send?.({ kind: "thinking", ts: 2000, source: parent, thinking: { text: "" } });
  await waitFor(() => expect(childBlock.textContent).toBe(""));
  await waitFor(() => expect(parentBlock?.querySelector("details")).toBeNull());
  expect(parentBlock?.textContent).toBe("");
});

it("shows retained disk usage for a stopped task", async () => {
  await renderTaskDetail({ taskState: "stopped", stoppedDiskUsedBytes: 2_097_152 });

  expect(screen.getByText("Disk 2.0 MiB")).toHaveAttribute(
    "title",
    "Writable disk space retained by this stopped task",
  );
});

it("hides unavailable disk usage for an old stopped task", async () => {
  await renderTaskDetail({ taskState: "stopped" });

  expect(screen.queryByText(/^Disk /)).not.toBeInTheDocument();
});

function resultEvent(ts: number): EventMessage {
  return {
    kind: "result",
    ts,
    result: {
      subtype: "success",
      isError: false,
      result: "done",
      totalCostUSD: 0,
      cost: { usd: 0, source: "reported" },
      duration: 1,
      durationAPI: 1,
      numTurns: 1,
      usage: {
        inputTokens: 0,
        outputTokens: 0,
        cacheCreationInputTokens: 0,
        cacheReadInputTokens: 0,
        reportedModel: "test",
      },
    },
  };
}

describe("TaskDetail", () => {
  it("keeps the current separator and focused skill link mounted during live flushes", async () => {
    let send: ((message: EventMessage) => void) | undefined;
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      send = (message) => emit(handlers, message);
      for (const message of [
        {
          kind: "init",
          ts: 1,
          init: { sessionID: "a", reportedModel: "m", agentVersion: "1", cwd: "/", harness: "codex" },
        },
        { kind: "skillLoad", ts: 2, skillLoad: { skill: "review", path: "/a/SKILL.md" } },
        { kind: "text", ts: 3, text: { text: "first response" } },
        resultEvent(4),
      ] satisfies EventMessage[])
        emit(handlers, message);
      return { addEventListener: vi.fn(), close: vi.fn(), onerror: null } as unknown as EventSource;
    });
    await renderTaskDetail({ taskId: "separator-identity" });
    const separator = await screen.findByText(/^Session started · m/);
    const link = within(screen.getByRole("navigation", { name: "Loaded skills" })).getByRole("link", {
      name: "review",
    });
    await userEvent.setup().tab();
    link.focus();
    send?.({ kind: "text", ts: 5, text: { text: "streamed response" } });
    await screen.findByText("streamed response", { exact: true });
    expect(screen.getAllByText(/^Session started · m/)).toHaveLength(1);
    expect(screen.getByText(/^Session started · m/)).toBe(separator);
    expect(
      within(screen.getByRole("navigation", { name: "Loaded skills" })).getByRole("link", { name: "review" }),
    ).toBe(link);
    expect(link).toHaveFocus();
  });

  it("keeps an expanded earlier context mounted and focused as later contexts close", async () => {
    let send: ((message: EventMessage) => void) | undefined;
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      send = (message) => emit(handlers, message);
      for (const message of [
        {
          kind: "init",
          ts: 1,
          init: { sessionID: "a", reportedModel: "m", agentVersion: "1", cwd: "/", harness: "codex" },
        },
        { kind: "text", ts: 2, text: { text: "old context" } },
        resultEvent(3),
        { kind: "system", ts: 4, system: { subtype: "compact_boundary" } },
        { kind: "text", ts: 5, text: { text: "current context" } },
        resultEvent(6),
      ] satisfies EventMessage[])
        emit(handlers, message);
      return { addEventListener: vi.fn(), close: vi.fn(), onerror: null } as unknown as EventSource;
    });
    await renderTaskDetail({ taskId: "closed-context-identity" });
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: /^Compacted session · 1 turn/ }));
    await user.click(await screen.findByRole("button", { name: /^1 message/ }));
    const body = await screen.findByText("old context", { exact: true });
    const toggle = screen.getByRole("button", { name: /^Compacted session · 1 turn/ });
    toggle.focus();
    const boundaries: EventMessage[] = [
      { kind: "system", ts: 10, system: { subtype: "compact_boundary" } },
      { kind: "system", ts: 20, system: { subtype: "context_cleared" } },
      {
        kind: "init",
        ts: 30,
        init: { sessionID: "new", reportedModel: "m", agentVersion: "1", cwd: "/", harness: "codex" },
      },
    ];
    for (const boundary of boundaries) {
      send?.(boundary);
      send?.({ kind: "text", ts: boundary.ts + 1, text: { text: `later response ${boundary.ts}` } });
      send?.(resultEvent(boundary.ts + 2));
      await screen.findByText(`later response ${boundary.ts}`, { exact: true });
      expect(screen.getByText("old context", { exact: true })).toBe(body);
      expect(screen.getAllByRole("button", { name: /^Compacted session · 1 turn/ })[0]).toBe(toggle);
      expect(toggle).toHaveFocus();
    }
  });

  it("keeps child tool DOM, output, errors, and scoped input separate from a simultaneous same-ID root tool", async (t) => {
    const root = { threadID: "root", turnID: "turn" };
    const child = { threadID: "child", turnID: "turn", agentID: "native:child", delegationToolUseID: "spawn" };
    const sibling = { threadID: "sibling", turnID: "turn", agentID: "native:sibling" };
    let send: ((message: EventMessage) => void) | undefined;
    const inputSpy = vi
      .spyOn(api, "getTaskToolInput")
      .mockResolvedValue({ toolUseID: "same", input: { scoped: "child input" } });
    t.after(() => inputSpy.mockRestore());
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      send = (message) => emit(handlers, message);
      emit(handlers, {
        kind: "toolUse",
        ts: 1000,
        source: root,
        toolUse: { toolUseID: "same", name: "Parent tool", input: {} },
      });
      emit(handlers, {
        kind: "toolUse",
        ts: 2000,
        source: child,
        toolUse: { toolUseID: "same", name: "Child tool", input: {}, inputTruncated: true },
      });
      emit(handlers, {
        kind: "toolUse",
        ts: 2100,
        source: sibling,
        toolUse: { toolUseID: "same", name: "Sibling tool", input: {} },
      });
      emit(handlers, { kind: "skillLoad", ts: 2200, source: root, skillLoad: { skill: "root-skill" } });
      emit(handlers, {
        kind: "todo",
        ts: 2300,
        source: root,
        todo: { toolUseID: "todo", todos: [{ content: "Root todo", activeForm: "Root todo", status: "pending" }] },
      });
      return { addEventListener: vi.fn(), close: vi.fn(), onerror: null } as unknown as EventSource;
    });
    await renderTaskDetail();
    const user = userEvent.setup();
    const card = screen
      .getAllByTestId("native-subagent-card")
      .find((entry) => entry.getAttribute("data-native-id") === "native:child");
    if (!card) throw new Error("Missing child card");
    const nativeSummary = card.querySelector("summary");
    if (!nativeSummary) throw new Error("Missing native summary");
    await user.click(nativeSummary);
    await user.click(within(card).getByText("0/1 tools: Child tool"));
    const call = within(card).getByTestId("tool-call");
    const summary = call.querySelector("summary");
    if (!summary) throw new Error("Missing tool summary");
    await user.click(summary);
    await user.click(within(call).getByRole("button", { name: "Load input" }));
    expect(inputSpy).toHaveBeenCalledWith("abc", "same", "native:child", "spawn", "child", "turn");
    summary.focus();
    send?.({
      kind: "toolOutputDelta",
      ts: 2900,
      source: sibling,
      toolOutputDelta: { toolUseID: "same", delta: "Sibling output" },
    });
    send?.({
      kind: "toolUse",
      ts: 2950,
      source: child,
      toolUse: { toolUseID: "next", name: "Child helper", input: {} },
    });
    send?.({
      kind: "toolOutputDelta",
      ts: 3000,
      source: child,
      toolOutputDelta: { toolUseID: "same", delta: "Child output" },
    });
    send?.({ kind: "text", ts: 3500, source: root, text: { text: "Parent continues" } });
    send?.({
      kind: "toolResult",
      ts: 4000,
      source: child,
      toolResult: { toolUseID: "same", duration: 2, error: "Child tool failed" },
    });
    send?.({
      kind: "system",
      ts: 4500,
      source: child,
      system: { subtype: "error", detail: "Provider child error detail" },
    });
    send?.({ kind: "skillLoad", ts: 4600, source: child, skillLoad: { skill: "child-skill" } });
    send?.({
      kind: "todo",
      ts: 4700,
      source: child,
      todo: { toolUseID: "todo", todos: [{ content: "Child todo", activeForm: "Child todo", status: "pending" }] },
    });
    send?.({ kind: "system", ts: 4800, source: child, system: { subtype: "compact_boundary" } });
    await waitFor(() => expect(within(card).getByText("Provider child error detail")).toBeInTheDocument());
    expect(within(card).getAllByTestId("tool-call")[0]).toBe(call);
    expect(call).toHaveAttribute("open");
    expect(summary).toHaveFocus();
    expect(within(call).getByText("Child output")).toBeInTheDocument();
    expect(within(call).getByText("Child tool failed")).toBeInTheDocument();
    expect(screen.getByText("0/1 tools: Parent tool")).toBeInTheDocument();
    expect(within(card).getByText("1/2 tools: Child tool, Child helper")).toBeInTheDocument();
    expect(within(card).queryByText("Sibling output")).not.toBeInTheDocument();
    expect(
      within(screen.getByRole("navigation", { name: "Loaded skills" })).getByText("root-skill"),
    ).toBeInTheDocument();
    const progress = screen.getByText("Todos 0/1").closest("details");
    expect(progress?.textContent).toContain("Root todo");
    expect(progress?.textContent).not.toContain("Child todo");
  });

  it("keeps an identified answer mounted when the successful result repeats it", async () => {
    const source = { threadID: "parent", rootThreadID: "parent", turnID: "turn", itemID: "answer" };
    let send: ((message: EventMessage) => void) | undefined;
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      send = (message) => emit(handlers, message);
      emit(handlers, { kind: "textDelta", ts: 1000, source, textDelta: { text: "Finished." } });
      return { addEventListener: vi.fn(), close: vi.fn(), onerror: null } as unknown as EventSource;
    });
    await renderTaskDetail();
    const block = screen.getByTestId("stream-message");
    send?.({ kind: "text", ts: 2000, source, text: { text: "Finished." } });
    const completion = resultEvent(3000);
    if (!completion.result) throw new Error("Missing result payload");
    completion.result.result = "Finished.";
    send?.(completion);
    await waitFor(() => expect(screen.getByText("Done")).toBeInTheDocument());
    expect(screen.getByTestId("stream-message")).toBe(block);
    expect(screen.getAllByText("Finished.")).toHaveLength(1);
  });

  it("omits empty session metadata without leaving extra separators", async () => {
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      emit(handlers, {
        kind: "init",
        ts: 1_000,
        init: {
          reportedModel: "gemini-3.8-flash-high",
          agentVersion: "",
          sessionID: "session-one",
          tools: [],
          cwd: "",
          harness: "antigravity",
        },
      });
      emit(handlers, { kind: "text", ts: 2_000, text: { text: "Verification is running." } });
      return { addEventListener: vi.fn(), close: vi.fn(), onerror: null } as unknown as EventSource;
    });
    await renderTaskDetail();
    expect(await screen.findByText("Session started · gemini-3.8-flash-high · session-one")).toBeInTheDocument();
  });

  it("replaces the Diff link with a repository state marker", async () => {
    await renderTaskDetail();

    expect(
      await screen.findByRole("link", {
        name: "my-repo: 2 changed files, 15 additions, 3 deletions, 1 uncommitted file, 1 commit ahead of upstream",
      }),
    ).toHaveAttribute("href", "/task/@abc+test-task/diff");
    expect(screen.queryByText("Diff")).not.toBeInTheDocument();
  });

  it("links task statistics to the full detail route", async () => {
    await renderTaskDetail();

    expect(screen.getByRole("link", { name: "Task statistics" })).toHaveAttribute("href", "/task/@abc+test-task/stats");
  });

  it("keeps header Git line totals when its capped title ellipsizes", async () => {
    const title = "OAuth security hardening and migration";
    await renderTaskDetail({ title });

    const titleElement = screen.getByText(title);
    Object.defineProperties(titleElement, {
      clientWidth: { configurable: true, value: 160 },
      scrollWidth: { configurable: true, value: 320 },
    });
    window.dispatchEvent(new Event("resize"));
    await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));

    const link = await screen.findByRole("link", {
      name: /my-repo: 2 changed files/,
    });
    expect(link).not.toHaveAttribute("data-elide-diff-stats");
  });

  it("elides header Git line totals when repository context ellipsizes", async () => {
    await renderTaskDetail();

    const repoLink = await screen.findByRole("link", { name: "my-repo" });
    const headerMeta = repoLink.parentElement;
    if (!headerMeta) throw new Error("repository link is missing its header context");
    Object.defineProperties(headerMeta, {
      clientWidth: { configurable: true, value: 160 },
      scrollWidth: { configurable: true, value: 320 },
    });
    window.dispatchEvent(new Event("resize"));
    await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));

    const diffLink = await screen.findByRole("link", {
      name: /my-repo: 2 changed files/,
    });
    expect(diffLink).toHaveAttribute("data-elide-diff-stats", "");
  });

  it("keeps header Git line totals on one center-aligned flex line", async () => {
    await renderTaskDetail();

    const header = screen.getByTestId("task-detail-header");
    Array.from(header.children).forEach((child, index) => {
      const height = 16 + index * 2;
      const top = 40 - height / 2;
      vi.spyOn(child, "getBoundingClientRect").mockReturnValue({
        bottom: top + height,
        height,
        left: 0,
        right: 0,
        toJSON: () => ({}),
        top,
        width: 0,
        x: 0,
        y: top,
      });
    });
    window.dispatchEvent(new Event("resize"));

    const link = await screen.findByRole("link", {
      name: /my-repo: 2 changed files/,
    });
    await waitFor(() => expect(link).not.toHaveAttribute("data-elide-diff-stats"));
  });

  it("opens agent screenshot links in the file viewer", async () => {
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      emit(handlers, {
        kind: "text",
        ts: 1000,
        text: {
          text: "[Screenshot: General](/home/user/src/caic/test-results/settings-general-desktop.png)",
        },
      });
      return { addEventListener: vi.fn(), close: vi.fn(), onerror: null } as unknown as EventSource;
    });
    await renderTaskDetail();
    const link = screen.getByRole("link", { name: "Screenshot: General" });
    expect(link).toHaveAttribute(
      "href",
      "/task/@abc/view?path=%2Fhome%2Fuser%2Fsrc%2Fcaic%2Ftest-results%2Fsettings-general-desktop.png",
    );
    expect(link).not.toHaveAttribute("target");
    expect(link).toHaveAttribute("data-app-link");
  });

  it("resolves file URIs and relative paths while preserving web links", async () => {
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      emit(handlers, {
        kind: "text",
        ts: 1000,
        text: {
          text: "[Skill](file:///home/user/skill%20notes.md) [Source](./src/App.tsx:12) [Website](https://example.com) [Mail](mailto:user@example.com) [Section](#section)",
        },
      });
      return { addEventListener: vi.fn(), close: vi.fn(), onerror: null } as unknown as EventSource;
    });
    await renderTaskDetail();
    // Markdown documents open in the in-app viewer, not a new tab.
    const skill = screen.getByRole("link", { name: "Skill" });
    expect(skill).toHaveAttribute("href", "/task/@abc/view?path=%2Fhome%2Fuser%2Fskill+notes.md");
    expect(skill).not.toHaveAttribute("target");
    expect(screen.getByRole("link", { name: "Source" })).toHaveAttribute(
      "href",
      "/task/@abc/view?path=.%2Fsrc%2FApp.tsx",
    );
    expect(screen.getByRole("link", { name: "Website" })).toHaveAttribute("href", "https://example.com");
    expect(screen.getByRole("link", { name: "Mail" })).toHaveAttribute("href", "mailto:user@example.com");
    expect(screen.getByRole("link", { name: "Section" })).toHaveAttribute("href", "#section");
  });

  it("links distinct skill paths and enriches a uniquely named skill", async () => {
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      emit(handlers, { kind: "skillLoad", ts: 1000, skillLoad: { skill: "review" } });
      emit(handlers, {
        kind: "skillLoad",
        ts: 1001,
        skillLoad: { skill: "code-quality", path: "/home/user/.claude/skills/code-quality/SKILL.md" },
      });
      emit(handlers, {
        kind: "skillLoad",
        ts: 1002,
        skillLoad: { skill: "code-quality", path: "/elsewhere/code-quality/SKILL.md" },
      });
      emit(handlers, {
        kind: "skillLoad",
        ts: 1003,
        skillLoad: { skill: "review", path: "~/.agents/skills/review/SKILL.md" },
      });
      return { addEventListener: vi.fn(), close: vi.fn(), onerror: null } as unknown as EventSource;
    });
    await renderTaskDetail();
    const nav = screen.getByRole("navigation", { name: "Loaded skills" });
    const links = within(nav).getAllByRole("link");
    expect(links.map((link) => link.textContent)).toEqual(["review", "code-quality", "code-quality"]);
    expect(links[0]).toHaveAttribute("href", "/task/@abc/view?path=%7E%2F.agents%2Fskills%2Freview%2FSKILL.md");
    expect(links[1]).toHaveAttribute(
      "href",
      "/task/@abc/view?path=%2Fhome%2Fuser%2F.claude%2Fskills%2Fcode-quality%2FSKILL.md",
    );
  });

  for (const subtype of ["compact_boundary", "context_cleared"]) {
    it(`keeps prior skill links at the ${subtype} separator and empties the header`, async () => {
      let send: ((message: EventMessage) => void) | undefined;
      taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
        send = (message) => emit(handlers, message);
        emit(handlers, {
          kind: "init",
          ts: 1,
          init: { sessionID: "skills-session", reportedModel: "m", agentVersion: "1", cwd: "/", harness: "claude" },
        });
        emit(handlers, { kind: "skillLoad", ts: 2, skillLoad: { skill: "review", path: "/a/SKILL.md" } });
        return { addEventListener: vi.fn(), close: vi.fn(), onerror: null } as unknown as EventSource;
      });
      await renderTaskDetail({ taskId: `prior-skills-${subtype}` });
      expect(screen.getByRole("navigation", { name: "Loaded skills" })).toBeInTheDocument();
      send?.({ kind: "system", ts: 3, system: { subtype } });
      await waitFor(() => expect(screen.queryByRole("navigation", { name: "Loaded skills" })).not.toBeInTheDocument());
      await userEvent.setup().click(screen.getByRole("button", { name: /^(Compacted|Cleared) session/ }));
      const prior = await screen.findByRole("navigation", { name: "Previous context skills" });
      expect(within(prior).getByRole("link", { name: "review" })).toHaveAttribute(
        "href",
        `/task/@prior-skills-${subtype}/view?path=%2Fa%2FSKILL.md`,
      );
      send?.({ kind: "skillLoad", ts: 4, skillLoad: { skill: "quality", path: "/b/SKILL.md" } });
      await waitFor(() =>
        expect(
          within(screen.getByRole("navigation", { name: "Loaded skills" })).getByRole("link", { name: "quality" }),
        ).toBeInTheDocument(),
      );
      expect(within(prior).getAllByRole("link")).toHaveLength(1);
    });
  }

  it("reveals each closed section's skill links when its session expands", async () => {
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      for (const message of [
        {
          kind: "init",
          ts: 0,
          init: { sessionID: "skills", reportedModel: "m", agentVersion: "1", cwd: "/", harness: "claude" },
        },
        { kind: "skillLoad", ts: 1, skillLoad: { skill: "review", path: "/a/SKILL.md" } },
        {
          kind: "result",
          ts: 2,
          result: { subtype: "success", result: "", durationMs: 0, totalCostUSD: 0, numTurns: 1, isError: false },
        },
        { kind: "system", ts: 3, system: { subtype: "compact_boundary" } },
        { kind: "skillLoad", ts: 4, skillLoad: { skill: "quality", path: "/b/SKILL.md" } },
        {
          kind: "result",
          ts: 5,
          result: { subtype: "success", result: "", durationMs: 0, totalCostUSD: 0, numTurns: 1, isError: false },
        },
        { kind: "system", ts: 6, system: { subtype: "context_cleared" } },
        { kind: "text", ts: 7, text: { text: "Fresh context" } },
        {
          kind: "result",
          ts: 8,
          result: { subtype: "success", result: "", durationMs: 0, totalCostUSD: 0, numTurns: 1, isError: false },
        },
      ] as EventMessage[])
        emit(handlers, message);
      return { addEventListener: vi.fn(), close: vi.fn(), onerror: null } as unknown as EventSource;
    });
    await renderTaskDetail();
    const user = userEvent.setup();
    expect(screen.queryByRole("navigation", { name: "Previous context skills" })).not.toBeInTheDocument();
    const sessions = screen.getAllByRole("button", { name: /^Compacted session/ });
    assert.ok(sessions[0]);
    await user.click(sessions[0]);
    expect(
      within(screen.getByRole("navigation", { name: "Previous context skills" })).getByRole("link"),
    ).toHaveTextContent("review");
    const secondSession = screen.getAllByRole("button", { name: /^Compacted session/ })[1];
    assert.ok(secondSession);
    await user.click(secondSession);
    const strips = screen.getAllByRole("navigation", { name: "Previous context skills" });
    expect(strips).toHaveLength(2);
    expect(strips.map((strip) => within(strip).getByRole("link").textContent)).toEqual(["review", "quality"]);
    expect(screen.queryByRole("navigation", { name: "Loaded skills" })).not.toBeInTheDocument();
  });

  it("keeps a cleared prompt and restarted response in the same visible section", async () => {
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      const events: EventMessage[] = [
        {
          kind: "init",
          ts: 1,
          init: { sessionID: "old", reportedModel: "m", agentVersion: "1", cwd: "/", harness: "claude" },
        },
        { kind: "text", ts: 2, text: { text: "Old response" } },
        {
          kind: "result",
          ts: 3,
          result: {
            subtype: "success",
            result: "",
            isError: false,
            numTurns: 1,
            totalCostUSD: 0,
            duration: 0,
            durationAPI: 0,
            usage: {
              inputTokens: 0,
              outputTokens: 0,
              cacheCreationInputTokens: 0,
              cacheReadInputTokens: 0,
              reportedModel: "m",
            },
          },
        },
        { kind: "system", ts: 4, system: { subtype: "context_cleared" } },
        { kind: "userInput", ts: 5, userInput: { text: "Execute the new plan" } },
        {
          kind: "init",
          ts: 6,
          init: { sessionID: "new", reportedModel: "m", agentVersion: "1", cwd: "/", harness: "claude" },
        },
        { kind: "text", ts: 7, text: { text: "Executing the new plan" } },
        {
          kind: "result",
          ts: 8,
          result: {
            subtype: "success",
            result: "",
            isError: false,
            numTurns: 1,
            totalCostUSD: 0,
            duration: 0,
            durationAPI: 0,
            usage: {
              inputTokens: 0,
              outputTokens: 0,
              cacheCreationInputTokens: 0,
              cacheReadInputTokens: 0,
              reportedModel: "m",
            },
          },
        },
      ];
      for (const event of events) emit(handlers, event);
      return { addEventListener: vi.fn(), close: vi.fn(), onerror: null } as unknown as EventSource;
    });
    await renderTaskDetail();
    expect(screen.getByText("Execute the new plan", { exact: true })).toBeVisible();
    expect(screen.getByText("Executing the new plan", { exact: true })).toBeVisible();
    expect(screen.getAllByRole("button", { name: /^Compacted session/ })).toHaveLength(1);
  });

  it("keeps same-session content stable and exposes it through an archive after compaction", async () => {
    let send: ((message: EventMessage) => void) | undefined;
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      send = (message) => emit(handlers, message);
      emit(handlers, {
        kind: "init",
        ts: 1,
        init: { sessionID: "same", reportedModel: "m", agentVersion: "1", cwd: "/", harness: "claude" },
      });
      emit(handlers, { kind: "text", ts: 2, text: { text: "First response" } });
      emit(handlers, resultEvent(3));
      return { close: vi.fn() } as unknown as EventSource;
    });
    await renderTaskDetail({ taskId: "boundary-contract" });
    const first = screen.getByText("First response", { exact: true });
    send?.({
      kind: "init",
      ts: 4,
      init: { sessionID: "same", reportedModel: "m", agentVersion: "1", cwd: "/", harness: "claude" },
    });
    expect(screen.getByText("First response", { exact: true })).toBe(first);
    send?.({ kind: "system", ts: 5, system: { subtype: "compact_boundary" } });
    send?.({ kind: "text", ts: 6, text: { text: "Current response" } });
    send?.(resultEvent(7));
    await screen.findByText("Current response", { exact: true });
    expect(screen.queryByText("First response", { exact: true })).not.toBeInTheDocument();
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: /^Compacted session/ }));
    await user.click(await screen.findByRole("button", { name: /^1 message/ }));
    expect(await screen.findByText("First response", { exact: true })).toBeVisible();
  });

  it("copies only the selected fenced code block", async () => {
    const user = userEvent.setup();
    const writeText = vi.fn().mockResolvedValue(undefined);
    const originalClipboard = navigator.clipboard;
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText },
    });

    try {
      taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
        emit(handlers, {
          kind: "text",
          ts: 1_000,
          text: {
            text: "Run this:\n\n```ts\nconst answer = 42;\n```\n\nThen continue.",
          },
        });
        return {
          addEventListener: vi.fn(),
          close: vi.fn(),
          onerror: null,
        } as unknown as EventSource;
      });

      await renderTaskDetail();

      const copyButton = screen.getByRole("button", {
        name: "Copy code block",
      });
      expect(copyButton.parentElement?.className).toMatch(/singleLineCodeBlock/);

      await user.click(copyButton);
      expect(writeText).toHaveBeenCalledWith("const answer = 42;\n");
    } finally {
      Object.defineProperty(navigator, "clipboard", {
        configurable: true,
        value: originalClipboard,
      });
    }
  });

  it("renders parent and child task navigation", async () => {
    await renderTaskDetail({
      parentTaskID: "parent",
      childTasks: [{ id: "child", title: "Review tests" }],
    });

    expect(screen.getByRole("navigation", { name: "Task hierarchy" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Parent task" })).toHaveAttribute("href", "/task/@parent");
    expect(screen.getByRole("link", { name: "Child: Review tests" })).toHaveAttribute("href", "/task/@child");
  });

  it("uses a child ID when its title is empty", async () => {
    await renderTaskDetail({ childTasks: [{ id: "untitled-child", title: "" }] });

    expect(screen.getByRole("link", { name: "Child: untitled-child" })).toHaveAttribute(
      "href",
      "/task/@untitled-child",
    );
  });

  it("offers quota recovery without replacing the normal task actions", async () => {
    const user = userEvent.setup();
    const onQuotaRecovery = vi.fn();
    const onFork = vi.fn();
    await renderTaskDetail({
      taskState: "waiting",
      rateLimit: {
        blocked: true,
        window: "5h",
        resetsAt: "2026-07-08T12:42:00Z" as ISOTimestamp,
      },
      onQuotaRecovery,
      onFork,
    });

    expect(screen.getByTestId("quota-recovery-detail")).toHaveTextContent("5h quota resets in 42m");
    await user.click(screen.getByTestId("quota-recovery-detail-action"));
    expect(onQuotaRecovery).toHaveBeenCalledWith("abc");

    await user.click(screen.getByRole("button", { name: "Context actions" }));
    await user.click(screen.getByRole("menuitem", { name: "Fork" }));
    expect(onFork).toHaveBeenCalledWith("abc");
  });

  it("shows quota status without an unavailable recovery action when there is no repository", async () => {
    await renderTaskDetail({
      repo: "",
      rateLimit: {
        blocked: true,
        window: "5h",
        resetsAt: "2026-07-08T12:42:00Z" as ISOTimestamp,
      },
      onQuotaRecovery: vi.fn(),
    });

    expect(screen.getByTestId("quota-recovery-detail")).toHaveTextContent("Agent quota exhausted");
    expect(screen.queryByTestId("quota-recovery-detail-action")).not.toBeInTheDocument();
  });

  it("allows dismissing the quota warning", async () => {
    const user = userEvent.setup();
    const onDismissQuotaWarning = vi.fn();
    await renderTaskDetail({
      taskId: "task-quota",
      taskState: "waiting",
      rateLimit: {
        blocked: true,
        window: "5h",
        resetsAt: "2026-07-08T12:42:00Z" as ISOTimestamp,
      },
      onDismissQuotaWarning,
    });

    expect(screen.getByTestId("quota-recovery-detail")).toBeInTheDocument();
    const dismissButton = screen.getByRole("button", { name: "Dismiss quota warning" });
    expect(dismissButton).toHaveAttribute("title", "Dismiss quota warning");
    await user.click(dismissButton);
    expect(onDismissQuotaWarning).toHaveBeenCalledWith("task-quota");
    expect(screen.queryByTestId("quota-recovery-detail")).not.toBeInTheDocument();
  });

  it("hides the quota warning when quotaDismissed is true", async () => {
    await renderTaskDetail({
      taskState: "waiting",
      rateLimit: {
        blocked: true,
        window: "5h",
        resetsAt: "2026-07-08T12:42:00Z" as ISOTimestamp,
      },
      quotaDismissed: true,
    });

    expect(screen.queryByTestId("quota-recovery-detail")).not.toBeInTheDocument();
  });

  it("repository state diff link href ends with /diff", async () => {
    await renderTaskDetail();
    const link = await screen.findByRole("link", {
      name: /my-repo: 2 changed files/,
    });
    expect(link).toHaveAttribute("href", "/task/@abc+test-task/diff");
  });

  it("clicking a repository state marker navigates to the diff", async () => {
    const user = userEvent.setup();
    await renderTaskDetail();
    await user.click(await screen.findByRole("link", { name: /my-repo: 2 changed files/ }));
    await waitFor(() => expect(window.location.pathname).toBe("/task/@abc+test-task/diff"));
  });

  it("renders Codex file-change diffs with colored lines", async () => {
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      const events: EventMessage[] = [
        {
          kind: "toolUse",
          ts: 1,
          toolUse: {
            toolUseID: "edit-1",
            name: "Edit",
            detail: "main.go",
            input: [
              {
                path: "/workspace/main.go",
                diff: '@@ -5,3 +5,3 @@\n func main() {\n-\tfmt.Println("Hello, World!")\n+\tfmt.Println("Hi, World!")\n }\n',
              },
            ],
            inputView: {
              kind: "fileChanges",
              files: [
                {
                  path: "/workspace/main.go",
                  patch:
                    '@@ -5,3 +5,3 @@\n func main() {\n-\tfmt.Println("Hello, World!")\n+\tfmt.Println("Hi, World!")\n }\n',
                },
              ],
            },
          },
        },
        {
          kind: "toolResult",
          ts: 2,
          toolResult: { toolUseID: "edit-1", duration: 0.1 },
        },
      ];
      for (const event of events) emit(handlers, event);
      return {
        addEventListener: vi.fn(),
        close: vi.fn(),
        onerror: null,
      } as unknown as EventSource;
    });

    const { getAllByText, getByText } = await renderTaskDetail({ harness: "codex" });

    expect(getAllByText("100ms")).toHaveLength(1);
    expect(getByText("/workspace/main.go")).toBeInTheDocument();
    const added = getByText((_, element) => element?.textContent === '+\tfmt.Println("Hi, World!")');
    const deleted = getByText((_, element) => element?.textContent === '-\tfmt.Println("Hello, World!")');
    expect(added.className).toMatch(/lineAdded/);
    expect(deleted.className).toMatch(/lineDeleted/);
  });

  it("shows canonical native status on the tool call that spawned it", async () => {
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      const events: EventMessage[] = [
        {
          kind: "toolUse",
          ts: 1_000,
          toolUse: {
            toolUseID: "spawn-1",
            name: "Agent",
            detail: "Joke about README.md",
            input: { description: "Joke about README.md" },
          },
        },
        {
          kind: "nativeSubagent",
          ts: 2_000,
          nativeSubagent: {
            id: "child-1",
            toolUseID: "spawn-1",
            label: "Joke about README.md",
            status: "running",
          },
        },
        {
          kind: "nativeSubagent",
          ts: 3_000,
          nativeSubagent: {
            id: "child-1",
            status: "completed",
            result: "Read me before you judge me.",
          },
        },
      ];
      for (const event of events) emit(handlers, event);
      return {
        addEventListener: vi.fn(),
        close: vi.fn(),
        onerror: null,
      } as unknown as EventSource;
    });

    const { getByText, getByTestId } = await renderTaskDetail();

    // The canonical card anchors to the transcript where the lifecycle settled.
    const anchored = getByTestId("native-subagents");
    expect(within(anchored).getByText("Completed")).toBeInTheDocument();
    expect(within(anchored).queryByText("Read me before you judge me.")).not.toBeInTheDocument();
    const summary = anchored.querySelector("summary");
    if (!summary) throw new Error("Missing native summary");
    await userEvent.setup().click(summary);
    expect(within(anchored).getByText("Read me before you judge me.")).toBeInTheDocument();
    // The spawning tool call carries the same correlated status.
    const call = getByText("Agent").closest("details");
    if (!call) throw new Error("tool card must have a details element");
    expect(within(call).getByText("Completed")).toBeInTheDocument();
  });

  it("renders a native card that has no transcript item to anchor to", async () => {
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      emit(handlers, {
        kind: "nativeSubagent",
        ts: 1_000,
        nativeSubagent: { id: "orphan", label: "Orphan", status: "running" },
      });
      return {
        addEventListener: vi.fn(),
        close: vi.fn(),
        onerror: null,
      } as unknown as EventSource;
    });

    const { getByTestId } = await renderTaskDetail();
    expect(within(getByTestId("native-subagents")).getByText("Subagent: Orphan")).toBeInTheDocument();
  });

  it("updates a pending tool duration without new events and freezes it on completion", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(10_000);
    let complete = () => {};
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      emit(handlers, {
        kind: "toolUse",
        ts: 8_000,
        toolUse: { toolUseID: "live-shell", name: "Bash", input: { command: "sleep 10" } },
      });
      complete = () =>
        emit(handlers, {
          kind: "toolResult",
          ts: 13_000,
          toolResult: { toolUseID: "live-shell", duration: 5 },
        });
      return { addEventListener: vi.fn(), close: vi.fn(), onerror: null } as unknown as EventSource;
    });
    try {
      await renderTaskDetail();
      expect(screen.getByTestId("tool-duration")).toHaveTextContent("0:02");
      vi.advanceTimersByTime(2_000);
      expect(screen.getByTestId("tool-duration")).toHaveTextContent("0:04");
      complete();
      vi.advanceTimersByTime(10_000);
      expect(screen.getByTestId("tool-duration")).toHaveTextContent("0:05");
    } finally {
      vi.useRealTimers();
    }
  });

  it("shows elapsed time on a single-event message block", async () => {
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      emit(handlers, {
        kind: "userInput",
        ts: 1_000,
        userInput: { text: "prompt" },
      });
      emit(handlers, {
        kind: "text",
        ts: 2_500,
        text: { text: "response" },
      });
      return {
        addEventListener: vi.fn(),
        close: vi.fn(),
        onerror: null,
      } as unknown as EventSource;
    });

    const { getByText } = await renderTaskDetail();

    expect(getByText("0:02")).toBeInTheDocument();
  });

  it("shows one timing control for a thinking-only block", async () => {
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      emit(handlers, {
        kind: "userInput",
        ts: 1_000,
        userInput: { text: "prompt" },
      });
      emit(handlers, {
        kind: "thinking",
        ts: 2_000,
        thinking: { text: "checking the implementation" },
      });
      return {
        addEventListener: vi.fn(),
        close: vi.fn(),
        onerror: null,
      } as unknown as EventSource;
    });

    const { getAllByLabelText } = await renderTaskDetail();

    const timing = getAllByLabelText("Timing details");
    expect(timing).toHaveLength(1);
    assert.ok(timing[0] !== undefined);
    expect(timing[0].querySelector("svg")).toBeNull();
  });

  it("shows one summed timing control for collapsed Codex thinking", async () => {
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      const usage = (ts: number): EventMessage => ({
        kind: "usage",
        ts,
        usage: {
          inputTokens: 10,
          outputTokens: 5,
          cacheCreationInputTokens: 0,
          cacheReadInputTokens: 0,
          reportedModel: "test",
        },
      });
      const events: EventMessage[] = [
        {
          kind: "thinkingDelta",
          ts: 1_000,
          thinkingDelta: { text: "first thought" },
        },
        { kind: "textDelta", ts: 2_000, textDelta: { text: "\n" } },
        { kind: "thinking", ts: 3_000, thinking: { text: "FIRST THOUGHT" } },
        { kind: "text", ts: 4_000, text: { text: "\n" } },
        usage(4_000),
        {
          kind: "thinkingDelta",
          ts: 5_000,
          thinkingDelta: { text: "second thought" },
        },
        { kind: "textDelta", ts: 6_000, textDelta: { text: "\n" } },
        { kind: "thinking", ts: 7_000, thinking: { text: "SECOND THOUGHT" } },
        { kind: "text", ts: 8_000, text: { text: "\n" } },
        usage(8_000),
        { kind: "text", ts: 9_000, text: { text: "done" } },
      ];
      for (const event of events) emit(handlers, event);
      return {
        addEventListener: vi.fn(),
        close: vi.fn(),
        onerror: null,
      } as unknown as EventSource;
    });

    const { getAllByLabelText } = await renderTaskDetail({ harness: "codex" });

    const timings = getAllByLabelText("Timing details");
    expect(timings).toHaveLength(2);
    expect(timings[0]).toHaveTextContent("0:05");
    expect(timings[1]).toHaveTextContent("0:03");
  });

  it("formats short result durations without misleading zero seconds", async () => {
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      const event = resultEvent(2_000);
      if (!event.result) throw new Error("result fixture is missing payload");
      event.result.duration = 0.125;
      emit(handlers, event);
      return {
        addEventListener: vi.fn(),
        close: vi.fn(),
        onerror: null,
      } as unknown as EventSource;
    });

    const { getByText, queryByText } = await renderTaskDetail();

    expect(getByText("125ms")).toBeInTheDocument();
    expect(getByText("1 turn")).toBeInTheDocument();
    expect(queryByText("0.0s")).not.toBeInTheDocument();
  });

  it("does not repeat a successful assistant response from the result payload", async () => {
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      emit(handlers, {
        kind: "text",
        ts: 1_000,
        text: { text: "Finished the requested change." },
      });
      const event = resultEvent(2_000);
      if (!event.result) throw new Error("result fixture is missing payload");
      event.result.result = "Finished the requested change.";
      emit(handlers, event);
      return {
        addEventListener: vi.fn(),
        close: vi.fn(),
        onerror: null,
      } as unknown as EventSource;
    });

    const { getAllByText } = await renderTaskDetail();

    expect(getAllByText("Finished the requested change.")).toHaveLength(1);
  });

  it("keeps result payload text for failed turns", async () => {
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      const event = resultEvent(2_000);
      if (!event.result) throw new Error("result fixture is missing payload");
      event.result.isError = true;
      event.result.result = "Provider request failed.";
      emit(handlers, event);
      return {
        addEventListener: vi.fn(),
        close: vi.fn(),
        onerror: null,
      } as unknown as EventSource;
    });

    const { getByText } = await renderTaskDetail();

    expect(getByText("Provider request failed.")).toBeInTheDocument();
  });

  it("opens invocation details from a completed turn's result card", async () => {
    const user = userEvent.setup();
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      const event = resultEvent(2_000);
      if (!event.result) throw new Error("result fixture is missing payload");
      event.result.usage.inputTokens = 1_000;
      event.result.usage.reportedModel = "turn-model";
      emit(handlers, event);
      return {
        addEventListener: vi.fn(),
        close: vi.fn(),
        onerror: null,
      } as unknown as EventSource;
    });

    await renderTaskDetail();

    expect(screen.getByTestId("turn-duration")).toHaveTextContent("0:01");
    await user.click(screen.getByRole("button", { name: "Turn invocation details" }));
    expect(screen.getByTestId("turn-invocation-dialog")).toHaveTextContent("turn-model");
  });

  it("opens invocation details from a collapsed turn without expanding it", async () => {
    const user = userEvent.setup();
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      const firstResult = resultEvent(2_000);
      const latestResult = resultEvent(4_000);
      if (!firstResult.result || !latestResult.result) throw new Error("result fixture is missing payload");
      firstResult.result.usage.reportedModel = "collapsed-model";
      latestResult.result.usage.reportedModel = "latest-model";
      const events: EventMessage[] = [
        { kind: "text", ts: 1_000, text: { text: "collapsed response" } },
        firstResult,
        { kind: "userInput", ts: 3_000, userInput: { text: "continue" } },
        latestResult,
      ];
      for (const event of events) emit(handlers, event);
      return {
        addEventListener: vi.fn(),
        close: vi.fn(),
        onerror: null,
      } as unknown as EventSource;
    });

    await renderTaskDetail();

    const [collapsedTurn] = screen.getAllByRole("button", {
      name: "Turn invocation details",
    });
    assert.ok(collapsedTurn !== undefined);
    await user.click(collapsedTurn);
    expect(screen.getByTestId("turn-invocation-dialog")).toHaveTextContent("collapsed-model");
    expect(screen.queryByText("collapsed response")).not.toBeInTheDocument();
  });

  it("right-aligns the duration on collapsed turns", async () => {
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      const firstResult = resultEvent(333_000);
      if (firstResult.result) firstResult.result.duration = 332;
      const events: EventMessage[] = [
        { kind: "userInput", ts: 1_000, userInput: { text: "first" } },
        { kind: "text", ts: 2_000, text: { text: "first response" } },
        firstResult,
        { kind: "userInput", ts: 334_000, userInput: { text: "second" } },
        { kind: "text", ts: 335_000, text: { text: "second response" } },
        resultEvent(336_000),
      ];
      for (const event of events) emit(handlers, event);
      return {
        addEventListener: vi.fn(),
        close: vi.fn(),
        onerror: null,
      } as unknown as EventSource;
    });

    const { getByText } = await renderTaskDetail();
    const duration = getByText("5:32");
    expect(duration.className).toMatch(/turnDuration/);
  });

  it("shows an explicit zero duration on collapsed turns without timing metadata", async () => {
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      const firstResult = resultEvent(0);
      const secondResult = resultEvent(0);
      if (!firstResult.result || !secondResult.result) throw new Error("result fixture is missing payload");
      firstResult.result.duration = 0;
      secondResult.result.duration = 0;
      const events: EventMessage[] = [
        { kind: "text", ts: 0, text: { text: "first response" } },
        firstResult,
        { kind: "userInput", ts: 0, userInput: { text: "second" } },
        { kind: "text", ts: 0, text: { text: "second response" } },
        secondResult,
      ];
      for (const event of events) emit(handlers, event);
      return {
        addEventListener: vi.fn(),
        close: vi.fn(),
        onerror: null,
      } as unknown as EventSource;
    });

    const { getAllByText } = await renderTaskDetail();

    expect(getAllByText("0s").some((duration) => duration.className.includes("turnDuration"))).toBe(true);
  });

  it("summarizes every turn in the collapsed session details", async () => {
    const user = userEvent.setup();
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      const firstResult = resultEvent(3_000);
      const secondResult = resultEvent(65_000);
      if (!firstResult.result || !secondResult.result) throw new Error("result fixture is missing payload");
      firstResult.result.duration = 2;
      secondResult.result.duration = 3;
      firstResult.result.usage.inputTokens = 100;
      secondResult.result.usage.inputTokens = 200;
      const events: EventMessage[] = [
        {
          kind: "init",
          ts: 1_000,
          init: {
            reportedModel: "test",
            agentVersion: "test",
            sessionID: "session-one",
            tools: [],
            cwd: "",
            harness: "test",
          },
        },
        {
          kind: "commitSnapshot",
          ts: 1_500,
          commitSnapshot: { baseline: true, repositoryCommits: [] },
        },
        { kind: "text", ts: 2_000, text: { text: "first response" } },
        firstResult,
        {
          kind: "commitSnapshot",
          ts: 3_100,
          commitSnapshot: {
            repositoryCommits: [],
            changeStat: { files: 2, linesAdded: 10, linesDeleted: 3, binaryFiles: 0 },
          },
        },
        { kind: "userInput", ts: 62_000, userInput: { text: "continue" } },
        { kind: "text", ts: 63_000, text: { text: "second response" } },
        secondResult,
        {
          kind: "commitSnapshot",
          ts: 65_100,
          commitSnapshot: {
            repositoryCommits: [],
            changeStat: { files: 4, linesAdded: 20, linesDeleted: 5, binaryFiles: 1 },
          },
        },
        { kind: "system", ts: 66_000, system: { subtype: "compact_boundary" } },
        {
          kind: "text",
          ts: 67_000,
          text: { text: "response after compaction" },
        },
        resultEvent(68_000),
      ];
      for (const event of events) emit(handlers, event);
      return {
        addEventListener: vi.fn(),
        close: vi.fn(),
        onerror: null,
      } as unknown as EventSource;
    });

    const { getByText, queryByText } = await renderTaskDetail();

    expect(getByText(/2 turns/).className).toMatch(/turnSummaryText/);
    expect(getByText("0:05").className).toMatch(/sessionDuration/);
    await user.click(screen.getByRole("button", { name: "Session invocation details" }));
    const dialog = screen.getByTestId("session-invocation-dialog");
    expect(dialog).toHaveTextContent("Combined turn time0:05");
    expect(dialog).toHaveTextContent("Combined API time0:02");
    expect(dialog).toHaveTextContent("Time awaiting user response0:59");
    expect(dialog).toHaveTextContent("Generated change6 file changes · +30 −8 · 1 binary");
    expect(dialog).toHaveTextContent("Cost$0.0000");
    expect(dialog).toHaveTextContent("New input300t");
    expect(queryByText("first response")).not.toBeInTheDocument();
  });

  it("shows recover actions for crashed tasks", async () => {
    const user = userEvent.setup();
    const onRevive = vi.fn();
    const { getByLabelText, getByText, getByTestId } = await renderTaskDetail({
      taskState: "crashed",
      onRevive,
    });
    expect(getByTestId("send-input")).toBeDisabled();

    await user.click(getByLabelText("Context actions"));
    await user.click(getByText("Revive"));

    expect(onRevive).toHaveBeenCalledWith("abc");
  });

  it("shows the complete task error", async () => {
    const error = "Error: failed to load extension from a very long runtime path";
    const { getByRole, getByText } = await renderTaskDetail({
      taskState: "failed",
      error,
    });

    expect(getByRole("heading", { name: "Task error" })).toBeInTheDocument();
    expect(getByText(error)).toBeInTheDocument();
  });

  it("keeps the prompt visible when a later input repeats it", async () => {
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      emit(handlers, {
        kind: "text",
        ts: 1,
        text: { text: "agent reply" },
      });
      emit(handlers, {
        kind: "userInput",
        ts: 2,
        userInput: { text: "same prompt" },
      });
      return {
        addEventListener: vi.fn(),
        close: vi.fn(),
        onerror: null,
      } as unknown as EventSource;
    });

    const { getByRole } = await renderTaskDetail({ initialPrompt: "same prompt" });

    expect(getByRole("heading", { name: "Prompt" })).toBeInTheDocument();
  });

  it("shows setup logs inside task details", async () => {
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      emit(handlers, {
        kind: "log",
        ts: 1,
        log: { line: "starting runtime" },
      });
      emit(handlers, {
        kind: "error",
        ts: 2,
        error: { err: "agent extension failed to load", line: "" },
      });
      return {
        addEventListener: vi.fn(),
        close: vi.fn(),
        onerror: null,
      } as unknown as EventSource;
    });

    const { getByRole, getByTestId, getByText } = await renderTaskDetail({
      taskState: "crashed",
      initialPrompt: "fetch origin then rebase on origin/main",
    });

    expect(getByRole("heading", { name: "Prompt" })).toBeInTheDocument();
    expect(getByText("fetch origin then rebase on origin/main")).toBeInTheDocument();
    expect(getByText("Setup logs")).toBeInTheDocument();
    expect(getByTestId("task-setup")).toHaveAttribute("open");
    expect(getByTestId("task-setup-logs")).toHaveTextContent("starting runtime");
    expect(getByTestId("task-message-area")).toContainElement(getByTestId("task-setup"));
    expect(getByTestId("task-message-area")).toHaveTextContent("agent extension failed to load");
  });

  it("shows zero instead of an implausible reconstructed setup duration", async () => {
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      emit(handlers, {
        kind: "log",
        ts: 1_500,
        log: { line: "starting runtime" },
      });
      emit(handlers, {
        kind: "init",
        ts: 46 * 60 * 60 * 1000,
        init: {
          reportedModel: "test",
          agentVersion: "test",
          sessionID: "session",
          cwd: "",
          harness: "test",
        },
      });
      return {
        addEventListener: vi.fn(),
        close: vi.fn(),
        onerror: null,
      } as unknown as EventSource;
    });

    const { getByText } = await renderTaskDetail({
      startedAt: new Date(1_000).toISOString(),
    });

    expect(getByText("0s")).toBeInTheDocument();
  });

  it("collapses setup logs after the agent session starts", async () => {
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      emit(handlers, {
        kind: "log",
        ts: 1_500,
        log: { line: "starting runtime" },
      });
      emit(handlers, {
        kind: "init",
        ts: 6_500,
        init: {
          reportedModel: "test",
          agentVersion: "test",
          sessionID: "session",
          cwd: "",
          harness: "test",
        },
      });
      return {
        addEventListener: vi.fn(),
        close: vi.fn(),
        onerror: null,
      } as unknown as EventSource;
    });

    const { getByTestId, getByText } = await renderTaskDetail({
      startedAt: new Date(1_000).toISOString(),
    });

    expect(getByTestId("task-setup")).not.toHaveAttribute("open");
    expect(getByText("0:06")).toBeInTheDocument();
  });

  it("shows in-progress context compaction", async () => {
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      emit(handlers, {
        kind: "system",
        ts: 1_000,
        system: { subtype: "compact_start", detail: "manual" },
      });
      return {
        addEventListener: vi.fn(),
        close: vi.fn(),
        onerror: null,
      } as unknown as EventSource;
    });

    await renderTaskDetail();

    expect(screen.getByTestId("compact-start")).toHaveTextContent("Compacting context… (manual)");
  });

  it("reports the harness context size on a compaction boundary", async () => {
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      emit(handlers, {
        kind: "system",
        ts: 1_000,
        system: {
          subtype: "compact_boundary",
          contextTokensBefore: 128_000,
          contextTokensAfter: 20_000,
        },
      });
      return {
        addEventListener: vi.fn(),
        close: vi.fn(),
        onerror: null,
      } as unknown as EventSource;
    });

    await renderTaskDetail();

    expect(screen.getByText(/Conversation compacted/)).toHaveTextContent(
      "Conversation compacted · 128kt → 20kt tokens",
    );
  });

  it("surfaces a failed context compaction", async () => {
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      emit(handlers, {
        kind: "system",
        ts: 1_000,
        system: { subtype: "compact_error", detail: "summarizer unavailable" },
      });
      return {
        addEventListener: vi.fn(),
        close: vi.fn(),
        onerror: null,
      } as unknown as EventSource;
    });

    await renderTaskDetail();

    expect(screen.getByTestId("compact-error")).toHaveTextContent("Context compaction failed: summarizer unavailable");
  });
});

// Helper type for a controllable fake EventSource.
type FakeES = {
  addEventListener: ReturnType<typeof vi.fn>;
  close: ReturnType<typeof vi.fn>;
  onerror: ((e: Event) => void) | null;
};

// Build a mock that fires the "ready" event synchronously so tests don't need
// to advance timers just to get the component into live mode.
function makeSyncReadyMock(created: FakeES[], capturedCb?: { value: ((ev: EventMessage) => void) | null }) {
  taskEventStreamMock.mockImplementation((_id, _cursor, handlers) => {
    if (capturedCb) capturedCb.value = (message) => emit(handlers, message);
    const fakeES: FakeES = {
      addEventListener: vi.fn(),
      close: vi.fn(),
      onerror: null,
    };
    created.push(fakeES);
    return fakeES as unknown as EventSource;
  });
}

describe("streamed conversation", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("multiple thinking blocks across tool calls are both visible", async () => {
    // Regression: ThinkingCard used findLast, so when thinking-1 → tool-1 → thinking-2 → tool-2
    // merged into one action group, only thinking-2 was rendered and thinking-1 was dropped.
    const created: FakeES[] = [];
    const capturedCb = { value: null as ((ev: EventMessage) => void) | null };
    makeSyncReadyMock(created, capturedCb);

    await renderTaskDetail();
    if (!capturedCb.value) throw new Error("history callback not captured");

    const cb = capturedCb.value;
    cb({ kind: "thinking", ts: 1, thinking: { text: "planning tool 1" } });
    cb({
      kind: "toolUse",
      ts: 2,
      toolUse: { toolUseID: "t1", name: "Read", input: {} },
    });
    cb({
      kind: "usage",
      ts: 3,
      usage: {
        inputTokens: 10,
        outputTokens: 5,
        cacheCreationInputTokens: 0,
        cacheReadInputTokens: 0,
        reportedModel: "m",
      },
    });
    cb({ kind: "thinking", ts: 4, thinking: { text: "planning tool 2" } });
    cb({
      kind: "toolUse",
      ts: 5,
      toolUse: { toolUseID: "t2", name: "Bash", input: {} },
    });
    cb(resultEvent(6));

    expect(document.body.textContent).toContain("planning tool 1");
    expect(document.body.textContent).toContain("planning tool 2");
  });

  it("labels overage as a reached 5-hour quota", async () => {
    const created: FakeES[] = [];
    const capturedCb = { value: null as ((ev: EventMessage) => void) | null };
    makeSyncReadyMock(created, capturedCb);

    await renderTaskDetail();
    if (!capturedCb.value) throw new Error("history callback not captured");

    capturedCb.value({
      kind: "rateLimit",
      ts: 1,
      rateLimit: {
        status: "rejected",
        rateLimitType: "five_hour",
        utilization: 1,
        isUsingOverage: true,
        resetsAt: "2024-03-21T04:26:40Z" as ISOTimestamp,
        overageResetsAt: "2024-04-01T00:00:00Z" as ISOTimestamp,
      },
    });
    vi.advanceTimersByTime(100);

    const resetTime = new Date("2024-03-21T04:26:40Z").toLocaleTimeString();
    expect(document.body.textContent).toContain(`5-hour quota reached; using extra usage · resets at ${resetTime}`);
    expect(document.body.textContent).not.toContain("monthly");
  });

  it("counts the local midnights crossed before a quota reset", async () => {
    vi.setSystemTime(new Date("2026-07-08T12:00:00Z"));
    const created: FakeES[] = [];
    const capturedCb = { value: null as ((ev: EventMessage) => void) | null };
    makeSyncReadyMock(created, capturedCb);

    await renderTaskDetail();
    if (!capturedCb.value) throw new Error("history callback not captured");

    capturedCb.value({
      kind: "rateLimit",
      ts: 1,
      rateLimit: {
        status: "rejected",
        rateLimitType: "seven_day",
        utilization: 1,
        resetsAt: "2026-07-10T13:00:00Z" as ISOTimestamp,
      },
    });
    vi.advanceTimersByTime(100);

    const resetTime = new Date("2026-07-10T13:00:00Z").toLocaleTimeString();
    expect(document.body.textContent).toContain(`resets in 2 days at ${resetTime}`);
  });

  it("labels a reset on the next calendar day as tomorrow", async () => {
    vi.setSystemTime(new Date("2026-07-08T12:00:00Z"));
    const created: FakeES[] = [];
    const capturedCb = { value: null as ((ev: EventMessage) => void) | null };
    makeSyncReadyMock(created, capturedCb);

    await renderTaskDetail();
    if (!capturedCb.value) throw new Error("history callback not captured");

    capturedCb.value({
      kind: "rateLimit",
      ts: 1,
      rateLimit: {
        status: "allowed_warning",
        rateLimitType: "seven_day",
        utilization: 0.89,
        resetsAt: "2026-07-09T13:00:00Z" as ISOTimestamp,
      },
    });
    vi.advanceTimersByTime(100);

    const resetTime = new Date("2026-07-09T13:00:00Z").toLocaleTimeString();
    expect(document.body.textContent).toContain("⚠️ 89% of 7-day quota used");
    expect(document.body.textContent).toContain(`resets tomorrow at ${resetTime}`);
  });

  it("omits the percentage when a warning reports no utilization", async () => {
    vi.setSystemTime(new Date("2026-07-08T12:00:00Z"));
    const created: FakeES[] = [];
    const capturedCb = { value: null as ((ev: EventMessage) => void) | null };
    makeSyncReadyMock(created, capturedCb);

    await renderTaskDetail();
    if (!capturedCb.value) throw new Error("history callback not captured");

    capturedCb.value({
      kind: "rateLimit",
      ts: 1,
      rateLimit: {
        status: "allowed_warning",
        rateLimitType: "seven_day_opus",
        utilization: -1,
        resetsAt: "2026-07-09T13:00:00Z" as ISOTimestamp,
      },
    });
    vi.advanceTimersByTime(100);

    expect(document.body.textContent).toContain("⚠️ seven-day-opus quota nearing its limit");
    expect(document.body.textContent).not.toContain("-100%");
  });

  it("live textDelta events render before the turn ends", async () => {
    // Regression: initial task detail streaming must show output while the
    // first agent turn is still running, not only after the result event.
    const created: FakeES[] = [];
    const capturedCb = { value: null as ((ev: EventMessage) => void) | null };
    makeSyncReadyMock(created, capturedCb);

    await renderTaskDetail();

    expect(capturedCb.value).not.toBeNull();

    if (!capturedCb.value) throw new Error("history callback not captured");
    capturedCb.value({
      kind: "textDelta",
      ts: 1,
      textDelta: { text: "agent reply" },
    });
    expect(document.body.textContent).not.toContain("agent reply");

    vi.advanceTimersByTime(100);

    expect(document.body.textContent).toContain("agent reply");
  });

  it("renders every live textDelta batch after a follow-up prompt", async () => {
    const created: FakeES[] = [];
    const capturedCb = { value: null as ((ev: EventMessage) => void) | null };
    makeSyncReadyMock(created, capturedCb);

    await renderTaskDetail();

    if (!capturedCb.value) throw new Error("history callback not captured");
    capturedCb.value(resultEvent(1));
    capturedCb.value({
      kind: "userInput",
      ts: 2,
      userInput: { text: "follow up" },
    });
    capturedCb.value({
      kind: "textDelta",
      ts: 3,
      textDelta: { text: "first batch" },
    });
    vi.advanceTimersByTime(100);
    expect(document.body.textContent).toContain("first batch");

    capturedCb.value({
      kind: "textDelta",
      ts: 4,
      textDelta: { text: " then second batch" },
    });
    vi.advanceTimersByTime(100);

    expect(document.body.textContent).toContain("first batch then second batch");
  });

  it("live ask events render immediately so the user can answer", async () => {
    const created: FakeES[] = [];
    const capturedCb = { value: null as ((ev: EventMessage) => void) | null };
    makeSyncReadyMock(created, capturedCb);

    const { getByTestId } = await renderTaskDetail({ taskState: "pending" });

    if (!capturedCb.value) throw new Error("history callback not captured");
    capturedCb.value({
      kind: "ask",
      ts: 1,
      ask: {
        toolUseID: "ask_1",
        questions: [
          {
            question: "Which option?",
            options: [{ label: "A" }, { label: "B" }],
          },
        ],
      },
    });

    expect(document.body.textContent).toContain("Which option?");
    expect(getByTestId("ask-option-A")).toBeEnabled();
    expect(getByTestId("task-detail-form")).toBeInTheDocument();
  });

  it("opens and navigates the context menu with arrow keys", async () => {
    vi.useRealTimers();
    const user = userEvent.setup();
    const created: FakeES[] = [];
    makeSyncReadyMock(created);

    const { getByLabelText, getByText, queryByText } = await renderTaskDetail({
      taskState: "waiting",
      supportsCompact: true,
      autoFocusPrompt: false,
    });

    // The overflow menu toggle should be present.
    const toggle = getByLabelText("Context actions");
    expect(toggle).toBeInTheDocument();

    // Menu items should not be visible before clicking.
    expect(queryByText("Clear context")).not.toBeInTheDocument();
    expect(queryByText("Compact context")).not.toBeInTheDocument();

    toggle.focus();
    await user.keyboard("{ArrowDown}");

    await vi.waitFor(() => expect(getByText("Push")).toHaveFocus());
    expect(getByText("Clear context")).toBeInTheDocument();
    expect(getByText("Compact context")).toBeInTheDocument();

    await user.keyboard("{ArrowDown}");
    expect(getByText("Push to main")).toHaveFocus();

    await user.keyboard("{ArrowDown}");
    expect(getByText("Stop")).toHaveFocus();

    await user.keyboard("{ArrowDown}{ArrowDown}");
    expect(getByText("Compact context")).toHaveFocus();
  });

  it("sends the reply with Ctrl+Enter from an action button without opening it", async () => {
    vi.useRealTimers();
    const user = userEvent.setup();
    const sendInput = vi.fn(async () => {});
    {
      await renderTaskDetail({ inputDraft: "follow up", autoFocusPrompt: false, onSendInput: sendInput });
      const actions = screen.getByRole("button", { name: "Context actions" });
      actions.focus();

      await user.keyboard("{Control>}{Enter}{/Control}");

      expect(sendInput).toHaveBeenCalledOnce();
      expect(sendInput).toHaveBeenCalledWith();
      expect(screen.queryByRole("menuitem", { name: "Push" })).not.toBeInTheDocument();
    }
  });

  it("does not send with Ctrl+Enter when the reply is disabled", async () => {
    vi.useRealTimers();
    const user = userEvent.setup();
    const sendInput = vi.fn(async () => {});
    {
      await renderTaskDetail({
        taskState: "crashed",
        inputDraft: "follow up",
        autoFocusPrompt: false,
        onSendInput: sendInput,
      });
      expect(screen.getByTestId("send-input")).toBeDisabled();
      screen.getByRole("button", { name: "Context actions" }).focus();

      await user.keyboard("{Control>}{Enter}{/Control}");

      expect(sendInput).not.toHaveBeenCalled();
    }
  });

  it("context menu is visible but items are disabled when task is running", async () => {
    vi.useRealTimers();
    const user = userEvent.setup();
    const created: FakeES[] = [];
    makeSyncReadyMock(created);

    const { getByLabelText, getByText } = await renderTaskDetail({
      taskState: "running",
      supportsCompact: true,
    });

    // Toggle is present even when running.
    const toggle = getByLabelText("Context actions");
    expect(toggle).toBeInTheDocument();

    await user.click(toggle);

    // Menu items are visible but disabled.
    const clearBtn = getByText("Clear context");
    const compactBtn = getByText("Compact context");
    expect(clearBtn).toBeDisabled();
    expect(compactBtn).toBeDisabled();
  });

  it("compact item is hidden when supportsCompact is false", async () => {
    vi.useRealTimers();
    const user = userEvent.setup();
    const created: FakeES[] = [];
    makeSyncReadyMock(created);

    const { getByLabelText, getByText, queryByText } = await renderTaskDetail({
      taskState: "waiting",
      supportsCompact: false,
    });

    await user.click(getByLabelText("Context actions"));

    expect(getByText("Clear context")).toBeInTheDocument();
    expect(queryByText("Compact context")).not.toBeInTheDocument();
  });

  it("shows initial prompt for failed task with no messages", async () => {
    const created: FakeES[] = [];
    makeSyncReadyMock(created);

    const { getByText } = await renderTaskDetail({
      taskState: "failed",
      initialPrompt: "Fix the login bug",
    });

    expect(getByText("Fix the login bug")).toBeInTheDocument();
  });
});

it("shows a carried input's server-calculated wait without loading the previous result", async () => {
  const events: EventMessage[] = [
    {
      kind: "init",
      ts: 1,
      init: { sessionID: "wait", reportedModel: "m", agentVersion: "1", cwd: "/", harness: "claude" },
    },
    resultEvent(1000),
    { kind: "userInput", ts: 5000, userInput: { text: "Carried continuation" } },
    { kind: "system", ts: 6000, system: { subtype: "compact_boundary" } },
    { kind: "text", ts: 7000, text: { text: "Current reply" } },
    resultEvent(8000),
  ];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) =>
    fixtureHistoryResponse(
      String(input),
      events.map((message, index) => ({ recordID: index + 1, index: 0, message })),
      "",
    ),
  );
  await renderTaskDetail({ taskId: "carried-wait" });
  expect(screen.getByText("wait 0:04")).toBeInTheDocument();
  expect(screen.getByText("Current reply")).toBeInTheDocument();
});

it("cancels a gated archived body when collapsed and keeps the current DOM mounted", async () => {
  const events: HistoryEvent[] = [
    {
      recordID: 1,
      index: 0,
      message: {
        kind: "init",
        ts: 1,
        init: { sessionID: "first", reportedModel: "m", agentVersion: "1", cwd: "/", harness: "claude" },
      },
    },
    { recordID: 2, index: 0, message: { kind: "text", ts: 2, text: { text: "Never mounted archive" } } },
    { recordID: 3, index: 0, message: resultEvent(3) },
    { recordID: 4, index: 0, message: { kind: "system", ts: 4, system: { subtype: "compact_boundary" } } },
    { recordID: 5, index: 0, message: { kind: "text", ts: 5, text: { text: "Stable current response" } } },
    { recordID: 6, index: 0, message: resultEvent(6) },
  ];
  let release: (response: Response) => void = () => {};
  let signal: AbortSignal | null = null;
  vi.spyOn(globalThis, "fetch").mockImplementation((input, init) => {
    if (String(input).endsWith("/slice/1/0/3/0")) {
      signal = init?.signal ?? null;
      return new Promise((resolve) => {
        release = resolve;
      });
    }
    return Promise.resolve(fixtureHistoryResponse(String(input), events, ""));
  });
  const view = await renderTaskDetail({ taskId: "archive-cancellation" });
  const current = screen.getByText("Stable current response");
  const header = screen.getByRole("button", { name: /^Compacted session/ });
  const user = userEvent.setup();
  await user.click(header);
  await waitFor(() => assert.ok(signal));
  await user.click(header);
  assert.equal((signal as AbortSignal | null)?.aborted, true);
  release(historyResponse(events.slice(0, 3)));
  expect(screen.queryByText("Never mounted archive")).not.toBeInTheDocument();
  expect(screen.getByText("Stable current response")).toBe(current);
  expect(screen.getByRole("button", { name: /^Compacted session/ })).toBe(header);
  view.unmount();
});

it("keeps an open cross-context child focused when it completes, archiving it on close", async () => {
  let send: ((message: EventMessage) => void) | undefined;
  taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
    send = (message) => emit(handlers, message);
    for (const message of [
      {
        kind: "init",
        ts: 1,
        init: { sessionID: "spanning", reportedModel: "m", agentVersion: "1", cwd: "/", harness: "claude" },
      },
      {
        kind: "nativeSubagent",
        ts: 1000,
        nativeSubagent: { id: "spanning-child", label: "Spanning review", status: "running" },
      },
      { kind: "text", ts: 1500, source: { agentID: "spanning-child" }, text: { text: "Old child output" } },
      resultEvent(2000),
      { kind: "system", ts: 3000, system: { subtype: "compact_boundary" } },
      { kind: "text", ts: 4000, text: { text: "Current conversation" } },
    ] satisfies EventMessage[])
      emit(handlers, message);
    return { close: vi.fn() } as unknown as EventSource;
  });
  await renderTaskDetail({ taskId: "spanning-child-focus" });
  const card = screen.getByTestId("native-subagent-card");
  const summary = card.querySelector("summary");
  assert.ok(summary);
  const user = userEvent.setup();
  await user.click(summary);
  await screen.findByText("Old child output");
  await user.click(screen.getByRole("button", { name: /^Compacted session/ }));
  summary.focus();
  send?.({
    kind: "nativeSubagent",
    ts: 5000,
    nativeSubagent: { id: "spanning-child", status: "completed", result: "Review complete" },
  });
  await waitFor(() => expect(within(card).getByText("Completed")).toBeInTheDocument());
  expect(screen.getByTestId("native-subagent-card")).toBe(card);
  expect(card).toHaveAttribute("open");
  expect(summary).toHaveFocus();
  await user.click(summary);
  await waitFor(() => expect(screen.getAllByTestId("native-subagent-card")).toHaveLength(1));
  expect(screen.getByTestId("native-subagent-card")).not.toBe(card);
  await waitFor(() => expect(screen.getByRole("button", { name: /^Compacted session/ })).toHaveFocus());
  expect(screen.getByTestId("native-subagent-card")).toHaveTextContent("Completed");
});

it("keeps completed background work in its origin section across equal-time compaction", async () => {
  taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
    for (const message of [
      {
        kind: "init",
        ts: 1000,
        init: { sessionID: "a", reportedModel: "m", agentVersion: "1", cwd: "/", harness: "claude" },
      },
      {
        kind: "backgroundCommand",
        ts: 1000,
        backgroundCommand: { id: "origin-shell", label: "Old shell", status: "running" },
      },
      resultEvent(1000),
      { kind: "system", ts: 1000, system: { subtype: "compact_boundary" } },
      { kind: "backgroundCommand", ts: 2000, backgroundCommand: { id: "origin-shell", status: "completed" } },
      { kind: "text", ts: 2000, text: { text: "Current context" } },
    ] satisfies EventMessage[])
      emit(handlers, message);
    return { close: vi.fn() } as unknown as EventSource;
  });
  await renderTaskDetail({ taskId: "background-origin" });
  expect(screen.queryByTestId("background-command-card")).not.toBeInTheDocument();
  await userEvent.setup().click(screen.getByRole("button", { name: /^Compacted session/ }));
  expect(await screen.findByTestId("background-command-card")).toHaveTextContent("Old shell");
});

for (const boundary of ["compaction", "changed init"]) {
  it(`keeps carried native and background work current after ${boundary}`, async () => {
    let send: ((message: EventMessage) => void) | undefined;
    const init = (sessionID: string): EventMessage => ({
      kind: "init",
      ts: 1000,
      init: { sessionID, reportedModel: "m", agentVersion: "1", cwd: "/", harness: "claude" },
    });
    taskEventStreamMock.mockImplementationOnce((_id, _cursor, handlers) => {
      send = (message) => emit(handlers, message);
      for (const message of [
        init("one"),
        { kind: "text", ts: 1000, text: { text: "First turn" } },
        resultEvent(1000),
        { kind: "userInput", ts: 1000, userInput: { text: "Unfinished turn" } },
        {
          kind: "nativeSubagent",
          ts: 1000,
          nativeSubagent: { id: "carried-child", label: "Carried child", status: "running" },
        },
        { kind: "text", ts: 1000, source: { agentID: "carried-child" }, text: { text: "Carried child transcript" } },
        {
          kind: "backgroundCommand",
          ts: 1000,
          backgroundCommand: { id: "carried-shell", label: "Carried shell", status: "running" },
        },
      ] satisfies EventMessage[])
        emit(handlers, message);
      return { close: vi.fn() } as unknown as EventSource;
    });
    await renderTaskDetail({ taskId: `carried-${boundary}` });
    const card = screen.getByTestId("native-subagent-card");
    const summary = card.querySelector("summary");
    assert.ok(summary);
    const user = userEvent.setup();
    await user.click(summary);
    await screen.findByText("Carried child transcript");
    summary.focus();
    send?.(
      boundary === "compaction" ? { kind: "system", ts: 1000, system: { subtype: "compact_boundary" } } : init("two"),
    );
    send?.({ kind: "text", ts: 1000, text: { text: "Second turn" } });
    send?.(resultEvent(1000));
    send?.({ kind: "nativeSubagent", ts: 1000, nativeSubagent: { id: "carried-child", status: "completed" } });
    send?.({ kind: "backgroundCommand", ts: 1000, backgroundCommand: { id: "carried-shell", status: "completed" } });
    await waitFor(() => expect(within(card).getByText("Completed")).toBeInTheDocument());
    expect(screen.getByTestId("native-subagent-card")).toBe(card);
    expect(summary).toHaveFocus();
    const archive = screen.getByTestId("history-section");
    const archived = within(archive).getByRole("button", { name: /^Compacted session/ });
    await user.click(archived);
    expect(screen.getAllByTestId("native-subagent-card")).toHaveLength(1);
    expect(screen.getAllByTestId("background-command-card")).toHaveLength(1);
    expect(within(archive).queryByTestId("native-subagent-card")).not.toBeInTheDocument();
    expect(within(archive).queryByTestId("background-command-card")).not.toBeInTheDocument();
    await user.click(summary);
    expect(screen.getByTestId("native-subagent-card")).toBe(card);
    expect(card).not.toHaveAttribute("open");
    expect(summary).toHaveFocus();
  });
}

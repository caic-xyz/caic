// Tests for the TaskDetail diff link navigation and SSE connection behaviour.
import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";
import type { EventMessage } from "@sdk/types.gen";
import { type JSX } from "solid-js";

const navigateMock = vi.fn();

// Mock the router so SSE tests don't need a real Router context.
vi.mock("@solidjs/router", () => ({
  useNavigate: () => navigateMock,
  useLocation: () => ({ pathname: "/task/@abc+test-task", query: {} }),
  A: (props: Record<string, unknown>) => (
    <a
      href={props.href as string}
      class={props.class as string}
      onClick={(e: MouseEvent) => {
        e.preventDefault();
        navigateMock(props.href);
      }}
    >
      {props.children as JSX.Element}
    </a>
  ),
}));

// Mock the API module to stub out EventSource (SSE) and other network calls.
vi.mock("../api", () => ({
  taskEventStream: vi.fn((_id: string, _cb: unknown) => {
    const fakeES = {
      addEventListener: vi.fn((_event: string, _handler: () => void) => {}),
      close: vi.fn(),
      onerror: null as ((e: Event) => void) | null,
    };
    // Fire "ready" asynchronously so the component transitions to live mode.
    setTimeout(() => {
      const readyCb = (fakeES.addEventListener as ReturnType<typeof vi.fn>).mock.calls.find(
        (c: unknown[]) => c[0] === "ready",
      );
      if (readyCb) (readyCb[1] as () => void)();
    }, 0);
    return fakeES;
  }),
  sendInput: vi.fn(),
  restartTask: vi.fn(),
  clearContext: vi.fn(() => Promise.resolve({ status: "cleared" })),
  compactContext: vi.fn(() => Promise.resolve({ status: "compacting" })),
  syncTask: vi.fn(),
  getTaskDiff: vi.fn(),
}));

// Import after mocks are set up.
import TaskDetail, { resetTaskDetailCachesForTest } from "./TaskDetail";
import { taskEventStream } from "../api";
import { HostModeProvider } from "../hostMode";

const baseProps = {
  taskId: "abc",
  taskState: "running",
  repo: "my-repo",
  remoteURL: "https://github.com/org/my-repo",
  branch: "feature-branch",
  baseBranch: "main",
  harness: "claude",
  onClose: () => {},
  onStop: () => {},
  onPurge: () => {},
  onRevive: () => {},
  inputDraft: "",
  onInputDraft: () => {},
  inputImages: [],
  onInputImages: () => {},
  onError: () => {},
};

function renderTaskDetail(props: Partial<Parameters<typeof TaskDetail>[0]> = {}) {
  return render(() => (
    <HostModeProvider>
      <TaskDetail {...baseProps} {...props} />
    </HostModeProvider>
  ));
}

function resultEvent(ts: number): EventMessage {
  return {
    kind: "result",
    ts,
    result: {
      subtype: "success",
      isError: false,
      result: "done",
      totalCostUSD: 0,
      duration: 1,
      durationAPI: 1,
      numTurns: 1,
      usage: {
        inputTokens: 0,
        outputTokens: 0,
        cacheCreationInputTokens: 0,
        cacheReadInputTokens: 0,
        model: "test",
      },
    },
  };
}

describe("TaskDetail", () => {

  afterEach(() => {
    navigateMock.mockClear();
    resetTaskDetailCachesForTest();
  });

  it("shows Diff link when diffStat has items", () => {
    const { getByText } = renderTaskDetail({ diffStat: [{ path: "file.ts", added: 10, deleted: 2 }] });
    expect(getByText("Diff")).toBeInTheDocument();
  });

  it("hides Diff link when diffStat is empty", () => {
    const { queryByText } = renderTaskDetail({ diffStat: [] });
    expect(queryByText("Diff")).not.toBeInTheDocument();
  });

  it("hides Diff link when diffStat is undefined", () => {
    const { queryByText } = renderTaskDetail({ diffStat: undefined });
    expect(queryByText("Diff")).not.toBeInTheDocument();
  });

  it("diff link href ends with /diff", () => {
    const { getByText } = renderTaskDetail({ diffStat: [{ path: "file.ts", added: 5, deleted: 1 }] });
    const link = getByText("Diff");
    expect(link.getAttribute("href")).toBe("/task/@abc+test-task/diff");
  });

  it("clicking diff link calls navigate with path/diff", async () => {
    const user = userEvent.setup();
    const { getByText } = renderTaskDetail({ diffStat: [{ path: "file.ts", added: 5, deleted: 1 }] });
    await user.click(getByText("Diff"));
    expect(navigateMock).toHaveBeenCalledWith("/task/@abc+test-task/diff");
  });

  it("shows recover actions for crashed tasks", async () => {
    const user = userEvent.setup();
    const onRevive = vi.fn();
    const { getByLabelText, getByText, getByTestId } = renderTaskDetail({ taskState: "crashed", onRevive });
    expect(getByTestId("send-input")).toBeDisabled();

    await user.click(getByLabelText("Context actions"));
    await user.click(getByText("Revive"));

    expect(onRevive).toHaveBeenCalledWith("abc");
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
function makeSyncReadyMock(
  created: FakeES[],
  capturedCb?: { value: ((ev: EventMessage) => void) | null },
) {
  vi.mocked(taskEventStream).mockImplementation((_id, cb) => {
    if (capturedCb) capturedCb.value = cb as (ev: EventMessage) => void;
    const fakeES: FakeES = {
      addEventListener: vi.fn((event: string, handler: () => void) => {
        if (event === "ready") handler();
      }),
      close: vi.fn(),
      onerror: null,
    };
    created.push(fakeES);
    return fakeES as unknown as EventSource;
  });
}

function makeManualReadyMock(
  created: FakeES[],
  capturedCb: { value: ((ev: EventMessage) => void) | null },
  readyHandler: { value: (() => void) | null },
) {
  vi.mocked(taskEventStream).mockImplementation((_id, cb) => {
    capturedCb.value = cb as (ev: EventMessage) => void;
    const fakeES: FakeES = {
      addEventListener: vi.fn((event: string, handler: () => void) => {
        if (event === "ready") readyHandler.value = handler;
      }),
      close: vi.fn(),
      onerror: null,
    };
    created.push(fakeES);
    return fakeES as unknown as EventSource;
  });
}

describe("SSE connection", () => {
  beforeEach(() => {
    resetTaskDetailCachesForTest();
    // Fake timers so we can control setTimeout for reconnect delays.
    vi.useFakeTimers();
  });

  afterEach(() => {
    resetTaskDetailCachesForTest();
    vi.useRealTimers();
  });

  it("duplicate onerror fires schedule only one reconnect", () => {
    // Regression test for the clearTimeout fix: a second onerror (which some
    // EventSource implementations fire) must cancel the first reconnect timer
    // and schedule exactly one new one, not pile up two connect() calls.
    // Pin Math.random so jitteredDelay is deterministic (factor = 0.75 + 0.5*0.5 = 1.0).
    const origRandom = Math.random;
    Math.random = () => 0.5;
    try {
      const created: FakeES[] = [];
      makeSyncReadyMock(created);

      renderTaskDetail();

      // createEffect runs synchronously during render in SolidJS.
      expect(created).toHaveLength(1);
      const es1 = created[0];

      // First onerror: sets timer (jitteredDelay(500)=500ms), es → null, delay → 750.
      if (!es1.onerror) throw new Error("onerror not set");
      es1.onerror(new Event("error"));
      // Second onerror: clears first timer, sets new timer (jitteredDelay(750)=750ms), delay → 1125.
      es1.onerror(new Event("error"));

      // Advance past the 750 ms timer (but not far enough to trigger a third).
      vi.advanceTimersByTime(800);

      // Exactly one reconnect: initial connect (1) + one timer-fired connect (2).
      expect(created).toHaveLength(2);
    } finally {
      Math.random = origRandom;
    }
  });

  it("multiple thinking blocks across tool calls are both visible", () => {
    // Regression: ThinkingCard used findLast, so when thinking-1 → tool-1 → thinking-2 → tool-2
    // merged into one action group, only thinking-2 was rendered and thinking-1 was dropped.
    const created: FakeES[] = [];
    const capturedCb = { value: null as ((ev: EventMessage) => void) | null };
    makeSyncReadyMock(created, capturedCb);

    renderTaskDetail();
    if (!capturedCb.value) throw new Error("taskEvents callback not captured");

    const cb = capturedCb.value;
    cb({ kind: "thinking", ts: 1, thinking: { text: "planning tool 1" } });
    cb({ kind: "toolUse", ts: 2, toolUse: { toolUseID: "t1", name: "Read", input: {} } });
    cb({ kind: "usage", ts: 3, usage: { inputTokens: 10, outputTokens: 5, cacheCreationInputTokens: 0, cacheReadInputTokens: 0, model: "m" } });
    cb({ kind: "thinking", ts: 4, thinking: { text: "planning tool 2" } });
    cb({ kind: "toolUse", ts: 5, toolUse: { toolUseID: "t2", name: "Bash", input: {} } });
    cb(resultEvent(6));

    expect(document.body.textContent).toContain("planning tool 1");
    expect(document.body.textContent).toContain("planning tool 2");
  });

  it("replayed textDelta events render once the SSE ready marker arrives", () => {
    // Replayed history can be huge; keep it off the DOM until the server sends
    // ready so grouping and reconciliation run once for the replay.
    const created: FakeES[] = [];
    const capturedCb = { value: null as ((ev: EventMessage) => void) | null };
    const readyHandler = { value: null as (() => void) | null };
    makeManualReadyMock(created, capturedCb, readyHandler);

    renderTaskDetail();

    if (!capturedCb.value) throw new Error("taskEvents callback not captured");
    capturedCb.value({ kind: "textDelta", ts: 1, textDelta: { text: "replayed output" } });

    expect(document.body.textContent).not.toContain("replayed output");
    expect(readyHandler.value).not.toBeNull();

    readyHandler.value?.();

    expect(document.body.textContent).toContain("replayed output");
  });

  it("live textDelta events render at the end of the turn", () => {
    // Verifies the buffered streaming path: deltas stay off the DOM until a
    // structural event ends the turn.
    const created: FakeES[] = [];
    const capturedCb = { value: null as ((ev: EventMessage) => void) | null };
    makeSyncReadyMock(created, capturedCb);

    renderTaskDetail();

    expect(capturedCb.value).not.toBeNull();

    // Push a textDelta live event (component is in live mode because ready fired).
    if (!capturedCb.value) throw new Error("taskEvents callback not captured");
    capturedCb.value({ kind: "textDelta", ts: 1, textDelta: { text: "agent reply" } });
    expect(document.body.textContent).not.toContain("agent reply");

    capturedCb.value(resultEvent(2));

    expect(document.body.textContent).toContain("agent reply");
  });

  it("live ask events render immediately so the user can answer", () => {
    const created: FakeES[] = [];
    const capturedCb = { value: null as ((ev: EventMessage) => void) | null };
    makeSyncReadyMock(created, capturedCb);

    renderTaskDetail({ taskState: "asking" });

    if (!capturedCb.value) throw new Error("taskEvents callback not captured");
    capturedCb.value({
      kind: "ask",
      ts: 1,
      ask: {
        toolUseID: "ask_1",
        questions: [{ question: "Which option?", options: [{ label: "A" }, { label: "B" }] }],
      },
    });

    expect(document.body.textContent).toContain("Which option?");
  });

  it("context menu button is visible in waiting state and opens menu with actions", async () => {
    vi.useRealTimers();
    const user = userEvent.setup();
    const created: FakeES[] = [];
    makeSyncReadyMock(created);

    const { getByLabelText, getByText, queryByText } = renderTaskDetail({ taskState: "waiting", supportsCompact: true });

    // The overflow menu toggle should be present.
    const toggle = getByLabelText("Context actions");
    expect(toggle).toBeInTheDocument();

    // Menu items should not be visible before clicking.
    expect(queryByText("Clear context")).not.toBeInTheDocument();
    expect(queryByText("Compact context")).not.toBeInTheDocument();

    // Click the toggle to open the menu.
    await user.click(toggle);

    // Both menu items should now be visible.
    expect(getByText("Clear context")).toBeInTheDocument();
    expect(getByText("Compact context")).toBeInTheDocument();
  });

  it("context menu is visible but items are disabled when task is running", async () => {
    vi.useRealTimers();
    const user = userEvent.setup();
    const created: FakeES[] = [];
    makeSyncReadyMock(created);

    const { getByLabelText, getByText } = renderTaskDetail({ taskState: "running", supportsCompact: true });

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

    const { getByLabelText, getByText, queryByText } = renderTaskDetail({ taskState: "waiting", supportsCompact: false });

    await user.click(getByLabelText("Context actions"));

    expect(getByText("Clear context")).toBeInTheDocument();
    expect(queryByText("Compact context")).not.toBeInTheDocument();
  });

  it("stops reconnecting for failed task with no messages after ready", () => {
    // Regression: before the fix, the stop condition required messages().length > 0,
    // so a failed task that produced no agent output would reconnect indefinitely.
    const created: FakeES[] = [];
    makeSyncReadyMock(created);

    renderTaskDetail({ taskState: "failed" });
    expect(created).toHaveLength(1);

    // ready fired synchronously; now simulate SSE close (server closes after ready for failed tasks).
    const es1 = created[0];
    if (!es1.onerror) throw new Error("onerror not set");
    es1.onerror(new Event("error"));

    // Advance well past any reconnect delay — no new connection should be created.
    vi.advanceTimersByTime(60_000);
    expect(created).toHaveLength(1);
  });

  it("stops reconnecting for failed task with messages after ready", () => {
    // Existing behaviour: with messages this already worked; verify it still does.
    const created: FakeES[] = [];
    const capturedCb = { value: null as ((ev: EventMessage) => void) | null };
    makeSyncReadyMock(created, capturedCb);

    renderTaskDetail({ taskState: "failed" });
    if (!capturedCb.value) throw new Error("taskEvents callback not captured");

    // Deliver a message so messages().length > 0.
    capturedCb.value({ kind: "textDelta", ts: 1, textDelta: { text: "some output" } });
    vi.advanceTimersByTime(20);

    const es1 = created[0];
    if (!es1.onerror) throw new Error("onerror not set");
    es1.onerror(new Event("error"));

    vi.advanceTimersByTime(60_000);
    expect(created).toHaveLength(1);
  });

  it("shows initial prompt for failed task with no messages", () => {
    const created: FakeES[] = [];
    makeSyncReadyMock(created);

    const { getByText } = renderTaskDetail({ taskState: "failed", initialPrompt: "Fix the login bug" });

    expect(getByText("Fix the login bug")).toBeInTheDocument();
  });
});

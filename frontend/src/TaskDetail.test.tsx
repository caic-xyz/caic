// Tests for the TaskDetail diff link navigation and SSE connection behaviour.
import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";
import type { EventMessage } from "@sdk/types.gen";

const navigateMock = vi.fn();

// Mock the router to avoid .jsx resolution issues with @solidjs/router dist.
vi.mock("@solidjs/router", () => ({
  useNavigate: () => navigateMock,
  useLocation: () => ({ pathname: "/task/@abc+test-task" }),
  A: (props: Record<string, unknown>) => (
    <a
      href={props.href as string}
      class={props.class as string}
      onClick={(e: MouseEvent) => {
        e.preventDefault();
        navigateMock(props.href);
      }}
    >
      {props.children as unknown}
    </a>
  ),
}));

// Mock the API module to stub out EventSource (SSE) and other network calls.
vi.mock("./api", () => ({
  taskEvents: vi.fn((_id: string, _cb: unknown) => {
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
  syncTask: vi.fn(),
  getTaskDiff: vi.fn(),
}));

// Import after mocks are set up.
import TaskDetail from "./TaskDetail";
import { taskEvents } from "./api";

afterEach(() => {
  navigateMock.mockClear();
});

const baseProps = {
  taskId: "abc",
  taskState: "running",
  repo: "my-repo",
  remoteURL: "https://github.com/org/my-repo",
  branch: "feature-branch",
  baseBranch: "main",
  harness: "claude",
  onClose: () => {},
  inputDraft: "",
  onInputDraft: () => {},
  inputImages: [],
  onInputImages: () => {},
};

describe("TaskDetail", () => {

  it("shows Diff link when diffStat has items", () => {
    const { getByText } = render(() => (
      <TaskDetail {...baseProps} diffStat={[{ path: "file.ts", added: 10, deleted: 2 }]} />
    ));
    expect(getByText("Diff")).toBeInTheDocument();
  });

  it("hides Diff link when diffStat is empty", () => {
    const { queryByText } = render(() => (
      <TaskDetail {...baseProps} diffStat={[]} />
    ));
    expect(queryByText("Diff")).not.toBeInTheDocument();
  });

  it("hides Diff link when diffStat is undefined", () => {
    const { queryByText } = render(() => (
      <TaskDetail {...baseProps} diffStat={undefined} />
    ));
    expect(queryByText("Diff")).not.toBeInTheDocument();
  });

  it("diff link href ends with /diff", () => {
    const { getByText } = render(() => (
      <TaskDetail {...baseProps} diffStat={[{ path: "file.ts", added: 5, deleted: 1 }]} />
    ));
    const link = getByText("Diff");
    expect(link.getAttribute("href")).toBe("/task/@abc+test-task/diff");
  });

  it("clicking diff link calls navigate with path/diff", async () => {
    const user = userEvent.setup();
    const { getByText } = render(() => (
      <TaskDetail {...baseProps} diffStat={[{ path: "file.ts", added: 5, deleted: 1 }]} />
    ));
    await user.click(getByText("Diff"));
    expect(navigateMock).toHaveBeenCalledWith("/task/@abc+test-task/diff");
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
  vi.mocked(taskEvents).mockImplementation((_id, cb) => {
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

describe("SSE connection", () => {
  beforeEach(() => {
    // Fake timers so we can control setTimeout (reconnect delays) and
    // requestAnimationFrame (live-event batching, polyfilled as setTimeout(16)).
    vi.useFakeTimers();
  });

  afterEach(() => {
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

      render(() => <TaskDetail {...baseProps} />);

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

    render(() => <TaskDetail {...baseProps} />);
    if (!capturedCb.value) throw new Error("taskEvents callback not captured");

    const cb = capturedCb.value;
    cb({ kind: "thinking", ts: 1, thinking: { text: "planning tool 1" } });
    cb({ kind: "toolUse", ts: 2, toolUse: { toolUseID: "t1", name: "Read", input: {} } });
    cb({ kind: "usage", ts: 3, usage: { inputTokens: 10, outputTokens: 5, cacheCreationInputTokens: 0, cacheReadInputTokens: 0, model: "m" } });
    cb({ kind: "thinking", ts: 4, thinking: { text: "planning tool 2" } });
    cb({ kind: "toolUse", ts: 5, toolUse: { toolUseID: "t2", name: "Bash", input: {} } });
    vi.advanceTimersByTime(20);

    expect(document.body.textContent).toContain("planning tool 1");
    expect(document.body.textContent).toContain("planning tool 2");
  });

  it("live textDelta events appear in the message list after SSE fires them", () => {
    // Verifies the rAF-batched streaming path: events pushed via the SSE
    // callback surface in the DOM after the animation frame is flushed.
    const created: FakeES[] = [];
    const capturedCb = { value: null as ((ev: EventMessage) => void) | null };
    makeSyncReadyMock(created, capturedCb);

    render(() => <TaskDetail {...baseProps} />);

    expect(capturedCb.value).not.toBeNull();

    // Push a textDelta live event (component is in live mode because ready fired).
    if (!capturedCb.value) throw new Error("taskEvents callback not captured");
    capturedCb.value({ kind: "textDelta", ts: 1, textDelta: { text: "agent reply" } });

    // Flush the rAF batch: vi.useFakeTimers() polyfills rAF as setTimeout(fn, 16).
    vi.advanceTimersByTime(20);

    expect(document.body.textContent).toContain("agent reply");
  });
});

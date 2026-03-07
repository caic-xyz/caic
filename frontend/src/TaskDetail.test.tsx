// Tests for the TaskDetail diff link navigation.
import { describe, it, expect, vi, afterEach } from "vitest";
import { render } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";

const navigateMock = vi.fn();

// Mock the router to avoid .jsx resolution issues with @solidjs/router dist.
vi.mock("@solidjs/router", () => ({
  useNavigate: () => navigateMock,
  useLocation: () => ({ pathname: "/task/@abc+test-task" }),
}));

// Mock the API module to stub out EventSource (SSE) and other network calls.
vi.mock("@sdk/api.gen", () => ({
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

afterEach(() => {
  navigateMock.mockClear();
});

describe("TaskDetail", () => {
  const baseProps = {
    taskId: "abc",
    taskState: "running",
    repo: "my-repo",
    repoURL: "https://github.com/org/my-repo",
    branch: "feature-branch",
    baseBranch: "main",
    onClose: () => {},
    inputDraft: "",
    onInputDraft: () => {},
    inputImages: [],
    onInputImages: () => {},
  };

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

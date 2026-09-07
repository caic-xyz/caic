// Tests for TaskInfo runtime metadata rendering.

import { render } from "@solidjs/testing-library";
import type { JSX } from "solid-js";
import { describe, expect, it, vi } from "vitest";

import type { TaskInfo as TaskInfoData } from "@sdk/types.gen";

const { navigateMock, getTaskInfoMock } = vi.hoisted(() => ({
  navigateMock: vi.fn(),
  getTaskInfoMock: vi.fn<() => Promise<TaskInfoData>>(),
}));

vi.mock("@solidjs/router", () => ({
  A: (props: { href: string; class?: string; children: JSX.Element }) => (
    <a class={props.class} href={props.href}>{props.children}</a>
  ),
  useNavigate: () => navigateMock,
}));

vi.mock("../api", () => ({
  getTaskInfo: getTaskInfoMock,
}));

import TaskInfo from "./TaskInfo";

describe("TaskInfo", () => {
  it("shows the runtime OS and CPU architecture as separate fields", async () => {
    getTaskInfoMock.mockResolvedValueOnce({
      id: "task-1",
      recorded: {
        state: "running",
        harness: "claude",
        capabilities: {},
        runtime: { id: "md-test" },
      },
      observed: {
        runtime: "docker",
        os: "linux",
        cpuArchitecture: "amd64",
      },
    });

    const { findByText } = render(() => (
      <TaskInfo taskId="task-1" repo="repo" branch="branch" taskPath="/task/task-1" />
    ));

    expect(await findByText("OS")).toBeInTheDocument();
    expect(await findByText("linux")).toBeInTheDocument();
    expect(await findByText("CPU architecture")).toBeInTheDocument();
    expect(await findByText("amd64")).toBeInTheDocument();
  });

  it("does not label standard cache snapshots as read-write", async () => {
    getTaskInfoMock.mockResolvedValueOnce({
      id: "task-1",
      recorded: {
        state: "running",
        harness: "claude",
        capabilities: {},
        runtime: { id: "md-test" },
        caches: [{ name: "npm", hostPath: "~/.npm", containerPath: "/home/user/.npm" }],
      },
    });

    const { findByText, queryByText } = render(() => (
      <TaskInfo taskId="task-1" repo="repo" branch="branch" taskPath="/task/task-1" />
    ));

    expect(await findByText("npm")).toBeInTheDocument();
    expect(queryByText("read-write")).not.toBeInTheDocument();
  });

  it("shows the clickable fork origin", async () => {
    getTaskInfoMock.mockResolvedValueOnce({
      id: "3BVLTPC1U000",
      recorded: {
        state: "running",
        harness: "claude",
        capabilities: {},
        runtime: { id: "md-test" },
        forkedFromTaskID: "3BL0EKDTO000",
      },
    });

    const { findByRole, findByText } = render(() => (
      <TaskInfo taskId="3BVLTPC1U000" repo="repo" branch="branch" taskPath="/task/3BVLTPC1U000" />
    ));

    expect(await findByText("Lineage")).toBeInTheDocument();
    const link = await findByRole("link", { name: "3BL0EKDTO000" });
    expect(link).toHaveAttribute("href", "/task/@3BL0EKDTO000");
  });

  it("shows a child origin instead of a duplicate fork origin", async () => {
    getTaskInfoMock.mockResolvedValueOnce({
      id: "child",
      recorded: {
        state: "running",
        harness: "claude",
        capabilities: {},
        runtime: { id: "md-test" },
        forkedFromTaskID: "parent",
        parentTaskID: "parent",
      },
    });

    const { findByText, queryByText } = render(() => (
      <TaskInfo taskId="child" repo="repo" branch="branch" taskPath="/task/child" />
    ));

    expect(await findByText("Child of")).toBeInTheDocument();
    expect(queryByText("Forked from")).not.toBeInTheDocument();
  });

  it("shows distinct fork and child origins", async () => {
    getTaskInfoMock.mockResolvedValueOnce({
      id: "child",
      recorded: {
        state: "running",
        harness: "claude",
        capabilities: {},
        runtime: { id: "md-test" },
        forkedFromTaskID: "snapshot-source",
        parentTaskID: "parent",
      },
    });

    const { findByText, findByRole } = render(() => (
      <TaskInfo taskId="child" repo="repo" branch="branch" taskPath="/task/child" />
    ));

    expect(await findByText("Forked from")).toBeInTheDocument();
    expect(await findByText("Child of")).toBeInTheDocument();
    expect(await findByRole("link", { name: "snapshot-source" })).toHaveAttribute("href", "/task/@snapshot-source");
    expect(await findByRole("link", { name: "parent" })).toHaveAttribute("href", "/task/@parent");
  });
});

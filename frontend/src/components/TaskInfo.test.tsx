// Tests for TaskInfo runtime metadata rendering.

import { describe, it } from "node:test";
import type { JSX } from "solid-js";
import { Route, Router } from "@solidjs/router";
import { render } from "@solidjs/testing-library";
import { expect, vi } from "@tests/expect";

import { api } from "../api";
import TaskInfo from "./TaskInfo";

function renderWithRouter(ui: () => JSX.Element) {
  return render(() => (
    <Router>
      <Route path="/*" component={ui} />
    </Router>
  ));
}

const getTaskInfoMock = vi.spyOn(api, "getTaskInfo");

describe("TaskInfo", () => {
  for (const observed of [undefined, 0, 1.5]) {
    it(`distinguishes requested CPUs from ${observed === undefined ? "unknown" : observed === 0 ? "unrestricted" : "fractional"} quota`, async () => {
      getTaskInfoMock.mockResolvedValueOnce({
        id: "task-1",
        recorded: { state: "running", harness: "claude", capabilities: {}, runtime: { id: "md-test" }, maxCPUs: 4 },
        observed:
          observed === undefined ? undefined : { runtime: "docker", ...(observed > 0 ? { cpuLimit: observed } : {}) },
      });
      const { findByText } = renderWithRouter(() => (
        <TaskInfo taskId="task-1" repo="repo" branch="branch" taskPath="/task/task-1" />
      ));
      const text =
        observed === undefined
          ? "4 requested, quota unknown"
          : observed === 0
            ? "4 requested, no explicit quota"
            : "4 requested, 1.5 CPUs";
      expect(await findByText(text)).toBeInTheDocument();
    });
  }

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

    const { findByText } = renderWithRouter(() => (
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

    const { findByText, queryByText } = renderWithRouter(() => (
      <TaskInfo taskId="task-1" repo="repo" branch="branch" taskPath="/task/task-1" />
    ));

    expect(await findByText("npm")).toBeInTheDocument();
    expect(queryByText("read-write")).not.toBeInTheDocument();
  });

  it("shows whether CAIC MCP delegation was enabled", async () => {
    getTaskInfoMock.mockResolvedValueOnce({
      id: "task-1",
      recorded: {
        state: "running",
        harness: "claude",
        caicMCP: true,
        capabilities: {},
        runtime: { id: "md-test" },
      },
    });

    const { findByText } = renderWithRouter(() => (
      <TaskInfo taskId="task-1" repo="repo" branch="branch" taskPath="/task/task-1" />
    ));

    expect(await findByText("CAIC MCP delegation")).toBeInTheDocument();
    expect(await findByText("yes")).toBeInTheDocument();
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

    const { findByRole, findByText } = renderWithRouter(() => (
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

    const { findByText, queryByText } = renderWithRouter(() => (
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

    const { findByText, findByRole } = renderWithRouter(() => (
      <TaskInfo taskId="child" repo="repo" branch="branch" taskPath="/task/child" />
    ));

    expect(await findByText("Forked from")).toBeInTheDocument();
    expect(await findByText("Child of")).toBeInTheDocument();
    expect(await findByRole("link", { name: "snapshot-source" })).toHaveAttribute("href", "/task/@snapshot-source");
    expect(await findByRole("link", { name: "parent" })).toHaveAttribute("href", "/task/@parent");
  });
});

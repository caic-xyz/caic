// Tests for the ProcessDetail process tree builder, flattening, and collapsing.

import { fireEvent, render, screen } from "@solidjs/testing-library";
import { describe, it, expect, vi } from "vitest";

import type { ISOTimestamp, ProcessInfo } from "@sdk/types.gen";

import { getTaskProcesses } from "../api";
import { buildTree, default as ProcessDetail, visibleProcesses } from "./ProcessDetail";
import type { ProcessNode } from "./ProcessDetail";

vi.mock("@solidjs/router", () => ({
  useNavigate: () => vi.fn(),
}));

vi.mock("../api", () => ({
  getTaskProcesses: vi.fn(),
  signalProcess: vi.fn(),
}));

function p(pid: number, ppid: number, command: string): ProcessInfo {
  return {
    pid,
    ppid,
    pgrp: ppid,
    user: "user",
    state: "S",
    priority: 20,
    nice: 0,
    threads: 1,
    cpu: 0,
    mem: 0,
    rssBytes: 0,
    cpuTime: 0,
    startedAt: new Date().toISOString() as ISOTimestamp,
    command,
  };
}

function flattenTree(roots: ProcessNode[], depth = 0): { pid: number; depth: number }[] {
  const result: { pid: number; depth: number }[] = [];
  for (const root of roots) {
    result.push({ pid: root.pid, depth });
    result.push(...flattenTree(root.children, depth + 1));
  }
  return result;
}

describe("ProcessDetail", () => {
  it("renders process metrics", async () => {
    const process = p(1, 0, "bash");
    process.openFDs = 17;
    process.startedAt = new Date(Date.now() - 90_000).toISOString() as ISOTimestamp;
    vi.mocked(getTaskProcesses).mockResolvedValue({ processes: [process] });

    render(() => <ProcessDetail taskId="task-1" repo="repo" branch="main" taskPath="/task/task-1" />);

    expect(await screen.findByText("1m 30s")).toBeInTheDocument();
    expect(screen.getByText("17")).toBeInTheDocument();
  });
});

describe("buildTree", () => {
  it("returns empty array for empty input", () => {
    expect(buildTree([])).toEqual([]);
  });

  it("single root process with no parent", () => {
    const procs = [p(1, 0, "bash")];
    const tree = buildTree(procs);
    expect(tree).toHaveLength(1);
    expect(tree[0].pid).toBe(1);
    expect(tree[0].children).toEqual([]);
  });

  it("multiple root processes", () => {
    const procs = [p(1, 0, "bash"), p(2, 0, "ssh"), p(10, 1, "sleep")];
    const tree = buildTree(procs);
    // Roots: 1 (with child 10) and 2 (no children).
    expect(tree).toHaveLength(2);
    expect(tree.map((n) => n.pid).sort()).toEqual([1, 2]);
    const bash = tree.find((n) => n.pid === 1);
    expect(bash).toBeDefined();
    if (!bash) throw new Error("unreachable");
    expect(bash.children).toHaveLength(1);
    expect(bash.children[0].pid).toBe(10);
    expect(bash.children[0].children).toEqual([]);
    const ssh = tree.find((n) => n.pid === 2);
    expect(ssh).toBeDefined();
    if (!ssh) throw new Error("unreachable");
    expect(ssh.children).toEqual([]);
  });

  it("nested parent-child chain", () => {
    const procs = [p(1, 0, "init"), p(10, 1, "bash"), p(100, 10, "make"), p(1000, 100, "gcc")];
    const tree = buildTree(procs);
    expect(tree).toHaveLength(1);
    expect(tree[0].pid).toBe(1);
    expect(tree[0].children).toHaveLength(1);
    expect(tree[0].children[0].pid).toBe(10);
    expect(tree[0].children[0].children).toHaveLength(1);
    expect(tree[0].children[0].children[0].pid).toBe(100);
    expect(tree[0].children[0].children[0].children).toHaveLength(1);
    expect(tree[0].children[0].children[0].children[0].pid).toBe(1000);
  });

  it("multiple children under same parent", () => {
    const procs = [p(1, 0, "bash"), p(10, 1, "make"), p(11, 1, "gcc"), p(12, 1, "ld")];
    const tree = buildTree(procs);
    expect(tree).toHaveLength(1);
    expect(tree[0].children).toHaveLength(3);
    expect(tree[0].children.map((c) => c.pid).sort()).toEqual([10, 11, 12]);
  });

  it("processes whose ppid refers outside the list become roots", () => {
    // ppid 999 is not in the list, so 10 becomes a root.
    const procs = [p(10, 999, "orphan"), p(11, 10, "child-of-orphan")];
    const tree = buildTree(procs);
    expect(tree).toHaveLength(1);
    expect(tree[0].pid).toBe(10);
    expect(tree[0].children).toHaveLength(1);
    expect(tree[0].children[0].pid).toBe(11);
  });

  it("preserves all ProcessInfo fields on tree nodes", () => {
    const procs = [p(5, 0, "myprocess")];
    procs[0].user = "root";
    procs[0].state = "R";
    procs[0].cpu = 12.5;
    procs[0].mem = 3.2;
    procs[0].cpuTime = 83_000_000_000;
    procs[0].rssBytes = 2_097_152;
    procs[0].startedAt = "2026-03-20T10:30:00Z" as ISOTimestamp;
    const tree = buildTree(procs);
    const node = tree[0];
    expect(node.pid).toBe(5);
    expect(node.ppid).toBe(0);
    expect(node.user).toBe("root");
    expect(node.state).toBe("R");
    expect(node.cpu).toBe(12.5);
    expect(node.mem).toBe(3.2);
    expect(node.cpuTime).toBe(83_000_000_000);
    expect(node.rssBytes).toBe(2_097_152);
    expect(node.startedAt).toBe("2026-03-20T10:30:00Z");
    expect(node.command).toBe("myprocess");
  });

  it("handles unordered input correctly", () => {
    // Children listed before parents should still nest correctly.
    const procs = [p(100, 10, "gcc"), p(10, 1, "make"), p(1, 0, "bash"), p(11, 1, "ld")];
    const tree = buildTree(procs);
    expect(tree).toHaveLength(1);
    expect(tree[0].pid).toBe(1);
    expect(tree[0].children.map((c) => c.pid).sort()).toEqual([10, 11]);
    const makeNode = tree[0].children.find((c) => c.pid === 10);
    expect(makeNode).toBeDefined();
    if (!makeNode) throw new Error("unreachable");
    expect(makeNode.children[0].pid).toBe(100);
  });

  it("flattens to expected depths", () => {
    const procs = [p(1, 0, "init"), p(2, 1, "daemon"), p(3, 2, "worker1"), p(4, 2, "worker2"), p(5, 0, "other")];
    const flat = flattenTree(buildTree(procs));
    expect(flat).toEqual([
      { pid: 1, depth: 0 },
      { pid: 2, depth: 1 },
      { pid: 3, depth: 2 },
      { pid: 4, depth: 2 },
      { pid: 5, depth: 0 },
    ]);
  });

  it("treats a self-parent process as a root", () => {
    const tree = buildTree([p(7, 7, "loop")]);
    expect(tree).toHaveLength(1);
    expect(tree[0].pid).toBe(7);
    expect(tree[0].children).toEqual([]);
  });

  it("cuts a parent cycle at its lowest PID", () => {
    const tree = buildTree([p(1, 2, "a"), p(2, 1, "b"), p(3, 2, "c")]);
    expect(tree).toHaveLength(1);
    expect(tree[0].pid).toBe(1);
    expect(tree[0].children.map((n) => n.pid)).toEqual([2]);
    expect(tree[0].children[0].children.map((n) => n.pid)).toEqual([3]);
  });

  it("labels depths in a 20,000-deep chain without overflowing", () => {
    const procs: ProcessInfo[] = [];
    for (let i = 1; i <= 20_000; i++) procs.push(p(i, i - 1, "bash"));
    const tree = buildTree(procs);
    expect(tree).toHaveLength(1);
    let node = tree[0];
    let depth = 0;
    while (node.children.length > 0) {
      node = node.children[0];
      depth += 1;
    }
    expect(depth).toBe(19_999);
    expect(node.depth).toBe(19_999);
  });
});

describe("visibleProcesses", () => {
  function chain(length: number): ProcessInfo[] {
    const procs: ProcessInfo[] = [];
    for (let i = 1; i <= length; i++) procs.push(p(i, i - 1, "bash"));
    return procs;
  }

  it("flattens depth-first in order", () => {
    const procs = [p(1, 0, "init"), p(2, 1, "a"), p(3, 2, "b"), p(4, 0, "other")];
    const visible = visibleProcesses(buildTree(procs), () => false);
    expect(visible.map((n) => n.pid)).toEqual([1, 2, 3, 4]);
  });

  it("omits the descendants of collapsed nodes", () => {
    const procs = [p(1, 0, "init"), p(2, 1, "a"), p(3, 2, "b"), p(4, 0, "other")];
    const visible = visibleProcesses(buildTree(procs), (node) => node.pid === 2);
    expect(visible.map((n) => n.pid)).toEqual([1, 2, 4]);
  });

  it("flattens a 50,000-deep chain without overflowing the call stack", () => {
    const visible = visibleProcesses(buildTree(chain(50_000)), () => false);
    expect(visible).toHaveLength(50_000);
    expect(visible[0].depth).toBe(0);
    expect(visible[visible.length - 1].depth).toBe(49_999);
  });
});

// Rendering a few hundred jsdom rows under coverage instrumentation can exceed
// the 5s default test timeout on a loaded CI runner, so these heavy tests set
// their own generous ceiling.
const HEAVY_TREE_TIMEOUT = 20_000;

describe("ProcessDetail tree collapsing", () => {
  function chain(length: number): ProcessInfo[] {
    const procs: ProcessInfo[] = [];
    for (let i = 1; i <= length; i++) procs.push(p(i, i - 1, `cmd${i}`));
    return procs;
  }

  function renderProcesses(procs: ProcessInfo[]): void {
    vi.mocked(getTaskProcesses).mockResolvedValue({ processes: procs });
    render(() => <ProcessDetail taskId="task-1" repo="repo" branch="main" taskPath="/task/task-1" />);
  }

  it("collapses subtrees past the auto-collapse depth", { timeout: HEAVY_TREE_TIMEOUT }, async () => {
    renderProcesses(chain(200));

    expect(await screen.findByText("cmd26")).toBeInTheDocument();
    expect(screen.getByText("cmd25")).toBeInTheDocument();
    expect(screen.queryByText("cmd27")).not.toBeInTheDocument();
    expect(screen.getAllByTitle("Expand children")).toHaveLength(1);
    expect(screen.getAllByTitle("Collapse children")).toHaveLength(25);

    fireEvent.click(screen.getByTitle("Expand children"));
    expect(screen.getByText("cmd27")).toBeInTheDocument();
    expect(screen.queryByText("cmd28")).not.toBeInTheDocument();
    expect(screen.getAllByTitle("Collapse children")).toHaveLength(26);
  });

  it("expands and collapses every subtree from the toolbar", { timeout: HEAVY_TREE_TIMEOUT }, async () => {
    renderProcesses(chain(200));

    expect(await screen.findByText("cmd26")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Expand all" }));
    expect(screen.getByText("cmd200")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Collapse all" }));
    expect(screen.getByText("cmd1")).toBeInTheDocument();
    expect(screen.queryByText("cmd2")).not.toBeInTheDocument();
  });

  it("renders a 20,000 process chain with a bounded row count", { timeout: HEAVY_TREE_TIMEOUT }, async () => {
    renderProcesses(chain(20_000));

    expect(await screen.findByText("cmd1")).toBeInTheDocument();
    expect(screen.getByText("cmd26")).toBeInTheDocument();
    expect(screen.getByText(/^20\D?000 processes$/)).toBeInTheDocument();
    expect(screen.getAllByRole("row").length).toBeLessThan(100);
  });
});

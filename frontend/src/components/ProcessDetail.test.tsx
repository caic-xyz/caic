// Tests for the process tree builder used by ProcessDetail.

import { describe, it, expect } from "vitest";

import type { ProcessInfo } from "@sdk/types.gen";

import { buildTree } from "./ProcessDetail";
import type { ProcessNode } from "./ProcessDetail";

function p(pid: number, ppid: number, command: string): ProcessInfo {
  return { pid, ppid, user: "user", state: "S", cpu: 0, mem: 0, time: "0:00", command };
}

function flattenTree(roots: ProcessNode[], depth = 0): { pid: number; depth: number }[] {
  const result: { pid: number; depth: number }[] = [];
  for (const root of roots) {
    result.push({ pid: root.pid, depth });
    result.push(...flattenTree(root.children, depth + 1));
  }
  return result;
}

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
    const procs = [
      p(1, 0, "bash"),
      p(2, 0, "ssh"),
      p(10, 1, "sleep"),
    ];
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
    const procs = [
      p(1, 0, "init"),
      p(10, 1, "bash"),
      p(100, 10, "make"),
      p(1000, 100, "gcc"),
    ];
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
    const procs = [
      p(1, 0, "bash"),
      p(10, 1, "make"),
      p(11, 1, "gcc"),
      p(12, 1, "ld"),
    ];
    const tree = buildTree(procs);
    expect(tree).toHaveLength(1);
    expect(tree[0].children).toHaveLength(3);
    expect(tree[0].children.map((c) => c.pid).sort()).toEqual([10, 11, 12]);
  });

  it("processes whose ppid refers outside the list become roots", () => {
    // ppid 999 is not in the list, so 10 becomes a root.
    const procs = [
      p(10, 999, "orphan"),
      p(11, 10, "child-of-orphan"),
    ];
    const tree = buildTree(procs);
    expect(tree).toHaveLength(1);
    expect(tree[0].pid).toBe(10);
    expect(tree[0].children).toHaveLength(1);
    expect(tree[0].children[0].pid).toBe(11);
  });

  it("preserves all ProcessInfo fields on tree nodes", () => {
    const procs = [
      p(5, 0, "myprocess"),
    ];
    procs[0].user = "root";
    procs[0].state = "R";
    procs[0].cpu = 12.5;
    procs[0].mem = 3.2;
    procs[0].time = "1:23";
    const tree = buildTree(procs);
    const node = tree[0];
    expect(node.pid).toBe(5);
    expect(node.ppid).toBe(0);
    expect(node.user).toBe("root");
    expect(node.state).toBe("R");
    expect(node.cpu).toBe(12.5);
    expect(node.mem).toBe(3.2);
    expect(node.time).toBe("1:23");
    expect(node.command).toBe("myprocess");
  });

  it("handles unordered input correctly", () => {
    // Children listed before parents should still nest correctly.
    const procs = [
      p(100, 10, "gcc"),
      p(10, 1, "make"),
      p(1, 0, "bash"),
      p(11, 1, "ld"),
    ];
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
    const procs = [
      p(1, 0, "init"),
      p(2, 1, "daemon"),
      p(3, 2, "worker1"),
      p(4, 2, "worker2"),
      p(5, 0, "other"),
    ];
    const flat = flattenTree(buildTree(procs));
    expect(flat).toEqual([
      { pid: 1, depth: 0 },
      { pid: 2, depth: 1 },
      { pid: 3, depth: 2 },
      { pid: 4, depth: 2 },
      { pid: 5, depth: 0 },
    ]);
  });
});

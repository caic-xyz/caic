// Full-page process tree viewer for a task's container.
// Processes are displayed as a tree grouped by parent/child PID relationships.
// The tree is built and flattened iteratively, and subtrees below
// AUTO_COLLAPSE_DEPTH start collapsed, so an unusually deep or cyclic process
// chain cannot overflow the render stack or mount tens of thousands of rows.
// Exports buildTree, visibleProcesses, and ProcessNode for unit testing.

import { createSignal, createEffect, createMemo, For, Show, onMount, onCleanup } from "solid-js";
import { useNavigate } from "@solidjs/router";
import ArrowBackIcon from "@material-symbols/svg-400/outlined/arrow_back.svg?solid";
import ChevronRightIcon from "@material-symbols/svg-400/outlined/chevron_right.svg?solid";
import ExpandIcon from "@material-symbols/svg-400/outlined/expand.svg?solid";

import type { ProcessInfo } from "@sdk/types.gen";

import { getTaskProcesses, signalProcess } from "../api";
import { formatBytes, formatElapsed, formatTime } from "../formatting";
import styles from "./ProcessDetail.module.css";

// AUTO_COLLAPSE_DEPTH is the tree depth at which subtrees start collapsed. A
// container can hold tens of thousands of processes in one parent chain, and
// painting every level eagerly makes the page unusable.
const AUTO_COLLAPSE_DEPTH = 25;

// CollapseMode is the tree-wide collapse override selected by the toolbar.
type CollapseMode = "default" | "expanded" | "collapsed";

// ProcessNode extends ProcessInfo with children and depth for tree rendering.
export interface ProcessNode extends ProcessInfo {
  children: ProcessNode[];
  // depth is the node's distance from its root, assigned by buildTree.
  depth: number;
}

// resolvedParents returns the effective parent PID for every process. The
// parent link of the lowest PID in a parent cycle is replaced with 0: a real
// process table is acyclic, but PID reuse can make a snapshot look cyclic, and
// the tree must stay finite.
function resolvedParents(byPID: ReadonlyMap<number, ProcessNode>): Map<number, number> {
  const parent = new Map<number, number>();
  for (const node of byPID.values()) {
    parent.set(node.pid, byPID.has(node.ppid) ? node.ppid : 0);
  }
  const settled = new Set<number>();
  for (const start of byPID.keys()) {
    if (settled.has(start)) continue;
    const path: number[] = [];
    const onPath = new Set<number>();
    let cur = start;
    for (;;) {
      if (onPath.has(cur)) {
        // Cut the cycle at its lowest PID so the cycle keeps a single root.
        const cycle = path.slice(path.indexOf(cur));
        let cut = cycle[0];
        for (const pid of cycle) {
          if (pid < cut) cut = pid;
        }
        parent.set(cut, 0);
        break;
      }
      if (settled.has(cur)) break;
      onPath.add(cur);
      path.push(cur);
      const next = parent.get(cur) ?? 0;
      if (next === 0) break;
      cur = next;
    }
    for (const pid of path) settled.add(pid);
  }
  return parent;
}

// assignDepths labels every node with its distance from a root. Iterative so a
// deep chain cannot overflow the call stack.
function assignDepths(roots: ProcessNode[]): void {
  const queue: ProcessNode[] = [...roots];
  for (let i = 0; i < queue.length; i++) {
    const node = queue[i];
    for (const child of node.children) {
      child.depth = node.depth + 1;
      queue.push(child);
    }
  }
}

// buildTree builds a tree from a flat process list using pid/ppid
// relationships. Roots are processes whose parent is absent from the list,
// plus the process that breaks a parent cycle.
export function buildTree(procs: ProcessInfo[]): ProcessNode[] {
  const byPID = new Map<number, ProcessNode>();
  for (const proc of procs) {
    byPID.set(proc.pid, { ...proc, children: [], depth: 0 });
  }
  const parent = resolvedParents(byPID);
  const roots: ProcessNode[] = [];
  for (const node of byPID.values()) {
    const ppid = parent.get(node.pid) ?? 0;
    const parentNode = ppid === 0 ? undefined : byPID.get(ppid);
    if (parentNode) {
      parentNode.children.push(node);
    } else {
      roots.push(node);
    }
  }
  assignDepths(roots);
  return roots;
}

// visibleProcesses flattens the tree depth-first for rendering and omits the
// descendants of collapsed nodes. It returns the stable ProcessNode identities
// so <For> reuses existing rows, and it is iterative so any tree depth renders
// without growing the call stack.
export function visibleProcesses(roots: ProcessNode[], isCollapsed: (node: ProcessNode) => boolean): ProcessNode[] {
  const visible: ProcessNode[] = [];
  const stack: ProcessNode[] = [];
  for (let i = roots.length - 1; i >= 0; i--) {
    stack.push(roots[i]);
  }
  while (stack.length > 0) {
    const node = stack.pop();
    if (node === undefined) break;
    visible.push(node);
    if (isCollapsed(node)) continue;
    for (let i = node.children.length - 1; i >= 0; i--) {
      stack.push(node.children[i]);
    }
  }
  return visible;
}

// State color mapping for process state characters.
function stateColor(state: string): string {
  switch (state) {
    case "R":
      return "var(--color-success)";
    case "D":
    case "Z":
      return "var(--color-danger)";
    case "T":
      return "var(--color-warning-text)";
    default:
      return "var(--color-text-muted)";
  }
}

interface Props {
  taskId: string;
  repo: string;
  branch: string;
  taskPath: string;
  onTaskRefreshError?: (taskId: string, err: unknown) => boolean;
}

interface RowProps {
  node: ProcessNode;
  collapsed: boolean;
  onToggle: (node: ProcessNode) => void;
  signallingPid: () => number | null;
  now: () => number;
  onSignal: (pid: number, sig: "SIGTERM" | "SIGKILL") => void;
}

function ProcessRow(props: RowProps) {
  const hasChildren = () => props.node.children.length > 0;
  const indent = () => props.node.depth * 10;

  return (
    <tr>
      <td class={`${styles.td} ${styles.actions}`}>
        <div class={styles.actionsRow}>
          <span class={styles.treeToggle} style={{ width: `${indent()}px`, "min-width": `${indent()}px` }}>
            <Show when={hasChildren()}>
              <button
                class={styles.toggleBtn}
                onClick={() => props.onToggle(props.node)}
                aria-expanded={!props.collapsed}
                title={props.collapsed ? "Expand children" : "Collapse children"}
              >
                {props.collapsed ? <ChevronRightIcon width={12} height={12} /> : <ExpandIcon width={12} height={12} />}
              </button>
            </Show>
          </span>
          <button
            class={styles.signalBtn}
            onClick={() => props.onSignal(props.node.pid, "SIGTERM")}
            disabled={props.signallingPid() === props.node.pid}
            title="Send SIGTERM (graceful termination)"
          >
            TERM
          </button>
          <button
            class={`${styles.signalBtn} ${styles.signalKill}`}
            onClick={() => props.onSignal(props.node.pid, "SIGKILL")}
            disabled={props.signallingPid() === props.node.pid}
            title="Send SIGKILL (force kill)"
          >
            KILL
          </button>
        </div>
      </td>
      <td class={styles.td}>{props.node.pid}</td>
      <td class={styles.td}>{props.node.pgrp}</td>
      <td class={styles.td}>{props.node.user}</td>
      <td class={styles.td}>
        <span class={styles.state} style={{ color: stateColor(props.node.state) }}>
          {props.node.state}
        </span>
      </td>
      <td class={styles.td}>{props.node.priority}</td>
      <td class={styles.td}>{props.node.nice}</td>
      <td class={styles.td}>{props.node.threads}</td>
      <td class={styles.td}>{props.node.openFDs ?? "—"}</td>
      <td class={styles.td}>{props.node.cpu.toFixed(1)}</td>
      <td class={styles.td}>{props.node.mem.toFixed(1)}</td>
      <td class={styles.td}>{formatBytes(props.node.rssBytes)}</td>
      <td class={styles.td}>{formatElapsed(props.node.cpuTime / 1_000_000)}</td>
      <td class={styles.td}>{formatTime(props.node.startedAt)}</td>
      <td class={styles.td}>{formatElapsed(props.now() - new Date(props.node.startedAt).getTime())}</td>
      <td class={`${styles.td} ${styles.cmd}`}>{props.node.command}</td>
    </tr>
  );
}

export default function ProcessDetail(props: Props) {
  const navigate = useNavigate();
  const [processes, setProcesses] = createSignal<ProcessInfo[] | null>(null);
  const [error, setError] = createSignal<string | null>(null);
  const [loading, setLoading] = createSignal(true);
  const [signallingPid, setSignallingPid] = createSignal<number | null>(null);
  const [now, setNow] = createSignal(Date.now());
  const [collapsed, setCollapsed] = createSignal<ReadonlyMap<number, boolean>>(new Map());
  const [collapseMode, setCollapseMode] = createSignal<CollapseMode>("default");

  const tree = createMemo(() => {
    const procs = processes();
    return procs ? buildTree(procs) : null;
  });

  const total = () => processes()?.length ?? 0;

  // isCollapsed resolves a node's collapse state: an explicit per-node override
  // wins, then the toolbar mode, then the depth-based default.
  const isCollapsed = (node: ProcessNode): boolean => {
    const override = collapsed().get(node.pid);
    if (override !== undefined) return override;
    if (collapseMode() === "expanded") return false;
    if (collapseMode() === "collapsed") return true;
    return node.depth >= AUTO_COLLAPSE_DEPTH;
  };

  const visible = createMemo(() => {
    const roots = tree();
    return roots ? visibleProcesses(roots, isCollapsed) : [];
  });

  const toggleCollapsed = (node: ProcessNode) => {
    const next = !isCollapsed(node);
    const overrides = new Map(collapsed());
    overrides.set(node.pid, next);
    setCollapsed(overrides);
  };

  const expandAll = () => {
    setCollapsed(new Map());
    setCollapseMode("expanded");
  };

  const collapseAll = () => {
    setCollapsed(new Map());
    setCollapseMode("collapsed");
  };

  const refresh = async () => {
    const id = props.taskId;
    const onTaskRefreshError = props.onTaskRefreshError;
    setLoading(true);
    setError(null);
    try {
      const resp = await getTaskProcesses(id);
      setProcesses(resp.processes);
    } catch (e: unknown) {
      if (!onTaskRefreshError?.(id, e)) setError(e instanceof Error ? e.message : "Unknown error");
    } finally {
      setLoading(false);
    }
  };

  createEffect(() => {
    void refresh();
  });

  // Escape navigates back to the task detail.
  onMount(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") navigate(props.taskPath);
    };
    document.addEventListener("keydown", onKey);
    const interval = setInterval(() => setNow(Date.now()), 1000);
    onCleanup(() => {
      clearInterval(interval);
      document.removeEventListener("keydown", onKey);
    });
  });

  const handleSignal = async (pid: number, sig: "SIGTERM" | "SIGKILL") => {
    const id = props.taskId;
    const onTaskRefreshError = props.onTaskRefreshError;
    setSignallingPid(pid);
    try {
      await signalProcess(id, String(pid), { signal: sig });
      await refresh();
    } catch (e: unknown) {
      if (!onTaskRefreshError?.(id, e)) setError(e instanceof Error ? e.message : "Failed to send signal");
    } finally {
      setSignallingPid(null);
    }
  };

  return (
    <div class={styles.container}>
      <div class={styles.header}>
        <button class={styles.backBtn} onClick={() => navigate(props.taskPath)} title="Back to task">
          <ArrowBackIcon width={20} height={20} />
        </button>
        <span class={styles.headerMeta}>
          <span class={styles.headerRepo}>{props.repo}</span>
          <span class={styles.headerBranch}>{props.branch}</span>
        </span>
      </div>
      <Show when={!loading() && !error() && total() > 0}>
        <div class={styles.toolbar}>
          <span class={styles.processCount}>{total().toLocaleString()} processes</span>
          <button class={styles.toolbarBtn} onClick={expandAll}>
            Expand all
          </button>
          <button class={styles.toolbarBtn} onClick={collapseAll}>
            Collapse all
          </button>
        </div>
      </Show>
      <div class={styles.content}>
        <Show when={loading()}>
          <div class={styles.loading}>Loading processes...</div>
        </Show>
        <Show when={error()}>
          <div class={styles.error}>{error()}</div>
        </Show>
        <Show when={!loading() && !error()}>
          <Show when={tree()} keyed fallback={<div class={styles.empty}>No running processes</div>}>
            {(roots) => (
              <Show when={roots.length > 0} fallback={<div class={styles.empty}>No running processes</div>}>
                <table class={styles.table}>
                  <thead>
                    <tr>
                      <th class={`${styles.th} ${styles.actionsHdr}`}>ACTIONS</th>
                      <th class={styles.th}>PID</th>
                      <th class={styles.th}>PGRP</th>
                      <th class={styles.th}>USER</th>
                      <th class={styles.th}>S</th>
                      <th class={styles.th}>PRI</th>
                      <th class={styles.th}>NI</th>
                      <th class={styles.th}>THREADS</th>
                      <th class={styles.th}>FDS</th>
                      <th class={styles.th}>CPU</th>
                      <th class={styles.th}>MEM</th>
                      <th class={styles.th}>RSS</th>
                      <th class={styles.th}>CPU TIME</th>
                      <th class={styles.th}>STARTED</th>
                      <th class={styles.th}>AGE</th>
                      <th class={styles.th}>COMMAND</th>
                    </tr>
                  </thead>
                  <tbody>
                    <For each={visible()}>
                      {(node) => (
                        <ProcessRow
                          node={node}
                          collapsed={isCollapsed(node)}
                          onToggle={toggleCollapsed}
                          signallingPid={signallingPid}
                          now={now}
                          onSignal={handleSignal}
                        />
                      )}
                    </For>
                  </tbody>
                </table>
              </Show>
            )}
          </Show>
        </Show>
      </div>
    </div>
  );
}

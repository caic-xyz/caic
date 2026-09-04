// Full-page process tree viewer for a task's container.
// Processes are displayed as a tree grouped by parent/child PID relationships.
// Exports buildTree and ProcessNode for unit testing.

import { createSignal, createEffect, createMemo, For, Show, onMount, onCleanup } from "solid-js";
import { useNavigate } from "@solidjs/router";
import ArrowBackIcon from "@material-symbols/svg-400/outlined/arrow_back.svg?solid";
import ChevronRightIcon from "@material-symbols/svg-400/outlined/chevron_right.svg?solid";
import ExpandIcon from "@material-symbols/svg-400/outlined/expand.svg?solid";

import type { ProcessInfo } from "@sdk/types.gen";

import { getTaskProcesses, signalProcess } from "../api";
import { formatBytes, formatElapsed, formatTime } from "../formatting";
import styles from "./ProcessDetail.module.css";

// ProcessNode extends ProcessInfo with a children list for tree rendering.
export interface ProcessNode extends ProcessInfo {
  children: ProcessNode[];
}

// Builds a tree from a flat process list using pid/ppid relationships.
// Roots are processes whose PPID is not found in the process list.
export function buildTree(procs: ProcessInfo[]): ProcessNode[] {
  const byPID = new Map<number, ProcessNode>();
  for (const p of procs) {
    byPID.set(p.pid, { ...p, children: [] });
  }
  const roots: ProcessNode[] = [];
  for (const node of byPID.values()) {
    const parent = byPID.get(node.ppid);
    if (parent) {
      parent.children.push(node);
    } else {
      roots.push(node);
    }
  }
  return roots;
}

// State color mapping for process state characters.
function stateColor(state: string): string {
  switch (state) {
    case "R": return "var(--color-success)";
    case "D": case "Z": return "var(--color-danger)";
    case "T": return "var(--color-warning-text)";
    default: return "var(--color-text-muted)";
  }
}

interface Props {
  taskId: string;
  repo: string;
  branch: string;
  taskPath: string;
  onTaskRefreshError?: (taskId: string, err: unknown) => boolean;
}

// Collapsed set shared across the component, keyed by pid.
type CollapsedSet = Set<number>;

interface RowProps {
  node: ProcessNode;
  depth: number;
  collapsed: CollapsedSet;
  toggleCollapsed: (pid: number) => void;
  signallingPid: () => number | null;
  now: () => number;
  onSignal: (pid: number, sig: "SIGTERM" | "SIGKILL") => void;
}

function ProcessRow(props: RowProps) {
  const hasChildren = () => props.node.children.length > 0;
  const isCollapsed = () => props.collapsed.has(props.node.pid);
  const indent = () => props.depth * 10;

  return (
    <>
      <tr>
        <td class={`${styles.td} ${styles.actions}`}>
          <div class={styles.actionsRow}>
            <span class={styles.treeToggle} style={{ width: `${indent()}px`, "min-width": `${indent()}px` }}>
              <Show when={hasChildren()}>
                <button
                  class={styles.toggleBtn}
                  onClick={() => props.toggleCollapsed(props.node.pid)}
                  title={isCollapsed() ? "Expand children" : "Collapse children"}
                >
                  {isCollapsed() ? (
                    <ChevronRightIcon width={12} height={12} />
                  ) : (
                    <ExpandIcon width={12} height={12} />
                  )}
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
          <span class={styles.state} style={{ color: stateColor(props.node.state) }}>{props.node.state}</span>
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
      <Show when={hasChildren() && !isCollapsed()}>
        <For each={props.node.children}>
          {(child) => (
            <ProcessRow
              node={child}
              depth={props.depth + 1}
              collapsed={props.collapsed}
              toggleCollapsed={props.toggleCollapsed}
              signallingPid={props.signallingPid}
              now={props.now}
              onSignal={props.onSignal}
            />
          )}
        </For>
      </Show>
    </>
  );
}

export default function ProcessDetail(props: Props) {
  const navigate = useNavigate();
  const [processes, setProcesses] = createSignal<ProcessInfo[] | null>(null);
  const [error, setError] = createSignal<string | null>(null);
  const [loading, setLoading] = createSignal(true);
  const [signallingPid, setSignallingPid] = createSignal<number | null>(null);
  const [now, setNow] = createSignal(Date.now());
  const [collapsed, setCollapsed] = createSignal<Set<number>>(new Set());

  const tree = createMemo(() => {
    const procs = processes();
    return procs ? buildTree(procs) : null;
  });

  const toggleCollapsed = (pid: number) => {
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (next.has(pid)) {
        next.delete(pid);
      } else {
        next.add(pid);
      }
      return next;
    });
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
      <div class={styles.content}>
        <Show when={loading()}>
          <div class={styles.loading}>Loading processes...</div>
        </Show>
        <Show when={error()}>
          <div class={styles.error}>{error()}</div>
        </Show>
        <Show when={!loading() && !error()}>
          <Show when={tree()} keyed fallback={
            <div class={styles.empty}>No running processes</div>
          }>
            {(roots) => (
              <Show when={roots.length > 0} fallback={
                <div class={styles.empty}>No running processes</div>
              }>
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
                    <For each={roots}>
                      {(root) => (
                        <ProcessRow
                          node={root}
                          depth={0}
                          collapsed={collapsed()}
                          toggleCollapsed={toggleCollapsed}
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

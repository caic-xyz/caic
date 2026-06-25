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
}

// Collapsed set shared across the component, keyed by pid.
type CollapsedSet = Set<number>;

interface RowProps {
  node: ProcessNode;
  depth: number;
  collapsed: CollapsedSet;
  toggleCollapsed: (pid: number) => void;
  signallingPid: () => number | null;
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
        <td class={styles.td}>
          <span class={styles.state} style={{ color: stateColor(props.node.state) }}>{props.node.state}</span>
        </td>
        <td class={styles.td}>{props.node.cpu.toFixed(1)}</td>
        <td class={styles.td}>{props.node.mem.toFixed(1)}</td>
        <td class={styles.td}>{props.node.time}</td>
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

  const refresh = () => {
    setLoading(true);
    setError(null);
    getTaskProcesses(props.taskId)
      .then((resp) => setProcesses(resp.processes))
      .catch((e) => setError(e instanceof Error ? e.message : "Unknown error"))
      .finally(() => setLoading(false));
  };

  createEffect(refresh);

  // Escape navigates back to the task detail.
  onMount(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") navigate(props.taskPath);
    };
    document.addEventListener("keydown", onKey);
    onCleanup(() => document.removeEventListener("keydown", onKey));
  });

  const handleSignal = async (pid: number, sig: "SIGTERM" | "SIGKILL") => {
    setSignallingPid(pid);
    try {
      await signalProcess(props.taskId, String(pid), { signal: sig });
      await refresh();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to send signal");
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
                      <th class={styles.th}>S</th>
                      <th class={styles.th}>CPU</th>
                      <th class={styles.th}>MEM</th>
                      <th class={styles.th}>TIME</th>
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

// Sidebar task list with collapsible panel, grouped by repo for active tasks.
import { For, Index, Show, createSignal } from "solid-js";
import type { Accessor } from "solid-js";
import type { Repo, Task } from "@sdk/types.gen";
import TaskCard from "./TaskCard";
import styles from "./TaskList.module.css";
import LeftPanelClose from "@material-symbols/svg-400/outlined/left_panel_close.svg?solid";
import LeftPanelOpen from "@material-symbols/svg-400/outlined/left_panel_open.svg?solid";
import ArrowRight from "@material-symbols/svg-400/outlined/arrow_right.svg?solid";
import ArrowDropDown from "@material-symbols/svg-400/outlined/arrow_drop_down.svg?solid";

export interface TaskListProps {
  tasks: Accessor<Task[]>;
  repos: Accessor<Repo[]>;
  selectedId: string | null;
  sidebarOpen: Accessor<boolean>;
  setSidebarOpen: (open: boolean) => void;
  now: Accessor<number>;
  onSelect: (id: string) => void;
  onStop: (id: string) => void;
  onPurge: (id: string) => void;
  onRevive: (id: string) => void;
  actionId: Accessor<string | null>;
  onDiffClick?: (id: string) => void;
}

const naturalCompare = (a: string, b: string) =>
  a.localeCompare(b, undefined, { numeric: true, sensitivity: "base" });

/** Sort tasks according to sidebar grouping: Active (repo/branch), then Stopped (ID desc), then Purged (ID desc). */
export function sortTasks(tasks: Task[]): Task[] {
  const active = tasks.filter((t) => t.state !== "stopped" && t.state !== "purged" && t.state !== "failed");
  const stopped = tasks.filter((t) => t.state === "stopped");
  const purged = tasks.filter((t) => t.state === "purged" || t.state === "failed");

  active.sort((a, b) => {
    const rc = naturalCompare(a.repos?.[0]?.name ?? "", b.repos?.[0]?.name ?? "");
    if (rc !== 0) return rc;
    return naturalCompare(a.repos?.[0]?.branch ?? "", b.repos?.[0]?.branch ?? "");
  });

  const idDesc = (a: Task, b: Task) => (b.id > a.id ? 1 : b.id < a.id ? -1 : 0);
  stopped.sort(idDesc);
  purged.sort(idDesc);

  return [...active, ...stopped, ...purged];
}

interface RepoGroup {
  repo: string;
  active: Task[];
  stopped: Task[];
  purged: Task[];
}

const NON_PASSING = new Set(["failure", "cancelled", "timed_out", "action_required", "stale"]);

function ciDotURL(repo: Repo): string | undefined {
  if (!repo.defaultBranchCIStatus) return undefined;
  const isGitLab = !!repo.remoteURL?.includes("gitlab.com");
  if (repo.defaultBranchCIStatus === "failure") {
    const failed = repo.defaultBranchChecks?.find((c) => NON_PASSING.has(c.conclusion));
    if (failed) {
      if (isGitLab) return `https://gitlab.com/${failed.owner}/${failed.repo}/-/jobs/${failed.jobID}`;
      if (failed.runID && failed.jobID) return `https://github.com/${failed.owner}/${failed.repo}/actions/runs/${failed.runID}/job/${failed.jobID}`;
    }
  }
  if (!repo.remoteURL) return undefined;
  return isGitLab ? repo.remoteURL + "/-/pipelines" : repo.remoteURL + "/actions";
}

const CI_DOT_COLOR: Record<string, string> = {
  pending: "var(--color-warning-border)",
  success: "var(--color-success)",
  failure: "var(--color-danger)",
};

export default function TaskList(props: TaskListProps) {
  const [expanded, setExpanded] = createSignal<Set<string>>(new Set());

  const toggleExpanded = (id: string) => {
    const next = new Set(expanded());
    if (next.has(id)) next.delete(id);
    else next.add(id);
    setExpanded(next);
  };

  const grouped = () => {
    const all = [...props.tasks()];

    const groups: Record<string, RepoGroup> = {};

    for (const t of all) {
      const repoName = t.repos?.[0]?.name;
      if (repoName) {
        if (!groups[repoName]) {
          groups[repoName] = { repo: repoName, active: [], stopped: [], purged: [] };
        }
        const g = groups[repoName];
        if (t.state === "purged" || t.state === "failed") {
          g.purged.push(t);
        } else if (t.state === "stopped") {
          g.stopped.push(t);
        } else {
          g.active.push(t);
        }
      }
    }

    const other: RepoGroup = { repo: "", active: [], stopped: [], purged: [] };
    for (const t of all) {
      if (!t.repos?.[0]?.name) {
        if (t.state === "purged" || t.state === "failed") {
          other.purged.push(t);
        } else if (t.state === "stopped") {
          other.stopped.push(t);
        } else {
          other.active.push(t);
        }
      }
    }

    const sortFn = (a: Task, b: Task) => (b.id > a.id ? 1 : b.id < a.id ? -1 : 0);
    const sortedGroups = Object.values(groups).sort((a, b) => naturalCompare(a.repo, b.repo));
    for (const g of sortedGroups) {
      g.active.sort((a, b) => naturalCompare(a.repos?.[0]?.branch ?? "", b.repos?.[0]?.branch ?? ""));
      g.stopped.sort(sortFn);
      g.purged.sort(sortFn);
    }
    
    if (other.active.length > 0 || other.stopped.length > 0 || other.purged.length > 0) {
      other.active.sort((a, b) => (b.id > a.id ? 1 : b.id < a.id ? -1 : 0));
      other.stopped.sort(sortFn);
      other.purged.sort(sortFn);
      sortedGroups.push(other);
    }

    return sortedGroups;
  };

  const renderTask = (t: () => Task) => (
    <TaskCard
      id={t().id}
      title={t().title}
      state={t().state}
      stateUpdatedAt={t().stateUpdatedAt}
      repo={t().repos?.[0]?.name ?? ""}
      remoteURL={t().repos?.[0]?.remoteURL}
      baseBranch={t().repos?.[0]?.baseBranch}
      branch={t().repos?.[0]?.branch ?? ""}
      harness={t().harness}
      model={t().model}
      costUSD={t().costUSD}
      duration={t().duration}
      numTurns={t().numTurns}
      activeInputTokens={t().activeInputTokens}
      activeCacheReadTokens={t().activeCacheReadTokens}
      cumulativeInputTokens={t().cumulativeInputTokens}
      cumulativeCacheCreationInputTokens={t().cumulativeCacheCreationInputTokens}
      cumulativeCacheReadInputTokens={t().cumulativeCacheReadInputTokens}
      cumulativeOutputTokens={t().cumulativeOutputTokens}
      contextWindowLimit={t().contextWindowLimit}
      startedAt={t().startedAt}
      turnStartedAt={t().turnStartedAt}
      diffStat={t().diffStat}
      error={t().error}
      inPlanMode={t().inPlanMode}
      tailscale={t().tailscale}
      usb={t().usb}
      display={t().display}
      forgePR={t().forgePR}
      ciStatus={t().ciStatus}
      selected={props.selectedId === t().id}
      now={props.now}
      onClick={() => props.onSelect(t().id)}
      onStop={() => props.onStop(t().id)}
      onPurge={() => props.onPurge(t().id)}
      onRevive={() => props.onRevive(t().id)}
      actionLoading={props.actionId() === t().id}
      onDiffClick={props.onDiffClick ? () => { const fn = props.onDiffClick; if (fn) fn(t().id); } : undefined}
    />
  );

  return (
    <>
      <div class={`${styles.list} ${props.selectedId !== null ? styles.narrow : ""} ${props.sidebarOpen() ? "" : styles.hidden}`}>
        <div class={styles.header}>
          <h2>Tasks</h2>
          <Show when={props.selectedId !== null}>
            <button class={styles.collapseBtn} onClick={() => props.setSidebarOpen(false)} title="Collapse sidebar"><LeftPanelClose width={20} height={20} /></button>
          </Show>
        </div>
        <Show when={props.tasks().length === 0}>
          <p class={styles.placeholder}>No tasks yet.</p>
        </Show>
        <For each={grouped()}>
          {(group) => {
            const repoMeta = () => props.repos().find((r) => r.path === group.repo);
            const stoppedKey = `stopped-${group.repo}`;
            const purgedKey = `purged-${group.repo}`;
            return (
            <div class={styles.repoGroup}>
              <div class={styles.repoGroupHeader}>
                {group.repo || "Other"}
                <Show when={repoMeta()} keyed>
                  {(meta) => (
                    <Show when={meta.defaultBranchCIStatus} keyed>
                      {(status) => {
                        const url = ciDotURL(meta);
                        const label = `Default branch CI: ${status}`;
                        return url
                          ? <a class={styles.ciDot} style={{ background: CI_DOT_COLOR[status] }} href={url} target="_blank" rel="noopener" title={label} onClick={(e) => e.stopPropagation()} />
                          : <span class={styles.ciDot} style={{ background: CI_DOT_COLOR[status] }} title={label} />;
                      }}
                    </Show>
                  )}
                </Show>
              </div>
              <Index each={group.active}>{renderTask}</Index>
              
              <Show when={group.stopped.length > 0}>
                <button class={styles.subGroupHeader} onClick={() => toggleExpanded(stoppedKey)}>
                  {expanded().has(stoppedKey) ? <ArrowDropDown width={18} height={18} /> : <ArrowRight width={18} height={18} />}
                  Stopped ({group.stopped.length})
                </button>
                <Show when={expanded().has(stoppedKey)}>
                  <Index each={group.stopped}>{renderTask}</Index>
                </Show>
              </Show>

              <Show when={group.purged.length > 0}>
                <button class={styles.subGroupHeader} onClick={() => toggleExpanded(purgedKey)}>
                  {expanded().has(purgedKey) ? <ArrowDropDown width={18} height={18} /> : <ArrowRight width={18} height={18} />}
                  Purged ({group.purged.length})
                </button>
                <Show when={expanded().has(purgedKey)}>
                  <Index each={group.purged}>{renderTask}</Index>
                </Show>
              </Show>
            </div>
            );
          }}
        </For>
      </div>
      <Show when={!props.sidebarOpen() && props.selectedId !== null}>
        <button class={styles.expandBtn} onClick={() => props.setSidebarOpen(true)} title="Expand sidebar"><LeftPanelOpen width={20} height={20} /></button>
      </Show>
    </>
  );
}


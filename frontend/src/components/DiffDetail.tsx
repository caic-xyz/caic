// Full-page repository status and unified diff viewer for a task's container changes.

import {
  createSignal,
  createEffect,
  createMemo,
  For,
  Show,
  onMount,
  onCleanup,
} from "solid-js";
import { useNavigate } from "@solidjs/router";
import ArrowBackIcon from "@material-symbols/svg-400/outlined/arrow_back.svg?solid";
import WrapTextIcon from "@material-symbols/svg-400/outlined/wrap_text.svg?solid";

import type {
  DiffFileStat,
  GitFileStatus,
  GitRepositoryStatus,
} from "@sdk/types.gen";

import { getTaskDiff } from "../api";
import { splitDiff } from "./diffLines";
import UnifiedDiffBlock from "./UnifiedDiffBlock";
import styles from "./DiffDetail.module.css";

interface Props {
  taskId: string;
  diffStat: DiffFileStat[];
  taskPath: string;
  onTaskRefreshError?: (taskId: string, err: unknown) => boolean;
}

export default function DiffDetail(props: Props) {
  const navigate = useNavigate();
  const [fullDiff, setFullDiff] = createSignal<string | null>(null);
  const [repositories, setRepositories] = createSignal<GitRepositoryStatus[]>(
    [],
  );
  const [error, setError] = createSignal<string | null>(null);
  const [loading, setLoading] = createSignal(true);
  // Collapsed files (all expanded by default).
  const [collapsedFiles, setCollapsedFiles] = createSignal<Set<string>>(
    new Set(),
  );
  const [lineWrap, setLineWrap] = createSignal(false);

  createEffect(() => {
    const id = props.taskId;
    const onTaskRefreshError = props.onTaskRefreshError;
    setLoading(true);
    setError(null);
    setCollapsedFiles(new Set<string>());
    getTaskDiff(id, "")
      .then((d) => {
        setFullDiff(d.diff);
        setRepositories(d.repositories);
      })
      .catch((e: unknown) => {
        if (onTaskRefreshError?.(id, e)) return;
        setError(e instanceof Error ? e.message : "Unknown error");
      })
      .finally(() => setLoading(false));
  });

  // Escape navigates back to the task detail.
  onMount(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") navigate(props.taskPath);
    };
    document.addEventListener("keydown", onKey);
    onCleanup(() => document.removeEventListener("keydown", onKey));
  });

  const fileDiffs = createMemo(() => {
    const raw = fullDiff();
    if (!raw) return [];
    return splitDiff(raw);
  });

  // Build a lookup from diffStat for +/- counts.
  const statByPath = createMemo(() => {
    const m = new Map<string, DiffFileStat>();
    for (const f of props.diffStat) m.set(f.path, f);
    return m;
  });

  function toggleFile(path: string) {
    setCollapsedFiles((prev) => {
      const next = new Set(prev);
      if (next.has(path)) next.delete(path);
      else next.add(path);
      return next;
    });
  }

  function statusLabels(file: GitFileStatus) {
    if (file.indexStatus === "?" && file.worktreeStatus === "?") {
      return [{ scope: "", label: "untracked" }];
    }
    const labels: { scope: string; label: string }[] = [];
    if (file.indexStatus)
      labels.push({ scope: "staged", label: gitStatusLabel(file.indexStatus) });
    if (file.worktreeStatus)
      labels.push({
        scope: "working tree",
        label: gitStatusLabel(file.worktreeStatus),
      });
    return labels;
  }

  return (
    <div class={styles.container}>
      <div class={styles.header}>
        <button
          class={styles.backBtn}
          onClick={() => navigate(props.taskPath)}
          title="Back to task"
        >
          <ArrowBackIcon width={20} height={20} />
        </button>
        <span class={styles.headerMeta}>Repository changes</span>
        <button
          type="button"
          class={`${styles.wrapToggle} ${lineWrap() ? styles.wrapToggleActive : ""}`}
          aria-label={lineWrap() ? "Disable line wrap" : "Enable line wrap"}
          aria-pressed={lineWrap()}
          title={lineWrap() ? "Disable line wrap" : "Enable line wrap"}
          onClick={() => setLineWrap((wrap) => !wrap)}
        >
          <WrapTextIcon width={20} height={20} />
        </button>
      </div>
      <div class={styles.fileList}>
        <Show when={loading()}>
          <div class={styles.diffLoading}>Loading diff...</div>
        </Show>
        <Show when={error()}>
          <div class={styles.diffError}>{error()}</div>
        </Show>
        <Show when={!loading() && !error()}>
          <div class={styles.statusList}>
            <For each={repositories()}>
              {(repo) => (
                <section class={styles.repoStatus}>
                  <div class={styles.repoHeading}>
                    <span class={styles.headerRepo}>{repo.name}</span>
                    <span class={styles.branchName}>
                      {repo.branch || "detached HEAD"}
                    </span>
                    <span class={styles.branchArrow} aria-hidden="true">
                      →
                    </span>
                    <span class={styles.upstreamName}>
                      {repo.upstream ?? "no upstream"}
                    </span>
                  </div>
                  <div class={styles.divergence}>
                    <Show
                      when={repo.upstream}
                      fallback="No upstream tracking branch configured"
                    >
                      {repo.ahead} {repo.ahead === 1 ? "commit" : "commits"}{" "}
                      ahead
                      <Show when={repo.behind > 0}>
                        {" "}
                        · {repo.behind} behind
                      </Show>
                    </Show>
                  </div>

                  <div class={styles.statusGroup}>
                    <h2>Commits ahead ({repo.commits.length})</h2>
                    <Show
                      when={repo.commits.length > 0}
                      fallback={
                        <p class={styles.cleanState}>
                          No commits ahead of upstream
                        </p>
                      }
                    >
                      <div class={styles.commitList}>
                        <For each={repo.commits}>
                          {(commit) => (
                            <article class={styles.commit}>
                              <div class={styles.commitHeading}>
                                <code>{commit.sha.slice(0, 8)}</code>
                                <span>{commit.subject}</span>
                              </div>
                              <Show when={commit.stat.length > 0}>
                                <div class={styles.commitStat}>
                                  <For each={commit.stat}>
                                    {(file) => (
                                      <div class={styles.commitStatFile}>
                                        <span class={styles.commitStatPath}>
                                          {file.path}
                                        </span>
                                        <Show
                                          when={!file.binary}
                                          fallback={
                                            <span class={styles.binary}>
                                              binary
                                            </span>
                                          }
                                        >
                                          <span class={styles.fileCounts}>
                                            <Show when={file.added > 0}>
                                              <span class={styles.added}>
                                                +{file.added}
                                              </span>
                                            </Show>
                                            <Show when={file.deleted > 0}>
                                              <span class={styles.deleted}>
                                                &minus;{file.deleted}
                                              </span>
                                            </Show>
                                          </span>
                                        </Show>
                                      </div>
                                    )}
                                  </For>
                                  <div class={styles.commitSummary}>
                                    {commit.stat.length}{" "}
                                    {commit.stat.length === 1
                                      ? "file"
                                      : "files"}{" "}
                                    changed
                                  </div>
                                </div>
                              </Show>
                            </article>
                          )}
                        </For>
                      </div>
                    </Show>
                  </div>

                  <div class={styles.statusGroup}>
                    <h2>Uncommitted changes ({repo.uncommitted.length})</h2>
                    <Show
                      when={repo.uncommitted.length > 0}
                      fallback={
                        <p class={styles.cleanState}>Working tree clean</p>
                      }
                    >
                      <div class={styles.uncommittedList}>
                        <For each={repo.uncommitted}>
                          {(file) => (
                            <div class={styles.uncommittedFile}>
                              <span class={styles.uncommittedPath}>
                                <Show when={file.originalPath}>
                                  <span>{file.originalPath} → </span>
                                </Show>
                                {file.path}
                              </span>
                              <span class={styles.statusBadges}>
                                <For each={statusLabels(file)}>
                                  {(status) => (
                                    <span class={styles.statusBadge}>
                                      <Show when={status.scope}>
                                        {status.scope}:{" "}
                                      </Show>
                                      {status.label}
                                    </span>
                                  )}
                                </For>
                              </span>
                            </div>
                          )}
                        </For>
                      </div>
                    </Show>
                  </div>
                </section>
              )}
            </For>
          </div>

          <section class={styles.combinedDiff}>
            <h2>Combined diff</h2>
            <Show
              when={fileDiffs().length > 0}
              fallback={
                <p class={styles.cleanState}>No changes relative to upstream</p>
              }
            >
              <For each={fileDiffs()}>
                {(fd) => {
                  const stat = () => statByPath().get(fd.path);
                  const collapsed = () => collapsedFiles().has(fd.path);
                  return (
                    <>
                      <div
                        class={`${styles.fileRow} ${styles.fileRowClickable}`}
                        role="button"
                        tabIndex={0}
                        onClick={() => toggleFile(fd.path)}
                        onKeyDown={(e) => {
                          if (e.key === "Enter" || e.key === " ") {
                            e.preventDefault();
                            toggleFile(fd.path);
                          }
                        }}
                      >
                        <span class={styles.collapseIndicator}>
                          {collapsed() ? "\u25b6" : "\u25bc"}
                        </span>
                        <span class={styles.filePath}>{fd.path}</span>
                        <Show
                          when={stat()?.binary}
                          fallback={
                            <span class={styles.fileCounts}>
                              <Show when={(stat()?.added ?? 0) > 0}>
                                <span class={styles.added}>
                                  +{stat()?.added}
                                </span>
                              </Show>
                              <Show when={(stat()?.deleted ?? 0) > 0}>
                                <span class={styles.deleted}>
                                  &minus;{stat()?.deleted}
                                </span>
                              </Show>
                            </span>
                          }
                        >
                          <span class={styles.binary}>binary</span>
                        </Show>
                      </div>
                      <Show when={!collapsed()}>
                        <UnifiedDiffBlock
                          lines={fd.lines}
                          lineWrap={lineWrap()}
                        />
                      </Show>
                    </>
                  );
                }}
              </For>
            </Show>
          </section>
        </Show>
      </div>
    </div>
  );
}

function gitStatusLabel(code: string): string {
  const labels: Record<string, string> = {
    A: "added",
    C: "copied",
    D: "deleted",
    M: "modified",
    R: "renamed",
    T: "type changed",
    U: "unmerged",
  };
  return labels[code] ?? code;
}

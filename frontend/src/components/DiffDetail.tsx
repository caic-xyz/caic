// Full-page repository status with collapsible per-file commit and uncommitted diffs.

import {
  createSignal,
  createEffect,
  For,
  Show,
  onMount,
  onCleanup,
} from "solid-js";
import { useNavigate } from "@solidjs/router";
import ArrowBackIcon from "@material-symbols/svg-400/outlined/arrow_back.svg?solid";
import WrapTextIcon from "@material-symbols/svg-400/outlined/wrap_text.svg?solid";

import type { GitFileStatus, GitRepositoryStatus } from "@sdk/types.gen";

import { getTaskDiff } from "../api";
import UnifiedDiffBlock from "./UnifiedDiffBlock";
import styles from "./DiffDetail.module.css";

interface Props {
  taskId: string;
  taskPath: string;
  onTaskRefreshError?: (taskId: string, err: unknown) => boolean;
}

export default function DiffDetail(props: Props) {
  const navigate = useNavigate();
  const [repositories, setRepositories] = createSignal<GitRepositoryStatus[]>(
    [],
  );
  const [error, setError] = createSignal<string | null>(null);
  const [loading, setLoading] = createSignal(true);
  const [lineWrap, setLineWrap] = createSignal(false);

  createEffect(() => {
    const id = props.taskId;
    const onTaskRefreshError = props.onTaskRefreshError;
    setLoading(true);
    setError(null);
    getTaskDiff(id, "")
      .then((d) => {
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

  function statusLabels(file: GitFileStatus) {
    if (file.indexStatus === "?" && file.worktreeStatus === "?") {
      return [{ scope: "", label: "untracked" }];
    }
    const labels: { scope: string; label: string }[] = [];
    if (file.indexStatus && file.indexStatus !== "M")
      labels.push({ scope: "staged", label: gitStatusLabel(file.indexStatus) });
    if (file.worktreeStatus && file.worktreeStatus !== "M")
      labels.push({
        scope: "",
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
                              <span
                                class={styles.commitGraph}
                                aria-hidden="true"
                              >
                                <span />
                              </span>
                              <div class={styles.commitHeading}>
                                <code class={styles.commitSha}>
                                  {commit.sha.slice(0, 8)}
                                </code>
                                <Show when={commit.decorations}>
                                  <span class={styles.commitDecorations}>
                                    {commit.decorations}
                                  </span>
                                </Show>
                                <time
                                  class={styles.commitDate}
                                  dateTime={commit.authoredDate}
                                >
                                  {commit.authoredDate}
                                </time>
                                <span class={styles.commitSubject}>
                                  {commit.subject}
                                </span>
                              </div>
                              <Show when={commit.stat.length > 0}>
                                <div class={styles.commitStat}>
                                  <For each={commit.stat}>
                                    {(file) => (
                                      <FileDiffRow
                                        path={file.path}
                                        added={file.added}
                                        deleted={file.deleted}
                                        binary={file.binary ?? false}
                                        diff={file.diff ?? ""}
                                        lineWrap={lineWrap()}
                                        variant="commit"
                                      />
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
                            <FileDiffRow
                              path={file.path}
                              originalPath={file.originalPath}
                              added={file.added}
                              deleted={file.deleted}
                              binary={file.binary}
                              diff={file.diff}
                              statuses={statusLabels(file)}
                              lineWrap={lineWrap()}
                              variant="uncommitted"
                            />
                          )}
                        </For>
                      </div>
                    </Show>
                  </div>
                </section>
              )}
            </For>
          </div>
        </Show>
      </div>
    </div>
  );
}

interface FileDiffRowProps {
  path: string;
  originalPath?: string;
  added: number;
  deleted: number;
  binary: boolean;
  diff: string;
  statuses?: { scope: string; label: string }[];
  lineWrap: boolean;
  variant: "commit" | "uncommitted";
}

function FileDiffRow(props: FileDiffRowProps) {
  const [expanded, setExpanded] = createSignal(false);

  return (
    <div
      class={styles.fileChange}
      classList={{
        [styles.uncommittedFileChange]: props.variant === "uncommitted",
      }}
    >
      <button
        type="button"
        class={styles.fileChangeButton}
        aria-expanded={expanded()}
        onClick={() => setExpanded((value) => !value)}
      >
        <span class={styles.collapseIndicator} aria-hidden="true">
          {expanded() ? "\u25bc" : "\u25b6"}
        </span>
        <span class={styles.fileChangePath}>
          <Show when={props.originalPath}>
            <span class={styles.originalPath}>{props.originalPath} → </span>
          </Show>
          {props.path}
        </span>
        <Show when={props.statuses}>
          <span class={styles.statusBadges}>
            <For each={props.statuses}>
              {(status) => (
                <span class={styles.statusBadge}>
                  <Show when={status.scope}>{status.scope}: </Show>
                  {status.label}
                </span>
              )}
            </For>
          </span>
        </Show>
        <FileCounts
          added={props.added}
          deleted={props.deleted}
          binary={props.binary}
        />
      </button>
      <Show when={expanded()}>
        <div class={styles.fileDiff}>
          <Show
            when={props.diff}
            fallback={<p class={styles.cleanState}>No textual diff</p>}
          >
            <UnifiedDiffBlock
              diff={props.diff}
              hideFileHeader
              lineWrap={props.lineWrap}
            />
          </Show>
        </div>
      </Show>
    </div>
  );
}

function FileCounts(props: {
  added: number;
  deleted: number;
  binary: boolean;
}) {
  return (
    <Show
      when={!props.binary}
      fallback={<span class={styles.binary}>binary</span>}
    >
      <span class={styles.fileCounts}>
        <Show when={props.added > 0}>
          <span class={styles.added}>+{props.added}</span>
        </Show>
        <Show when={props.deleted > 0}>
          <span class={styles.deleted}>&minus;{props.deleted}</span>
        </Show>
      </span>
    </Show>
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

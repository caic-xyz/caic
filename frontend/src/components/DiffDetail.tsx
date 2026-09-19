// Full-page repository status with shared stale-refresh metadata and persistent lazy file rows.

import { createSignal, createEffect, For, Show, onMount, onCleanup, untrack } from "solid-js";
import { createStore, reconcile } from "solid-js/store";
import { useNavigate } from "@solidjs/router";
import ArrowBackIcon from "@material-symbols/svg-400/outlined/arrow_back.svg?solid";
import WrapTextIcon from "@material-symbols/svg-400/outlined/wrap_text.svg?solid";

import type {
  DiffIndexCommit,
  DiffIndexFileStat,
  DiffIndexFileStatus,
  DiffIndexRepository,
} from "@sdk/types.gen";

import { taskDiffCache } from "../diffCache";
import UnifiedDiffBlock from "./UnifiedDiffBlock";
import styles from "./DiffDetail.module.css";

interface Props {
  taskId: string;
  taskPath: string;
  onTaskRefreshError?: (taskId: string, err: unknown) => boolean;
}

type ViewFileStat = DiffIndexFileStat & { id: string };
type ViewFileStatus = DiffIndexFileStatus & { id: string };
type ViewCommit = Omit<DiffIndexCommit, "stat"> & {
  id: string;
  stat: ViewFileStat[];
};
type ViewRepository = Omit<DiffIndexRepository, "commits" | "uncommitted"> & {
  id: string;
  commits: ViewCommit[];
  uncommitted: ViewFileStatus[];
};

function indexedRepositories(repositories: DiffIndexRepository[]): ViewRepository[] {
  return repositories.map((repo, repositoryIndex) => ({
    ...repo,
    id: `${repositoryIndex}\0${repo.name}`,
    commits: repo.commits.map((commit) => ({
      ...commit,
      id: `${repo.name}\0${commit.sha}`,
      stat: commit.stat.map((file) => ({
        ...file,
        id: `${repo.name}\0${commit.sha}\0${file.path}`,
      })),
    })),
    uncommitted: repo.uncommitted.map((file) => ({
      ...file,
      id: `${repo.name}\0${file.originalPath ?? ""}\0${file.path}`,
    })),
  }));
}

export default function DiffDetail(props: Props) {
  const navigate = useNavigate();
  const [repositories, setRepositories] = createStore<ViewRepository[]>([]);
  const [error, setError] = createSignal<string | null>(null);
  const [loading, setLoading] = createSignal(true);
  const [stale, setStale] = createSignal(false);
  const [refreshError, setRefreshError] = createSignal<string | null>(null);
  const [unavailable, setUnavailable] = createSignal(false);
  const [indexVersion, setIndexVersion] = createSignal(0);
  const [lineWrap, setLineWrap] = createSignal(false);
  const [expandedRows, setExpandedRows] = createSignal<Set<string>>(new Set());

  createEffect(() => {
    const id = props.taskId;
    const onTaskRefreshError = props.onTaskRefreshError;
    const update = () => {
      const snapshot = taskDiffCache.snapshot(id);
      setRepositories(
        reconcile(indexedRepositories(snapshot.data?.repositories ?? []), {
          key: "id",
        }),
      );
      setLoading(snapshot.loading && snapshot.data === null);
      setStale(snapshot.loading && snapshot.data !== null);
      setUnavailable(snapshot.data === null && !snapshot.loading && snapshot.error === null);
      setIndexVersion(snapshot.version);
      setError(
        snapshot.error && snapshot.data === null
          ? snapshot.error instanceof Error
            ? snapshot.error.message
            : "Unknown error"
          : null,
      );
      setRefreshError(
        snapshot.error && snapshot.data !== null
          ? snapshot.error instanceof Error
            ? snapshot.error.message
            : "Unknown error"
          : null,
      );
    };
    const unsubscribe = taskDiffCache.subscribe(id, update);
    update();
    taskDiffCache.revalidateIndex(id).catch((e: unknown) => {
      onTaskRefreshError?.(id, e);
    });
    onCleanup(unsubscribe);
  });

  const toggleRow = (key: string) => {
    setExpandedRows((current) => {
      const next = new Set(current);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  // Escape navigates back to the task detail.
  onMount(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") navigate(props.taskPath);
    };
    document.addEventListener("keydown", onKey);
    onCleanup(() => document.removeEventListener("keydown", onKey));
  });

  function statusLabels(file: DiffIndexFileStatus) {
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
          <div class={styles.diffLoading} role="status" aria-live="polite">
            Loading diff...
          </div>
        </Show>
        <Show when={stale()}>
          <div class={styles.diffLoading} role="status" aria-live="polite">
            Updating diff...
          </div>
        </Show>
        <Show when={refreshError()}>
          {(message) => (
            <div class={styles.diffError} role="alert">
              Diff may be out of date: {message()}
            </div>
          )}
        </Show>
        <Show when={unavailable()}>
          <div class={styles.diffError} role="status">
            Diff unavailable
          </div>
        </Show>
        <Show when={error()}>
          <div class={styles.diffError}>{error()}</div>
        </Show>
        <Show when={!loading() && !error()}>
          <div class={styles.statusList}>
            <For each={repositories}>
              {(repo, repositoryIndex) => (
                <section class={styles.repoStatus}>
                  <div class={styles.repoHeading}>
                    <span class={styles.headerRepo}>{repo.name}</span>
                    <span class={styles.branchName}>{repo.branch || "detached HEAD"}</span>
                    <span class={styles.branchArrow} aria-hidden="true">
                      →
                    </span>
                    <span class={styles.upstreamName}>{repo.upstream ?? "no upstream"}</span>
                  </div>
                  <div class={styles.divergence}>
                    <Show when={repo.upstream} fallback="No upstream tracking branch configured">
                      {repo.ahead} {repo.ahead === 1 ? "commit" : "commits"} ahead
                      <Show when={repo.behind > 0}> · {repo.behind} behind</Show>
                    </Show>
                  </div>

                  <div class={styles.statusGroup}>
                    <h2>Commits ahead ({repo.commits.length})</h2>
                    <Show
                      when={repo.commits.length > 0}
                      fallback={<p class={styles.cleanState}>No commits ahead of upstream</p>}
                    >
                      <div class={styles.commitList}>
                        <For each={repo.commits}>
                          {(commit) => (
                            <article class={styles.commit}>
                              <span class={styles.commitGraph} aria-hidden="true">
                                <span />
                              </span>
                              <div class={styles.commitHeading}>
                                <code class={styles.commitSha}>{commit.sha.slice(0, 8)}</code>
                                <Show when={commit.decorations}>
                                  <span class={styles.commitDecorations}>{commit.decorations}</span>
                                </Show>
                                <time class={styles.commitDate} dateTime={commit.authoredDate}>
                                  {commit.authoredDate}
                                </time>
                                <span class={styles.commitSubject}>{commit.subject}</span>
                              </div>
                              <Show when={commit.stat.length > 0}>
                                <div class={styles.commitStat}>
                                  <For each={commit.stat}>
                                    {(file) => {
                                      return (
                                        <FileDiffRow
                                          path={file.path}
                                          added={file.added}
                                          deleted={file.deleted}
                                          binary={file.binary ?? false}
                                          loadDiff={() =>
                                            taskDiffCache.loadPatch({
                                              taskId: props.taskId,
                                              repository: String(repositoryIndex()),
                                              commit: commit.sha,
                                              path: file.path,
                                              originalPath: "",
                                            })
                                          }
                                          onLoadError={(err) =>
                                            props.onTaskRefreshError?.(props.taskId, err) ?? false
                                          }
                                          lineWrap={lineWrap()}
                                          variant="commit"
                                          expanded={expandedRows().has(file.id)}
                                          onToggle={() => toggleRow(file.id)}
                                          indexVersion={indexVersion()}
                                        />
                                      );
                                    }}
                                  </For>
                                  <div class={styles.commitSummary}>
                                    {commit.stat.length}{" "}
                                    {commit.stat.length === 1 ? "file" : "files"} changed
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
                      fallback={<p class={styles.cleanState}>Working tree clean</p>}
                    >
                      <div class={styles.uncommittedList}>
                        <For each={repo.uncommitted}>
                          {(file) => {
                            return (
                              <FileDiffRow
                                path={file.path}
                                originalPath={file.originalPath}
                                added={file.added}
                                deleted={file.deleted}
                                binary={file.binary}
                                loadDiff={() =>
                                  taskDiffCache.loadPatch({
                                    taskId: props.taskId,
                                    repository: String(repositoryIndex()),
                                    commit: "",
                                    path: file.path,
                                    originalPath: file.originalPath ?? "",
                                  })
                                }
                                onLoadError={(err) =>
                                  props.onTaskRefreshError?.(props.taskId, err) ?? false
                                }
                                statuses={statusLabels(file)}
                                lineWrap={lineWrap()}
                                variant="uncommitted"
                                expanded={expandedRows().has(file.id)}
                                onToggle={() => toggleRow(file.id)}
                                indexVersion={indexVersion()}
                              />
                            );
                          }}
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
  loadDiff: () => Promise<string>;
  onLoadError: (err: unknown) => boolean;
  statuses?: { scope: string; label: string }[];
  lineWrap: boolean;
  variant: "commit" | "uncommitted";
  expanded: boolean;
  onToggle: () => void;
  indexVersion: number;
}

function FileDiffRow(props: FileDiffRowProps) {
  let toggleButton: HTMLButtonElement | undefined;
  let retryButton: HTMLButtonElement | undefined;
  let focusFrame: number | undefined;
  const [diff, setDiff] = createSignal<string | null>(null);
  const [loadError, setLoadError] = createSignal<string | null>(null);
  const [loading, setLoading] = createSignal(false);
  let loadedVersion = -1;
  let pendingVersion = -1;
  onCleanup(() => {
    if (focusFrame !== undefined) cancelAnimationFrame(focusFrame);
  });
  const pathLabel = () =>
    props.originalPath ? `${props.originalPath} → ${props.path}` : props.path;

  const load = async (version: number, force: boolean) => {
    if (loading()) {
      if (props.variant === "uncommitted") pendingVersion = Math.max(pendingVersion, version);
      return;
    }
    if (!force && diff() !== null && (props.variant === "commit" || version <= loadedVersion))
      return;
    if (version >= pendingVersion) pendingVersion = -1;
    const retryWasFocused = document.activeElement === retryButton;
    setLoading(true);
    try {
      setDiff(await props.loadDiff());
      // A background index refresh can start this load before the user reaches
      // the retry control, so re-check focus before removing that control
      // instead of trusting only the snapshot taken before the await.
      const restoreToggleFocus = retryWasFocused || document.activeElement === retryButton;
      setLoadError(null);
      if (restoreToggleFocus) {
        if (focusFrame !== undefined) cancelAnimationFrame(focusFrame);
        focusFrame = requestAnimationFrame(() => {
          focusFrame = undefined;
          if (toggleButton?.isConnected) toggleButton.focus();
        });
      }
    } catch (err: unknown) {
      if (!props.onLoadError(err)) {
        setLoadError(err instanceof Error ? err.message : "Unknown error");
      }
    } finally {
      loadedVersion = version;
      setLoading(false);
      if (pendingVersion > loadedVersion) {
        if (props.expanded) void load(pendingVersion, false);
      }
    }
  };

  const toggleExpanded = () => {
    props.onToggle();
  };

  createEffect(() => {
    const expanded = props.expanded;
    const version = props.indexVersion;
    if (!expanded) return;
    untrack(() => void load(version, false));
  });

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
        aria-expanded={props.expanded}
        aria-label={pathLabel()}
        title={pathLabel()}
        onClick={toggleExpanded}
        ref={(el) => {
          toggleButton = el;
        }}
      >
        <span class={styles.collapseIndicator} aria-hidden="true">
          {props.expanded ? "\u25bc" : "\u25b6"}
        </span>
        <span class={styles.fileChangePath} data-testid="diff-file-path">
          <Show when={props.originalPath}>
            {(originalPath) => (
              <>
                <FilePath path={originalPath()} class={styles.originalPath} />
                <span class={styles.renameArrow}>{" → "}</span>
              </>
            )}
          </Show>
          <FilePath path={props.path} />
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
        <FileCounts added={props.added} deleted={props.deleted} binary={props.binary} />
      </button>
      <Show when={props.expanded}>
        <div class={styles.fileDiff}>
          <Show
            when={diff()}
            fallback={
              <Show when={diff() === ""}>
                <p class={styles.cleanState}>No textual diff</p>
              </Show>
            }
          >
            {(loadedDiff) => (
              <UnifiedDiffBlock diff={loadedDiff()} hideFileHeader lineWrap={props.lineWrap} />
            )}
          </Show>
          <Show when={loadError()}>
            {(message) => (
              <div class={styles.fileDiffError} role="alert">
                <span>{message()}</span>
                <button
                  type="button"
                  aria-label={`Retry diff for ${pathLabel()}`}
                  aria-disabled={loading()}
                  onClick={() => void load(props.indexVersion, true)}
                  ref={(el) => {
                    retryButton = el;
                  }}
                >
                  Retry
                </button>
                <Show when={loading()}>
                  <span class={styles.diffLoading} role="status" aria-live="polite">
                    Retrying file diff...
                  </span>
                </Show>
              </div>
            )}
          </Show>
          <Show when={loading() && !loadError()}>
            <p class={styles.diffLoading} role="status" aria-live="polite">
              {diff() === null ? "Loading file diff..." : "Updating file diff..."}
            </p>
          </Show>
        </div>
      </Show>
    </div>
  );
}

function FilePath(props: { path: string; class?: string }) {
  let pathEl: HTMLSpanElement | undefined;
  const [displayPath, setDisplayPath] = createSignal("");

  const updateDisplayPath = (path: string) => {
    if (
      !pathEl ||
      typeof window.matchMedia !== "function" ||
      !window.matchMedia("(max-width: 768px)").matches
    ) {
      setDisplayPath(path);
      return;
    }

    const context = document.createElement("canvas").getContext("2d");
    if (!context) return;
    const style = getComputedStyle(pathEl);
    context.font = style.font;
    const letterSpacing = Number.parseFloat(style.letterSpacing);
    const measureText = (text: string) =>
      context.measureText(text).width +
      (Number.isFinite(letterSpacing) ? letterSpacing * (text.length - 1) : 0);
    setDisplayPath(elidePathAtBoundary(path, pathEl.clientWidth, measureText));
  };

  createEffect(() => {
    updateDisplayPath(props.path);
  });

  onMount(() => {
    if (!pathEl) return;
    updateDisplayPath(props.path);
    if (typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(() => updateDisplayPath(props.path));
    observer.observe(pathEl);
    onCleanup(() => observer.disconnect());
  });

  return (
    <span
      class={`${styles.pathValue} ${props.class ?? ""}`}
      data-testid="diff-path-value"
      ref={(el) => {
        pathEl = el;
      }}
    >
      {displayPath()}
    </span>
  );
}

export function elidePathAtBoundary(
  path: string,
  availableWidth: number,
  measureText: (text: string) => number,
): string {
  if (measureText(path) <= availableWidth) return path;

  const parts = path.split("/");
  const filename = parts.at(-1) ?? path;
  const directories = parts.slice(0, -1);
  if (directories.length === 0) return filename;

  const firstDirectory = directories[0];
  for (let kept = directories.length - 2; kept >= 0; kept -= 1) {
    const suffix = directories.slice(directories.length - kept);
    const candidate = [firstDirectory, "…", ...suffix, filename].join("/");
    if (measureText(candidate) <= availableWidth) return candidate;
  }

  const filenameWithEllipsis = `…/${filename}`;
  return measureText(filenameWithEllipsis) <= availableWidth ? filenameWithEllipsis : filename;
}

function FileCounts(props: { added: number; deleted: number; binary: boolean }) {
  return (
    <Show when={!props.binary} fallback={<span class={styles.binary}>binary</span>}>
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

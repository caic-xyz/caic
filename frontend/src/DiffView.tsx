// Full-page diff viewer for a task's file changes.
import { createSignal, createEffect, createMemo, For, Show } from "solid-js";
import { useNavigate } from "@solidjs/router";
import type { DiffFileStat } from "@sdk/types.gen";
import { getTaskDiff } from "@sdk/api.gen";
import ArrowBackIcon from "@material-symbols/svg-400/outlined/arrow_back.svg?solid";
import styles from "./DiffView.module.css";

interface FileDiff {
  path: string;
  content: string;
}

/** Extract the file path from a single diff section. */
function extractDiffPath(section: string): string {
  // Prefer +++ line: "+++ b/path" or "+++ path" (most reliable).
  const plus = section.match(/^\+\+\+ (?:[a-z]\/)?(.+)/m);
  if (plus && plus[1] !== "/dev/null") return plus[1];
  // Deleted files: use --- line.
  const minus = section.match(/^--- (?:[a-z]\/)?(.+)/m);
  if (minus && minus[1] !== "/dev/null") return minus[1];
  // Last resort: diff --git line with any single-char prefix.
  const git = section.match(/^diff --git .\/(.+) .\/\1$/m);
  if (git) return git[1];
  return "unknown";
}

/** Split a unified diff into per-file sections on "diff --git" boundaries. */
function splitDiff(raw: string): FileDiff[] {
  const parts = raw.split(/^(?=diff --git )/m);
  return parts
    .filter((p) => p.trim())
    .map((part) => ({ path: extractDiffPath(part), content: part }));
}

interface Props {
  taskId: string;
  diffStat: DiffFileStat[];
  repo: string;
  branch: string;
  taskPath: string;
}

export default function DiffView(props: Props) {
  const navigate = useNavigate();
  const [fullDiff, setFullDiff] = createSignal<string | null>(null);
  const [error, setError] = createSignal<string | null>(null);
  const [loading, setLoading] = createSignal(true);
  // Collapsed files (all expanded by default).
  const [collapsedFiles, setCollapsedFiles] = createSignal<Set<string>>(new Set());

  createEffect(() => {
    const id = props.taskId;
    setLoading(true);
    setError(null);
    setCollapsedFiles(new Set());
    getTaskDiff(id)
      .then((d) => setFullDiff(d.diff))
      .catch((e) => setError(e instanceof Error ? e.message : "Unknown error"))
      .finally(() => setLoading(false));
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
      <div class={styles.fileList}>
        <Show when={loading()}>
          <div class={styles.diffLoading}>Loading diff...</div>
        </Show>
        <Show when={error()}>
          <div class={styles.diffError}>{error()}</div>
        </Show>
        <Show when={!loading() && !error()}>
          <For each={fileDiffs()}>
            {(fd) => {
              const stat = () => statByPath().get(fd.path);
              const collapsed = () => collapsedFiles().has(fd.path);
              return (
                <>
                  <div class={`${styles.fileRow} ${styles.fileRowClickable}`} role="button" tabIndex={0} onClick={() => toggleFile(fd.path)} onKeyDown={(e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); toggleFile(fd.path); } }}>
                    <span class={styles.collapseIndicator}>{collapsed() ? "\u25b6" : "\u25bc"}</span>
                    <span class={styles.filePath}>{fd.path}</span>
                    <Show when={stat()?.binary} fallback={
                      <span class={styles.fileCounts}>
                        <Show when={(stat()?.added ?? 0) > 0}><span class={styles.added}>+{stat()?.added}</span></Show>
                        <Show when={(stat()?.deleted ?? 0) > 0}><span class={styles.deleted}>&minus;{stat()?.deleted}</span></Show>
                      </span>
                    }>
                      <span class={styles.binary}>binary</span>
                    </Show>
                  </div>
                  <Show when={!collapsed()}>
                    <pre class={styles.diffContent}>
                      <For each={fd.content.split("\n")}>
                        {(line) => {
                          let cls = "";
                          if (line.startsWith("+")) cls = styles.diffLineAdded;
                          else if (line.startsWith("-")) cls = styles.diffLineDeleted;
                          else if (line.startsWith("@@")) cls = styles.diffLineHunk;
                          else if (line.startsWith("diff ")) cls = styles.diffLineHeader;
                          return <div class={cls}>{line}</div>;
                        }}
                      </For>
                    </pre>
                  </Show>
                </>
              );
            }}
          </For>
        </Show>
      </div>
    </div>
  );
}

// Full-page diff viewer for a task's file changes.

import { createSignal, createEffect, createMemo, For, Show, onMount, onCleanup } from "solid-js";
import { useNavigate } from "@solidjs/router";
import ArrowBackIcon from "@material-symbols/svg-400/outlined/arrow_back.svg?solid";

import type { DiffFileStat } from "@sdk/types.gen";

import { getTaskDiff } from "../api";
import { splitDiff } from "./diffLines";
import UnifiedDiffBlock from "./UnifiedDiffBlock";
import styles from "./DiffDetail.module.css";

interface Props {
  taskId: string;
  diffStat: DiffFileStat[];
  repos?: { name: string; branch: string }[];
  taskPath: string;
}

export default function DiffDetail(props: Props) {
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
    setCollapsedFiles(new Set<string>());
    getTaskDiff(id, "")
      .then((d) => setFullDiff(d.diff))
      .catch((e) => setError(e instanceof Error ? e.message : "Unknown error"))
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

  return (
    <div class={styles.container}>
      <div class={styles.header}>
        <button class={styles.backBtn} onClick={() => navigate(props.taskPath)} title="Back to task">
          <ArrowBackIcon width={20} height={20} />
        </button>
        <span class={styles.headerMeta}>
          <For each={props.repos ?? []}>
            {(r, i) => (
              <>
                {i() > 0 ? ", " : ""}
                <span class={styles.headerRepo}>{r.name}</span>
                <span class={styles.headerBranch}>{r.branch}</span>
              </>
            )}
          </For>
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
                    <UnifiedDiffBlock lines={fd.lines} />
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

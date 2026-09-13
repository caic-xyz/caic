// RepoStateIcons renders compact Git and diff-state markers, optionally linking to a repository diff.

import { createSignal, onCleanup, onMount, Show } from "solid-js";
import { A } from "@solidjs/router";

import EditIcon from "@material-symbols/svg-400/outlined/edit_note.svg?solid";
import AheadIcon from "@material-symbols/svg-400/outlined/arrow_upward.svg?solid";
import BehindIcon from "@material-symbols/svg-400/outlined/arrow_downward.svg?solid";
import ConflictIcon from "@material-symbols/svg-400/outlined/warning.svg?solid";
import OperationIcon from "@material-symbols/svg-400/outlined/sync.svg?solid";

import type { DiffFileStat, GitRepositoryState } from "@sdk/types.gen";

import styles from "./RepoStateIcons.module.css";

interface Props {
  state?: GitRepositoryState;
  href?: string;
  elideDiffStats?: boolean;
}

function countLabel(count: number, singular: string, plural: string): string {
  return `${count} ${count === 1 ? singular : plural}`;
}

export function repoStateLabel(state?: GitRepositoryState): string {
  if (!state) return "";
  const labels: string[] = [];
  if (state.changedFiles > 0) labels.push(countLabel(state.changedFiles, "changed file", "changed files"));
  if (state.added > 0) labels.push(countLabel(state.added, "addition", "additions"));
  if (state.deleted > 0) labels.push(countLabel(state.deleted, "deletion", "deletions"));
  if (state.conflicts > 0) labels.push(countLabel(state.conflicts, "conflict", "conflicts"));
  if (state.operation) labels.push(`${state.operation} in progress`);
  if (state.uncommittedFiles > 0) labels.push(countLabel(state.uncommittedFiles, "uncommitted file", "uncommitted files"));
  if (state.ahead > 0) labels.push(countLabel(state.ahead, "commit ahead of upstream", "commits ahead of upstream"));
  if (state.behind > 0) labels.push(countLabel(state.behind, "commit behind upstream", "commits behind upstream"));
  return labels.join(", ");
}

// diffStatState adapts a task-level diff stat when live repository status is unavailable.
export function diffStatState(files?: readonly DiffFileStat[]): GitRepositoryState | undefined {
  if (!files?.length) return undefined;
  return {
    name: "",
    branch: "",
    ahead: 0,
    behind: 0,
    changedFiles: files.length,
    added: files.reduce((total, file) => total + file.added, 0),
    deleted: files.reduce((total, file) => total + file.deleted, 0),
    uncommittedFiles: 0,
    conflicts: 0,
  };
}

function DiffStats(props: { state?: GitRepositoryState }) {
  return (
    <Show when={(props.state?.changedFiles ?? 0) > 0}>
      <span class={styles.diffStats} data-testid="repo-state-diff-stats">
        {props.state?.changedFiles}f
        {" "}
        <span class={styles.added}>+{props.state?.added}</span>
        {" "}
        <span class={styles.deleted}>&minus;{props.state?.deleted}</span>
      </span>
    </Show>
  );
}

export default function RepoStateIcons(props: Props) {
  const [elideDiffStats, setElideDiffStats] = createSignal(false);
  let markerRef: HTMLElement | undefined;
  let resizeFrame: number | undefined;

  function scheduleDiffStatsElision() {
    const parent = markerRef?.parentElement;
    if (!parent) return;
    if (resizeFrame !== undefined) cancelAnimationFrame(resizeFrame);
    if (elideDiffStats()) setElideDiffStats(false);
    resizeFrame = requestAnimationFrame(() => {
      resizeFrame = undefined;
      setElideDiffStats(parent.scrollWidth > parent.clientWidth);
    });
  }

  onMount(() => {
    scheduleDiffStatsElision();
    window.addEventListener("resize", scheduleDiffStatsElision);
    onCleanup(() => {
      window.removeEventListener("resize", scheduleDiffStatsElision);
      if (resizeFrame !== undefined) cancelAnimationFrame(resizeFrame);
    });
  });

  const label = () => repoStateLabel(props.state);
  const shouldElideDiffStats = () => props.elideDiffStats || elideDiffStats();
  const diffLabel = () => {
    const status = label();
    if (!status) return "View diff";
    return props.state?.name ? `View diff for ${props.state.name}: ${status}` : `View diff: ${status}`;
  };
  const markers = () => (
    <>
      <Show when={label()}>
        <DiffStats state={props.state} />
        <Show when={(props.state?.conflicts ?? 0) > 0}>
          <span class={styles.conflict}><ConflictIcon class={styles.icon} aria-hidden="true" />{props.state?.conflicts}</span>
        </Show>
        <Show when={props.state?.operation}>
          <span class={styles.operation}><OperationIcon class={styles.icon} aria-hidden="true" /></span>
        </Show>
        <Show when={(props.state?.uncommittedFiles ?? 0) > 0}>
          <span class={styles.dirty}><EditIcon class={styles.icon} aria-hidden="true" />{props.state?.uncommittedFiles}</span>
        </Show>
        <Show when={(props.state?.ahead ?? 0) > 0}>
          <span class={styles.ahead}><AheadIcon class={styles.icon} aria-hidden="true" />{props.state?.ahead}</span>
        </Show>
        <Show when={(props.state?.behind ?? 0) > 0}>
          <span class={styles.behind}><BehindIcon class={styles.icon} aria-hidden="true" />{props.state?.behind}</span>
        </Show>
      </Show>
    </>
  );

  return (
    <Show when={label()}>
      <Show when={props.href} keyed fallback={
        <span
          class={styles.icons}
          data-elide-diff-stats={shouldElideDiffStats() ? "" : undefined}
          ref={(element) => { markerRef = element; }}
          role="img"
          aria-label={label()}
          title={label()}
        >
          {markers()}
        </span>
      }>
        {(href) => (
          <A
            class={styles.link}
            data-elide-diff-stats={shouldElideDiffStats() ? "" : undefined}
            href={href}
            ref={(element) => { markerRef = element; }}
            aria-label={diffLabel()}
            title={diffLabel()}
          >
            <span class={styles.icons} aria-hidden="true">{markers()}</span>
          </A>
        )}
      </Show>
    </Show>
  );
}

// SyncActivity renders one push attempt in the task log: status, branch, duration, hook output, failure, and cancel.

import { createEffect, createMemo, createSignal, on, onCleanup, Show } from "solid-js";
import BlockIcon from "@material-symbols/svg-400/outlined/block.svg?solid";
import CancelIcon from "@material-symbols/svg-400/outlined/cancel.svg?solid";
import CheckCircleIcon from "@material-symbols/svg-400/outlined/check_circle.svg?solid";
import ErrorIcon from "@material-symbols/svg-400/outlined/error.svg?solid";
import LoadingIcon from "@material-symbols/svg-400/outlined/progress_activity.svg?solid";

import { createLiveDuration } from "../liveDuration";
import type { SyncEntry } from "../syncActivity";
import { formatTimingDuration } from "../timing";
import ActionButton from "./ActionButton";
import styles from "./SyncActivity.module.css";

const statusLabels = { pushed: "Pushed", failed: "Failed", cancelled: "Cancelled" } as const;

// Distance from the bottom, in px, within which the log still counts as followed.
const followSlack = 4;

interface Props {
  entry: SyncEntry;
  // Resolves true once the cancel request was accepted. null where the entry
  // is not the task's live push, such as a previous context.
  onCancel: (() => Promise<boolean>) | null;
}

export default function SyncActivity(props: Props) {
  const running = () => props.entry.status === null;
  const state = () => props.entry.status ?? "running";
  const [cancelling, setCancelling] = createSignal(false);
  const liveMs = createLiveDuration(
    () => (props.entry.anchorTs > 0 ? props.entry.anchorTs : null),
    () => props.entry.durationMs,
    running,
  );
  const duration = createMemo(() => {
    const ms = liveMs();
    return ms === null ? null : formatTimingDuration(ms);
  });
  let log: HTMLPreElement | undefined;
  // The log follows new output until the reader scrolls up or selects text.
  let following = true;
  const atBottom = (el: HTMLElement) => el.scrollHeight - el.scrollTop - el.clientHeight <= followSlack;
  createEffect(
    on(
      () => props.entry.output,
      () => {
        if (log && following) log.scrollTop = log.scrollHeight;
      },
    ),
  );
  const onSelectionChange = () => {
    const selection = document.getSelection();
    if (
      log &&
      selection &&
      !selection.isCollapsed &&
      (log.contains(selection.anchorNode) || log.contains(selection.focusNode))
    )
      following = false;
  };
  document.addEventListener("selectionchange", onSelectionChange);
  onCleanup(() => document.removeEventListener("selectionchange", onSelectionChange));
  async function cancel(onCancel: () => Promise<boolean>) {
    setCancelling(true);
    if (!(await onCancel())) setCancelling(false);
  }
  return (
    <div
      class={styles.entry}
      role="group"
      data-testid="sync-activity"
      data-state={state()}
      aria-label={`Push${props.entry.branch ? ` to ${props.entry.branch}` : ""}`}
    >
      <div class={styles.header}>
        <span class={styles.icon} aria-hidden="true">
          <Show when={props.entry.status} fallback={<LoadingIcon class={styles.spinner} />}>
            {(status) =>
              status() === "pushed" ? <CheckCircleIcon /> : status() === "failed" ? <ErrorIcon /> : <BlockIcon />
            }
          </Show>
        </span>
        <span class={styles.title}>
          Push
          <Show when={props.entry.branch}>
            {(branch) => (
              <>
                {" to "}
                <code class={styles.branch}>{branch()}</code>
              </>
            )}
          </Show>
        </span>
        <span class={styles.status} role="status">
          {props.entry.status ? statusLabels[props.entry.status] : "Running"}
        </span>
        <Show when={duration()}>
          {(value) => (
            <span class={styles.duration} data-testid="sync-duration">
              {value()}
            </span>
          )}
        </Show>
        <Show when={running() && props.onCancel} keyed>
          {(onCancel) => (
            <ActionButton
              class={styles.cancel}
              label="Cancel push"
              disabled={cancelling()}
              aria-busy={cancelling() ? true : undefined}
              onClick={() => void cancel(onCancel)}
            >
              <CancelIcon width={20} height={20} />
            </ActionButton>
          )}
        </Show>
      </div>
      <Show when={props.entry.output}>
        <pre
          class={styles.log}
          ref={log}
          tabindex="0"
          aria-label="Push output"
          data-testid="sync-output"
          onScroll={(event) => {
            following = atBottom(event.currentTarget);
          }}
        >
          {props.entry.output}
        </pre>
      </Show>
      <Show when={props.entry.status !== "cancelled" && props.entry.error}>
        {(error) => (
          <p class={styles.error} data-testid="sync-error">
            {error()}
          </p>
        )}
      </Show>
    </div>
  );
}

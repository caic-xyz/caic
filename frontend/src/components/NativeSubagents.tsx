// Canonical subagent, batch, and background-command activity cards, rendered inline in the task transcript.

import { For, Show } from "solid-js";

import type { BackgroundCommandActivity, NativeActivity } from "../nativeSubagents";
import { backgroundCommandStatus, nativeActivityStatus } from "../nativeSubagents";
import { formatTimingDuration } from "../timing";
import styles from "./NativeSubagents.module.css";

export function NativeActivityStatus(props: { activity: NativeActivity; settled: boolean }) {
  return (
    <span class={styles.status} data-state={props.activity.status}>
      {nativeActivityStatus(props.activity, props.settled)}
    </span>
  );
}

// NativeAgentCard renders one folded lifecycle: identity, lifecycle status, and
// the result the harness reported. It is a details/summary so keyboard users can
// expand it without a pointer.
export function NativeAgentCard(props: { activity: NativeActivity; settled: boolean }) {
  const activity = () => props.activity;
  return (
    <details
      class={styles.card}
      data-testid="native-subagent-card"
      data-native-id={activity().id}
      data-background={activity().background ? "true" : undefined}
    >
      <summary>
        <span>
          {activity().scope === "batch" ? "Batch" : "Subagent"}: {activity().label || "Unnamed"}
        </span>
        <NativeActivityStatus activity={activity()} settled={props.settled} />
        <Show when={activity().background}>
          <span class={styles.background}>background</span>
        </Show>
        <Show when={activity().startedAt !== null && activity().endedAt !== null}>
          <span class={styles.duration} data-testid="native-subagent-duration">
            {formatTimingDuration(Math.max(0, (activity().endedAt ?? 0) - (activity().startedAt ?? 0)))}
          </span>
        </Show>
      </summary>
      <dl>
        <dt>Identity</dt>
        <dd>{activity().id}</dd>
        <Show when={activity().groupID}>
          <dt>Group</dt>
          <dd>{activity().groupID}</dd>
        </Show>
      </dl>
      <Show when={activity().prompt}>
        <h4>Prompt</h4>
        <pre>{activity().prompt}</pre>
      </Show>
      <h4>Result</h4>
      <pre>{activity().result || "No result reported by the harness."}</pre>
    </details>
  );
}

// NativeAgents renders the cards anchored to one transcript item. It renders
// nothing when the item has no anchored activity.
export default function NativeAgents(props: { activities: NativeActivity[]; settled: boolean }) {
  return (
    <Show when={props.activities.length > 0}>
      <div class={styles.anchored} data-testid="native-subagents">
        <For each={props.activities}>
          {(activity) => <NativeAgentCard activity={activity} settled={props.settled} />}
        </For>
      </div>
    </Show>
  );
}

// BackgroundCommandStatusChip is the compact tool-row indicator: it resolves a
// detached shell's outcome instead of leaving a perpetual "running in
// background" badge on the tool call that spawned it.
export function BackgroundCommandStatusChip(props: { command: BackgroundCommandActivity; settled: boolean }) {
  return (
    <span class={styles.status} data-state={props.command.status} data-testid="background-command-chip">
      <Show
        when={props.command.status === "running"}
        fallback={
          <Show
            when={props.command.exitCode !== undefined && props.command.exitCode !== null}
            fallback={backgroundCommandStatus(props.command, props.settled)}
          >
            exit {props.command.exitCode}
          </Show>
        }
      >
        running in background
      </Show>
    </span>
  );
}

// BackgroundCommandCard renders one folded lifecycle: identity, lifecycle
// status, exit code, and the result the harness reported. It is a
// details/summary so keyboard users can expand it without a pointer.
export function BackgroundCommandCard(props: { command: BackgroundCommandActivity; settled: boolean }) {
  const command = () => props.command;
  return (
    <details class={styles.card} data-testid="background-command-card" data-command-id={command().id}>
      <summary>
        <span>Background command: {command().label || "Shell command"}</span>
        <span class={styles.status} data-state={command().status}>
          {backgroundCommandStatus(command(), props.settled)}
        </span>
        <Show when={command().exitCode !== undefined && command().exitCode !== null}>
          <span class={styles.status} data-state={command().status} data-testid="background-command-exit">
            exit {command().exitCode}
          </span>
        </Show>
        <Show when={command().startedAt !== null && command().endedAt !== null}>
          <span class={styles.duration} data-testid="background-command-duration">
            {formatTimingDuration(Math.max(0, (command().endedAt ?? 0) - (command().startedAt ?? 0)))}
          </span>
        </Show>
      </summary>
      <dl>
        <dt>Identity</dt>
        <dd>{command().id}</dd>
        <Show when={command().outputRef}>
          <dt>Output</dt>
          <dd>{command().outputRef}</dd>
        </Show>
      </dl>
      <h4>Result</h4>
      <pre>{command().result || "No result reported by the harness."}</pre>
    </details>
  );
}

// BackgroundCommands renders the cards anchored to one transcript item. It
// renders nothing when the item has no anchored command.
export function BackgroundCommands(props: { commands: BackgroundCommandActivity[]; settled: boolean }) {
  return (
    <Show when={props.commands.length > 0}>
      <div class={styles.anchored} data-testid="background-commands">
        <For each={props.commands}>
          {(command) => <BackgroundCommandCard command={command} settled={props.settled} />}
        </For>
      </div>
    </Show>
  );
}

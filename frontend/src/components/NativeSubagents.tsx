// Canonical subagent and batch activity cards, rendered inline in the task transcript.

import { For, Show } from "solid-js";

import type { NativeActivity } from "../nativeSubagents";
import { nativeActivityStatus } from "../nativeSubagents";
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

// Canonical native-agent and batch activity cards, distinct from navigable CAIC child tasks.

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

export default function NativeSubagents(props: { activities: NativeActivity[]; settled: boolean }) {
  const active = (batch: boolean) =>
    props.settled
      ? 0
      : props.activities.filter((s) => s.status === "running" && (s.scope === "batch") === batch)
          .length;
  return (
    <Show when={props.activities.length > 0}>
      <section
        class={styles.panel}
        aria-label="Native subagent activity"
        data-testid="native-subagents"
      >
        <h3>
          Native subagents{" "}
          <span class={styles.counts}>
            {active(false)} agents active · {active(true)} batches active
          </span>
        </h3>
        <p class={styles.hint}>
          Activity inside this task, not separate CAIC tasks. Batch outcomes do not describe
          individual agents.
        </p>
        <For each={props.activities}>
          {(s) => (
            <details class={styles.card} data-testid="native-subagent-card" data-native-id={s.id}>
              <summary>
                <span>
                  {s.scope === "batch" ? "Native batch" : "Native agent"}: {s.label || "Unnamed"}
                </span>
                <NativeActivityStatus activity={s} settled={props.settled} />
                <Show when={s.startedAt !== null && s.endedAt !== null}>
                  <span class={styles.duration}>
                    {formatTimingDuration(Math.max(0, (s.endedAt ?? 0) - (s.startedAt ?? 0)))}
                  </span>
                </Show>
              </summary>
              <dl>
                <dt>Native identity</dt>
                <dd>{s.id}</dd>
                <Show when={s.groupID}>
                  <dt>Group</dt>
                  <dd>{s.groupID}</dd>
                </Show>
                <Show when={s.toolUseID}>
                  <dt>Tool call</dt>
                  <dd>{s.toolUseID}</dd>
                </Show>
              </dl>
              <Show when={s.prompt}>
                <h4>Prompt</h4>
                <pre>{s.prompt}</pre>
              </Show>
              <h4>Result</h4>
              <pre>{s.result || "No result reported by the harness."}</pre>
            </details>
          )}
        </For>
      </section>
    </Show>
  );
}

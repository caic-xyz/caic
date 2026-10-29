// Native activity disclosures keep summaries mounted and build child transcripts and results only while open.

import { createMemo, createSignal, For, Show, type JSX } from "solid-js";

import type { BackgroundCommandActivity, NativeActivity } from "../nativeSubagents";
import { backgroundCommandStatus, nativeActivityStatus, nativeTerminal } from "../nativeSubagents";
import { formatTimingDuration } from "../timing";
import { createLiveDuration } from "../liveDuration";
import styles from "./NativeSubagents.module.css";
import type { MessageGroup } from "../grouping";
import { aggregateToolGroups, IncrementalMessageGrouper } from "../grouping";

type TranscriptRenderer = (groups: () => MessageGroup[]) => JSX.Element;

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
export function NativeAgentCard(props: {
  activity: NativeActivity;
  settled: boolean;
  renderTranscript?: TranscriptRenderer;
}) {
  const activity = () => props.activity;
  const [open, setOpen] = createSignal(false);
  const duration = createLiveDuration(
    () => activity().startedAt,
    () => {
      const { startedAt, endedAt } = activity();
      return startedAt !== null && endedAt !== null ? Math.max(0, endedAt - startedAt) : null;
    },
    () => !props.settled && activity().status === "running",
  );
  return (
    <details
      class={styles.card}
      data-testid="native-subagent-card"
      data-native-id={activity().id}
      data-background={activity().background ? "true" : undefined}
      onToggle={(event) => setOpen(event.currentTarget.open)}
    >
      <summary>
        <span>
          {activity().scope === "batch" ? "Batch" : "Subagent"}: {activity().label || "Unnamed"}
        </span>
        <NativeActivityStatus activity={activity()} settled={props.settled} />
        <Show when={activity().background}>
          <span class={styles.background}>background</span>
        </Show>
        <Show when={duration() !== null}>
          <span class={styles.duration} data-testid="native-subagent-duration">
            {formatTimingDuration(duration() ?? 0)}
          </span>
        </Show>
      </summary>
      <Show when={open()}>
        <NativeAgentDetails activity={activity()} renderTranscript={props.renderTranscript} />
      </Show>
    </details>
  );
}

function NativeAgentDetails(props: { activity: NativeActivity; renderTranscript?: TranscriptRenderer }) {
  const grouper = new IncrementalMessageGrouper();
  const groups = createMemo(() => {
    if (!props.renderTranscript) return [];
    const activity = props.activity;
    const grouped = grouper.group(activity.messages ?? []);
    return aggregateToolGroups(nativeTerminal(activity.status) ? grouper.finish() : grouped);
  });
  const showResult = createMemo(() => {
    if (!groups().length) return true;
    const result = props.activity.result?.trim();
    if (!result) return false;
    return !groups().some((group) => {
      if (group.content) return group.kind === "text" && group.content.complete && group.content.text.trim() === result;
      return group.events.findLast((event) => event.kind === "text")?.text?.text.trim() === result;
    });
  });
  return (
    <>
      <dl>
        <dt>Identity</dt>
        <dd>{props.activity.id}</dd>
        <Show when={props.activity.groupID}>
          <dt>Group</dt>
          <dd>{props.activity.groupID}</dd>
        </Show>
      </dl>
      <Show when={props.activity.prompt}>
        <h4>Prompt</h4>
        <pre>{props.activity.prompt}</pre>
      </Show>
      <Show when={groups().length}>{props.renderTranscript?.(groups)}</Show>
      <Show when={showResult()}>
        <h4>Result</h4>
        <pre>{props.activity.result || "No result reported by the harness."}</pre>
      </Show>
    </>
  );
}

// NativeAgents renders the cards anchored to one transcript item. It renders
// nothing when the item has no anchored activity.
export default function NativeAgents(props: {
  activities: NativeActivity[];
  settled: boolean;
  renderTranscript?: TranscriptRenderer;
}) {
  const byID = createMemo(() => new Map(props.activities.map((activity) => [activity.id, activity])));
  const ids = createMemo(() => props.activities.map((activity) => activity.id));
  return (
    <Show when={props.activities.length > 0}>
      <div class={styles.anchored} data-testid="native-subagents">
        <For each={ids()}>
          {(id) => (
            <Show when={byID().get(id)}>
              {(activity) => (
                <NativeAgentCard
                  activity={activity()}
                  settled={props.settled}
                  renderTranscript={props.renderTranscript}
                />
              )}
            </Show>
          )}
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
        when={props.command.status === "running" && !props.settled}
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
  const [open, setOpen] = createSignal(false);
  const duration = createLiveDuration(
    () => command().startedAt,
    () => {
      const { startedAt, endedAt } = command();
      return startedAt !== null && endedAt !== null ? Math.max(0, endedAt - startedAt) : null;
    },
    () => !props.settled && command().status === "running",
  );
  return (
    <details
      class={styles.card}
      data-testid="background-command-card"
      data-command-id={command().id}
      onToggle={(event) => setOpen(event.currentTarget.open)}
    >
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
        <Show when={duration() !== null}>
          <span class={styles.duration} data-testid="background-command-duration">
            {formatTimingDuration(duration() ?? 0)}
          </span>
        </Show>
      </summary>
      <Show when={open()}>
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
      </Show>
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

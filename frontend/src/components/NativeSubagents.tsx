// Native activity disclosures keep summaries mounted and build child transcripts and results only while open.

import { createMemo, createSignal, For, Show, type JSX } from "solid-js";

import type { HistoryActivity } from "@sdk/types.gen";
import { createActivityHistory } from "../historyDisclosure";
import { NativeActivityTracker, BackgroundCommandTracker } from "../nativeSubagents";
import Button from "./Button";
import LoadingIcon from "@material-symbols/svg-400/outlined/progress_activity.svg?solid";
import { backgroundCommandStatus, nativeActivityStatus, nativeTerminal } from "../nativeSubagents";
import { formatTimingDuration } from "../timing";
import { createLiveDuration } from "../liveDuration";
import styles from "./NativeSubagents.module.css";
import type { MessageGroup } from "../grouping";
import { aggregateToolGroups, IncrementalMessageGrouper } from "../grouping";

type TranscriptRenderer = (groups: () => MessageGroup[]) => JSX.Element;

export function NativeActivityStatus(props: { activity: HistoryActivity; settled: boolean }) {
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
  activity: HistoryActivity;
  settled: boolean;
  renderTranscript?: TranscriptRenderer;
  onOpenChange?: (open: boolean) => void;
}) {
  const activity = () => props.activity;
  const [open, setOpen] = createSignal(false);
  const duration = createLiveDuration(
    () => {
      const at = activity().startedAt;
      return at ? Date.parse(at) : null;
    },
    () => {
      const { startedAt, endedAt } = activity();
      return startedAt && endedAt ? Math.max(0, Date.parse(endedAt) - Date.parse(startedAt)) : null;
    },
    () => !props.settled && activity().status === "running",
  );
  return (
    <details
      class={styles.card}
      data-testid="native-subagent-card"
      data-native-id={activity().id}
      data-background={activity().background ? "true" : undefined}
      onToggle={(event) => {
        const expanded = event.currentTarget.open;
        setOpen(expanded);
        props.onOpenChange?.(expanded);
      }}
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

function NativeAgentDetails(props: { activity: HistoryActivity; renderTranscript?: TranscriptRenderer }) {
  const body = createActivityHistory(() => props.activity);
  const tracker = new NativeActivityTracker();
  const folded = createMemo(() =>
    tracker.derive(body.messages()).find((activity) => activity.id === props.activity.id),
  );

  const grouper = new IncrementalMessageGrouper();
  const groups = createMemo(() => {
    if (!props.renderTranscript) return [];
    const activity = props.activity;
    const grouped = grouper.group(
      body.messages().filter((event) => event.kind !== "nativeSubagent" && event.kind !== "backgroundCommand"),
    );
    return aggregateToolGroups(nativeTerminal(activity.status) ? grouper.finish() : grouped);
  });
  const showResult = createMemo(() => {
    if (!groups().length) return true;
    const result = folded()?.result?.trim();
    if (!result) return false;
    return !groups().some((group) => {
      if (group.content) return group.kind === "text" && group.content.complete && group.content.text.trim() === result;
      return group.events.findLast((event) => event.kind === "text")?.text?.text.trim() === result;
    });
  });
  return (
    <>
      <Show when={body.loading()}>
        <span role="status" aria-label="Loading subagent history">
          <LoadingIcon width="13" height="13" aria-hidden="true" />
        </span>
      </Show>
      <Show when={body.error()}>
        {(error) => (
          <p role="alert">
            {error()} <Button onClick={body.retry}>Retry</Button>
          </p>
        )}
      </Show>
      <dl>
        <dt>Identity</dt>
        <dd>{props.activity.id}</dd>
        <Show when={props.activity.groupID}>
          <dt>Group</dt>
          <dd>{props.activity.groupID}</dd>
        </Show>
      </dl>
      <Show when={folded()?.prompt}>
        <h4>Prompt</h4>
        <pre>{folded()?.prompt}</pre>
      </Show>
      <Show when={groups().length}>{props.renderTranscript?.(groups)}</Show>
      <Show when={showResult()}>
        <h4>Result</h4>
        <pre>{folded()?.result || "No result reported by the harness."}</pre>
      </Show>
    </>
  );
}

// NativeAgents renders the cards anchored to one transcript item. It renders
// nothing when the item has no anchored activity.
export default function NativeAgents(props: {
  activities: HistoryActivity[];
  settled: boolean;
  renderTranscript?: TranscriptRenderer;
  onOpenChange?: (id: string, open: boolean) => void;
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
                  onOpenChange={(open) => props.onOpenChange?.(id, open)}
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
export function BackgroundCommandStatusChip(props: { command: HistoryActivity; settled: boolean }) {
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
export function BackgroundCommandCard(props: { command: HistoryActivity; settled: boolean }) {
  const command = () => props.command;
  const [open, setOpen] = createSignal(false);
  const duration = createLiveDuration(
    () => {
      const at = command().startedAt;
      return at ? Date.parse(at) : null;
    },
    () => {
      const { startedAt, endedAt } = command();
      return startedAt && endedAt ? Math.max(0, Date.parse(endedAt) - Date.parse(startedAt)) : null;
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
        <BackgroundCommandDetails command={command()} />
      </Show>
    </details>
  );
}

function BackgroundCommandDetails(props: { command: HistoryActivity }) {
  const body = createActivityHistory(() => props.command);
  const tracker = new BackgroundCommandTracker();
  const folded = createMemo(() => tracker.derive(body.messages()).find((command) => command.id === props.command.id));
  return (
    <>
      <Show when={body.loading()}>
        <span role="status" aria-label="Loading command history">
          <LoadingIcon width="13" height="13" aria-hidden="true" />
        </span>
      </Show>
      <Show when={body.error()}>
        {(error) => (
          <p role="alert">
            {error()} <Button onClick={body.retry}>Retry</Button>
          </p>
        )}
      </Show>
      <dl>
        <dt>Identity</dt>
        <dd>{props.command.id}</dd>
        <Show when={props.command.outputRef}>
          <dt>Output</dt>
          <dd>{props.command.outputRef}</dd>
        </Show>
      </dl>
      <h4>Result</h4>
      <pre>{folded()?.result || "No result reported by the harness."}</pre>
    </>
  );
}

// BackgroundCommands renders the cards anchored to one transcript item. It
// renders nothing when the item has no anchored command.
export function BackgroundCommands(props: { commands: HistoryActivity[]; settled: boolean }) {
  const byID = createMemo(() => new Map(props.commands.map((command) => [command.id, command])));
  const ids = createMemo(() => props.commands.map((command) => command.id));
  return (
    <Show when={ids().length > 0}>
      <div class={styles.anchored} data-testid="background-commands">
        <For each={ids()}>
          {(id) => (
            <Show when={byID().get(id)}>
              {(command) => <BackgroundCommandCard command={command()} settled={props.settled} />}
            </Show>
          )}
        </For>
      </div>
    </Show>
  );
}

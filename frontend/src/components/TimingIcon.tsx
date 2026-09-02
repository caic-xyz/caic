// TimingIcon shows compact event, turn, and user-response timing details.

import { createMemo, Show } from "solid-js";

import type { EventMessage } from "@sdk/types.gen";

import { formatTimingDuration, IncrementalEventTimingTracker } from "../timing";
import Tooltip from "./Tooltip";
import styles from "./TimingIcon.module.css";

interface Props {
  events: readonly EventMessage[];
  userWaitMs?: ReadonlyMap<EventMessage, number>;
  previousEventTs?: ReadonlyMap<EventMessage, number>;
  range?: { start: number; end: number } | null;
  showZero?: boolean;
}

export default function TimingIcon(props: Props) {
  const eventTracker = new IncrementalEventTimingTracker();
  const eventTiming = createMemo(() => eventTracker.derive(props.events));
  const effectiveRange = createMemo(() => {
    const timing = eventTiming();
    const range = props.range === undefined ? timing.range : props.range;
    if (!range || range.end > range.start || props.range !== undefined) return range;

    const previousTs = timing.firstTimedEvent
      ? props.previousEventTs?.get(timing.firstTimedEvent)
      : undefined;
    return previousTs !== undefined && previousTs < range.start
      ? { start: previousTs, end: range.end }
      : range;
  });
  const details = createMemo(() => {
    const lines: string[] = [];
    const timing = eventTiming();
    const result = timing.resultEvent?.result;
    const input = timing.inputEvent;
    const waitMs = input ? props.userWaitMs?.get(input) : undefined;
    const range = effectiveRange();
    const start = range?.start ?? null;
    const end = range?.end ?? null;
    const hasRange = start !== null && end !== null && end > start;
    const hasWait = waitMs !== undefined && waitMs > 0;

    if (result && result.duration > 0) {
      lines.push(`Turn: ${formatTimingDuration(result.duration * 1000)}`);
      if (result.durationAPI > 0) lines.push(`API: ${formatTimingDuration(result.durationAPI * 1000)}`);
    } else if (hasRange && !hasWait) {
      lines.push(`Element: ${formatTimingDuration(end - start)}`);
    }
    if (hasWait) lines.push(`User wait: ${formatTimingDuration(waitMs)}`);
    if (lines.length === 0) return props.showZero ? "Timing unavailable" : "";
    if (start !== null) {
      lines.push(`${end === start ? "At" : "Started"}: ${new Date(start).toLocaleString()}`);
    }
    if (hasRange) {
      lines.push(`Finished: ${new Date(end).toLocaleString()}`);
    }
    return lines.join("\n");
  });
  const displayedDuration = createMemo(() => {
    const timing = eventTiming();
    const result = timing.resultEvent?.result;
    if (result && result.duration > 0) return { ms: result.duration * 1000, prefix: "" };
    const input = timing.inputEvent;
    const waitMs = input ? props.userWaitMs?.get(input) : undefined;
    if (waitMs !== undefined && waitMs > 0) return { ms: waitMs, prefix: "wait " };
    const range = effectiveRange();
    if (range && range.end > range.start) return { ms: range.end - range.start, prefix: "" };
    return props.showZero ? { ms: 0, prefix: "" } : null;
  });

  return (
    <Show when={details()} keyed>
      {(text) => (
        <Tooltip text={text} class={styles.tooltip}>
          <span class={styles.content} aria-label="Timing details" data-testid="timing-duration">
            <Show when={displayedDuration()} keyed>
              {(duration) => <span class={styles.value}>{duration.prefix}{duration.ms === 0 ? "0s" : formatTimingDuration(duration.ms)}</span>}
            </Show>
          </span>
        </Tooltip>
      )}
    </Show>
  );
}

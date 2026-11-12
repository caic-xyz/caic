// TimingIcon shows compact event, turn, and user-response timing details, ticking while owned work runs.

import { createMemo, Show } from "solid-js";

import type { EventMessage } from "@sdk/types.gen";

import { createLiveDuration } from "../liveDuration";
import { formatTimingDuration, IncrementalEventTimingTracker } from "../timing";
import Tooltip from "./Tooltip";
import styles from "./TimingIcon.module.css";

interface Props {
  events: readonly EventMessage[];
  segments?: readonly (readonly EventMessage[])[];
  userWaitMs?: ReadonlyMap<EventMessage, number>;
  previousEventTs?: ReadonlyMap<EventMessage, number>;
  range?: { start: number; end: number } | null;
  showZero?: boolean;
  // Work the element owns beyond its events, such as a command that outlives
  // its tool call: the latest settled end extends the range, and running work
  // makes the duration tick.
  workEnd?: number;
  running?: boolean;
}

export default function TimingIcon(props: Props) {
  const eventTracker = new IncrementalEventTimingTracker();
  const eventTiming = createMemo(() => eventTracker.derive(props.events));
  const effectiveRange = createMemo(() => {
    const timing = eventTiming();
    const own = props.range === undefined ? timing.range : props.range;
    const workEnd = props.workEnd;
    const range = own && workEnd !== undefined && workEnd > own.end ? { start: own.start, end: workEnd } : own;
    if (!range || range.end > range.start || props.range !== undefined) return range;

    const previousTs = timing.firstTimedEvent ? props.previousEventTs?.get(timing.firstTimedEvent) : undefined;
    return previousTs !== undefined && previousTs < range.start ? { start: previousTs, end: range.end } : range;
  });
  const liveMs = createLiveDuration(
    () => effectiveRange()?.start ?? null,
    () => null,
    () => props.running === true,
  );
  const rangeMs = () => {
    const range = effectiveRange();
    return range ? Math.max(range.end - range.start, liveMs() ?? 0) : 0;
  };
  const combinedDurationMs = createMemo(() => {
    if (props.segments === undefined) return null;
    let total = 0;
    let timedSegments = 0;
    for (const events of props.segments) {
      let start = Number.POSITIVE_INFINITY;
      let end = 0;
      let firstEvent: EventMessage | undefined;
      for (const event of events) {
        if (event.ts <= 0) continue;
        if (event.ts < start) {
          start = event.ts;
          firstEvent = event;
        }
        end = Math.max(end, event.ts);
      }
      if (!Number.isFinite(start)) continue;
      if (end > start) {
        total += end - start;
        timedSegments++;
        continue;
      }
      const previousTs = firstEvent ? props.previousEventTs?.get(firstEvent) : undefined;
      if (previousTs !== undefined && previousTs < start) {
        total += start - previousTs;
        timedSegments++;
      }
    }
    return timedSegments > 0 ? total : null;
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
    const elapsedMs = rangeMs();
    const hasRange = start !== null && elapsedMs > 0;
    const hasWait = waitMs !== undefined && waitMs > 0;

    const combinedMs = combinedDurationMs();
    if (combinedMs !== null) {
      lines.push(`Combined elements: ${formatTimingDuration(combinedMs)}`);
    } else if (result && result.duration > 0) {
      lines.push(`Turn: ${formatTimingDuration(result.duration * 1000)}`);
      if (result.durationAPI > 0) lines.push(`API: ${formatTimingDuration(result.durationAPI * 1000)}`);
    } else if (hasRange && !hasWait) {
      lines.push(`Element: ${formatTimingDuration(elapsedMs)}`);
    }
    if (hasWait) lines.push(`User wait: ${formatTimingDuration(waitMs)}`);
    if (lines.length === 0) return props.showZero ? "Timing unavailable" : "";
    if (start !== null) {
      lines.push(`${hasRange ? "Started" : "At"}: ${new Date(start).toLocaleString()}`);
    }
    if (hasRange && end !== null && !props.running) {
      lines.push(`Finished: ${new Date(end).toLocaleString()}`);
    }
    return lines.join("\n");
  });
  const displayedDuration = createMemo(() => {
    const combinedMs = combinedDurationMs();
    if (combinedMs !== null) return { ms: combinedMs, prefix: "" };
    const timing = eventTiming();
    const result = timing.resultEvent?.result;
    if (result && result.duration > 0) return { ms: result.duration * 1000, prefix: "" };
    const input = timing.inputEvent;
    const waitMs = input ? props.userWaitMs?.get(input) : undefined;
    if (waitMs !== undefined && waitMs > 0) return { ms: waitMs, prefix: "wait " };
    const elapsedMs = rangeMs();
    if (elapsedMs > 0) return { ms: elapsedMs, prefix: "" };
    return props.showZero ? { ms: 0, prefix: "" } : null;
  });

  return (
    <Show when={details()} keyed>
      {(text) => (
        <Tooltip text={text} class={styles.tooltip}>
          <span class={styles.content} aria-label="Timing details" data-testid="timing-duration">
            <Show when={displayedDuration()} keyed>
              {(duration) => (
                <span class={styles.value}>
                  {duration.prefix}
                  {duration.ms === 0 ? "0s" : formatTimingDuration(duration.ms)}
                </span>
              )}
            </Show>
          </span>
        </Tooltip>
      )}
    </Show>
  );
}

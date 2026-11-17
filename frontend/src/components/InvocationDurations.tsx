// Transcript durations: a turn's or archived context's time links to its Performance view.

import { A, useLocation } from "@solidjs/router";

import type { HistorySection, HistoryTurn } from "@sdk/types.gen";
import { taskContextPath, taskPathFromPathname, taskTurnPath } from "../taskPath";
import { formatTotalDuration } from "../timing";
import Duration from "./Duration";
import styles from "./InvocationDurations.module.css";

// TurnDurationLink ends a turn row with the same time element as other rows;
// the time links to the turn's Performance view.
export function TurnDurationLink(props: { turn: HistoryTurn }) {
  const location = useLocation();
  const duration = () => formatTotalDuration(props.turn.durationMs);
  return (
    <A
      href={taskTurnPath(taskPathFromPathname(location.pathname), props.turn.number)}
      class={styles.trigger}
      aria-label={`Turn ${props.turn.number} performance, ${duration()}`}
      title="Turn performance"
      data-testid="turn-performance-link"
    >
      <Duration>{duration()}</Duration>
    </A>
  );
}

// ContextDurationLink ends an archived context's row the same way: its time
// links to the context's Performance view.
export function ContextDurationLink(props: { section: HistorySection }) {
  const location = useLocation();
  const duration = () => formatTotalDuration(props.section.durationMs);
  return (
    <A
      href={taskContextPath(taskPathFromPathname(location.pathname), props.section.id)}
      class={styles.trigger}
      aria-label={`Context ${props.section.id} performance, ${duration()}`}
      title="Context performance"
      data-testid="context-performance-link"
    >
      <Duration>{duration()}</Duration>
    </A>
  );
}

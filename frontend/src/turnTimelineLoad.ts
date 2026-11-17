// Loads one turn's timeline and refreshes it after task-stream changes while the shown timeline can still change.

import { batch, createEffect, createMemo, createSignal, on, onCleanup, untrack, type Accessor } from "solid-js";
import { createStore, reconcile } from "solid-js/store";

import type { HistoryTurnTimeline } from "@sdk/types.gen";
import type { TaskHistory } from "./taskEventTimeline";

// liveRefreshIntervalMs spaces live refreshes: each one rescans the turn on the
// server, and a streaming turn changes the task stream many times per second.
export const liveRefreshIntervalMs = 1_000;

// maxRetryDelayMs caps the wait between failed refreshes.
export const maxRetryDelayMs = 30_000;

// retryDelayMs is the wait after the nth consecutive failed refresh: one
// refresh interval, doubling with each failure up to maxRetryDelayMs.
export function retryDelayMs(failures: number): number {
  return Math.min(maxRetryDelayMs, liveRefreshIntervalMs * 2 ** Math.max(0, failures - 1));
}

export type TurnTimelineState = "loading" | "error" | "ready";

interface Loaded {
  state: TurnTimelineState;
  message: string;
  // timeline is the shown timeline: null while loading or after a failed
  // load, kept after a failed refresh.
  timeline: HistoryTurnTimeline | null;
}

// changing reports a timeline that can still change: the turn had not ended
// when it was read, or a subagent or command the turn started still runs.
function changing(timeline: HistoryTurnTimeline): boolean {
  return (
    !timeline.endedAt ||
    (timeline.spans ?? []).some(
      (span) => (span.kind === "subagent" || span.kind === "background") && span.status === "running",
    )
  );
}

// createTurnTimelineLoad fetches turn number's timeline once per turn and
// history generation. While the turn runs, each task-stream checkpoint asks
// for a refresh; afterwards, only a manifest change does, and only while the
// shown timeline can still change. A timeline read before the turn completed
// is stale once it has, so its arrival asks for a refresh, and so does a
// failed refresh of a timeline that can still change. Refreshes keep the
// shown timeline, merge the new one into it so unchanged spans and calls keep
// their identity, and run one at a time, at most once per
// liveRefreshIntervalMs. Nothing is requested while nothing changes. A failed
// refresh keeps the shown timeline, reports the error state, and backs off
// per retryDelayMs until a refresh succeeds or the turn or generation
// changes; retry refreshes at once.
export function createTurnTimelineLoad(
  history: Pick<TaskHistory, "manifest" | "through" | "readTurn">,
  turnNumber: Accessor<number>,
  turnRunning: Accessor<boolean>,
) {
  // Memos keep a new turn object with the same number or state, such as a
  // repriced turn or the running turn completing, from reloading the timeline.
  const number = createMemo(turnNumber);
  const running = createMemo(turnRunning);
  const [loaded, setLoaded] = createStore<Loaded>({ state: "loading", message: "", timeline: null });
  const [attempt, setAttempt] = createSignal(0);
  // A manifest change within one generation must not reload the timeline.
  const generation = createMemo(() => history.manifest()?.bootstrap.generation ?? "");

  let refresh: AbortController | null = null;
  let timer: ReturnType<typeof setTimeout> | null = null;
  let dirty = false;
  // nextStart is the earliest time the next automatic refresh may start.
  let nextStart = Number.NEGATIVE_INFINITY;
  let failures = 0;
  // stale reports a timeline read while the turn ran that has since completed.
  const stale = (timeline: HistoryTurnTimeline) => !timeline.endedAt && !untrack(running);
  const stopRefresh = () => {
    refresh?.abort();
    refresh = null;
    if (timer !== null) clearTimeout(timer);
    timer = null;
    dirty = false;
    failures = 0;
    nextStart = Number.NEGATIVE_INFINITY;
  };
  const run = () => {
    dirty = false;
    nextStart = Date.now() + liveRefreshIntervalMs;
    const controller = new AbortController();
    refresh = controller;
    history
      .readTurn(untrack(number), controller.signal)
      .then(
        (timeline) => {
          if (controller.signal.aborted) return;
          failures = 0;
          batch(() => {
            setLoaded("timeline", reconcile(timeline));
            setLoaded({ state: "ready", message: "" });
          });
          if (stale(timeline)) dirty = true;
        },
        // The change that asked for the refresh may be the last, so a timeline
        // that can still change retries.
        (error: unknown) => {
          if (controller.signal.aborted) return;
          failures++;
          nextStart = Date.now() + retryDelayMs(failures);
          setLoaded({ state: "error", message: error instanceof Error ? error.message : String(error) });
          const shown = loaded.timeline;
          if (shown !== null && changing(shown)) dirty = true;
        },
      )
      .finally(() => {
        if (refresh !== controller) return;
        refresh = null;
        if (dirty) schedule();
      });
  };
  const schedule = () => {
    dirty = true;
    if (refresh !== null || timer !== null) return;
    const wait = nextStart - Date.now();
    if (wait <= 0) run();
    else
      timer = setTimeout(() => {
        timer = null;
        run();
      }, wait);
  };

  createEffect(
    on([number, generation, attempt], ([requested, current]) => {
      stopRefresh();
      if (!current) return;
      const controller = new AbortController();
      onCleanup(() => {
        controller.abort();
        stopRefresh();
      });
      setLoaded({ state: "loading", message: "", timeline: null });
      history.readTurn(requested, controller.signal).then(
        (timeline) => {
          if (controller.signal.aborted) return;
          setLoaded({ state: "ready", message: "", timeline });
          if (stale(timeline)) schedule();
        },
        (error: unknown) => {
          if (!controller.signal.aborted)
            setLoaded({
              state: "error",
              message: error instanceof Error ? error.message : String(error),
              timeline: null,
            });
        },
      );
    }),
  );

  // A running turn changes with every record; otherwise only its completion
  // or the end of work it started changes it, and both change the manifest.
  const pulse = () => (running() ? history.through() : history.manifest());
  createEffect(
    on(
      [pulse, running],
      () => {
        const shown = untrack(() => loaded.timeline);
        if (shown !== null && changing(shown)) schedule();
      },
      { defer: true },
    ),
  );

  return {
    state: () => loaded.state,
    message: () => loaded.message,
    timeline: () => loaded.timeline,
    // retry reloads after a failed load, and refreshes at once after a
    // failed refresh.
    retry: () => {
      if (loaded.timeline === null) {
        setAttempt((n) => n + 1);
        return;
      }
      if (timer !== null) clearTimeout(timer);
      timer = null;
      if (refresh === null) run();
      else dirty = true;
    },
  };
}

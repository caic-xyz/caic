// TurnStrip plots turn durations on one scale, the running turn's as it grows; each bar links to its turn view, and the strip is one Tab stop.

import { createEffect, createMemo, createSignal, For, on, onCleanup, onMount, Show, type Accessor } from "solid-js";

import type { HistoryRunningTurn, HistoryTurn } from "@sdk/types.gen";
import { createLiveDuration } from "../liveDuration";
import { formatExactSeconds, formatTotalDuration } from "../timing";
import styles from "./TurnStrip.module.css";

// Below the sparse bar's minimum width plus its gap, bars drop the gap and
// shrink to 1px so dense tasks still fit.
const sparseBarPx = 6;

interface StripView {
  dense: boolean;
  // Visible turn numbers when the strip overflows, else null.
  range: { first: number; last: number } | null;
  clipStart: boolean;
  clipEnd: boolean;
}

// createRunningElapsed is the one clock for a running turn's figures in a
// view: its strip bar, summary, details, and timeline axis read the same value.
export function createRunningElapsed(running: Accessor<HistoryRunningTurn | null>): Accessor<number | null> {
  return createLiveDuration(
    () => {
      const turn = running();
      return turn ? Date.parse(turn.startedAt) : null;
    },
    () => null,
    () => running() !== null,
  );
}

// Anchors marked `link` navigate through the router. The current turn comes from props
// because task URLs with and without their slug name the same turn.
export default function TurnStrip(props: {
  turns: readonly HistoryTurn[];
  // The running turn follows the completed turns; its bar is runningMs long,
  // on the clock of the view that shows the turn's other running figures.
  running: HistoryRunningTurn | null;
  runningMs: number | null;
  // scaleMs is the least top of the scale, so strips of a part of the task
  // share the task's scale.
  scaleMs: number;
  // The turn shown beside the strip, or null in the overview.
  current: number | null;
  href: (turn: number) => string;
}) {
  const elapsed = () => (props.running ? props.runningMs : null);
  const longest = createMemo(() =>
    props.turns.reduce((max, turn) => Math.max(max, turn.durationMs), Math.max(props.scaleMs, elapsed() ?? 0)),
  );
  const count = () => props.turns.length + (props.running ? 1 : 0);
  // Keyboard positions count bars from 1; a bar's turn number can differ in a
  // strip of a part of the task.
  const numberAt = (position: number) => props.turns[position - 1]?.number ?? props.running?.number ?? 0;
  const positionOf = (turn: number) => {
    const index = props.turns.findIndex((candidate) => candidate.number === turn);
    return index >= 0 ? index + 1 : props.running?.number === turn ? props.turns.length + 1 : 0;
  };
  let strip: HTMLOListElement | undefined;
  const [view, setView] = createSignal<StripView>({ dense: false, range: null, clipStart: false, clipEnd: false });
  // The overview has no current turn; arrow keys there move the Tab stop between bars.
  const [focused, setFocused] = createSignal<number | null>(null);
  // A memo: every bar reads the Tab stop, and finding it scans the turns, so
  // focusing a bar must not rescan them once per bar.
  const tabStop = createMemo(
    () => (props.current !== null && positionOf(props.current) > 0 ? props.current : null) ?? focused() ?? numberAt(1),
  );

  const measure = () => {
    if (!strip) return;
    const width = strip.clientWidth;
    const dense = count() > 0 && width / count() < sparseBarPx;
    const left = strip.scrollLeft;
    const overflow = strip.scrollWidth - width > 1;
    let range: StripView["range"] = null;
    if (overflow) {
      const items = strip.children;
      let first = 0;
      let last = 0;
      for (let i = 0; i < items.length; i++) {
        const item = items[i] as HTMLElement;
        if (item.offsetLeft + item.offsetWidth <= left) continue;
        if (item.offsetLeft >= left + width) break;
        first ||= i + 1;
        last = i + 1;
      }
      if (first > 0) range = { first, last };
    }
    setView({
      dense,
      range,
      clipStart: overflow && left > 0,
      clipEnd: overflow && left + width < strip.scrollWidth - 1,
    });
  };
  onMount(() => {
    if (!strip) return;
    const observer = new ResizeObserver(measure);
    observer.observe(strip);
    onCleanup(() => observer.disconnect());
  });
  // Scroll the strip alone: scrollIntoView would also move the conversation.
  const reveal = (turn: number) => {
    const item = strip?.querySelector<HTMLElement>(`[data-turn="${turn}"]`);
    if (!strip || !item) return;
    const start = item.offsetLeft;
    const end = start + item.offsetWidth;
    if (start < strip.scrollLeft || end > strip.scrollLeft + strip.clientWidth)
      strip.scrollLeft = start - (strip.clientWidth - item.offsetWidth) / 2;
  };
  // Measure first: the dense layout changes where the current bar sits.
  createEffect(
    on([count, () => props.current], ([, current]) => {
      measure();
      if (current !== null) reveal(current);
      measure();
    }),
  );
  // ←/→ in the turn view change the current turn; keep focus on its bar.
  createEffect(
    on(
      () => props.current,
      (current) => {
        if (current === null || !strip?.contains(document.activeElement)) return;
        strip.querySelector<HTMLElement>(`[data-turn="${current}"] a`)?.focus();
      },
      { defer: true },
    ),
  );
  const onKeyDown = (event: KeyboardEvent) => {
    if (props.current !== null || event.altKey || event.ctrlKey || event.metaKey || event.shiftKey) return;
    const from = positionOf(tabStop());
    const position =
      event.key === "ArrowLeft"
        ? from - 1
        : event.key === "ArrowRight"
          ? from + 1
          : event.key === "Home"
            ? 1
            : event.key === "End"
              ? count()
              : 0;
    if (position < 1 || position > count()) return;
    event.preventDefault();
    const target = numberAt(position);
    setFocused(target);
    reveal(target);
    strip?.querySelector<HTMLElement>(`[data-turn="${target}"] a`)?.focus();
  };
  const bar = (number: number, label: () => string, title: () => string, share: () => number, running: boolean) => (
    <li class={styles.item} data-turn={number}>
      <a
        link
        href={props.href(number)}
        class={styles.bar}
        classList={{ [styles.current]: number === props.current, [styles.running]: running }}
        tabIndex={number === tabStop() ? 0 : -1}
        aria-current={number === props.current ? "page" : undefined}
        aria-label={label()}
        title={title()}
        onFocus={() => setFocused(number)}
      >
        <span class={styles.fill} style={{ "--turn-share": share() }} />
      </a>
    </li>
  );

  return (
    <Show when={count() > 0} fallback={<p class={styles.empty}>No completed turns yet.</p>}>
      <div class={styles.chart}>
        <div class={styles.scale} aria-hidden="true">
          <span>{formatTotalDuration(longest())}</span>
          <span>0</span>
        </div>
        <ol
          ref={strip}
          class={styles.strip}
          classList={{
            [styles.dense]: view().dense,
            [styles.clipStart]: view().clipStart,
            [styles.clipEnd]: view().clipEnd,
          }}
          aria-label="Turn durations"
          data-testid="turn-strip"
          onScroll={measure}
          onKeyDown={onKeyDown}
        >
          <For each={props.turns}>
            {(turn) =>
              bar(
                turn.number,
                () => `Turn ${turn.number}, ${formatExactSeconds(turn.durationMs)}`,
                () => `Turn ${turn.number} · ${formatTotalDuration(turn.durationMs)}`,
                () => (longest() > 0 ? turn.durationMs / longest() : 0),
                false,
              )
            }
          </For>
          <Show when={props.running} keyed>
            {(running) =>
              bar(
                running.number,
                () => `Turn ${running.number}, running, ${formatExactSeconds(elapsed() ?? 0)} so far`,
                () => `Turn ${running.number} · running · ${formatTotalDuration(elapsed() ?? 0)}`,
                () => (longest() > 0 ? (elapsed() ?? 0) / longest() : 0),
                true,
              )
            }
          </Show>
        </ol>
        <Show when={view().range}>
          {(range) => (
            <p class={styles.range} data-testid="turn-strip-range">
              Turns {numberAt(range().first)}–{numberAt(range().last)} of{" "}
              {numberAt(1) === 1 ? count() : `${numberAt(1)}–${numberAt(count())}`}
            </p>
          )}
        </Show>
      </div>
    </Show>
  );
}

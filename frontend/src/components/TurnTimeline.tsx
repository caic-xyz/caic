// TurnTimeline draws one turn's model, tool, subagent, and background spans as lanes on one axis, linked to the transcript; a running turn's lanes grow in place.

import { createEffect, createMemo, createSignal, For, on, onCleanup, Show, type JSX } from "solid-js";
import { createStore, reconcile } from "solid-js/store";
import { useNavigate } from "@solidjs/router";

import type { HistoryTurnSpan, HistoryTurnSpanKind, HistoryTurnTimeline } from "@sdk/types.gen";
import { isChildEvent, toolIdentity } from "../grouping";
import { formatExactSeconds, formatTotalDuration } from "../timing";
import { useTranscriptLinks } from "../TaskTimelineContext";
import { nativeLinkKey, toolLinkKey, type TranscriptTarget } from "../transcriptLinks";
import { boundTooltipText } from "../tooltipText";
import Table from "./Table";
import styles from "./TurnTimeline.module.css";

type LaneID = "model" | "tools" | "subagents" | "childTools" | "background";

const laneNames: Record<LaneID, string> = {
  model: "Model (inferred)",
  tools: "Tools",
  subagents: "Subagents",
  childTools: "Subagent tools",
  background: "Background",
};
const laneOrder: readonly LaneID[] = ["model", "tools", "subagents", "childTools", "background"];
const omittedNouns: Record<HistoryTurnSpanKind, [string, string]> = {
  model: ["model gap", "model gaps"],
  tool: ["tool call", "tool calls"],
  subagent: ["subagent", "subagents"],
  background: ["background command", "background commands"],
};

// Spans narrower than this share of the axis still draw at a visible width, so
// packing keeps them this far apart to avoid overlapping marks.
const minShare = 0.004;

interface TimelineSpan {
  // key identifies the span across refreshes of a running turn.
  key: string;
  span: HistoryTurnSpan;
  lane: LaneID;
  // Display end clipped to the turn; the span continues past it when beyond.
  end: number;
  beyond: boolean;
  name: string;
  timing: string;
  linkKeys: readonly string[];
}

interface Lane {
  key: LaneID;
  summary: string;
  rows: TimelineSpan[][];
  spans: TimelineSpan[];
}

function laneOf(span: HistoryTurnSpan): LaneID {
  switch (span.kind) {
    case "model":
      return "model";
    case "subagent":
      return "subagents";
    case "background":
      return "background";
    case "tool":
      return span.activityID || isChildEvent(span) ? "childTools" : "tools";
  }
}

// provisional reports the model span of a running turn that grows until the
// next tool call or the turn's end.
function provisional(span: HistoryTurnSpan): boolean {
  return span.kind === "model" && span.endMs === undefined;
}

// endText states where a span ends relative to the turn: within it, after it,
// or not at all yet. A running turn's axis ends now, so no span ends after it.
function endText(span: HistoryTurnSpan, duration: number, running: boolean): string {
  if (span.endMs === undefined) {
    if (provisional(span)) return `so far ${formatExactSeconds(duration - span.startMs)}, provisional`;
    if (span.kind === "tool") return running ? "still running" : "open at turn end";
    return span.status === "running" ? "still running" : "end unknown";
  }
  if (span.endMs > duration && !running) return `ends ${formatExactSeconds(span.endMs - duration)} after the turn`;
  return `lasts ${formatExactSeconds(span.endMs - span.startMs)}`;
}

// outcome names a failed or interrupted span; endText covers the others.
function outcome(span: HistoryTurnSpan): string {
  return span.status === "failed" || span.status === "interrupted" ? `, ${span.status}` : "";
}

function spanName(span: HistoryTurnSpan): string {
  if (span.kind === "model") return provisional(span) ? "Model time (inferred, provisional)" : "Model time (inferred)";
  const label = span.label || (span.kind === "background" ? span.commandID : span.activityID) || span.kind;
  return span.detail ? `${label} ${span.detail}` : label;
}

// spanTarget is the transcript item that a span stands for.
function spanTarget(span: HistoryTurnSpan, turnStart: number): TranscriptTarget {
  const at = turnStart + span.startMs;
  switch (span.kind) {
    case "model":
      // Model output during the gap finishes by its end, so the row that holds
      // the gap's last moment shows what the model produced.
      return { toolKey: null, nativeID: null, ownerAgentID: null, at: turnStart + (span.endMs ?? span.startMs) - 1 };
    case "subagent":
      return { toolKey: null, nativeID: span.activityID ?? null, ownerAgentID: null, at };
    case "tool":
    case "background":
      return {
        toolKey: span.toolUseID ? toolIdentity(span.source, span.toolUseID) : null,
        nativeID: null,
        ownerAgentID: span.activityID ?? null,
        at,
      };
  }
}

// spanKey identifies a span across refreshes: a model gap by its start, which
// a later tool call does not move, and the others by their transcript item.
function spanKey(span: HistoryTurnSpan): string {
  switch (span.kind) {
    case "model":
      return `model:${span.startMs}`;
    case "subagent":
      return `subagent:${span.activityID ?? span.startMs}`;
    case "background":
      return `background:${span.commandID ?? span.startMs}`;
    case "tool":
      return `tool:${span.toolUseID ? toolIdentity(span.source, span.toolUseID) : span.startMs}`;
  }
}

function linkKeys(span: HistoryTurnSpan): string[] {
  if (span.kind === "subagent") return span.activityID ? [nativeLinkKey(span.activityID)] : [];
  if (span.toolUseID) return [toolLinkKey(toolIdentity(span.source, span.toolUseID))];
  return [];
}

// pack places each span in the first row whose last span ended before it starts.
function pack(spans: readonly TimelineSpan[], duration: number): TimelineSpan[][] {
  const gap = duration * minShare;
  const rows: { end: number; spans: TimelineSpan[] }[] = [];
  for (const item of spans) {
    const end = Math.max(item.end, item.span.startMs + gap);
    const row = rows.find((candidate) => candidate.end <= item.span.startMs);
    if (row) {
      row.spans.push(item);
      row.end = end;
    } else rows.push({ end, spans: [item] });
  }
  return rows.map((row) => row.spans);
}

// tickStep is the round step that divides the axis into at most five parts.
export function tickStep(duration: number): number {
  const rough = duration / 5;
  for (const step of [
    1, 2, 5, 10, 20, 50, 100, 200, 500, 1_000, 2_000, 5_000, 10_000, 15_000, 30_000, 60_000, 120_000, 300_000, 600_000,
    900_000, 1_800_000, 3_600_000,
  ]) {
    if (step >= rough) return step;
  }
  return Math.ceil(rough / 3_600_000) * 3_600_000;
}

// tickLabel names a gridline; below a one-second step, rounding to whole
// seconds would give neighbouring gridlines the same label.
function tickLabel(at: number, step: number): string {
  if (step >= 1_000 || at === 0) return formatTotalDuration(at);
  return `${(at / 1_000).toFixed(step < 100 ? 2 : 1)}s`;
}

function timelineLanes(timeline: HistoryTurnTimeline, duration: number, running: boolean): Lane[] {
  const byLane = new Map<LaneID, TimelineSpan[]>();
  for (const span of timeline.spans ?? []) {
    const lane = laneOf(span);
    const end = Math.min(span.endMs ?? duration, duration);
    const item: TimelineSpan = {
      key: spanKey(span),
      span: { ...span },
      lane,
      end: Math.max(end, span.startMs),
      beyond: span.endMs === undefined || span.endMs > duration,
      name: spanName(span),
      timing: `${span.startMs > 0 ? `at ${formatExactSeconds(span.startMs)}` : "from the turn start"}, ${endText(span, duration, running)}`,
      linkKeys: linkKeys(span),
    };
    const list = byLane.get(lane);
    if (list) list.push(item);
    else byLane.set(lane, [item]);
  }
  const lanes: Lane[] = [];
  for (const lane of laneOrder) {
    const spans = byLane.get(lane) ?? [];
    // The model lane always shows, even when tools cover the whole turn.
    if (spans.length === 0 && lane !== "model") continue;
    const count = spans.length;
    const summary =
      lane === "model"
        ? formatTotalDuration(timeline.modelMs)
        : lane === "tools"
          ? `${count} ${count === 1 ? "call" : "calls"} · ${formatTotalDuration(duration - timeline.modelMs)}`
          : lane === "childTools"
            ? `${count} ${count === 1 ? "call" : "calls"}`
            : String(count);
    lanes.push({ key: lane, summary, rows: pack(spans, duration), spans });
  }
  return lanes;
}

export default function TurnTimeline(props: {
  timeline: HistoryTurnTimeline;
  // runningMs is a running turn's elapsed time on the view's clock; between
  // refreshes it extends the axis, so the axis end matches the view's other
  // running figures. Null for a completed turn.
  runningMs: number | null;
  conversationHref: string;
  // children draw more lanes on the same axis, below the spans.
  children: JSX.Element;
}) {
  const links = useTranscriptLinks();
  const navigate = useNavigate();
  const start = () => Date.parse(props.timeline.startedAt);
  const running = () => !props.timeline.endedAt;
  const duration = () => Math.max(0, props.timeline.durationMs, running() ? (props.runningMs ?? 0) : 0);
  // Refreshes merge into the lanes by span key, so a growing turn keeps each
  // span's button, its focus, and the page's scroll position.
  const [view, setView] = createStore<{ lanes: Lane[] }>({ lanes: [] });
  createEffect(() =>
    setView("lanes", reconcile(timelineLanes(props.timeline, duration(), running()), { key: "key", merge: false })),
  );
  const lanes = () => view.lanes;
  const step = () => tickStep(duration());
  // The end label and room for one tick label own the last stretch of the
  // axis, as a share of its width.
  const [reserve, setReserve] = createSignal(0.15);
  let endLabel: HTMLSpanElement | undefined;
  const observeAxis = (axis: HTMLDivElement) => {
    const observer = new ResizeObserver(() => {
      const width = axis.clientWidth;
      if (width <= 0 || !endLabel) return;
      const rem = parseFloat(getComputedStyle(document.documentElement).fontSize);
      setReserve(Math.min(0.5, (endLabel.offsetWidth + 3.5 * rem) / width));
    });
    observer.observe(axis);
    onCleanup(() => observer.disconnect());
  };
  const ticks = createMemo(() => {
    const total = duration();
    if (total <= 0) return [0];
    const values: number[] = [];
    for (let at = 0; at <= total * (1 - reserve()); at += step()) values.push(at);
    return values;
  });

  // The plot is one Tab stop: arrows move between spans, Enter or Space reveals
  // one. The stop stays on its span while the turn grows; another turn, or a
  // span that no longer exists, moves it to the first span.
  const [active, setActive] = createSignal("");
  let plot: HTMLDivElement | undefined;
  const tabStop = createMemo(() =>
    lanes().some((lane) => lane.spans.some((item) => item.key === active()))
      ? active()
      : (lanes()[0]?.spans[0]?.key ?? ""),
  );
  createEffect(
    on(
      () => props.timeline.number,
      () => setActive(""),
      { defer: true },
    ),
  );
  const focusSpan = (key: string) => {
    setActive(key);
    plot?.querySelector<HTMLElement>(`[data-span="${CSS.escape(key)}"]`)?.focus();
  };
  const onKeyDown = (event: KeyboardEvent) => {
    if (event.altKey || event.ctrlKey || event.metaKey || event.shiftKey) return;
    const all = lanes();
    const laneIndex = all.findIndex((candidate) => candidate.spans.some((item) => item.key === tabStop()));
    const lane = all[laneIndex];
    if (!lane) return;
    const position = lane.spans.findIndex((item) => item.key === tabStop());
    const current = lane.spans[position];
    let target: TimelineSpan | undefined;
    switch (event.key) {
      case "ArrowLeft":
        target = lane.spans[position - 1];
        break;
      case "ArrowRight":
        target = lane.spans[position + 1];
        break;
      case "Home":
        target = lane.spans[0];
        break;
      case "End":
        target = lane.spans.at(-1);
        break;
      case "ArrowUp":
      case "ArrowDown": {
        const direction = event.key === "ArrowUp" ? -1 : 1;
        for (let i = laneIndex + direction; i >= 0 && i < all.length && !target; i += direction) {
          const spans = all[i]?.spans ?? [];
          const at = current?.span.startMs ?? 0;
          target = spans.reduce<TimelineSpan | undefined>(
            (best, item) =>
              !best || Math.abs(item.span.startMs - at) < Math.abs(best.span.startMs - at) ? item : best,
            undefined,
          );
        }
        break;
      }
      default:
        return;
    }
    // Arrow keys stay inside the plot even at its edges: the turn view's ←/→
    // turn navigation ignores handled keys.
    event.preventDefault();
    if (target) focusSpan(target.key);
  };
  const activate = (item: TimelineSpan) => {
    setActive(item.key);
    const revealer = links.revealer();
    if (!revealer) return;
    const target = spanTarget(item.span, start());
    if (revealer.visible()) revealer.reveal(target, false);
    else {
      // The conversation is behind this view: show it, then reveal there.
      revealer.reveal(target, true);
      navigate(props.conversationHref);
    }
  };
  const omitted = () => props.timeline.omitted ?? [];

  return (
    <figure class={styles.timeline} data-testid="turn-timeline">
      <figcaption class={styles.caption}>
        <span class={styles.title}>Timeline</span>
        <span class={styles.note}>
          Model time is inferred: API calls report only their end.
          {running() ? " The turn is running: its latest model time is provisional." : ""}
        </span>
      </figcaption>
      <div
        ref={plot}
        class={styles.plot}
        role="group"
        aria-label={`Turn ${props.timeline.number} timeline, axis ends ${running() ? "now, " : ""}at ${formatTotalDuration(duration())}; arrow keys move between spans, Enter shows one in the conversation`}
        onKeyDown={onKeyDown}
        data-testid="turn-timeline-plot"
      >
        <div ref={observeAxis} class={styles.axis} aria-hidden="true">
          <For each={ticks()}>
            {(at) => (
              <span class={styles.tick} style={{ "--at": duration() > 0 ? at / duration() : 0 }}>
                {tickLabel(at, step())}
              </span>
            )}
          </For>
          <span ref={endLabel} class={styles.end}>
            {running() ? "now" : "end"} {formatTotalDuration(duration())}
          </span>
        </div>
        <For each={lanes()}>
          {(lane) => (
            <div class={styles.lane} data-lane={lane.key} data-testid="turn-timeline-lane">
              <div class={styles.laneLabel}>
                {laneNames[lane.key]} <span class={styles.laneSummary}>{lane.summary}</span>
              </div>
              <div
                class={styles.track}
                classList={{ [styles.inferred]: lane.key === "model" }}
                style={{ "--tick-share": duration() > 0 ? step() / duration() : 1 }}
              >
                <For each={lane.rows}>
                  {(row) => (
                    <div class={styles.row}>
                      <For each={row}>
                        {(item) => (
                          <button
                            type="button"
                            class={styles.span}
                            classList={{
                              [styles.failed]: item.span.status === "failed",
                              [styles.provisional]: provisional(item.span),
                              [styles.linked]: item.linkKeys.includes(links.hovered() ?? ""),
                            }}
                            style={{
                              "--start": duration() > 0 ? item.span.startMs / duration() : 0,
                              "--length": duration() > 0 ? (item.end - item.span.startMs) / duration() : 1,
                            }}
                            tabIndex={item.key === tabStop() ? 0 : -1}
                            aria-label={`${laneNames[lane.key]}: ${item.name}, ${item.timing}${outcome(item.span)}`}
                            title={boundTooltipText(`${item.name} · ${item.timing}`)}
                            data-span={item.key}
                            data-testid="turn-timeline-span"
                            onFocus={() => setActive(item.key)}
                            onClick={() => activate(item)}
                          >
                            <Show when={item.beyond}>
                              <span class={styles.arrow} data-testid="turn-timeline-beyond" />
                            </Show>
                          </button>
                        )}
                      </For>
                    </div>
                  )}
                </For>
              </div>
            </div>
          )}
        </For>
      </div>
      {props.children}
      <Show when={omitted().length > 0}>
        <p class={styles.omitted} data-testid="turn-timeline-omitted">
          Not shown:{" "}
          {omitted()
            .map((o) => {
              const [one, many] = omittedNouns[o.kind];
              return `${o.count} shorter ${o.count === 1 ? one : many} (${formatTotalDuration(o.durationMs)} in total)`;
            })
            .join(", ")}
          .
        </p>
      </Show>
      <details class={styles.spans}>
        <summary>Timeline spans ({props.timeline.spans?.length ?? 0})</summary>
        <Table label="Timeline spans" dense class={styles.spanTable}>
          <thead>
            <tr>
              <th scope="col">Lane</th>
              <th scope="col" data-align="text">
                Item
              </th>
              <th scope="col">Start</th>
              <th scope="col">End</th>
            </tr>
          </thead>
          <tbody>
            <For each={lanes().flatMap((lane) => lane.spans)}>
              {(item) => (
                <tr>
                  <td>{laneNames[item.lane]}</td>
                  <td class={styles.item} data-align="text">
                    <button type="button" class={styles.itemLink} onClick={() => activate(item)}>
                      {item.name}
                    </button>
                  </td>
                  <td>{formatExactSeconds(item.span.startMs)}</td>
                  <td class={styles.endCell}>
                    {item.span.endMs === undefined
                      ? endText(item.span, duration(), running())
                      : `${formatExactSeconds(item.span.endMs)}${item.span.endMs > duration() && !running() ? " (after the turn)" : ""}`}
                    {outcome(item.span)}
                  </td>
                </tr>
              )}
            </For>
          </tbody>
        </Table>
      </details>
    </figure>
  );
}

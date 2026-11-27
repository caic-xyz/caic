// TurnDetail shows one turn's time apart from its waits for the user's answers, per-call tokens, tool time, resources, and cost beside the task's other turns, live while it runs, with ←/→ turn navigation.

import { createMemo, For, Show, type JSX } from "solid-js";
import { A } from "@solidjs/router";
import PreviousIcon from "@material-symbols/svg-400/outlined/chevron_left.svg?solid";
import NextIcon from "@material-symbols/svg-400/outlined/chevron_right.svg?solid";

import type { HistoryRunningTurn, HistoryTurn, HistoryTurnTimeline } from "@sdk/types.gen";
import { formatChangeStat, formatMoney, formatTime, formatTokens, formatUSD } from "../formatting";
import { durationTickStep, formatExactSeconds, formatTimingDuration, formatTotalDuration } from "../timing";
import { useTaskTimeline } from "../TaskTimelineContext";
import { createTurnTimelineLoad } from "../turnTimelineLoad";
import { runningAskMs, runningTurnTimeMs, turnTimeMs } from "../turnTime";
import ActionButton from "./ActionButton";
import Button from "./Button";
import { PerformanceBody, PerformanceSection, useArrowStepping } from "./PerformanceDetail";
import Table from "./Table";
import TurnCalls from "./TurnCalls";
import TurnResources from "./TurnResources";
import TurnStrip, { createRunningElapsed } from "./TurnStrip";
import TurnTimeline from "./TurnTimeline";
import TurnTools from "./TurnTools";
import styles from "./TurnDetail.module.css";

// ViewedTurn is a completed turn, or the running turn that has no result yet.
export type ViewedTurn = { state: "completed"; turn: HistoryTurn } | { state: "running"; turn: HistoryRunningTurn };

function exactDuration(ms: number): string {
  return ms > 0 ? formatExactSeconds(ms) : "—";
}

// timelineTokens sums the calls and unattributed usage a timeline reports.
function timelineTokens(timeline: HistoryTurnTimeline): number {
  const sum = (t: {
    inputTokens: number;
    cacheCreationInputTokens: number;
    cacheReadInputTokens: number;
    outputTokens: number;
  }) => t.inputTokens + t.cacheCreationInputTokens + t.cacheReadInputTokens + t.outputTokens;
  return (
    (timeline.calls ?? []).reduce((total, call) => total + sum(call), 0) +
    (timeline.omittedCalls ? sum(timeline.omittedCalls) : 0) +
    (timeline.unattributed ? sum(timeline.unattributed) : 0)
  );
}

export default function TurnDetail(props: {
  viewed: ViewedTurn;
  turns: readonly HistoryTurn[];
  running: HistoryRunningTurn | null;
  href: (turn: number) => string;
  contextHref: (section: string) => string;
  // conversationHref shows the conversation when this view hides it.
  conversationHref: string;
  // costUpdating marks the cost figures while their estimates refresh.
  costUpdating: boolean;
}) {
  const number = () => props.viewed.turn.number;
  const completed = () => (props.viewed.state === "completed" ? props.viewed.turn : null);
  const previous = () => (number() > 1 ? number() - 1 : null);
  const next = () => {
    if (number() < props.turns.length) return number() + 1;
    return props.running && props.running.number > number() ? props.running.number : null;
  };
  const go = useArrowStepping(previous, next, (target) => props.href(target));

  const history = useTaskTimeline();
  const load = createTurnTimelineLoad(history, number, () => props.viewed.state === "running");
  // A failed refresh keeps the shown timeline under its error.
  const timeline = load.timeline;
  // One clock for every running figure: the strip bar, the summary, the
  // details, and the timeline axis. It never trails the axis end the server
  // last derived, so a refresh cannot show a later time than the summary.
  const clock = createRunningElapsed(() => props.running);
  const runningMs = () => {
    if (!props.running) return null;
    const ready = props.viewed.state === "running" ? timeline() : null;
    return Math.max(clock() ?? 0, ready?.durationMs ?? 0);
  };
  const viewedRunningMs = () => (props.viewed.state === "running" ? runningMs() : null);
  // The running turn's own time and its waits for answers, within its wall time.
  const runningTurn = () => (props.viewed.state === "running" ? props.running : null);
  const runningTurnMs = () => {
    const turn = runningTurn();
    return turn ? runningTurnTimeMs(turn, viewedRunningMs() ?? 0) : 0;
  };
  const askMs = () => {
    const turn = completed();
    if (turn) return turn.askMs ?? 0;
    const running = runningTurn();
    return running ? runningAskMs(running, viewedRunningMs() ?? 0) : 0;
  };
  // axisMs is the timeline's axis length, as TurnTimeline draws it.
  const axisMs = (ready: HistoryTurnTimeline) => viewedRunningMs() ?? ready.durationMs;
  const runningSummary = createMemo(() => {
    const parts: { value: string; label: string; cost: boolean }[] = [
      { value: runningTurn()?.askingSince ? "Awaiting you" : "Running", label: "", cost: false },
      { value: formatTotalDuration(runningTurnMs()), label: "so far", cost: false },
    ];
    const ready = timeline();
    if (ready) parts.push({ value: formatTokens(timelineTokens(ready)), label: "tokens", cost: false });
    return parts;
  });

  const started = () => (Date.parse(props.viewed.turn.startedAt) > 0 ? formatTime(props.viewed.turn.startedAt) : "—");
  const summary = () => {
    const turn = completed();
    if (!turn) return runningSummary();
    const usage = turn.usage;
    const tokens = usage.inputTokens + usage.cacheCreationInputTokens + usage.cacheReadInputTokens + usage.outputTokens;
    const parts: { value: string; label: string; cost: boolean }[] = [
      { value: formatTotalDuration(turnTimeMs(turn)), label: "turn", cost: false },
    ];
    if (turn.durationAPIMs > 0)
      parts.push({ value: formatTimingDuration(turn.durationAPIMs), label: "API", cost: false });
    if (turn.cost)
      parts.push({
        value: `${turn.cost.source === "estimated" ? "~" : ""}${formatUSD(turn.cost.usd)}`,
        label: "",
        cost: true,
      });
    parts.push({ value: formatTokens(tokens), label: "tokens", cost: false });
    if (turn.changeStat) parts.push({ value: formatChangeStat(turn.changeStat, false), label: "", cost: false });
    return parts;
  };
  const row = (label: string, value: JSX.Element, updating: boolean) => (
    <tr>
      <th scope="row">{label}</th>
      <td class={`${styles.value} updating-value`} aria-busy={updating ? "true" : undefined}>
        {value}
      </td>
    </tr>
  );
  const contextRow = () => {
    const section = props.viewed.turn.sectionID;
    return row("Context", <A href={props.contextHref(section)}>Context {section}</A>, false);
  };
  const completedRows = (turn: HistoryTurn) => {
    const usage = turn.usage;
    return (
      <>
        {row("Model", turn.model || "Unknown", false)}
        {turn.effort ? row("Effort", turn.effort, false) : null}
        {row("Started", started(), false)}
        {row("Turn time", exactDuration(turnTimeMs(turn)), false)}
        {(turn.askMs ?? 0) > 0 ? row("Awaiting your answers", exactDuration(turn.askMs ?? 0), false) : null}
        {row("API time", exactDuration(turn.durationAPIMs), false)}
        {row("New input tokens", usage.inputTokens.toLocaleString(), false)}
        {row("Cache write tokens", usage.cacheCreationInputTokens.toLocaleString(), false)}
        {row("Cache read tokens", usage.cacheReadInputTokens.toLocaleString(), false)}
        {row("Output tokens", usage.outputTokens.toLocaleString(), false)}
        {row(
          "Thinking tokens",
          (usage.reasoningOutputTokens ?? 0) > 0 ? (usage.reasoningOutputTokens ?? 0).toLocaleString() : "—",
          false,
        )}
        {row("Context at end", turn.contextTokens ? `${turn.contextTokens.toLocaleString()} tokens` : "Unknown", false)}
        {row("Cost", turn.cost ? `$${formatMoney(turn.cost.usd, 4)}` : "Unavailable", props.costUpdating)}
        {turn.cost
          ? row(
              "Cost source",
              turn.cost.source === "estimated" ? "Estimated from token prices" : "Reported by the harness",
              false,
            )
          : null}
        {turn.changeStat ? row("Change", formatChangeStat(turn.changeStat, false), false) : null}
        {contextRow()}
      </>
    );
  };

  return (
    <PerformanceBody testId="turn-detail">
      <PerformanceSection
        title="Turn durations"
        actions={
          <span class={styles.stepper}>
            <ActionButton label="Previous turn" disabled={previous() === null} onClick={() => go(previous())}>
              <PreviousIcon width={20} height={20} aria-hidden="true" />
            </ActionButton>
            <ActionButton label="Next turn" disabled={next() === null} onClick={() => go(next())}>
              <NextIcon width={20} height={20} aria-hidden="true" />
            </ActionButton>
          </span>
        }
      >
        <TurnStrip
          turns={props.turns}
          running={props.running}
          runningMs={runningMs()}
          current={number()}
          href={props.href}
        />
      </PerformanceSection>
      <PerformanceSection label={`Turn ${number()}`} testId="turn-summary">
        <p class={styles.summary} data-testid="turn-summary-line">
          <For each={summary()}>
            {(part, index) => (
              <>
                <Show when={index() > 0}>
                  <span class={styles.separator}> · </span>
                </Show>
                <span class={styles.summaryPart}>
                  <strong class="updating-value" aria-busy={part.cost && props.costUpdating ? "true" : undefined}>
                    {part.value}
                  </strong>
                  {part.label ? ` ${part.label}` : ""}
                </span>
              </>
            )}
          </For>
        </p>
        <Show when={askMs() > 0}>
          <p class={styles.wait} data-testid="turn-answer-wait">
            Waited <strong>{formatTimingDuration(askMs())}</strong> for your answers
          </p>
        </Show>
        <Show when={(completed()?.waitMs ?? 0) > 0}>
          <p class={styles.wait} data-testid="turn-user-wait">
            Then waited <strong>{formatTimingDuration(completed()?.waitMs ?? 0)}</strong> for the next prompt
          </p>
        </Show>
        <Show when={load.state() === "loading"}>
          <p class={styles.status} role="status">
            Loading timeline…
          </p>
        </Show>
        <Show when={load.state() === "error"}>
          <p class={styles.status} role="alert">
            {timeline() ? "Timeline not updated" : "Timeline unavailable"}: {load.message()}{" "}
            <Button onClick={load.retry}>Retry</Button>
          </p>
        </Show>
        <Show when={timeline()}>
          {(ready) => (
            <>
              <TurnTimeline timeline={ready()} runningMs={viewedRunningMs()} conversationHref={props.conversationHref}>
                <TurnResources
                  stats={history.stats()}
                  startMs={Date.parse(ready().startedAt)}
                  durationMs={axisMs(ready())}
                  tickShare={axisMs(ready()) > 0 ? durationTickStep(axisMs(ready()), 5) / axisMs(ready()) : 1}
                />
              </TurnTimeline>
              <TurnCalls timeline={ready()} costUpdating={props.costUpdating} />
              <TurnTools tools={ready().tools ?? []} />
            </>
          )}
        </Show>
        <Table label="Turn details" class={styles.details}>
          <tbody>
            <Show
              when={completed()}
              fallback={
                <>
                  {row("Started", started(), false)}
                  {row("Turn time", `Running, ${formatTotalDuration(runningTurnMs())} so far`, false)}
                  {contextRow()}
                </>
              }
            >
              {(turn) => completedRows(turn())}
            </Show>
          </tbody>
        </Table>
      </PerformanceSection>
    </PerformanceBody>
  );
}

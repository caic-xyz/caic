// ContextDetail shows one context section from its server-derived summary: its turns on the task's strip scale, why it ended, context growth per turn, and totals.

import { createMemo, For, Show } from "solid-js";
import PreviousIcon from "@material-symbols/svg-400/outlined/chevron_left.svg?solid";
import NextIcon from "@material-symbols/svg-400/outlined/chevron_right.svg?solid";

import type { HistoryRunningTurn, HistorySection, HistoryTurn } from "@sdk/types.gen";
import { contextEndDetail } from "../contextEnd";
import { formatChangeStat, formatMoney, formatTime, formatTokens, formatUSD } from "../formatting";
import { formatExactSeconds, formatTotalDuration } from "../timing";
import ActionButton from "./ActionButton";
import ChartDataTable from "./ChartDataTable";
import { PerformanceBody, PerformanceSection, useArrowStepping } from "./PerformanceDetail";
import Table from "./Table";
import TurnStrip, { createRunningElapsed } from "./TurnStrip";
import styles from "./ContextDetail.module.css";

const growthColumns = [
  "Turn",
  "Turn time",
  "Context at end",
  "Window",
  "New input",
  "Cache write",
  "Cache read",
  "Output",
  "Cost",
] as const;

// labelStep labels every column of a short context and round turn positions
// of a long one, so labels never touch at the narrowest column width.
function labelStep(count: number): number {
  if (count <= 12) return 1;
  if (count <= 30) return 5;
  return count <= 60 ? 10 : 25;
}

interface Totals {
  durationMs: number;
  apiMs: number;
  waitMs: number;
  inputTokens: number;
  cacheWriteTokens: number;
  cacheReadTokens: number;
  outputTokens: number;
  reasoningTokens: number;
  costUSD: number;
  costCount: number;
  estimated: boolean;
  change: { files: number; linesAdded: number; linesDeleted: number; binaryFiles: number } | null;
}

function totalsOf(turns: readonly HistoryTurn[]): Totals {
  const totals: Totals = {
    durationMs: 0,
    apiMs: 0,
    waitMs: 0,
    inputTokens: 0,
    cacheWriteTokens: 0,
    cacheReadTokens: 0,
    outputTokens: 0,
    reasoningTokens: 0,
    costUSD: 0,
    costCount: 0,
    estimated: false,
    change:
      turns.length > 0 && turns.every((turn) => turn.changeStat !== undefined)
        ? { files: 0, linesAdded: 0, linesDeleted: 0, binaryFiles: 0 }
        : null,
  };
  for (const turn of turns) {
    totals.durationMs += turn.durationMs;
    totals.apiMs += turn.durationAPIMs;
    totals.waitMs += turn.waitMs ?? 0;
    totals.inputTokens += turn.usage.inputTokens;
    totals.cacheWriteTokens += turn.usage.cacheCreationInputTokens;
    totals.cacheReadTokens += turn.usage.cacheReadInputTokens;
    totals.outputTokens += turn.usage.outputTokens;
    totals.reasoningTokens += turn.usage.reasoningOutputTokens ?? 0;
    if (turn.cost) {
      totals.costUSD += turn.cost.usd;
      totals.costCount++;
      if (turn.cost.source === "estimated") totals.estimated = true;
    }
    if (totals.change && turn.changeStat) {
      totals.change.files += turn.changeStat.files;
      totals.change.linesAdded += turn.changeStat.linesAdded;
      totals.change.linesDeleted += turn.changeStat.linesDeleted;
      totals.change.binaryFiles += turn.changeStat.binaryFiles;
    }
  }
  return totals;
}

function costText(turn: HistoryTurn): string {
  if (!turn.cost) return "—";
  return `${turn.cost.source === "estimated" ? "~" : ""}$${formatMoney(turn.cost.usd, 4)}`;
}

export default function ContextDetail(props: {
  section: HistorySection;
  sections: readonly HistorySection[];
  // turns are all the task's completed turns; the strip shares their scale.
  turns: readonly HistoryTurn[];
  running: HistoryRunningTurn | null;
  href: (section: string) => string;
  turnHref: (turn: number) => string;
  // costUpdating marks the cost figures while their estimates refresh.
  costUpdating: boolean;
}) {
  const position = () => props.sections.findIndex((section) => section.id === props.section.id) + 1;
  const previous = () => (position() > 1 ? position() - 1 : null);
  const next = () => (position() < props.sections.length ? position() + 1 : null);
  const go = useArrowStepping(previous, next, (target) => props.href(props.sections[target - 1]?.id ?? ""));

  const turns = createMemo(() => props.turns.filter((turn) => turn.sectionID === props.section.id));
  const running = () => (props.running?.sectionID === props.section.id ? props.running : null);
  const runningMs = createRunningElapsed(running);
  const scaleMs = () => props.turns.reduce((max, turn) => Math.max(max, turn.durationMs), 0);
  const totals = createMemo(() => totalsOf(turns()));
  const models = () => [
    ...new Set(
      turns()
        .map((turn) => turn.usage.reportedModel || turn.model)
        .filter(Boolean),
    ),
  ];
  const tokens = () =>
    totals().inputTokens + totals().cacheWriteTokens + totals().cacheReadTokens + totals().outputTokens;
  const costComplete = () => turns().length > 0 && totals().costCount === turns().length;
  const summary = () => {
    const parts: { value: string; label: string; cost: boolean }[] = [
      { value: String(turns().length), label: turns().length === 1 ? "turn" : "turns", cost: false },
      { value: formatTotalDuration(totals().durationMs), label: "turn time", cost: false },
    ];
    if (totals().costCount > 0)
      parts.push({
        value: `${totals().estimated ? "~" : ""}${formatUSD(totals().costUSD)}${costComplete() ? "" : "+"}`,
        label: "",
        cost: true,
      });
    parts.push({ value: formatTokens(tokens()), label: "tokens", cost: false });
    return parts;
  };

  // The growth chart's explicit domain holds every turn's context, every
  // window, and the context the harness compacted.
  const compactedFrom = () => {
    const system = props.section.endBoundary?.message.system;
    return system?.subtype === "compact_boundary" ? (system.contextTokensBefore ?? 0) : 0;
  };
  const contextMax = createMemo(() =>
    Math.max(
      1,
      compactedFrom(),
      ...turns().map((turn) => Math.max(turn.contextTokens ?? 0, turn.usage.contextWindow ?? 0)),
    ),
  );
  const known = () => turns().filter((turn) => (turn.contextTokens ?? 0) > 0);
  const growthRows = () =>
    turns().map((turn) => [
      String(turn.number),
      formatExactSeconds(turn.durationMs),
      turn.contextTokens ? turn.contextTokens.toLocaleString() : "Unknown",
      turn.usage.contextWindow ? turn.usage.contextWindow.toLocaleString() : "Unknown",
      turn.usage.inputTokens.toLocaleString(),
      turn.usage.cacheCreationInputTokens.toLocaleString(),
      turn.usage.cacheReadInputTokens.toLocaleString(),
      turn.usage.outputTokens.toLocaleString(),
      costText(turn),
    ]);
  const growthSummary = () => {
    const values = known();
    const first = values[0];
    const last = values.at(-1);
    if (!first || !last) return "Context sizes are unknown for these turns.";
    if (first === last) return `${formatTokens(first.contextTokens ?? 0)} after turn ${first.number}`;
    const peak = values.reduce((max, turn) => Math.max(max, turn.contextTokens ?? 0), 0);
    return `${formatTokens(first.contextTokens ?? 0)} after turn ${first.number} → ${formatTokens(last.contextTokens ?? 0)} after turn ${last.number} · peak ${formatTokens(peak)}`;
  };
  const row = (label: string, value: string, updating: boolean) => (
    <tr>
      <th scope="row">{label}</th>
      <td class={`${styles.value} updating-value`} aria-busy={updating ? "true" : undefined}>
        {value}
      </td>
    </tr>
  );
  const started = () => {
    const at = turns()[0]?.startedAt ?? running()?.startedAt ?? "";
    return Date.parse(at) > 0 ? formatTime(at) : "—";
  };
  const ended = () => {
    const at = props.section.endBoundary?.message.ts ?? 0;
    return at > 0 ? formatTime(new Date(at).toISOString()) : "—";
  };

  return (
    <PerformanceBody testId="context-detail">
      <PerformanceSection
        title="Turn durations"
        actions={
          <span class={styles.stepper}>
            <ActionButton label="Previous context" disabled={previous() === null} onClick={() => go(previous())}>
              <PreviousIcon width={20} height={20} aria-hidden="true" />
            </ActionButton>
            <ActionButton label="Next context" disabled={next() === null} onClick={() => go(next())}>
              <NextIcon width={20} height={20} aria-hidden="true" />
            </ActionButton>
          </span>
        }
      >
        <TurnStrip
          turns={turns()}
          running={running()}
          runningMs={runningMs()}
          scaleMs={scaleMs()}
          current={null}
          href={props.turnHref}
        />
      </PerformanceSection>
      <PerformanceSection label={`Context ${props.section.id}`} testId="context-summary">
        <p class={styles.summary} data-testid="context-summary-line">
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
        <p class={styles.end} data-testid="context-end">
          {contextEndDetail(props.section)}
        </p>
        <Show when={turns().length > 0}>
          <figure class={styles.growth} data-testid="context-growth">
            <figcaption class={styles.caption}>
              <span class={styles.title}>Context at each turn's end</span>
              <span class={styles.note}>{growthSummary()}</span>
            </figcaption>
            <div
              class={styles.chart}
              role="img"
              aria-label={`Context size at the end of each turn: ${growthSummary()}. The turns table lists exact values.`}
            >
              <div class={styles.scale} aria-hidden="true">
                <span>{formatTokens(contextMax())}</span>
                <span>0</span>
              </div>
              <ol class={styles.columns} aria-hidden="true">
                <For each={turns()}>
                  {(turn, index) => (
                    <li class={styles.column} data-testid="context-growth-turn">
                      <div class={styles.plot}>
                        <Show when={(turn.usage.contextWindow ?? 0) > 0}>
                          <span
                            class={styles.window}
                            style={{ "--share": (turn.usage.contextWindow ?? 0) / contextMax() }}
                          />
                        </Show>
                        <Show when={(turn.contextTokens ?? 0) > 0}>
                          <span class={styles.fill} style={{ "--share": (turn.contextTokens ?? 0) / contextMax() }} />
                        </Show>
                        <Show when={compactedFrom() > 0}>
                          <span class={styles.compacted} style={{ "--share": compactedFrom() / contextMax() }} />
                        </Show>
                      </div>
                      <span class={styles.label}>
                        {index() % labelStep(turns().length) === 0 || index() === turns().length - 1 ? turn.number : ""}
                      </span>
                    </li>
                  )}
                </For>
              </ol>
            </div>
            <ul class={styles.legend} aria-hidden="true">
              <li>
                <span class={`${styles.swatch} ${styles.fillSwatch}`} />
                Context
              </li>
              <Show when={turns().some((turn) => (turn.usage.contextWindow ?? 0) > 0)}>
                <li>
                  <span class={`${styles.swatch} ${styles.windowSwatch}`} />
                  Window
                </li>
              </Show>
              <Show when={compactedFrom() > 0}>
                <li>
                  <span class={`${styles.swatch} ${styles.compactedSwatch}`} />
                  Compacted at
                </li>
              </Show>
            </ul>
            <ChartDataTable
              caption="Turns in this context"
              regionLabel="Turns in this context"
              columns={growthColumns}
              rows={growthRows()}
              updatingColumns={props.costUpdating ? [growthColumns.indexOf("Cost")] : undefined}
            />
          </figure>
        </Show>
        <Table label="Context totals" class={styles.details}>
          <tbody>
            {row("Started", started(), false)}
            <Show when={props.section.endBoundary}>{row("Ended", ended(), false)}</Show>
            {row(models().length === 1 ? "Model" : "Models", models().join(", ") || "Unknown", false)}
            {row("Turn time", formatExactSeconds(totals().durationMs), false)}
            {row("API time", formatExactSeconds(totals().apiMs), false)}
            {row("Time awaiting user", formatExactSeconds(totals().waitMs), false)}
            {row("Messages", props.section.textCount.toLocaleString(), false)}
            {row("Tool calls", props.section.toolCount.toLocaleString(), false)}
            {row("New input tokens", totals().inputTokens.toLocaleString(), false)}
            {row("Cache write tokens", totals().cacheWriteTokens.toLocaleString(), false)}
            {row("Cache read tokens", totals().cacheReadTokens.toLocaleString(), false)}
            {row("Output tokens", totals().outputTokens.toLocaleString(), false)}
            {row(
              "Thinking tokens",
              totals().reasoningTokens > 0 ? totals().reasoningTokens.toLocaleString() : "—",
              false,
            )}
            {row(
              "Cost",
              totals().costCount === 0
                ? "Unavailable"
                : `${totals().estimated ? "Estimated " : ""}$${formatMoney(totals().costUSD, 4)}${costComplete() ? "" : `, ${turns().length - totals().costCount} of ${turns().length} turns unpriced`}`,
              props.costUpdating,
            )}
            <Show when={totals().change}>{(change) => row("Change", formatChangeStat(change(), true), false)}</Show>
          </tbody>
        </Table>
      </PerformanceSection>
    </PerformanceBody>
  );
}

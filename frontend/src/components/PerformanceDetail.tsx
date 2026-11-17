// Renders compact task usage, turn/tool analytics, and bounded resource history for the Performance view, and steps its child views.

import { loadCode } from "../codeLoad";
import { createMemo, lazy, onCleanup, onMount, Show, Suspense, type JSX } from "solid-js";
import { useNavigate } from "@solidjs/router";

import type { EventStats } from "@sdk/types.gen";

import { formatTokens, formatUSD } from "../formatting";
import type { ToolTimingSummary } from "../taskStats";
import type { HistoryTurn } from "@sdk/types.gen";
import { isEditing } from "./KeyboardShortcuts";
import styles from "./PerformanceDetail.module.css";

const statsCharts = () => loadCode(import("./StatsCharts"));
const StatsCharts = lazy(() => statsCharts().then((module) => ({ default: module.default })));
const SampleTable = lazy(() => statsCharts().then((module) => ({ default: module.ResourceSamples })));
const noStats: readonly EventStats[] = [];
const noTools: readonly ToolTimingSummary[] = [];
const noTurns: readonly HistoryTurn[] = [];

export interface TaskUsageSummary {
  inputTokens: number;
  cacheWriteInputTokens: number;
  cacheReadInputTokens: number;
  outputTokens: number;
  costUSD: number;
  costKnown: boolean;
  costComplete: boolean;
}

interface UsageDetails extends TaskUsageSummary {
  reasoningOutputTokens: number;
  totalTokens: number;
}

interface PerformanceContentProps {
  tools: readonly ToolTimingSummary[];
  stats: EventStats[];
  turns: readonly HistoryTurn[];
  usage?: TaskUsageSummary;
  // Sections that lead the scrolling content, such as the turn strip.
  children?: JSX.Element;
  // costUpdating marks the cost figures while their estimates refresh.
  costUpdating?: boolean;
}

function sumTurnUsage(turns: readonly HistoryTurn[]): UsageDetails {
  const completeCost = turns.length > 0 && turns.every((turn) => turn.cost !== undefined);
  return turns.reduce<UsageDetails>(
    (total, turn) => {
      const usage = turn.usage;
      total.inputTokens += usage.inputTokens;
      total.cacheWriteInputTokens += usage.cacheCreationInputTokens;
      total.cacheReadInputTokens += usage.cacheReadInputTokens;
      total.outputTokens += usage.outputTokens;
      total.reasoningOutputTokens += usage.reasoningOutputTokens ?? 0;
      total.totalTokens +=
        usage.inputTokens + usage.cacheCreationInputTokens + usage.cacheReadInputTokens + usage.outputTokens;
      if (turn.cost !== undefined) total.costUSD += turn.cost.usd;
      return total;
    },
    {
      inputTokens: 0,
      cacheWriteInputTokens: 0,
      cacheReadInputTokens: 0,
      outputTokens: 0,
      reasoningOutputTokens: 0,
      totalTokens: 0,
      costUSD: 0,
      costKnown: turns.some((turn) => turn.cost !== undefined),
      costComplete: completeCost,
    },
  );
}

// useArrowStepping moves a Performance child view, such as a turn or a context,
// to its previous or next sibling with ←/→ outside editable fields and dialogs.
// It returns the navigation the view's stepper buttons use.
export function useArrowStepping(
  previous: () => number | null,
  next: () => number | null,
  href: (target: number) => string,
): (target: number | null) => void {
  const navigate = useNavigate();
  const go = (target: number | null) => {
    if (target !== null) navigate(href(target));
  };
  onMount(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.defaultPrevented || event.altKey || event.ctrlKey || event.metaKey || event.shiftKey) return;
      if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") return;
      if (isEditing(event.target) || document.querySelector("dialog[open]")) return;
      const target = event.key === "ArrowLeft" ? previous() : next();
      if (target === null) return;
      event.preventDefault();
      go(target);
    };
    document.addEventListener("keydown", onKey);
    onCleanup(() => document.removeEventListener("keydown", onKey));
  });
  return go;
}

// PerformanceBody is the Performance view's scrolling column of sections.
export function PerformanceBody(props: { testId?: string; children: JSX.Element }) {
  return (
    <div class={styles.content} data-testid={props.testId}>
      {props.children}
    </div>
  );
}

// PerformanceSection frames one Performance section; actions sit at the end of
// its header row. A section has a visible title, or a label that names it only
// programmatically when the surrounding view already states its scope.
type PerformanceSectionProps = (
  { title: Exclude<JSX.Element, null | undefined | boolean>; label?: never } | { label: string; title?: never }
) & {
  actions?: JSX.Element;
  testId?: string;
  children: JSX.Element;
};

export function PerformanceSection(props: PerformanceSectionProps) {
  return (
    <section
      class={styles.section}
      aria-label={props.title === undefined ? props.label : undefined}
      data-testid={props.testId}
    >
      <Show when={props.title !== undefined || props.actions}>
        <div class={styles.sectionHeader}>
          <Show when={props.title !== undefined}>
            <h2 class={styles.sectionTitle}>{props.title}</h2>
          </Show>
          {props.actions}
        </div>
      </Show>
      {props.children}
    </section>
  );
}

export default function PerformanceContent(props: PerformanceContentProps) {
  const toolSummaries = () => props.tools;
  const usage = createMemo<UsageDetails>(() => {
    const fromTurns = sumTurnUsage(props.turns);
    if (!props.usage) return fromTurns;
    return {
      ...props.usage,
      reasoningOutputTokens: fromTurns.reasoningOutputTokens,
      totalTokens:
        props.usage.inputTokens +
        props.usage.cacheWriteInputTokens +
        props.usage.cacheReadInputTokens +
        props.usage.outputTokens,
      costKnown: props.usage.costKnown || fromTurns.costKnown,
      costUSD: props.usage.costKnown ? props.usage.costUSD : fromTurns.costUSD,
    };
  });
  const cacheHitRate = () => {
    const details = usage();
    const input = details.inputTokens + details.cacheWriteInputTokens + details.cacheReadInputTokens;
    return input > 0 ? details.cacheReadInputTokens / input : 0;
  };
  const costPerMillionTokens = () => {
    const details = usage();
    return details.totalTokens > 0 ? (details.costUSD * 1_000_000) / details.totalTokens : 0;
  };

  return (
    <PerformanceBody testId="task-stats-content">
      {props.children}
      <Show when={usage().totalTokens > 0}>
        <PerformanceSection title="Usage" testId="task-usage-summary">
          <div class={styles.usageGrid}>
            <div class={styles.usageMetric} title="Input tokens that were neither written to nor read from cache">
              <span class={styles.usageLabel}>New input</span>
              <strong>{formatTokens(usage().inputTokens)}</strong>
            </div>
            <div class={styles.usageMetric} title="Input tokens written to the provider prompt cache">
              <span class={styles.usageLabel}>Cache write</span>
              <strong>{formatTokens(usage().cacheWriteInputTokens)}</strong>
            </div>
            <div class={styles.usageMetric} title="Input tokens served from the provider prompt cache">
              <span class={styles.usageLabel}>Cache read</span>
              <strong>{formatTokens(usage().cacheReadInputTokens)}</strong>
            </div>
            <div class={styles.usageMetric} title="All generated output tokens, including thinking tokens">
              <span class={styles.usageLabel}>Output</span>
              <strong>{formatTokens(usage().outputTokens)}</strong>
            </div>
            <div class={styles.usageMetric} title="Thinking or reasoning tokens; included in output">
              <span class={styles.usageLabel}>Thinking</span>
              <strong>{usage().reasoningOutputTokens > 0 ? formatTokens(usage().reasoningOutputTokens) : "—"}</strong>
            </div>
          </div>
          <div class={styles.efficiencyRow}>
            <span>
              <span class={styles.usageLabel}>Total </span>
              {formatTokens(usage().totalTokens)}
            </span>
            <span title="Share of input context served from cache">
              <span class={styles.usageLabel}>Cache hit </span>
              {Math.round(cacheHitRate() * 100)}%
            </span>
            <span class="updating-value" aria-busy={props.costUpdating ? "true" : undefined}>
              <span class={styles.usageLabel}>Cost </span>
              {usage().costKnown
                ? `${formatUSD(usage().costUSD)}${usage().costComplete ? "" : " known subtotal"}`
                : "—"}
            </span>
            <Show when={usage().costComplete && usage().costUSD > 0}>
              <span
                class="updating-value"
                title="Reported cost divided by total token volume"
                aria-busy={props.costUpdating ? "true" : undefined}
              >
                <span class={styles.usageLabel}>Effective </span>
                {formatUSD(costPerMillionTokens())}/Mt
              </span>
            </Show>
          </div>
        </PerformanceSection>
      </Show>
      <Show when={props.turns.length > 0 || toolSummaries().length > 0}>
        <PerformanceSection title="Analytics" testId="task-analytics-charts">
          <Suspense fallback={<div class={styles.noData}>Loading charts…</div>}>
            <StatsCharts stats={noStats} turns={props.turns} tools={toolSummaries()} />
          </Suspense>
        </PerformanceSection>
      </Show>
      <PerformanceSection title="Resources">
        <Show when={props.stats.length > 0} fallback={<div class={styles.noData}>No data yet</div>}>
          <Suspense fallback={<div class={styles.noData}>Loading resource history…</div>}>
            <StatsCharts samples={false} stats={props.stats} turns={noTurns} tools={noTools} />
          </Suspense>
        </Show>
      </PerformanceSection>
      <Show when={props.stats.length > 0}>
        <section class={styles.section} aria-label="Exact resource samples">
          <Suspense fallback={<div class={styles.noData}>Loading samples…</div>}>
            <SampleTable scope="task" stats={props.stats} />
          </Suspense>
        </section>
      </Show>
    </PerformanceBody>
  );
}

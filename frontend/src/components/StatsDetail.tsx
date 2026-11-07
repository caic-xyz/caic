// Renders compact task usage, turn/tool analytics, and bounded resource history.

import { createMemo, lazy, Show, Suspense, type JSX } from "solid-js";
import TaskViewHeader from "./TaskViewHeader";

import type { EventStats } from "@sdk/types.gen";

import { formatMoney, formatTokens } from "../formatting";
import type { ToolTimingSummary } from "../taskStats";
import type { HistoryTurn } from "@sdk/types.gen";
import styles from "./StatsDetail.module.css";

const statsCharts = () => import("./StatsCharts");
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

interface StatsContentProps {
  tools: readonly ToolTimingSummary[];
  stats: EventStats[];
  turns: readonly HistoryTurn[];
  usage?: TaskUsageSummary;
}

interface StatsDetailProps extends StatsContentProps {
  branch: string;
  repo: string;
  taskPath: string;
  status?: JSX.Element;
}

function formatUSD(usd: number): string {
  return `$${formatMoney(usd, usd < 0.01 ? 4 : 2)}`;
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

export function StatsContent(props: StatsContentProps) {
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
    <div class={styles.content} data-testid="task-stats-content">
      <Show when={usage().totalTokens > 0}>
        <section class={styles.section} data-testid="task-usage-summary">
          <h2 class={styles.sectionTitle}>Usage</h2>
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
            <span>
              <span class={styles.usageLabel}>Cost </span>
              {usage().costKnown
                ? `${formatUSD(usage().costUSD)}${usage().costComplete ? "" : " known subtotal"}`
                : "—"}
            </span>
            <Show when={usage().costComplete && usage().costUSD > 0}>
              <span title="Reported cost divided by total token volume">
                <span class={styles.usageLabel}>Effective </span>
                {formatUSD(costPerMillionTokens())}/Mt
              </span>
            </Show>
          </div>
        </section>
      </Show>
      <Show when={props.turns.length > 0 || toolSummaries().length > 0}>
        <section class={styles.section} data-testid="task-analytics-charts">
          <h2 class={styles.sectionTitle}>Analytics</h2>
          <Suspense fallback={<div class={styles.noData}>Loading charts…</div>}>
            <StatsCharts stats={noStats} turns={props.turns} tools={toolSummaries()} />
          </Suspense>
        </section>
      </Show>
      <section class={styles.section}>
        <h2 class={styles.sectionTitle}>Resources</h2>
        <Show when={props.stats.length > 0} fallback={<div class={styles.noData}>No data yet</div>}>
          <Suspense fallback={<div class={styles.noData}>Loading resource history…</div>}>
            <StatsCharts samples={false} stats={props.stats} turns={noTurns} tools={noTools} />
          </Suspense>
        </Show>
      </section>
      <Show when={props.stats.length > 0}>
        <section class={styles.section} aria-label="Exact resource samples">
          <Suspense fallback={<div class={styles.noData}>Loading samples…</div>}>
            <SampleTable scope="task" stats={props.stats} />
          </Suspense>
        </section>
      </Show>
    </div>
  );
}

export default function StatsDetail(props: StatsDetailProps) {
  return (
    <div class={styles.container}>
      <TaskViewHeader taskPath={props.taskPath} label="Performance controls">
        <span>Performance</span>
        <span class={styles.headerMeta}>
          <span class={styles.headerRepo}>{props.repo}</span>
          <span class={styles.headerBranch}>{props.branch}</span>
        </span>
      </TaskViewHeader>
      <Show when={props.status}>
        <div class={styles.status}>{props.status}</div>
      </Show>
      <StatsContent tools={props.tools} stats={props.stats} turns={props.turns} usage={props.usage} />
    </div>
  );
}

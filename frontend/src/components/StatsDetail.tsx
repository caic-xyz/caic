// StatsDetail renders a task's usage, timing analytics, and resource history as a full-page view.

import { createMemo, lazy, onCleanup, onMount, Show, Suspense } from "solid-js";
import { useNavigate } from "@solidjs/router";
import ArrowBackIcon from "@material-symbols/svg-400/outlined/arrow_back.svg?solid";

import type { EventMessage, EventStats } from "@sdk/types.gen";

import {
  IncrementalToolTimingTracker,
  type ToolTimingSummary,
} from "../taskStats";
import type { TurnTiming } from "../timing";
import styles from "./StatsDetail.module.css";

const StatsCharts = lazy(() => import("./StatsCharts"));
const noStats: readonly EventStats[] = [];
const noTools: readonly ToolTimingSummary[] = [];
const noTurns: readonly TurnTiming[] = [];

export interface TaskUsageSummary {
  inputTokens: number;
  cacheWriteInputTokens: number;
  cacheReadInputTokens: number;
  outputTokens: number;
  costUSD: number;
}

interface UsageDetails extends TaskUsageSummary {
  reasoningOutputTokens: number;
  totalTokens: number;
}

interface StatsContentProps {
  events: readonly EventMessage[];
  stats: EventStats[];
  turns: TurnTiming[];
  usage?: TaskUsageSummary;
}

interface StatsDetailProps extends StatsContentProps {
  branch: string;
  repo: string;
  taskPath: string;
}

function formatUsageTokens(tokens: number): string {
  if (tokens >= 1_000_000) return `${(tokens / 1_000_000).toFixed(1)}Mt`;
  if (tokens >= 1_000) return `${(tokens / 1_000).toFixed(1)}kt`;
  return `${tokens}t`;
}

function formatUSD(usd: number): string {
  return `$${usd.toFixed(usd < 0.01 ? 4 : 2)}`;
}

function sumTurnUsage(turns: TurnTiming[]): UsageDetails {
  return turns.reduce<UsageDetails>(
    (total, turn) => {
      const usage = turn.result.usage;
      total.inputTokens += usage.inputTokens;
      total.cacheWriteInputTokens += usage.cacheCreationInputTokens;
      total.cacheReadInputTokens += usage.cacheReadInputTokens;
      total.outputTokens += usage.outputTokens;
      total.reasoningOutputTokens += usage.reasoningOutputTokens ?? 0;
      total.totalTokens +=
        usage.inputTokens +
        usage.cacheCreationInputTokens +
        usage.cacheReadInputTokens +
        usage.outputTokens;
      total.costUSD += turn.result.totalCostUSD;
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
    },
  );
}

export function StatsContent(props: StatsContentProps) {
  const toolTimingTracker = new IncrementalToolTimingTracker();
  const toolSummaries = createMemo(() =>
    toolTimingTracker.derive(props.events),
  );
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
    };
  });
  const cacheHitRate = () => {
    const details = usage();
    const input =
      details.inputTokens +
      details.cacheWriteInputTokens +
      details.cacheReadInputTokens;
    return input > 0 ? details.cacheReadInputTokens / input : 0;
  };
  const costPerMillionTokens = () => {
    const details = usage();
    return details.totalTokens > 0
      ? (details.costUSD * 1_000_000) / details.totalTokens
      : 0;
  };

  return (
    <div class={styles.content} data-testid="task-stats-content">
      <Show when={usage().totalTokens > 0}>
        <section class={styles.section} data-testid="task-usage-summary">
          <h2 class={styles.sectionTitle}>Usage</h2>
          <div class={styles.usageGrid}>
            <div
              class={styles.usageMetric}
              title="Input tokens that were neither written to nor read from cache"
            >
              <span class={styles.usageLabel}>New input</span>
              <strong>{formatUsageTokens(usage().inputTokens)}</strong>
            </div>
            <div
              class={styles.usageMetric}
              title="Input tokens written to the provider prompt cache"
            >
              <span class={styles.usageLabel}>Cache write</span>
              <strong>
                {formatUsageTokens(usage().cacheWriteInputTokens)}
              </strong>
            </div>
            <div
              class={styles.usageMetric}
              title="Input tokens served from the provider prompt cache"
            >
              <span class={styles.usageLabel}>Cache read</span>
              <strong>{formatUsageTokens(usage().cacheReadInputTokens)}</strong>
            </div>
            <div
              class={styles.usageMetric}
              title="All generated output tokens, including thinking tokens"
            >
              <span class={styles.usageLabel}>Output</span>
              <strong>{formatUsageTokens(usage().outputTokens)}</strong>
            </div>
            <div
              class={styles.usageMetric}
              title="Thinking or reasoning tokens; included in output"
            >
              <span class={styles.usageLabel}>Thinking</span>
              <strong>
                {usage().reasoningOutputTokens > 0
                  ? formatUsageTokens(usage().reasoningOutputTokens)
                  : "—"}
              </strong>
            </div>
          </div>
          <div class={styles.efficiencyRow}>
            <span>
              <span class={styles.usageLabel}>Total </span>
              {formatUsageTokens(usage().totalTokens)}
            </span>
            <span title="Share of input context served from cache">
              <span class={styles.usageLabel}>Cache hit </span>
              {Math.round(cacheHitRate() * 100)}%
            </span>
            <Show when={usage().costUSD > 0}>
              <span>
                <span class={styles.usageLabel}>Cost </span>
                {formatUSD(usage().costUSD)}
              </span>
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
            <StatsCharts
              stats={noStats}
              turns={props.turns}
              tools={toolSummaries()}
            />
          </Suspense>
        </section>
      </Show>
      <section class={styles.section}>
        <h2 class={styles.sectionTitle}>Resources</h2>
        <Show
          when={props.stats.length > 0}
          fallback={<div class={styles.noData}>No data yet</div>}
        >
          <Suspense
            fallback={
              <div class={styles.noData}>Loading resource history…</div>
            }
          >
            <StatsCharts stats={props.stats} turns={noTurns} tools={noTools} />
          </Suspense>
        </Show>
      </section>
    </div>
  );
}

export default function StatsDetail(props: StatsDetailProps) {
  const navigate = useNavigate();

  onMount(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") navigate(props.taskPath);
    };
    document.addEventListener("keydown", onKey);
    onCleanup(() => document.removeEventListener("keydown", onKey));
  });

  return (
    <div class={styles.container}>
      <div class={styles.header}>
        <button
          class={styles.backBtn}
          onClick={() => navigate(props.taskPath)}
          title="Back to task"
        >
          <ArrowBackIcon width={20} height={20} />
        </button>
        <span>Performance</span>
        <span class={styles.headerMeta}>
          <span class={styles.headerRepo}>{props.repo}</span>
          <span class={styles.headerBranch}>{props.branch}</span>
        </span>
      </div>
      <StatsContent
        events={props.events}
        stats={props.stats}
        turns={props.turns}
        usage={props.usage}
      />
    </div>
  );
}

// StatsIcon surfaces task token volume and cost, with detailed usage, timing, and container resource statistics.

import { createMemo, createSignal, For, Show } from "solid-js";

import type { EventStats } from "@sdk/types.gen";

import { formatDuration, formatTokens } from "../formatting";
import { formatTimingDuration, type TurnTiming } from "../timing";
import styles from "./StatsIcon.module.css";

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

function formatUsageTokens(tokens: number): string {
  if (tokens >= 1_000_000) return `${(tokens / 1_000_000).toFixed(1)}Mt`;
  if (tokens >= 1_000) return `${(tokens / 1_000).toFixed(1)}kt`;
  return `${tokens}t`;
}

function formatUSD(usd: number): string {
  return `$${usd.toFixed(usd < 0.01 ? 4 : 2)}`;
}

function sumTurnUsage(turns: TurnTiming[]): UsageDetails {
  return turns.reduce<UsageDetails>((total, turn) => {
    const u = turn.result.usage;
    total.inputTokens += u.inputTokens;
    total.cacheWriteInputTokens += u.cacheCreationInputTokens;
    total.cacheReadInputTokens += u.cacheReadInputTokens;
    total.outputTokens += u.outputTokens;
    total.reasoningOutputTokens += u.reasoningOutputTokens ?? 0;
    total.totalTokens += u.inputTokens + u.cacheCreationInputTokens + u.cacheReadInputTokens + u.outputTokens;
    total.costUSD += turn.result.totalCostUSD;
    return total;
  }, {
    inputTokens: 0,
    cacheWriteInputTokens: 0,
    cacheReadInputTokens: 0,
    outputTokens: 0,
    reasoningOutputTokens: 0,
    totalTokens: 0,
    costUSD: 0,
  });
}

function formatBytes(bytes: number): string {
  if (bytes <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const i = Math.min(Math.floor(Math.log2(bytes) / 10), units.length - 1);
  const val = bytes / Math.pow(1024, i);
  return `${val < 10 ? val.toFixed(1) : Math.round(val)} ${units[i]}`;
}

// Color for CPU/MEM bars: ratio is 0–1 of a hard limit.
function barColor(ratio: number): string {
  if (ratio >= 0.85) return "var(--color-danger)";
  if (ratio >= 0.5) return "var(--color-warning-text)";
  return "var(--color-success)";
}

// Color for NET bar: absolute thresholds on total bytes (cumulative).
function netColor(bytes: number): string {
  if (bytes >= 1e9) return "var(--color-danger)";      // ≥ 1 GB
  if (bytes >= 100e6) return "var(--color-warning-text)"; // ≥ 100 MB
  return "var(--color-success)";
}

// Color for DISK bar: absolute thresholds on writable layer size.
function diskColor(bytes: number): string {
  if (bytes >= 10e9) return "var(--color-danger)";       // ≥ 10 GB
  if (bytes >= 5e9) return "var(--color-warning-text)";  // ≥ 5 GB
  return "var(--color-success)";
}

interface MiniBar {
  ratio: number;
  label: string;
  color?: string;
}

function MiniBarGroup(props: { bars: MiniBar[] }) {
  return (
    <div class={styles.miniBarGroup}>
      <For each={props.bars}>
        {(b) => (
          <div class={styles.miniBar} title={b.label}>
            <div
              class={styles.miniBarFill}
              style={{ height: `${Math.round(b.ratio * 100)}%`, background: b.color ?? barColor(b.ratio) }}
            />
          </div>
        )}
      </For>
    </div>
  );
}

export default function StatsIcon(props: { stats: EventStats[]; turns: TurnTiming[]; usage?: TaskUsageSummary }) {
  const [open, setOpen] = createSignal(false);

  // Current stats: last sample.
  const latest = () => props.stats[props.stats.length - 1];

  // Normalize NET: max bytes/s across all samples.
  const maxNet = () => Math.max(1, ...props.stats.map((s) => s.netRx + s.netTx));
  // Normalize DISK: max DiskUsed across all samples.
  const maxDisk = () => Math.max(1, ...props.stats.map((s) => Math.max(0, s.diskUsed)));

  const cpuRatio = () => Math.min(1, (latest()?.cpuPerc ?? 0) / 100);
  const memRatio = () => { const l = latest(); return l ? Math.min(1, l.memPerc / 100) : 0; };
  const netRatio = () => {
    const s = latest();
    if (!s) return 0;
    return Math.min(1, (s.netRx + s.netTx) / maxNet());
  };
  const diskRatio = () => {
    const s = latest();
    if (!s) return 0;
    return Math.min(1, Math.max(0, s.diskUsed) / maxDisk());
  };

  const hasStats = () => props.stats.length > 0;

  // Last N samples for history bars (most recent last).
  const recentStats = () => props.stats.slice(-5);

  const perfs = () => props.turns;
  const usage = createMemo<UsageDetails>(() => {
    const fromTurns = sumTurnUsage(props.turns);
    if (!props.usage) return fromTurns;
    const u = props.usage;
    return {
      ...u,
      reasoningOutputTokens: fromTurns.reasoningOutputTokens,
      totalTokens: u.inputTokens + u.cacheWriteInputTokens + u.cacheReadInputTokens + u.outputTokens,
    };
  });
  const cacheHitRate = () => {
    const u = usage();
    const input = u.inputTokens + u.cacheWriteInputTokens + u.cacheReadInputTokens;
    return input > 0 ? u.cacheReadInputTokens / input : 0;
  };
  const costPerMillionTokens = () => {
    const u = usage();
    return u.totalTokens > 0 ? u.costUSD * 1_000_000 / u.totalTokens : 0;
  };

  return (
    <div class={styles.wrapper}>
      <button
        class={`${styles.iconBtn}${open() ? ` ${styles.iconBtnActive}` : ""}`}
        onClick={() => setOpen((v) => !v)}
        title="Task usage and performance statistics"
        aria-label="Task statistics"
      >
        <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true">
          {/* Top-left: CPU */}
          <rect x="0" y={8 - Math.round(cpuRatio() * 8)} width="6" height={Math.round(cpuRatio() * 8)} rx="1"
            fill={hasStats() ? barColor(cpuRatio()) : "var(--color-border)"} />
          {/* Top-right: MEM */}
          <rect x="10" y={8 - Math.round(memRatio() * 8)} width="6" height={Math.round(memRatio() * 8)} rx="1"
            fill={hasStats() ? barColor(memRatio()) : "var(--color-border)"} />
          {/* Bottom-left: NET */}
          <rect x="0" y={9 + (8 - Math.round(netRatio() * 8))} width="6" height={Math.round(netRatio() * 8)} rx="1"
            fill={hasStats() ? netColor((latest()?.netRx ?? 0) + (latest()?.netTx ?? 0)) : "var(--color-border)"} />
          {/* Bottom-right: DISK */}
          <rect x="10" y={9 + (8 - Math.round(diskRatio() * 8))} width="6" height={Math.round(diskRatio() * 8)} rx="1"
            fill={hasStats() ? diskColor(latest()?.diskUsed ?? 0) : "var(--color-border)"} />
        </svg>
        <Show when={usage().totalTokens > 0}>
          <span class={styles.iconSummary}>
            {formatTokens(usage().totalTokens)}
            <Show when={usage().costUSD > 0}><span class={styles.iconSummarySeparator}> · </span>{formatUSD(usage().costUSD)}</Show>
          </span>
        </Show>
      </button>
      <Show when={open()}>
        <div class={styles.popup}>
          <Show when={usage().totalTokens > 0}>
            <div class={styles.popupSection} data-testid="task-usage-summary">
              <div class={styles.popupSectionTitle}>Usage</div>
              <div class={styles.usageGrid}>
                <div class={styles.usageMetric} title="Input tokens that were neither written to nor read from cache">
                  <span class={styles.usageLabel}>New input</span>
                  <strong>{formatUsageTokens(usage().inputTokens)}</strong>
                </div>
                <div class={styles.usageMetric} title="Input tokens written to the provider prompt cache">
                  <span class={styles.usageLabel}>Cache write</span>
                  <strong>{formatUsageTokens(usage().cacheWriteInputTokens)}</strong>
                </div>
                <div class={styles.usageMetric} title="Input tokens served from the provider prompt cache">
                  <span class={styles.usageLabel}>Cache read</span>
                  <strong>{formatUsageTokens(usage().cacheReadInputTokens)}</strong>
                </div>
                <div class={styles.usageMetric} title="All generated output tokens, including thinking tokens">
                  <span class={styles.usageLabel}>Output</span>
                  <strong>{formatUsageTokens(usage().outputTokens)}</strong>
                </div>
                <div class={styles.usageMetric} title="Thinking or reasoning tokens; included in output">
                  <span class={styles.usageLabel}>Thinking</span>
                  <strong>{usage().reasoningOutputTokens > 0 ? formatUsageTokens(usage().reasoningOutputTokens) : "—"}</strong>
                </div>
              </div>
              <div class={styles.efficiencyRow}>
                <span><span class={styles.usageLabel}>Total </span>{formatUsageTokens(usage().totalTokens)}</span>
                <span title="Share of input context served from cache"><span class={styles.usageLabel}>Cache hit </span>{Math.round(cacheHitRate() * 100)}%</span>
                <Show when={usage().costUSD > 0}>
                  <span><span class={styles.usageLabel}>Cost </span>{formatUSD(usage().costUSD)}</span>
                  <span title="Reported cost divided by total token volume"><span class={styles.usageLabel}>Effective </span>{formatUSD(costPerMillionTokens())}/Mt</span>
                </Show>
              </div>
            </div>
          </Show>
          <div class={styles.popupSection}>
            <div class={styles.popupSectionTitle}>Resources</div>
            <Show when={latest()} keyed fallback={<div class={styles.noData}>No data yet</div>}>
              {(s) => {
                const recent = recentStats();
                return (
                  <table class={styles.statsTable}>
                    <tbody>
                      <tr>
                        <td class={styles.statLabel}>CPU</td>
                        <td class={styles.statValue}>{s.cpuPerc.toFixed(1)}%</td>
                        <td class={styles.statBars}>
                          <MiniBarGroup bars={recent.map((r) => ({ ratio: Math.min(1, r.cpuPerc / 100), label: `${r.cpuPerc.toFixed(1)}%` }))} />
                        </td>
                      </tr>
                      <tr>
                        <td class={styles.statLabel}>MEM</td>
                        <td class={styles.statValue}>{formatBytes(s.memUsed)}<span class={styles.statSub}>/{formatBytes(s.memLimit)}</span></td>
                        <td class={styles.statBars}>
                          <MiniBarGroup bars={recent.map((r) => ({ ratio: Math.min(1, r.memPerc / 100), label: `${r.memPerc.toFixed(1)}%` }))} />
                        </td>
                      </tr>
                      <tr>
                        <td class={styles.statLabel}>NET</td>
                        <td class={styles.statValue}>
                          <span title="Received">{formatBytes(s.netRx)}</span>
                          <span class={styles.statSub}> / </span>
                          <span title="Transmitted">{formatBytes(s.netTx)}</span>
                        </td>
                        <td class={styles.statBars}>
                          <MiniBarGroup bars={recent.map((r) => ({ ratio: Math.min(1, (r.netRx + r.netTx) / maxNet()), label: formatBytes(r.netRx + r.netTx), color: netColor(r.netRx + r.netTx) }))} />
                        </td>
                      </tr>
                      <tr>
                        <td class={styles.statLabel}>DISK</td>
                        <td class={styles.statValue}>{s.diskUsed >= 0 ? formatBytes(s.diskUsed) : "—"}</td>
                        <td class={styles.statBars}>
                          <MiniBarGroup bars={recent.map((r) => ({ ratio: Math.min(1, Math.max(0, r.diskUsed) / maxDisk()), label: r.diskUsed >= 0 ? formatBytes(r.diskUsed) : "—", color: diskColor(Math.max(0, r.diskUsed)) }))} />
                        </td>
                      </tr>
                    </tbody>
                  </table>
                );
              }}
            </Show>
          </div>
          <Show when={perfs().length > 0}>
            <div class={styles.popupSection}>
              <div class={styles.popupSectionTitle}>Invocations</div>
              <table class={styles.perfTable} data-testid="invocation-usage">
                <thead>
                  <tr>
                    <th class={styles.perfTh}>#</th>
                    <th class={styles.perfTh}>Model</th>
                    <th class={styles.perfTh}>Timing</th>
                    <th class={styles.perfTh}>Cost</th>
                  </tr>
                </thead>
                <tbody>
                  <For each={perfs()}>
                    {(p, index) => {
                      const r = p.result;
                      return (
                        <>
                          <tr>
                            <td class={styles.perfTd}>{index() + 1}</td>
                            <td class={`${styles.perfTd} ${styles.modelCell}`}>{r.usage.model || "—"}</td>
                            <td class={styles.perfTd}>
                              {r.duration > 0 ? formatDuration(r.duration) : "—"}
                              <span class={styles.perfSub}> API {r.durationAPI > 0 ? formatDuration(r.durationAPI) : "—"} · wait {p.waitMs !== null && p.waitMs > 0 ? formatTimingDuration(p.waitMs) : "—"}</span>
                            </td>
                            <td class={styles.perfTd}>{r.totalCostUSD > 0 ? formatUSD(r.totalCostUSD) : "—"}</td>
                          </tr>
                          <tr class={styles.tokenDetailRow}>
                            <td />
                            <td colspan={3} class={styles.tokenDetailCell}>
                              <span title="New input">New {formatUsageTokens(r.usage.inputTokens)}</span>
                              <span title="Cache write">Write {formatUsageTokens(r.usage.cacheCreationInputTokens)}</span>
                              <span title="Cache read">Read {formatUsageTokens(r.usage.cacheReadInputTokens)}</span>
                              <span title="Output">Out {formatUsageTokens(r.usage.outputTokens)}</span>
                              <Show when={(r.usage.reasoningOutputTokens ?? 0) > 0}>
                                <span title="Thinking; included in output">Think {formatUsageTokens(r.usage.reasoningOutputTokens ?? 0)}</span>
                              </Show>
                            </td>
                          </tr>
                        </>
                      );
                    }}
                  </For>
                </tbody>
              </table>
            </div>
          </Show>
        </div>
      </Show>
    </div>
  );
}

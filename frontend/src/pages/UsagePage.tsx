// UsagePage shows cross-task analytics in Monitoring with URL-backed range and subject filters.

import { createMemo, createSignal, For, lazy, onMount, Show, Suspense } from "solid-js";
import { useSearchParams } from "@solidjs/router";
import RefreshIcon from "@material-symbols/svg-400/outlined/refresh.svg?solid";

import type { UsageDashboardResp } from "@sdk/types.gen";

import { api } from "../api";
import ActionButton from "../components/ActionButton";
import SectionControls from "../components/SectionControls";
import { formatCost, formatTokens } from "../formatting";
import {
  summarizeUsageDays,
  usageCacheHitRate,
  usageDailySeries,
  usageDaysForRange,
  usageRangeFrom,
  type UsageDailyPoint,
  type UsageDrilldown,
} from "../usageDashboard";
import styles from "./UsagePage.module.css";

const UsageCharts = lazy(() => import("../components/UsageCharts"));

function formatUsageDuration(ms: number): string {
  if (ms < 1000) return `${ms.toFixed(1)} ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)} s`;
  if (ms < 3_600_000) return `${(ms / 60_000).toFixed(1)} min`;
  return `${(ms / 3_600_000).toFixed(1)} h`;
}

export default function UsagePage() {
  const [dashboard, setDashboard] = createSignal<UsageDashboardResp | null>(null);
  const [search, setSearch] = useSearchParams();
  const range = () => usageRangeFrom(typeof search.range === "string" ? search.range : "30");
  const drilldown = createMemo<UsageDrilldown | null>(() => {
    const harness = typeof search.harness === "string" ? search.harness : undefined;
    const model = typeof search.model === "string" ? search.model : undefined;
    return harness || model ? { harness, model } : null;
  });
  const [error, setError] = createSignal("");
  const [loading, setLoading] = createSignal(false);

  const refresh = async () => {
    setLoading(true);
    setError("");
    try {
      setDashboard(await api.getUsageDashboard());
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "Usage data could not be loaded. Try refreshing.");
    } finally {
      setLoading(false);
    }
  };

  onMount(() => {
    void refresh();
  });

  const days = createMemo(() => usageDaysForRange(dashboard()?.days ?? [], range()));
  const summary = createMemo(() => summarizeUsageDays(days(), drilldown()));
  const points = createMemo(() => usageDailySeries(dashboard()?.days ?? [], range(), drilldown()));
  const cacheHitRate = () => usageCacheHitRate(summary().tokens);

  // Router-owned filters restore on reload and browser history navigation.
  const applyDrilldown = (next: UsageDrilldown | null) => {
    setSearch({ harness: next?.harness, model: next?.model }, { replace: false, scroll: false });
  };
  const selectModel = (name: string) => applyDrilldown({ ...(drilldown() ?? {}), model: name });
  const selectHarness = (name: string) => applyDrilldown({ ...(drilldown() ?? {}), harness: name });
  const drillLabel = () => {
    const active = drilldown();
    if (!active) return "";
    const parts = [];
    if (active.model) parts.push(`model ${active.model}`);
    if (active.harness) parts.push(`harness ${active.harness}`);
    return parts.join(" in ");
  };

  return (
    <section aria-label="Usage">
      <SectionControls>
        <div class={styles.controls}>
          <select
            aria-label="Usage date range"
            value={range()}
            onChange={(event) => setSearch({ range: event.currentTarget.value }, { replace: false, scroll: false })}
          >
            <option value="7">Last 7 days</option>
            <option value="30">Last 30 days</option>
            <option value="90">Last 90 days</option>
            <option value="all">All retained days</option>
          </select>
          <ActionButton label="Refresh usage" loading={loading()} onClick={() => void refresh()}>
            <RefreshIcon width={20} height={20} />
          </ActionButton>
        </div>
      </SectionControls>
      <Show when={error()}>
        <p class={styles.error} role="alert">
          {error()}
        </p>
      </Show>

      <Show
        when={dashboard()}
        fallback={<p class={styles.empty}>{loading() ? "Loading usage…" : "Usage data is unavailable."}</p>}
      >
        <Show when={days().length > 0} fallback={<p class={styles.empty}>No usage was recorded in this range.</p>}>
          <Show when={drilldown()}>
            <p class={styles.drilldown} role="status">
              Showing usage for <strong>{drillLabel()}</strong>
              {" · "}
              <button type="button" class={styles.clearLink} onClick={() => applyDrilldown(null)}>
                Show all usage
              </button>
            </p>
          </Show>
          <section class={styles.metrics} aria-label="Usage summary">
            <div>
              <span>Total tokens</span>
              <strong>{formatTokens(summary().totalTokens)}</strong>
            </div>
            <div>
              <span>Turns</span>
              <strong>{summary().turns}</strong>
            </div>
            <div>
              <span>Cache hit rate</span>
              <strong>{(cacheHitRate() * 100).toFixed(1)}%</strong>
            </div>
            <div>
              <span>Reported cost</span>
              <strong>{formatCost(summary().costUSD)}</strong>
            </div>
            <div>
              <span>Skill task-days</span>
              <strong>{summary().skills.reduce((total, skill) => total + skill.count, 0)}</strong>
            </div>
            <div>
              <span>Compactions</span>
              <strong>{summary().compactions}</strong>
            </div>
            <div>
              <span>API time</span>
              <strong>{formatUsageDuration(summary().apiMs)}</strong>
            </div>
            <div>
              <span>Turn wall time</span>
              <strong>{formatUsageDuration(summary().wallMs)}</strong>
            </div>
            <div>
              <span>Errored turns</span>
              <strong>{summary().erroredTurns}</strong>
            </div>
            <div>
              <span>Subagent spawns</span>
              <strong>{summary().subagentSpawns}</strong>
            </div>
          </section>

          <Suspense fallback={<p class={styles.empty}>Loading charts…</p>}>
            <UsageCharts points={points()} subject={drillLabel()} />
          </Suspense>

          <div class={styles.lowerGrid}>
            <section class={styles.panel}>
              <h2>Models</h2>
              <Leaderboard entries={summary().models} showCost kind="model" onSelect={selectModel} />
            </section>
            <section class={styles.panel}>
              <h2>Harnesses</h2>
              <Leaderboard entries={summary().harnesses} showCost showCache kind="harness" onSelect={selectHarness} />
            </section>
            <section class={styles.panel}>
              <h2>Tools</h2>
              <ToolTable entries={summary().tools} />
            </section>
            <section class={styles.panel}>
              <h2>Repositories</h2>
              <CountList entries={summary().repos} empty="No repository activity." />
            </section>
            <section class={styles.panel}>
              <h2>Skills</h2>
              <CountList entries={summary().skills} empty="No skill use." />
            </section>
            <section class={styles.panel}>
              <h2>Daily totals</h2>
              <DailyTotals points={points()} />
            </section>
          </div>
        </Show>
      </Show>
    </section>
  );
}

function Leaderboard(props: {
  entries: ReturnType<typeof summarizeUsageDays>["models"];
  showCost: boolean;
  showCache?: boolean;
  kind?: "model" | "harness";
  onSelect?: (name: string) => void;
}) {
  return (
    <Show when={props.entries.length > 0} fallback={<p class={styles.noRows}>No usage in this range.</p>}>
      <div class={styles.tableWrap} role="region" aria-label="Usage leaderboard" tabIndex={0}>
        <table class={styles.table}>
          <thead>
            <tr>
              <th scope="col">Name</th>
              <th scope="col">Tokens</th>
              <th scope="col">Turns</th>
              <th scope="col">Compactions</th>
              <Show when={props.showCost}>
                <th scope="col">Cost</th>
              </Show>
              <Show when={props.showCache}>
                <th scope="col">Cache hit</th>
              </Show>
            </tr>
          </thead>
          <tbody>
            <For each={props.entries}>
              {(entry) => (
                <tr>
                  <th scope="row">
                    <Show when={props.kind && props.onSelect} fallback={entry.name}>
                      <button
                        type="button"
                        class={styles.rowButton}
                        title={`Show only ${props.kind} ${entry.name}`}
                        onClick={() => props.onSelect?.(entry.name)}
                      >
                        {entry.name}
                      </button>
                    </Show>
                  </th>
                  <td>{formatTokens(entry.tokens)}</td>
                  <td>{entry.turns}</td>
                  <td>{entry.compactions}</td>
                  <Show when={props.showCost}>
                    <td>{formatCost(entry.costUSD)}</td>
                  </Show>
                  <Show when={props.showCache}>
                    <td>{entry.cacheHitRate === undefined ? "—" : `${(entry.cacheHitRate * 100).toFixed(1)}%`}</td>
                  </Show>
                </tr>
              )}
            </For>
          </tbody>
        </table>
      </div>
    </Show>
  );
}

function DailyTotals(props: { points: readonly UsageDailyPoint[] }) {
  return (
    <div class={styles.tableWrap} role="region" aria-label="Daily usage totals" tabIndex={0}>
      <table class={styles.table}>
        <thead>
          <tr>
            <th scope="col">Day</th>
            <th scope="col">Tokens</th>
            <th scope="col">Cost</th>
          </tr>
        </thead>
        <tbody>
          <For each={[...props.points].reverse()}>
            {(point) => (
              <tr>
                <th scope="row">{point.inProgress ? `${point.day} (in progress)` : point.day}</th>
                <td>{formatTokens(point.tokens)}</td>
                <td>{formatCost(point.costUSD)}</td>
              </tr>
            )}
          </For>
        </tbody>
      </table>
    </div>
  );
}

function ToolTable(props: { entries: ReturnType<typeof summarizeUsageDays>["tools"] }) {
  return (
    <Show when={props.entries.length > 0} fallback={<p class={styles.noRows}>No tool calls in this range.</p>}>
      <div class={styles.tableWrap} role="region" aria-label="Tool durations" tabIndex={0}>
        <table class={styles.table}>
          <thead>
            <tr>
              <th scope="col">Tool</th>
              <th scope="col">Calls</th>
              <th scope="col">Timed</th>
              <th scope="col">Total</th>
              <th scope="col">Median</th>
            </tr>
          </thead>
          <tbody>
            <For each={props.entries}>
              {(entry) => (
                <tr>
                  <th scope="row" title={entry.name}>
                    {entry.name}
                  </th>
                  <td>{entry.calls}</td>
                  <td>{entry.timedCalls}</td>
                  <td>{entry.timedCalls ? formatUsageDuration(entry.durationMs) : "—"}</td>
                  <td>{entry.medianMs === null ? "—" : formatUsageDuration(entry.medianMs)}</td>
                </tr>
              )}
            </For>
          </tbody>
        </table>
      </div>
    </Show>
  );
}

function CountList(props: { entries: { name: string; count: number }[]; empty: string }) {
  return (
    <Show when={props.entries.length > 0} fallback={<p class={styles.noRows}>{props.empty}</p>}>
      <ol class={styles.countList}>
        <For each={props.entries}>
          {(entry) => (
            <li>
              <span>{entry.name}</span>
              <strong>{entry.count}</strong>
            </li>
          )}
        </For>
      </ol>
    </Show>
  );
}

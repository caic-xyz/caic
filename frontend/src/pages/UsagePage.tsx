// UsagePage is the /usage route for cross-task daily usage analytics.

import { createMemo, createSignal, For, lazy, onMount, Show, Suspense } from "solid-js";

import type { UsageDashboardResp } from "@sdk/types.gen";

import { api } from "../api";
import Button from "../components/Button";
import { Layout } from "../components/Layout";
import { formatCost, formatTokens } from "../formatting";
import {
  summarizeUsageDays,
  usageCacheHitRate,
  usageDaysForRange,
  usageRangeFrom,
  type UsageRange,
} from "../usageDashboard";
import styles from "./UsagePage.module.css";

const UsageCharts = lazy(() => import("../components/UsageCharts"));

function rangeLabel(range: UsageRange): string {
  return range === "all" ? "All retained days" : `Last ${range} days`;
}

function formatDay(day: string | undefined): string {
  if (!day) return "";
  const value = new Date(`${day}T00:00:00Z`);
  return Number.isNaN(value.getTime()) ? day : value.toLocaleDateString();
}

function formatUsageDuration(ms: number): string {
  if (ms < 1000) return `${ms.toFixed(1)} ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)} s`;
  if (ms < 3_600_000) return `${(ms / 60_000).toFixed(1)} min`;
  return `${(ms / 3_600_000).toFixed(1)} h`;
}

export default function UsagePage() {
  const [dashboard, setDashboard] = createSignal<UsageDashboardResp | null>(null);
  const [range, setRange] = createSignal<UsageRange>("30");
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
  const summary = createMemo(() => summarizeUsageDays(days()));
  const cacheHitRate = () => usageCacheHitRate(summary().tokens);
  const dataSince = () => formatDay(dashboard()?.dataSince);

  return (
    <Layout>
      <main class={styles.page}>
        <div class={styles.header}>
          <div>
            <h1>Usage</h1>
            <p>Cross-task activity from the daily usage rollup. Repository counts are distinct task-days.</p>
          </div>
          <div class={styles.controls}>
            <label>
              <span class={styles.controlLabel}>Range</span>
              <select
                aria-label="Usage date range"
                value={range()}
                onChange={(event) => setRange(usageRangeFrom(event.currentTarget.value))}
              >
                <option value="7">Last 7 days</option>
                <option value="30">Last 30 days</option>
                <option value="90">Last 90 days</option>
                <option value="all">All retained days</option>
              </select>
            </label>
            <Button type="button" variant="gray" loading={loading()} onClick={() => void refresh()}>
              {loading() ? "Refreshing…" : "Refresh"}
            </Button>
          </div>
        </div>

        <Show when={dataSince()}>
          <p class={styles.meta}>
            Data since {dataSince()} · {rangeLabel(range())}
          </p>
        </Show>
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
              <UsageCharts days={days()} />
            </Suspense>

            <div class={styles.lowerGrid}>
              <section class={styles.panel}>
                <h2>Models</h2>
                <p>Tokens, turns, and reported cost over the selected range.</p>
                <Leaderboard entries={summary().models} showCost />
              </section>
              <section class={styles.panel}>
                <h2>Harnesses</h2>
                <p>Tokens, turns, reported cost, and cached input over the selected range.</p>
                <Leaderboard entries={summary().harnesses} showCost showCache />
              </section>
              <section class={styles.panel}>
                <h2>Tools</h2>
                <p>
                  Calls count on their start day; timed calls and duration count on completion day. Total time sums
                  concurrent calls, and average uses measured calls only.
                </p>
                <ToolTable entries={summary().tools} />
              </section>
              <section class={styles.panel}>
                <h2>Repositories</h2>
                <p>Task-days over the selected range.</p>
                <CountList entries={summary().repos} empty="No repository activity." />
              </section>
              <section class={styles.panel}>
                <h2>Skills</h2>
                <p>Task-days over the selected range.</p>
                <CountList entries={summary().skills} empty="No skill use." />
              </section>
            </div>
          </Show>
        </Show>
      </main>
    </Layout>
  );
}

function Leaderboard(props: {
  entries: ReturnType<typeof summarizeUsageDays>["models"];
  showCost: boolean;
  showCache?: boolean;
}) {
  return (
    <Show when={props.entries.length > 0} fallback={<p class={styles.noRows}>No usage in this range.</p>}>
      <div
        class={styles.tableWrap}
        role="region"
        aria-label="Usage leaderboard"
        // eslint-disable-next-line jsx-a11y/no-noninteractive-tabindex -- overflow region must be keyboard-scrollable
        tabIndex={0}
      >
        <table class={styles.table}>
          <thead>
            <tr>
              <th scope="col">Name</th>
              <th scope="col">Tokens</th>
              <th scope="col">Turns</th>
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
                  <th scope="row">{entry.name}</th>
                  <td>{formatTokens(entry.tokens)}</td>
                  <td>{entry.turns}</td>
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

function ToolTable(props: { entries: ReturnType<typeof summarizeUsageDays>["tools"] }) {
  return (
    <Show when={props.entries.length > 0} fallback={<p class={styles.noRows}>No tool calls in this range.</p>}>
      <div
        class={styles.tableWrap}
        role="region"
        aria-label="Tool durations"
        // eslint-disable-next-line jsx-a11y/no-noninteractive-tabindex -- overflow region must be keyboard-scrollable
        tabIndex={0}
      >
        <table class={styles.table}>
          <thead>
            <tr>
              <th scope="col">Tool</th>
              <th scope="col">Calls</th>
              <th scope="col">Timed</th>
              <th scope="col">Total</th>
              <th scope="col">Average</th>
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
                  <td>{entry.timedCalls ? formatUsageDuration(entry.durationMs / entry.timedCalls) : "—"}</td>
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

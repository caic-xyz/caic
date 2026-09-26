// UsagePage is the /usage route for cross-task daily usage analytics.

import { createMemo, createSignal, For, lazy, onCleanup, onMount, Show, Suspense } from "solid-js";

import type { UsageDashboardResp } from "@sdk/types.gen";

import { api } from "../api";
import Button from "../components/Button";
import { Layout } from "../components/Layout";
import { formatCost, formatTokens } from "../formatting";
import {
  drilldownEqual,
  drilldownFromHash,
  hashFromDrilldown,
  summarizeUsageDays,
  usageCacheHitRate,
  usageDailySeries,
  usageDaysForRange,
  usageRangeFrom,
  type UsageDrilldown,
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
  const [drilldown, setDrilldown] = createSignal<UsageDrilldown | null>(null);
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
  const points = createMemo(() => usageDailySeries(days(), drilldown()));
  const cacheHitRate = () => usageCacheHitRate(summary().tokens);
  const dataSince = () => formatDay(dashboard()?.dataSince);

  // applyDrilldown keeps the location hash fragment in sync so a filtered
  // view can be shared or restored after a reload; history navigation and
  // manual hash edits flow back through the hashchange listener.
  const applyDrilldown = (next: UsageDrilldown | null) => {
    setDrilldown(next);
    const fragment = hashFromDrilldown(next);
    const url = fragment || `${window.location.pathname}${window.location.search}`;
    if (!window.location.href.endsWith(url)) {
      window.history.pushState(null, "", url);
    }
  };
  const onHashChange = () => {
    const next = drilldownFromHash(window.location.hash);
    if (!drilldownEqual(next, drilldown())) setDrilldown(next);
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

  onMount(() => {
    void refresh();
    setDrilldown(drilldownFromHash(window.location.hash));
    window.addEventListener("hashchange", onHashChange);
    window.addEventListener("popstate", onHashChange);
  });
  onCleanup(() => {
    window.removeEventListener("hashchange", onHashChange);
    window.removeEventListener("popstate", onHashChange);
  });

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
            <Show when={drillLabel()}> · filtered to {drillLabel()}</Show>
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
                <p>
                  Tokens, turns, and reported cost over the selected range. Select a row to drill every panel down to
                  that model.
                </p>
                <Leaderboard entries={summary().models} showCost kind="model" onSelect={selectModel} />
              </section>
              <section class={styles.panel}>
                <h2>Harnesses</h2>
                <p>
                  Tokens, turns, reported cost, and cached input over the selected range. Select a row to drill every
                  panel down to that harness.
                </p>
                <Leaderboard entries={summary().harnesses} showCost showCache kind="harness" onSelect={selectHarness} />
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
  kind?: "model" | "harness";
  onSelect?: (name: string) => void;
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

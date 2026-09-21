// MetricsPage is the /metrics route for server measurements.

import { For, Show, createSignal, onMount } from "solid-js";

import type { MetricSeries, MetricsResp } from "@sdk/types.gen";

import { api } from "../api";
import Button from "../components/Button";
import { Layout } from "../components/Layout";
import { formatBytes, formatDuration } from "../formatting";
import styles from "./MetricsPage.module.css";

const noMetricSeries: MetricsResp["series"] = [];

type Stat = "sum" | "p50" | "p95" | "max" | "last";

// A statistic only means something for some kinds: a counter has a running
// total, and a gauge has a current value and an observed range, but neither has
// a distribution.
function hasStat(kind: string, stat: Stat): boolean {
  switch (kind) {
    case "counter":
    case "updowncounter":
      return stat === "sum" || stat === "last";
    case "gauge":
      return stat === "max" || stat === "last";
    default:
      return true;
  }
}

function formatAmount(amount: number, unit: string): string {
  switch (unit) {
    case "s":
      return formatDuration(amount);
    case "By":
      return formatBytes(amount);
    default:
      return Number.isInteger(amount) ? String(amount) : String(Math.round(amount * 100) / 100);
  }
}

function formatStat(metric: MetricSeries, stat: Stat): string {
  return hasStat(metric.kind, stat) ? formatAmount(metric[stat], metric.unit) : "—";
}

function formatAttrs(attrs: MetricSeries["attrs"]): string {
  if (!attrs) return "—";
  const entries = Object.entries(attrs);
  return entries.length === 0 ? "—" : entries.map(([key, value]) => `${key}=${value}`).join(", ");
}

export default function MetricsPage() {
  const [metrics, setMetrics] = createSignal<MetricsResp | null>(null);
  const [error, setError] = createSignal("");
  const [loading, setLoading] = createSignal(false);

  const refresh = async () => {
    setLoading(true);
    setError("");
    try {
      setMetrics(await api.getMetrics());
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "Could not load metrics");
    } finally {
      setLoading(false);
    }
  };

  onMount(() => {
    void refresh();
  });

  const series = () => metrics()?.series ?? noMetricSeries;
  const identity = () => {
    const resource = metrics()?.resource;
    if (!resource?.serviceName) return "";
    const name = resource.serviceVersion ? `${resource.serviceName} ${resource.serviceVersion}` : resource.serviceName;
    return resource.host ? `${name} on ${resource.host}.` : `${name}.`;
  };
  const since = () => {
    const value = metrics()?.since;
    if (!value) return "";
    const date = new Date(value);
    return Number.isNaN(date.getTime()) ? "" : `Recording since ${date.toLocaleString()}.`;
  };
  const meta = () => [identity(), since()].filter(Boolean).join(" ");

  return (
    <Layout>
      <div class={styles.metricsPage}>
        <div class={styles.metricsPanel}>
          <h2 class={styles.metricsPanelTitle}>Metrics</h2>
          <p class={styles.metricsDescription}>
            Measurements of server operations and resources. A histogram is read through its percentiles, a counter
            through its total, and a gauge through its current value and observed range. Percentiles and totals cover
            the most recent retained measurements per series.
          </p>
          <Show when={meta()}>
            <p class={styles.metricsMeta}>{meta()}</p>
          </Show>
          <Show when={error()}>
            <p class={styles.metricsError} role="alert">
              {error()}
            </p>
          </Show>
          <Show when={series().length > 0} fallback={<p class={styles.metricsDescription}>Nothing recorded yet.</p>}>
            <div class={styles.tableScroll}>
              <table class={styles.table}>
                <thead>
                  <tr>
                    <th scope="col">Name</th>
                    <th scope="col">Kind</th>
                    <th scope="col">Outcome</th>
                    <th scope="col">Attributes</th>
                    <th scope="col">Count</th>
                    <th scope="col">Total</th>
                    <th scope="col">p50</th>
                    <th scope="col">p95</th>
                    <th scope="col">Max</th>
                    <th scope="col">Last</th>
                  </tr>
                </thead>
                <tbody>
                  <For each={series()}>
                    {(metric) => (
                      <tr>
                        <th scope="row">{metric.name}</th>
                        <td>{metric.kind}</td>
                        <td>{metric.outcome}</td>
                        <td>{formatAttrs(metric.attrs)}</td>
                        <td>{metric.count}</td>
                        <td>{formatStat(metric, "sum")}</td>
                        <td>{formatStat(metric, "p50")}</td>
                        <td>{formatStat(metric, "p95")}</td>
                        <td>{formatStat(metric, "max")}</td>
                        <td>{formatStat(metric, "last")}</td>
                      </tr>
                    )}
                  </For>
                </tbody>
              </table>
            </div>
          </Show>
          <div class={styles.actions}>
            <Button type="button" variant="gray" loading={loading()} onClick={() => void refresh()}>
              {loading() ? "Refreshing…" : "Refresh"}
            </Button>
          </div>
        </div>
      </div>
    </Layout>
  );
}

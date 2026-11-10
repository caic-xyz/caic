// MetricsPage shows server operation measurements in Monitoring.

import { For, Show, createSignal, onMount } from "solid-js";
import RefreshIcon from "@material-symbols/svg-400/outlined/refresh.svg?solid";

import type { MetricSeries, MetricsResp } from "@sdk/types.gen";

import { api } from "../api";
import ActionButton from "../components/ActionButton";
import MetricDistribution from "../components/MetricDistribution";
import Table from "../components/Table";
import SectionControls from "../components/SectionControls";
import { formatBytes, formatDuration } from "../formatting";
import styles from "./MetricsPage.module.css";

const noMetricSeries: NonNullable<MetricsResp["series"]> = [];

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
    <section aria-label="Metrics">
      <SectionControls>
        <div class={styles.controls}>
          <ActionButton label="Refresh metrics" loading={loading()} onClick={() => void refresh()}>
            <RefreshIcon width={20} height={20} />
          </ActionButton>
        </div>
      </SectionControls>
      <div class={styles.metricsPanel}>
        <Show when={meta()}>
          <p class={styles.metricsMeta}>{meta()}</p>
        </Show>
        <Show when={error()}>
          <p class={styles.metricsError} role="alert">
            {error()}
          </p>
        </Show>
        <Show when={series().length > 0} fallback={<p class={styles.metricsEmpty}>Nothing recorded yet.</p>}>
          <Table class={styles.tableScroll} label="Operation measurements">
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Kind</th>
                <th scope="col">Outcome</th>
                <th scope="col">Attributes</th>
                <th scope="col">Count</th>
                <th scope="col">Total</th>
                <th scope="col">Spread</th>
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
                    <td class={styles.spreadCell}>
                      <MetricDistribution
                        percentiles={hasStat(metric.kind, "p95")}
                        format={(value) => formatAmount(value, metric.unit)}
                        p50={metric.p50}
                        p95={metric.p95}
                        max={metric.max}
                      />
                    </td>
                    <td>{formatStat(metric, "p50")}</td>
                    <td>{formatStat(metric, "p95")}</td>
                    <td>{formatStat(metric, "max")}</td>
                    <td>{formatStat(metric, "last")}</td>
                  </tr>
                )}
              </For>
            </tbody>
          </Table>
        </Show>
      </div>
    </section>
  );
}

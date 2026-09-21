// MetricsPage is the /metrics route for server operation latency.

import { For, Show, createSignal, onMount } from "solid-js";

import type { MetricsResp } from "@sdk/types.gen";

import { api } from "../api";
import Button from "../components/Button";
import { Layout } from "../components/Layout";
import styles from "./MetricsPage.module.css";

const noMetricSeries: MetricsResp["series"] = [];

function formatMillis(ms: number): string {
  if (ms < 1) return "<1 ms";
  if (ms < 1000) return `${ms.toFixed(0)} ms`;
  return `${Math.round(ms / 10) / 100} s`;
}

function formatAttrs(attrs: MetricsResp["series"][number]["attrs"]): string {
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
            Duration of server operations such as container launch, diff, and push, slowest p95 first. Percentiles cover
            the most recent retained calls per operation.
          </p>
          <Show when={meta()}>
            <p class={styles.metricsMeta}>{meta()}</p>
          </Show>
          <Show when={error()}>
            <p class={styles.metricsError} role="alert">
              {error()}
            </p>
          </Show>
          <Show
            when={series().length > 0}
            fallback={<p class={styles.metricsDescription}>No operations recorded yet.</p>}
          >
            <div class={styles.tableScroll}>
              <table class={styles.table}>
                <thead>
                  <tr>
                    <th scope="col">Operation</th>
                    <th scope="col">Outcome</th>
                    <th scope="col">Attributes</th>
                    <th scope="col">Calls</th>
                    <th scope="col">p50</th>
                    <th scope="col">p95</th>
                    <th scope="col">Max</th>
                  </tr>
                </thead>
                <tbody>
                  <For each={series()}>
                    {(metric) => (
                      <tr>
                        <th scope="row">{metric.name}</th>
                        <td>{metric.outcome}</td>
                        <td>{formatAttrs(metric.attrs)}</td>
                        <td>{metric.calls}</td>
                        <td>{formatMillis(metric.p50Ms)}</td>
                        <td>{formatMillis(metric.p95Ms)}</td>
                        <td>{formatMillis(metric.maxMs)}</td>
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

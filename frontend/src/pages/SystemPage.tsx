// SystemPage is Monitoring's live whole-machine resource history and logical-CPU view.

import { For, Show, Suspense, createMemo, createSignal, lazy, onCleanup, onMount } from "solid-js";
import RefreshIcon from "@material-symbols/svg-400/outlined/refresh.svg?solid";
import type { SystemStatsResp } from "@sdk/types.gen";

import { api } from "../api";
import ActionButton from "../components/ActionButton";
import type { ResourceSample } from "../components/StatsCharts";
import SectionControls from "../components/SectionControls";
import styles from "./SystemPage.module.css";

const statsCharts = () => import("../components/StatsCharts");
const ResourceCharts = lazy(() => statsCharts().then((m) => ({ default: m.ResourceCharts })));
const ResourceSamples = lazy(() => statsCharts().then((m) => ({ default: m.ResourceSamples })));

export default function SystemPage() {
  const [data, setData] = createSignal<SystemStatsResp | null>(null);
  const [error, setError] = createSignal("");
  const [refreshing, setRefreshing] = createSignal(false);
  let active = true;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const refresh = async () => {
    // A manual refresh restarts the polling cadence instead of adding to it.
    clearTimeout(timer);
    try {
      const result = await api.getSystemStats();
      if (active) {
        setData(result);
        setError(result.error ?? "");
      }
    } catch (e: unknown) {
      if (active) setError(e instanceof Error ? e.message : "Could not load system utilization");
    } finally {
      if (active) timer = setTimeout(() => void refresh(), 5000);
    }
  };
  const refreshNow = async () => {
    setRefreshing(true);
    try {
      await refresh();
    } finally {
      setRefreshing(false);
    }
  };
  onMount(() => void refresh());
  onCleanup(() => {
    active = false;
    clearTimeout(timer);
  });
  const stats = createMemo<ResourceSample[]>((previous) => {
    const samples = data()?.samples ?? [];
    const last = samples.at(-1);
    // The collector only appends immutable samples and evicts the oldest.
    if (samples.length === previous.length && (!last || new Date(last.time).getTime() === previous.at(-1)?.ts)) {
      return previous;
    }
    return samples.map((s) => ({
      ts: new Date(s.time).getTime(),
      cpuPerc: (s.cpus ?? []).reduce((sum, cpu) => sum + cpu.usagePerc, 0),
      cpuCount: (s.cpus ?? []).length,
      cpuCeiling: null,
      cpuQuota: null,
      runtimeCPUs: null,
      memUsed: s.memUsed,
      memLimit: s.memTotal,
      memPerc: s.memPerc,
      netRx: s.netRx,
      netTx: s.netTx,
      blockRead: 0,
      blockWrite: 0,
      diskUsed: s.diskUsed,
      diskTotal: s.diskTotal,
    }));
  }, []);
  const latest = () => data()?.samples?.at(-1);
  return (
    <section class={styles.page} aria-label="System">
      <SectionControls>
        <div class={styles.controls}>
          <ActionButton label="Refresh system" loading={refreshing()} onClick={() => void refreshNow()}>
            <RefreshIcon width={20} height={20} />
          </ActionButton>
        </div>
      </SectionControls>
      <Show when={error()}>
        <p role="alert" class={styles.error}>
          {error()} Retained samples may be out of date.
        </p>
      </Show>
      <Show when={stats().length > 0} fallback={<p>Waiting for system samples…</p>}>
        <Suspense fallback={<p>Loading charts…</p>}>
          <ResourceCharts samples={false} stats={stats()} scope="machine" />
          <h3 class={styles.cpuTitle}>Logical CPUs ({latest()?.cpus?.length ?? 0})</h3>
          <div class={styles.cpuGrid}>
            <For each={latest()?.cpus ?? []}>
              {(cpu) => (
                <div class={styles.cpuRow}>
                  <span>{cpu.id}</span>
                  <meter min={0} max={100} value={cpu.usagePerc} aria-label={`${cpu.id} utilization`} />
                  <span>{cpu.usagePerc.toFixed(1)}%</span>
                </div>
              )}
            </For>
          </div>
          <ResourceSamples scope="machine" stats={stats()} />
        </Suspense>
      </Show>
    </section>
  );
}

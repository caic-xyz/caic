// SystemPage is Monitoring's live whole-machine resource history and logical-CPU view.

import { For, Show, Suspense, createMemo, createSignal, lazy, onCleanup, onMount } from "solid-js";
import type { SystemStatsResp } from "@sdk/types.gen";

import { api } from "../api";
import type { ResourceSample } from "../components/StatsCharts";
import styles from "./SystemPage.module.css";

const ResourceCharts = lazy(() => import("../components/StatsCharts").then((m) => ({ default: m.ResourceCharts })));

export default function SystemPage() {
  const [data, setData] = createSignal<SystemStatsResp | null>(null);
  const [error, setError] = createSignal("");
  const [cpuDetails, setCPUDetails] = createSignal(false);
  let active = true;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const refresh = async () => {
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
      cpuPerc: s.cpus.reduce((sum, cpu) => sum + cpu.usagePerc, 0),
      cpuCount: s.cpus.length,
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
  const latest = () => data()?.samples.at(-1);
  return (
    <section class={styles.page} aria-label="System">
      <h2>System</h2>
      <p class={styles.description}>
        The machine running caic, including all processes. Five-second samples; five minutes of history. CPU usage is
        100% per fully busy logical CPU. Memory excludes available and reclaimable cache. Network totals include all
        interfaces, so traffic may be counted on multiple interfaces. Disk usage covers the filesystem containing the
        server working directory.
      </p>
      <Show when={error()}>
        <p role="alert" class={styles.error}>
          {error()} Retained samples may be out of date.
        </p>
      </Show>
      <Show when={stats().length > 0} fallback={<p>Waiting for system samples…</p>}>
        <Suspense fallback={<p>Loading charts…</p>}>
          <ResourceCharts stats={stats()} scope="machine" />
          <details class={styles.cpuDetails} onToggle={(event) => setCPUDetails(event.currentTarget.open)}>
            <summary>Logical CPUs ({latest()?.cpus.length ?? 0})</summary>
            <Show when={cpuDetails()}>
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
            </Show>
          </details>
        </Suspense>
      </Show>
    </section>
  );
}

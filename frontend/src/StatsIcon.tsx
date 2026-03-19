// StatsIcon renders a 2×2 bar-chart icon in the task header that opens a popup
// showing container resource history (CPU, MEM, NET, DISK) and per-turn perf data.
import { createSignal, For, Show } from "solid-js";
import type { EventStats } from "@sdk/types.gen";
import type { Session } from "./grouping";
import { formatDuration, formatTokens } from "./formatting";
import styles from "./StatsIcon.module.css";

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

interface TurnPerf {
  index: number;
  result: NonNullable<Session["turns"][number]["result"]>;
}

function collectTurnPerfs(sessions: Session[]): TurnPerf[] {
  const perfs: TurnPerf[] = [];
  let idx = 0;
  for (const session of sessions) {
    for (const turn of session.turns) {
      if (turn.result) perfs.push({ index: idx, result: turn.result });
      idx++;
    }
  }
  return perfs;
}

export default function StatsIcon(props: { stats: EventStats[]; sessions: Session[] }) {
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

  const perfs = () => collectTurnPerfs(props.sessions);

  return (
    <div class={styles.wrapper}>
      <button
        class={`${styles.iconBtn}${open() ? ` ${styles.iconBtnActive}` : ""}`}
        onClick={() => setOpen((v) => !v)}
        title="Container resource stats"
        aria-label="Resource stats"
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
      </button>
      <Show when={open()}>
        <div class={styles.popup}>
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
              <table class={styles.perfTable}>
                <thead>
                  <tr>
                    <th class={styles.perfTh}>#</th>
                    <th class={styles.perfTh}>Wall</th>
                    <th class={styles.perfTh}>API</th>
                    <th class={styles.perfTh}>Cost</th>
                    <th class={styles.perfTh}>Tokens</th>
                  </tr>
                </thead>
                <tbody>
                  <For each={perfs()}>
                    {(p) => {
                      const r = p.result;
                      const totalTokens = r.usage.inputTokens + r.usage.cacheCreationInputTokens + r.usage.cacheReadInputTokens + r.usage.outputTokens;
                      return (
                        <tr>
                          <td class={styles.perfTd}>{p.index + 1}</td>
                          <td class={styles.perfTd}>{formatDuration(r.duration)}</td>
                          <td class={styles.perfTd}>{formatDuration(r.durationAPI)}</td>
                          <td class={styles.perfTd}>{r.totalCostUSD > 0 ? `$${r.totalCostUSD.toFixed(4)}` : "—"}</td>
                          <td class={styles.perfTd}>{formatTokens(totalTokens)}</td>
                        </tr>
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

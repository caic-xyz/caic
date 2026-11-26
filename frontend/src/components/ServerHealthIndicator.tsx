// Always-visible CPU and memory levels link to System with accessible readings and sampling/connection status.

import CPUIcon from "@material-symbols/svg-400/outlined/memory.svg?solid";
import MemoryIcon from "@material-symbols/svg-400/outlined/memory_alt.svg?solid";
import type { ServerHealth } from "@sdk/types.gen";

import { formatBytes } from "../formatting";
import { hostCPUPercent, hostMemoryPercent, type ResourceLevel } from "../resourceLevel";
import styles from "./ServerHealthIndicator.module.css";
import Tooltip from "./Tooltip";

const levelClass: Record<ResourceLevel, string> = {
  high: styles.high,
  normal: styles.normal,
  unavailable: styles.unavailable,
  warning: styles.warning,
};

export default function ServerHealthIndicator(props: { health: ServerHealth | null; connected: boolean }) {
  const level = (pick: (h: ServerHealth) => ResourceLevel | undefined): ResourceLevel => {
    const h = props.health;
    return (h && props.connected && !h.error && pick(h)) || "unavailable";
  };
  const cpuLevel = () => level((h) => h.cpuLevel);
  const memoryLevel = () => level((h) => h.memLevel);
  const description = () => {
    const h = props.health;
    if (!h) return `Waiting for the first system sample${!props.connected ? "\nUpdates unavailable" : ""}`;
    const percent = (p: number | null) => (p === null ? "capacity unknown" : `${p.toFixed(1)}%`);
    const cpu = percent(hostCPUPercent(h));
    const memory = percent(hostMemoryPercent(h));
    const status = [h.error ? `Sampling error: ${h.error}` : "", !props.connected ? "Updates unavailable" : ""]
      .filter(Boolean)
      .join("\n");
    return `CPU: ${h.cpuPerc.toFixed(1)}% · ${(h.cpuPerc / 100).toFixed(2)} / ${h.cpuCount} CPUs (${cpu})\nRAM: ${formatBytes(h.memUsed)} / ${formatBytes(h.memTotal)} (${memory})${status ? `\n${status}` : ""}`;
  };
  const label = () => `CPU ${cpuLevel()}, memory ${memoryLevel()} — open System`;
  return (
    <span class={styles.wrapper} data-testid="server-health-slot">
      <Tooltip
        text={description()}
        trigger={(tip) => (
          <a {...tip} link class={styles.link} href="/monitoring?section=system" aria-label={label()}>
            <CPUIcon class={levelClass[cpuLevel()]} aria-hidden="true" data-testid="cpu-alarm" />
            <MemoryIcon class={levelClass[memoryLevel()]} aria-hidden="true" data-testid="memory-alarm" />
          </a>
        )}
      />
    </span>
  );
}

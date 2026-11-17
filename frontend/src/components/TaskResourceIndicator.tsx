// Compact CPU and memory levels link to task performance with container-relative readings and host attribution.

import { A } from "@solidjs/router";

import CPUIcon from "@material-symbols/svg-400/outlined/memory.svg?solid";
import MemoryIcon from "@material-symbols/svg-400/outlined/memory_alt.svg?solid";
import type { EventStats } from "@sdk/types.gen";

import { formatBytes } from "../formatting";
import {
  busiestShare,
  isSampleFresh,
  taskLevel,
  type HostPressure,
  type HostShare,
  type ResourceLevel,
} from "../resourceLevel";
import styles from "./TaskResourceIndicator.module.css";

const levelClass: Record<ResourceLevel, string> = {
  high: styles.high,
  normal: styles.normal,
  unavailable: styles.unavailable,
  warning: styles.warning,
};

export default function TaskResourceIndicator(props: {
  href: string;
  sample: EventStats | null;
  now: number;
  hostPressure: HostPressure;
}) {
  const fresh = () => (props.sample && isSampleFresh(props.sample, props.now) ? props.sample : null);
  const cpu = () => {
    const s = props.sample;
    return s && !s.cpuError && s.cpuCeiling && s.cpuCeiling > 0 ? s.cpuPerc / s.cpuCeiling : null;
  };
  const memory = () => {
    const s = props.sample;
    return s && s.memLimit > 0 ? (s.memUsed / s.memLimit) * 100 : null;
  };
  // Stale samples are excluded from the busiest task, so they take no host share.
  const cpuShare = () => (fresh() ? props.hostPressure.cpu : null);
  const memoryShare = () => (fresh() ? props.hostPressure.memory : null);
  const cpuLevel = () => {
    const s = fresh();
    return s ? taskLevel(cpu(), s.cpuPerc, cpuShare()) : "unavailable";
  };
  const memoryLevel = () => {
    const s = fresh();
    return s ? taskLevel(memory(), s.memUsed, memoryShare()) : "unavailable";
  };
  const hostLine = (name: string, usage: number, share: HostShare | null) =>
    share
      ? [`Host ${name} ${share.hostPercent.toFixed(1)}%: ${busiestShare(usage, share).toFixed(0)}% of busiest task`]
      : [];
  const description = () => {
    const s = props.sample;
    if (!s) return "Resources: pending";
    const c = cpu();
    const m = memory();
    return [
      `CPU: ${(s.cpuPerc / 100).toFixed(2)} / ${s.cpuCeiling ?? "??"} CPUs (${c === null ? "??" : `${c.toFixed(1)}%`})`,
      `RAM: ${formatBytes(s.memUsed)} / ${s.memLimit > 0 ? formatBytes(s.memLimit) : "??"} (${m === null ? "??" : `${m.toFixed(1)}%`})`,
      ...hostLine("CPU", s.cpuPerc, cpuShare()),
      ...hostLine("RAM", s.memUsed, memoryShare()),
      ...(s.cpuError ? ["CPU metadata: unavailable"] : []),
      ...(fresh() ? [] : ["Sample: stale"]),
    ].join("\n");
  };
  return (
    <A
      class={styles.levels}
      href={props.href}
      title={description()}
      aria-label={`Task performance: ${description()}`}
      data-testid="task-resource-levels"
      onClick={(event) => event.stopPropagation()}
    >
      <span class={levelClass[cpuLevel()]} data-testid="task-cpu-level">
        <CPUIcon aria-hidden="true" />
      </span>
      <span class={levelClass[memoryLevel()]} data-testid="task-memory-level">
        <MemoryIcon aria-hidden="true" />
      </span>
    </A>
  );
}

// Compact CPU and memory levels link to task performance with accessible container-relative readings.

import { A } from "@solidjs/router";

import CPUIcon from "@material-symbols/svg-400/outlined/memory.svg?solid";
import MemoryIcon from "@material-symbols/svg-400/outlined/memory_alt.svg?solid";
import type { EventStats } from "@sdk/types.gen";

import { formatBytes } from "../formatting";
import styles from "./TaskResourceIndicator.module.css";

export default function TaskResourceIndicator(props: { href: string; sample: EventStats | null; now: number }) {
  const stale = () => !props.sample || props.now - props.sample.ts > 15_000;
  const cpu = () => {
    const s = props.sample;
    return s && !s.cpuError && s.cpuCeiling && s.cpuCeiling > 0 ? s.cpuPerc / s.cpuCeiling : null;
  };
  const memory = () => {
    const s = props.sample;
    return s && s.memLimit > 0 ? (s.memUsed / s.memLimit) * 100 : null;
  };
  const level = (percent: number | null) => {
    if (stale() || percent === null) return styles.unavailable;
    if (percent >= 90) return styles.high;
    if (percent >= 80) return styles.warning;
    return styles.normal;
  };
  const description = () => {
    const s = props.sample;
    if (!s) return "Container resources: waiting for the first sample";
    const c = cpu();
    const m = memory();
    return [
      `CPU: ${(s.cpuPerc / 100).toFixed(2)} / ${s.cpuCeiling ?? "unknown"} CPUs (${c === null ? "configured ceiling unknown" : `${c.toFixed(1)}% of configured ceiling`})`,
      "Inherited CPU limits and process affinity are unreported.",
      `RAM: ${formatBytes(s.memUsed)} / ${s.memLimit > 0 ? formatBytes(s.memLimit) : "unknown"} (${m === null ? "limit unknown" : `${m.toFixed(1)}% of limit`})`,
      ...(s.cpuError ? [`CPU metadata warning: ${s.cpuError}`] : []),
      ...(stale() ? ["Updates unavailable; showing last sample"] : []),
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
      <span class={level(cpu())} data-testid="task-cpu-level">
        <CPUIcon aria-hidden="true" />
      </span>
      <span class={level(memory())} data-testid="task-memory-level">
        <MemoryIcon aria-hidden="true" />
      </span>
    </A>
  );
}

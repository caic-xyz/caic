// Always-visible CPU and memory levels link to System with accessible readings and sampling/connection status.

import { createEffect, createSignal, createUniqueId, onCleanup, Show } from "solid-js";
import CPUIcon from "@material-symbols/svg-400/outlined/memory.svg?solid";
import MemoryIcon from "@material-symbols/svg-400/outlined/memory_alt.svg?solid";
import type { ServerHealth } from "@sdk/types.gen";

import { formatBytes } from "../formatting";
import styles from "./ServerHealthIndicator.module.css";

type Level = "high" | "normal" | "unavailable" | "warning";

const levelClass: Record<Level, string> = {
  high: styles.high,
  normal: styles.normal,
  unavailable: styles.unavailable,
  warning: styles.warning,
};

export default function ServerHealthIndicator(props: { health: ServerHealth | null; connected: boolean }) {
  const [open, setOpen] = createSignal(false);
  const id = createUniqueId();
  const level = (percent: number | null): Level => {
    if (!props.connected || props.health?.error || percent === null) return "unavailable";
    if (percent >= 90) return "high";
    if (percent >= 80) return "warning";
    return "normal";
  };
  const cpuLevel = () =>
    level(props.health && props.health.cpuCount > 0 ? props.health.cpuPerc / props.health.cpuCount : null);
  const memoryLevel = () =>
    level(props.health && props.health.memTotal > 0 ? (props.health.memUsed / props.health.memTotal) * 100 : null);
  const description = () => {
    const h = props.health;
    if (!h) return `Waiting for the first system sample${!props.connected ? "\nUpdates unavailable" : ""}`;
    const cpu = h.cpuCount > 0 ? `${(h.cpuPerc / h.cpuCount).toFixed(1)}%` : "capacity unknown";
    const memory = h.memTotal > 0 ? `${((h.memUsed / h.memTotal) * 100).toFixed(1)}%` : "capacity unknown";
    const status = [h.error ? `Sampling error: ${h.error}` : "", !props.connected ? "Updates unavailable" : ""]
      .filter(Boolean)
      .join("\n");
    return `CPU: ${h.cpuPerc.toFixed(1)}% · ${(h.cpuPerc / 100).toFixed(2)} / ${h.cpuCount} CPUs (${cpu})\nRAM: ${formatBytes(h.memUsed)} / ${formatBytes(h.memTotal)} (${memory})${status ? `\n${status}` : ""}`;
  };
  const label = () => `CPU ${cpuLevel()}, memory ${memoryLevel()} — open System`;
  createEffect(() => {
    if (!open()) return;
    // A hover tooltip may be open while focus is elsewhere. Dismiss before
    // page shortcuts, but let foreground dialogs and menus own Escape first.
    const dismiss = (event: KeyboardEvent) => {
      if (
        event.key !== "Escape" ||
        event.defaultPrevented ||
        document.querySelector('dialog[open], [role="dialog"], [role="menu"], [role="listbox"]')
      )
        return;
      event.preventDefault();
      event.stopImmediatePropagation();
      setOpen(false);
    };
    document.addEventListener("keydown", dismiss, true);
    onCleanup(() => document.removeEventListener("keydown", dismiss, true));
  });
  return (
    <span
      role="presentation"
      class={styles.wrapper}
      onMouseLeave={() => setOpen(false)}
      data-testid="server-health-slot"
    >
      <a
        link
        class={styles.link}
        href="/monitoring?section=system"
        aria-label={label()}
        aria-describedby={open() ? id : undefined}
        onMouseEnter={() => setOpen(true)}
        onFocus={() => setOpen(true)}
        onBlur={() => setOpen(false)}
        onClick={() => setOpen(false)}
      >
        <CPUIcon class={levelClass[cpuLevel()]} aria-hidden="true" data-testid="cpu-alarm" />
        <MemoryIcon class={levelClass[memoryLevel()]} aria-hidden="true" data-testid="memory-alarm" />
      </a>
      <Show when={open()}>
        <span class={styles.tooltip} role="tooltip" id={id}>
          {description()}
        </span>
      </Show>
    </span>
  );
}

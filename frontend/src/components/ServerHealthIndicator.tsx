// Always-visible CPU and memory levels link to System with accessible readings and sampling/connection status.

import { createEffect, createSignal, createUniqueId, onCleanup, Show } from "solid-js";
import CPUIcon from "@material-symbols/svg-400/outlined/memory.svg?solid";
import MemoryIcon from "@material-symbols/svg-400/outlined/memory_alt.svg?solid";
import type { ServerHealth } from "@sdk/types.gen";

import { formatBytes } from "../formatting";
import { hostCPUPercent, hostLevel, hostMemoryPercent, type ResourceLevel } from "../resourceLevel";
import styles from "./ServerHealthIndicator.module.css";

const levelClass: Record<ResourceLevel, string> = {
  high: styles.high,
  normal: styles.normal,
  unavailable: styles.unavailable,
  warning: styles.warning,
};

export default function ServerHealthIndicator(props: { health: ServerHealth | null; connected: boolean }) {
  const [open, setOpen] = createSignal(false);
  const id = createUniqueId();
  const level = (percent: (h: ServerHealth) => number | null): ResourceLevel => {
    const h = props.health;
    const p = h && props.connected && !h.error ? percent(h) : null;
    return p === null ? "unavailable" : hostLevel(p);
  };
  const cpuLevel = () => level(hostCPUPercent);
  const memoryLevel = () => level(hostMemoryPercent);
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

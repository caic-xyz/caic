// Classifies host and task CPU/memory levels, and attributes host pressure to the tasks that drive it.

import type { EventStats, ServerHealth } from "@sdk/types.gen";

export type ResourceLevel = "high" | "normal" | "unavailable" | "warning";

/** A task sample older than this no longer describes the running container. */
const sampleTTLMs = 15_000;

const rank: Record<ResourceLevel, number> = { unavailable: 0, normal: 1, warning: 2, high: 3 };

export function isSampleFresh(sample: EventStats, now: number): boolean {
  return now - sample.ts <= sampleTTLMs;
}

/** Classifies a host percentage of machine capacity. */
export function hostLevel(percent: number): ResourceLevel {
  if (percent >= 90) return "high";
  if (percent >= 80) return "warning";
  return "normal";
}

/** Classifies a task percentage of its own container capacity. */
function containerLevel(percent: number): ResourceLevel {
  if (percent >= 75) return "high";
  if (percent >= 50) return "warning";
  return "normal";
}

/** Host-wide usage of one resource while the host is at warning or high level. */
export interface HostShare {
  /** Host usage as a percentage of machine capacity. */
  hostPercent: number;
  /** Largest absolute usage among fresh task samples, in the task sample unit. */
  busiest: number;
}

/** Null members mean the host is normal or its reading is unavailable. */
export interface HostPressure {
  cpu: HostShare | null;
  memory: HostShare | null;
}

export function hostCPUPercent(h: ServerHealth): number | null {
  return h.cpuCount > 0 ? h.cpuPerc / h.cpuCount : null;
}

export function hostMemoryPercent(h: ServerHealth): number | null {
  return h.memTotal > 0 ? (h.memUsed / h.memTotal) * 100 : null;
}

/** Derives host pressure from the latest health reading and the fresh task samples. */
export function hostPressure(health: ServerHealth | null, connected: boolean, samples: EventStats[]): HostPressure {
  const share = (percent: number | null, usage: (s: EventStats) => number): HostShare | null => {
    if (percent === null || hostLevel(percent) === "normal") return null;
    const busiest = Math.max(0, ...samples.map(usage));
    return busiest > 0 ? { hostPercent: percent, busiest } : null;
  };
  if (!health || health.error || !connected) return { cpu: null, memory: null };
  return {
    cpu: share(hostCPUPercent(health), (s) => s.cpuPerc),
    memory: share(hostMemoryPercent(health), (s) => s.memUsed),
  };
}

export function sameHostPressure(a: HostPressure, b: HostPressure): boolean {
  const same = (x: HostShare | null, y: HostShare | null) =>
    x === y || (x !== null && y !== null && x.hostPercent === y.hostPercent && x.busiest === y.busiest);
  return same(a.cpu, b.cpu) && same(a.memory, b.memory);
}

/**
 * Classifies one task resource. The level is the higher of the container-relative
 * level and the host-contribution level. The contribution scales the task's
 * absolute usage so the busiest task maps to the host percentage, then applies
 * host thresholds: the busiest task takes the host level, and tasks close to it
 * share it. A task can thus be colored while using a fraction of its container.
 */
export function taskLevel(containerPercent: number | null, usage: number, share: HostShare | null): ResourceLevel {
  const own = containerPercent === null ? "unavailable" : containerLevel(containerPercent);
  if (!share) return own;
  const contribution = hostLevel((share.hostPercent * busiestShare(usage, share)) / 100);
  return rank[contribution] > rank[own] ? contribution : own;
}

/** Task usage relative to the busiest task, as a percentage. */
export function busiestShare(usage: number, share: HostShare): number {
  return (usage / share.busiest) * 100;
}

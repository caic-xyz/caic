// Classifies host and task CPU/memory levels, and attributes host pressure to the tasks that drive it most.

import type { EventStats, ResourceLevel as HostLevel, ServerHealth } from "@sdk/types.gen";

/** A server-classified host level, or unavailable when no reading applies. */
export type ResourceLevel = HostLevel | "unavailable";

/** A task sample older than this no longer describes the running container. */
const sampleTTLMs = 15_000;

const rank: Record<ResourceLevel, number> = { unavailable: 0, normal: 1, warning: 2, high: 3 };

export function isSampleFresh(sample: EventStats, now: number): boolean {
  return now - sample.ts <= sampleTTLMs;
}

/** Classifies a task percentage of its own container capacity. */
function containerLevel(percent: number): ResourceLevel {
  if (percent >= 75) return "high";
  if (percent >= 50) return "warning";
  return "normal";
}

/**
 * The tasks that drive one host resource while the host is at warning or high
 * level, as absolute usage thresholds in the task sample unit.
 */
export interface HostShare {
  /** Server-classified host level, given to the busiest task. */
  level: HostLevel;
  /** Largest usage among fresh task samples. */
  busiest: number;
  /** Smallest usage still marked as a contributor; tasks from here up to busiest are warning. */
  floor: number;
}

/** Null members mean the host is normal or its reading is unavailable. */
export interface HostPressure {
  cpu: HostShare | null;
  memory: HostShare | null;
}

/** A contributor below this share of host capacity is noise, not pressure. */
const contributorCapacityShare = 0.05;
/** From the third, a contributor uses at least this fraction of the previous one. */
const contributorStep = 0.5;

export function hostCPUPercent(h: ServerHealth): number | null {
  return h.cpuCount > 0 ? h.cpuPerc / h.cpuCount : null;
}

export function hostMemoryPercent(h: ServerHealth): number | null {
  return h.memTotal > 0 ? (h.memUsed / h.memTotal) * 100 : null;
}

/**
 * Derives host pressure from the latest health reading and the fresh task samples.
 * Tasks are ranked by absolute usage. The busiest takes the host level. The
 * runner-up is a contributor when it uses at least 5% of host capacity; each
 * later task also needs at least half of the previous contributor. A few heavy
 * tasks are thus marked together and a long tail of small ones is not.
 */
export function hostPressure(health: ServerHealth | null, connected: boolean, samples: EventStats[]): HostPressure {
  const share = (
    level: HostLevel | undefined,
    capacity: number,
    usage: (s: EventStats) => number,
  ): HostShare | null => {
    if (level === undefined || level === "normal" || capacity <= 0) return null;
    const usages = samples
      .map(usage)
      .filter((u) => u > 0)
      .sort((x, y) => y - x);
    const busiest = usages[0];
    if (busiest === undefined) return null;
    let floor = busiest;
    for (const [i, u] of usages.entries()) {
      if (i === 0) continue;
      if (u < capacity * contributorCapacityShare || (i > 1 && u < floor * contributorStep)) break;
      floor = u;
    }
    return { level, busiest, floor };
  };
  if (!health || health.error || !connected) return { cpu: null, memory: null };
  return {
    cpu: share(health.cpuLevel, health.cpuCount * 100, (s) => s.cpuPerc),
    memory: share(health.memLevel, health.memTotal, (s) => s.memUsed),
  };
}

export function sameHostPressure(a: HostPressure, b: HostPressure): boolean {
  const same = (x: HostShare | null, y: HostShare | null) =>
    x === y || (x !== null && y !== null && x.level === y.level && x.busiest === y.busiest && x.floor === y.floor);
  return same(a.cpu, b.cpu) && same(a.memory, b.memory);
}

/**
 * Classifies one task resource as the higher of its container-relative level and
 * its host-contribution level: the busiest task takes the host level and the other
 * contributors are warning. A task can thus be colored while using a fraction of
 * its container.
 */
export function taskLevel(containerPercent: number | null, usage: number, share: HostShare | null): ResourceLevel {
  const own = containerPercent === null ? "unavailable" : containerLevel(containerPercent);
  if (!share) return own;
  const contribution: ResourceLevel =
    usage >= share.busiest ? share.level : usage >= share.floor ? "warning" : "normal";
  return rank[contribution] > rank[own] ? contribution : own;
}

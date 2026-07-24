// Shared quota helpers for task-card countdowns and recovery notifications.

import type { Task } from "@sdk/types.gen";

/** Tracks quota-blocked waiting tasks and reports their one-time recovery transition. */
export class QuotaRecoveryTracker {
  private blockedTaskIDs = new Set<string>();

  /**
   * Records the current quota state and returns waiting tasks whose quota just became available.
   * Tasks that leave waiting are not reported: they are no longer blocked on quota.
   */
  update(tasks: Task[]): Task[] {
    const nextBlockedTaskIDs = new Set<string>();
    const recovered: Task[] = [];

    for (const task of tasks) {
      const blocked = task.state === "waiting" && task.rateLimit?.blocked === true;
      if (blocked) {
        nextBlockedTaskIDs.add(task.id);
      } else if (task.state === "waiting" && this.blockedTaskIDs.has(task.id)) {
        recovered.push(task);
      }
    }

    this.blockedTaskIDs = nextBlockedTaskIDs;
    return recovered;
  }
}

export function formatQuotaCountdown(resetsAt: string, now: number): string {
  const diffMs = Date.parse(resetsAt) - now;
  if (!Number.isFinite(diffMs) || diffMs <= 0) return "now";

  const totalMinutes = Math.ceil(diffMs / 60_000);
  if (totalMinutes < 60) return `${totalMinutes}m`;

  const hours = Math.floor(totalMinutes / 60);
  const minutes = totalMinutes % 60;
  if (hours < 24) return `${hours}h ${minutes}m`;

  const days = Math.floor(hours / 24);
  return `${days}d ${hours % 24}h`;
}

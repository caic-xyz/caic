// Voice task context formats caic task changes for active browser voice sessions.

import type { Task } from "@sdk/types.gen";

/**
 * taskLabel names a task for a voice context update.
 *
 * The session task number is only included when more than one task is alive;
 * a lone task needs no disambiguating reference.
 */
function taskLabel(task: Task, taskNumber: number, numbered: boolean): string {
  const shortName = task.title || task.id;
  return numbered ? `Task #${taskNumber} (${shortName})` : shortName;
}

export function buildTaskCIContext(task: Task, taskNumber: number, numbered: boolean): string {
  const pr = task.forgePR ? ` PR #${task.forgePR}` : "";
  return `[${taskLabel(task, taskNumber, numbered)}${pr} — CI: failure]`;
}

export function buildTaskStateContext(task: Task, taskNumber: number, numbered: boolean): string | null {
  const label = taskLabel(task, taskNumber, numbered);
  switch (task.state) {
    case "asking":
    case "waiting":
    case "has_plan":
      return `[${label} — ${task.state}]`;
    case "purged":
      return task.result ? `[${label} — completed: ${task.result}]` : null;
    case "stopped":
      return `[${label} — stopped]`;
    case "crashed":
      return `[${label} — crashed: ${task.error ?? "unknown"}]`;
    case "failed":
      return `[${label} — failed: ${task.error ?? "unknown"}]`;
    case "pending":
    case "branching":
    case "provisioning":
    case "starting":
    case "running":
    case "pulling":
    case "pushing":
    case "stopping":
    case "purging":
      return null;
  }
}

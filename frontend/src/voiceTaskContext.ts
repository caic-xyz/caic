// Voice task context formats caic task changes for active browser voice sessions.

import type { Task } from "@sdk/types.gen";

export function buildTaskCreatedContext(task: Task, taskNumber: number): string {
  const shortName = task.title || task.id;
  return `[Task #${taskNumber} created (${shortName}) — ${task.state}]`;
}

export function buildTaskCIContext(task: Task, taskNumber: number): string {
  const shortName = task.title || task.id;
  const pr = task.forgePR ? ` PR #${task.forgePR}` : "";
  return `[Task #${taskNumber} (${shortName})${pr} — CI: failure]`;
}

export function buildTaskStateContext(task: Task, taskNumber: number): string | null {
  const shortName = task.title || task.id;
  switch (task.state) {
    case "asking":
    case "waiting":
    case "has_plan":
      return `[Task #${taskNumber} (${shortName}) — ${task.state}]`;
    case "purged":
      return task.result
        ? `[Task #${taskNumber} (${shortName}) — completed: ${task.result}]`
        : null;
    case "stopped":
      return `[Task #${taskNumber} (${shortName}) — stopped]`;
    case "crashed":
      return `[Task #${taskNumber} (${shortName}) — crashed: ${task.error ?? "unknown"}]`;
    case "failed":
      return `[Task #${taskNumber} (${shortName}) — failed: ${task.error ?? "unknown"}]`;
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

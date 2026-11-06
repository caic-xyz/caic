// Formats caic CI failures for active browser voice sessions.

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
  return `[${taskLabel(task, taskNumber, numbered)}${pr} — CI: failure; taskID: ${task.id}]`;
}

// Formats a task's background-work counts and lists the running items that history metadata reports.

import type { HistoryBackgroundCommand, HistoryMetadata, TaskBackground } from "@sdk/types.gen";
import { toolIdentity } from "./grouping";

export type BackgroundKind = "command" | "agent" | "watch";

export interface BackgroundItem {
  kind: BackgroundKind;
  id: string;
  label: string;
  startedAt: number | null;
  // toolKey identifies the tool call that started a command or a watch; null
  // for an agent, whose card carries its id, or for a command without a call.
  toolKey: string | null;
  // ownerAgentID names the native agent whose transcript holds the call.
  ownerAgentID: string | null;
}

const kindOrder: readonly BackgroundKind[] = ["command", "agent", "watch"];

function count(n: number, noun: string): string {
  if (n === 1) return `1 ${noun}`;
  return `${n} ${noun}${noun.endsWith("h") ? "es" : "s"}`;
}

// backgroundSummary is the task card row, or null when no background work runs.
export function backgroundSummary(background: TaskBackground | undefined): string | null {
  if (!background) return null;
  const parts = [
    background.commands > 0 ? count(background.commands, "command") : "",
    background.agents > 0 ? count(background.agents, "agent") : "",
    background.watches > 0 ? count(background.watches, "watch") : "",
  ].filter(Boolean);
  return parts.length ? `Background: ${parts.join(" · ")}` : null;
}

// commandSettled reports whether a settled task makes a running command record
// stale. Only a command whose end wakes the agent keeps the task running; another
// command keeps running while the task waits.
export function commandSettled(command: Pick<HistoryBackgroundCommand, "wakesAgent">, settled: boolean): boolean {
  return settled && !!command.wakesAgent;
}

// activeBackgroundItems lists running commands, background agents, and
// watches. A settled task lists only the commands that do not wake the agent:
// its other running records are stale.
export function activeBackgroundItems(
  metadata: Pick<HistoryMetadata, "activities" | "backgroundCommands"> | undefined,
  settled: boolean,
): BackgroundItem[] {
  if (!metadata) return [];
  const items: BackgroundItem[] = [];
  for (const command of metadata.backgroundCommands ?? []) {
    if (command.status !== "running" || commandSettled(command, settled)) continue;
    items.push({
      kind: command.watch ? "watch" : "command",
      id: command.id,
      label: command.label || command.id,
      startedAt: command.startedAt ? Date.parse(command.startedAt) : null,
      toolKey: command.toolUseID ? toolIdentity(command.source, command.toolUseID) : null,
      ownerAgentID: command.source?.agentID || null,
    });
  }
  for (const activity of metadata.activities ?? []) {
    if (settled || !activity.background || activity.status !== "running") continue;
    items.push({
      kind: "agent",
      id: activity.id,
      label: activity.label || activity.id,
      startedAt: activity.startedAt ? Date.parse(activity.startedAt) : null,
      toolKey: null,
      ownerAgentID: null,
    });
  }
  return items.sort((a, b) => kindOrder.indexOf(a.kind) - kindOrder.indexOf(b.kind));
}

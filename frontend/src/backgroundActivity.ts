// Formats background-work summaries and derives the running items shown in task details.

import type {
  HistoryActivity,
  HistoryBackgroundCommand,
  HistoryMetadata,
  TaskBackground,
  TaskBackgroundItem,
} from "@sdk/types.gen";
import { toolIdentity } from "./grouping";
import { formatTimingDuration } from "./timing";
import { firstLine } from "./tooltipText";

export type BackgroundKind = TaskBackgroundItem["kind"];

export interface BackgroundItem {
  kind: BackgroundKind;
  // kindName names what runs: the agent kind the harness reports, or the
  // generic name of the item's kind.
  kindName: string;
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

// genericKindNames is each kind's kindName when nothing more specific is
// known; its icon then says as much.
export const genericKindNames: Readonly<Record<BackgroundKind, string>> = {
  agent: "Agent",
  command: "Command",
  watch: "Watch",
};

// agentKindName names an agent by the kind its harness reports, a batch
// orchestration as "Batch", and any other agent generically.
function agentKindName(activity: Pick<HistoryActivity, "kind" | "scope">): string {
  if (activity.kind) return activity.kind;
  return activity.scope === "batch" ? "Batch" : genericKindNames.agent;
}

function count(n: number, noun: string): string {
  if (n === 1) return `1 ${noun}`;
  return `${n} ${noun}${noun.endsWith("h") ? "es" : "s"}`;
}

export interface BackgroundCount {
  kind: BackgroundKind;
  count: number;
}

// backgroundCounts lists each kind of running background work in display
// order, or nothing when none runs.
export function backgroundCounts(background: TaskBackground | undefined): BackgroundCount[] {
  if (!background) return [];
  const byKind: Record<BackgroundKind, number> = {
    command: background.commands,
    agent: background.agents,
    watch: background.watches,
  };
  return kindOrder.filter((kind) => byKind[kind] > 0).map((kind) => ({ kind, count: byKind[kind] }));
}

// backgroundLabel names the counts, such as "Background: 2 agents · 1 watch".
export function backgroundLabel(counts: readonly BackgroundCount[]): string {
  return `Background: ${counts.map((c) => count(c.count, c.kind)).join(" · ")}`;
}

// backgroundItemTitle names an item on one line: a multi-line command keeps its first line.
export function backgroundItemTitle(kindName: string, label: string): string {
  return label && label !== kindName ? `${kindName}: ${firstLine(label)}` : kindName;
}

export function backgroundTooltip(
  counts: readonly BackgroundCount[],
  items: readonly TaskBackgroundItem[] | undefined,
  now: number,
): string {
  if (!items?.length) return "";
  const lines: string[] = [];
  for (const kind of kindOrder) {
    for (const item of items) {
      if (item.kind !== kind) continue;
      const name =
        kind === "agent" ? agentKindName({ kind: item.agentKind, scope: item.agentScope }) : genericKindNames[kind];
      const start = kind === "agent" && item.startedAt ? Date.parse(item.startedAt) : NaN;
      const elapsed = Number.isFinite(start) ? ` (${formatTimingDuration(Math.max(0, now - start))})` : "";
      lines.push(`• ${backgroundItemTitle(name, item.label)}${elapsed}`);
    }
  }
  const remaining = counts.reduce((total, entry) => total + entry.count, 0) - items.length;
  if (remaining > 0) lines.push(`${remaining} more`);
  return lines.join("\n");
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
      kindName: command.watch ? genericKindNames.watch : genericKindNames.command,
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
      kindName: agentKindName(activity),
      id: activity.id,
      label: activity.label || activity.id,
      startedAt: activity.startedAt ? Date.parse(activity.startedAt) : null,
      toolKey: null,
      ownerAgentID: null,
    });
  }
  return items.sort((a, b) => kindOrder.indexOf(a.kind) - kindOrder.indexOf(b.kind));
}

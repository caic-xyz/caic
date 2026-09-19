// Incrementally folds canonical native activity across turns, compaction, and replaced
// history. A terminal observation's result supersedes an earlier interim one.

import type { EventMessage, EventNativeSubagent } from "@sdk/types.gen";

import type { MessageGroup, MsgItem, Session, Turn } from "./grouping";

export interface NativeActivity extends EventNativeSubagent {
  startedAt: number | null;
  endedAt: number | null;
  // anchorTs positions the card in the transcript: the observation that created
  // it or last changed its folded lifecycle status. A running card therefore
  // stays at its spawn while heartbeats arrive, and moves once when it settles.
  // 0 when no observation carried a timestamp.
  anchorTs: number;
}

export function nativeTerminal(status: EventNativeSubagent["status"]): boolean {
  return status === "completed" || status === "failed" || status === "interrupted";
}

// Missing terminal evidence is not success or interruption. A settled parent only
// makes its last running observation stale; it never invents a terminal outcome.
export function nativeActivityStatus(activity: NativeActivity, settled: boolean): string {
  if (settled && activity.status === "running") return "Last observed running · outcome unknown";
  if (activity.status === "unknown") return "Status unknown";
  return activity.status[0].toUpperCase() + activity.status.slice(1);
}

export class NativeActivityTracker {
  private previous: EventMessage[] = [];
  private byID = new Map<string, NativeActivity>();
  private snapshot: NativeActivity[] = [];

  derive(messages: EventMessage[]): NativeActivity[] {
    const reset =
      messages.length < this.previous.length ||
      (this.previous.length > 0 &&
        (messages[0] !== this.previous[0] ||
          messages[this.previous.length - 1] !== this.previous[this.previous.length - 1]));
    let changed = reset;
    if (reset) {
      this.byID.clear();
      this.previous = [];
    }
    for (let i = this.previous.length; i < messages.length; i++) {
      const ev = messages[i];
      const s = ev.nativeSubagent;
      if (ev.kind !== "nativeSubagent" || !s?.id) continue;
      const old = this.byID.get(s.id);
      const status =
        old && (nativeTerminal(old.status) || s.status === "unknown") ? old.status : s.status;
      const next: NativeActivity = {
        ...s,
        toolUseID: old?.toolUseID || s.toolUseID,
        scope: old?.scope || s.scope,
        groupID: old?.groupID || s.groupID,
        label: old?.label || s.label,
        prompt: old?.prompt || s.prompt,
        // A terminal observation upgrades an interim result, but terminal
        // results do not overwrite each other.
        result:
          s.result && (!old?.result || (!nativeTerminal(old.status) && nativeTerminal(s.status)))
            ? s.result
            : old?.result || s.result,
        status,
        // Detachment is monotonic: a background launch stays background.
        background: old?.background || s.background,
        // Reposition only on creation or a folded status change, so running
        // heartbeats do not drag an active card toward the newest content.
        anchorTs:
          !old || status !== old.status ? (ev.ts > 0 ? ev.ts : (old?.anchorTs ?? 0)) : old.anchorTs,
        startedAt: old?.startedAt ?? (s.status === "running" && ev.ts > 0 ? ev.ts : null),
        endedAt: old?.endedAt ?? (nativeTerminal(s.status) && ev.ts > 0 ? ev.ts : null),
      };
      if (
        old &&
        Object.keys(next).every(
          (key) => next[key as keyof NativeActivity] === old[key as keyof NativeActivity],
        )
      )
        continue;
      this.byID.set(s.id, next);
      changed = true;
    }
    this.previous = messages.slice();
    if (changed) this.snapshot = [...this.byID.values()];
    return this.snapshot;
  }
}

function groupEndTs(group: MessageGroup): number {
  let max = 0;
  for (const ev of group.events) if (ev.ts > max) max = ev.ts;
  return max;
}

function turnEndTs(turn: Turn): number {
  let max = 0;
  for (const group of turn.groups) max = Math.max(max, groupEndTs(group));
  return max;
}

function sessionEndTs(session: Session): number {
  let max = 0;
  for (const turn of session.turns) max = Math.max(max, turnEndTs(turn));
  return max;
}

// itemAnchorTs is the timestamp through which an item already represents
// transcript content. Headers only introduce the content that follows them, so
// they never anchor a card.
function itemAnchorTs(item: MsgItem): number | null {
  switch (item.kind) {
    case "group":
      return groupEndTs(item.group);
    case "elided":
      return turnEndTs(item.turn);
    case "sessionElided":
      return sessionEndTs(item.session);
    case "sessionHeader":
    case "sessionBoundary":
    case "expandedHeader":
      return null;
  }
}

// assignNativeAnchors maps each activity to the key of the transcript item it
// renders below: the last content item at or before its anchor timestamp. A
// settled card therefore appears where it settled, and a running card stays
// where it spawned. An activity before any content, or without a timestamp,
// falls back to the first anchorable item.
export function assignNativeAnchors(
  items: readonly MsgItem[],
  activities: readonly NativeActivity[],
): Map<string, NativeActivity[]> {
  const byAnchor = new Map<string, NativeActivity[]>();
  const anchors: { key: string; endTs: number }[] = [];
  let previousEndTs = 0;
  for (const item of items) {
    const endTs = itemAnchorTs(item);
    if (endTs === null) continue;
    // Items are chronological, but clamp so a missing timestamp cannot make a
    // later item a worse anchor than an earlier one.
    const clamped = Math.max(previousEndTs, endTs);
    anchors.push({ key: item.key, endTs: clamped });
    previousEndTs = clamped;
  }
  if (anchors.length === 0) return byAnchor;
  const fallback = anchors[0].key;
  for (const activity of activities) {
    let key = fallback;
    for (const anchor of anchors) {
      if (anchor.endTs > activity.anchorTs) break;
      key = anchor.key;
    }
    const list = byAnchor.get(key);
    if (list) list.push(activity);
    else byAnchor.set(key, [activity]);
  }
  return byAnchor;
}

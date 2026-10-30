// Folds native lifecycles and routes canonical child messages without building undisclosed transcripts.

import type { EventBackgroundCommand, EventMessage, EventMessageSource, EventNativeSubagent } from "@sdk/types.gen";

import type { MessageGroup, MsgItem, Turn } from "./grouping";
import { childAgentID } from "./grouping";

export interface NativeActivity extends EventNativeSubagent {
  source?: EventMessageSource;
  messages?: EventMessage[];
  startedAt: number | null;
  endedAt: number | null;
  // anchorTs positions the card in the transcript: the observation that created
  // it or last changed its folded lifecycle status. A running card therefore
  // stays at its spawn while heartbeats arrive, and moves once when it settles.
  // 0 when no observation carried a timestamp.
  anchorTs: number;
}

export function nativeTerminal(status: string): boolean {
  return status === "completed" || status === "failed" || status === "interrupted";
}

// Missing terminal evidence is not success or interruption. A settled parent or
// incomplete historical window makes a running observation stale, not terminal.
export function nativeActivityStatus(activity: { status: string }, settled: boolean): string {
  if (settled && activity.status === "running") return "Last observed running · outcome unknown";
  if (activity.status === "unknown") return "Status unknown";
  return activity.status.charAt(0).toUpperCase() + activity.status.slice(1);
}

export class NativeActivityTracker {
  private processed = 0;
  private firstEvent: EventMessage | null = null;
  private lastEvent: EventMessage | null = null;
  private byID = new Map<string, NativeActivity>();
  private snapshot: NativeActivity[] = [];
  private messagesByAgent = new Map<string, EventMessage[]>();
  private byDelegation = new Map<string, Map<string, Set<string>>>();
  private pendingNamespaces = new Map<string, Set<string>>();
  routingRevision = 0;

  ownerFor(event: EventMessage): string | null {
    const canonical = childAgentID(event);
    if (canonical) return canonical;
    const source = event.source;
    if (!source?.delegationToolUseID) return null;
    const namespace = source.rootThreadID || source.threadID || "";
    const candidates = new Set<string>();
    for (const [scope, ids] of this.byDelegation.get(source.delegationToolUseID) ?? []) {
      if (namespace && scope && namespace !== scope) continue;
      if (!scope && (this.pendingNamespaces.get(source.delegationToolUseID)?.size ?? 0) > 1) continue;
      for (const id of ids) candidates.add(id);
    }
    return candidates.size === 1 ? (candidates.values().next().value ?? null) : null;
  }

  derive(messages: readonly EventMessage[]): NativeActivity[] {
    const reset =
      messages.length < this.processed ||
      (this.processed > 0 && (messages[0] !== this.firstEvent || messages[this.processed - 1] !== this.lastEvent));
    let changed = reset;
    if (reset) {
      this.byID.clear();
      this.messagesByAgent.clear();
      this.byDelegation.clear();
      this.pendingNamespaces.clear();
      this.routingRevision++;
      this.processed = 0;
    }
    // Discover reported relationships before routing actor events. New or
    // ambiguous correlation is a structural change; ordinary deltas never replay history.
    let routingChanged = false;
    for (let i = this.processed; i < messages.length; i++) {
      const event = messages[i];
      if (!event) throw new Error("Missing timeline event");
      const namespace = event.source?.rootThreadID || event.source?.threadID || "";
      const delegation = event.source?.delegationToolUseID;
      if (delegation && !event.source?.agentID) {
        const pendingScopes = this.pendingNamespaces.get(delegation) ?? new Set<string>();
        if (!pendingScopes.has(namespace)) routingChanged = true;
        pendingScopes.add(namespace);
        this.pendingNamespaces.set(delegation, pendingScopes);
      }
      const native = event.nativeSubagent;
      if (event.kind !== "nativeSubagent" || !native?.toolUseID || !native.id) continue;
      const scopes = this.byDelegation.get(native.toolUseID) ?? new Map<string, Set<string>>();
      const ids = scopes.get(namespace) ?? new Set<string>();
      if (!ids.has(native.id)) routingChanged = true;
      ids.add(native.id);
      scopes.set(namespace, ids);
      this.byDelegation.set(native.toolUseID, scopes);
    }
    if (routingChanged) {
      this.routingRevision++;
      if (this.processed > 0) {
        this.byID.clear();
        this.messagesByAgent.clear();
        this.processed = 0;
      }
      changed = true;
    }
    const changedChildren = new Set<string>();
    const appendChildMessage = (id: string, event: EventMessage): void => {
      let transcript = this.messagesByAgent.get(id);
      if (!transcript) {
        transcript = [];
        this.messagesByAgent.set(id, transcript);
        const existing = this.byID.get(id);
        if (existing) this.byID.set(id, { ...existing, anchorTs: existing.startedAt ?? event.ts });
        else this.byID.set(id, { id, status: "unknown", startedAt: null, endedAt: null, anchorTs: event.ts });
      }
      transcript.push(event);
      changedChildren.add(id);
      changed = true;
    };
    for (let i = this.processed; i < messages.length; i++) {
      const ev = messages[i];
      if (!ev) throw new Error("Missing timeline event");
      // Lifecycle subjects must fold globally even when their actor is another child.
      const lifecycle = ev.kind === "nativeSubagent" || ev.kind === "backgroundCommand";
      const childID = lifecycle ? null : this.ownerFor(ev);
      if (childID !== null) {
        if (ev.kind !== "rateLimit" || ev.rateLimit?.rateLimitType === "thread_goal") appendChildMessage(childID, ev);
        continue;
      }
      const s = ev.nativeSubagent;
      if (ev.kind !== "nativeSubagent" || !s?.id) continue;
      const old = this.byID.get(s.id);
      const status = old && (nativeTerminal(old.status) || s.status === "unknown") ? old.status : s.status;
      const next: NativeActivity = {
        ...s,
        source: old?.source ?? ev.source,
        messages: old?.messages,
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
          old && this.messagesByAgent.has(s.id)
            ? old.anchorTs
            : !old || status !== old.status
              ? ev.ts > 0
                ? ev.ts
                : (old?.anchorTs ?? 0)
              : old.anchorTs,
        startedAt: old?.startedAt ?? (s.status === "running" && ev.ts > 0 ? ev.ts : null),
        endedAt: old?.endedAt ?? (nativeTerminal(s.status) && ev.ts > 0 ? ev.ts : null),
      };
      if (
        old &&
        Object.keys(next).every((key) => next[key as keyof NativeActivity] === old[key as keyof NativeActivity])
      )
        continue;
      this.byID.set(s.id, next);
      if (this.messagesByAgent.has(s.id)) changedChildren.add(s.id);
      changed = true;
    }
    for (const id of changedChildren) {
      const transcript = this.messagesByAgent.get(id);
      const activity = this.byID.get(id);
      if (transcript && activity) {
        this.byID.set(id, { ...activity, messages: transcript });
      }
    }
    this.processed = messages.length;
    this.firstEvent = messages[0] ?? null;
    this.lastEvent = messages.at(-1) ?? null;
    if (changed) this.snapshot = [...this.byID.values()];
    return this.snapshot;
  }
}

function groupEndTs(group: MessageGroup): number {
  if (group.messageKey) return group.events[0]?.ts ?? 0;
  let max = 0;
  for (const ev of group.events) if (ev.ts > max) max = ev.ts;
  return max;
}

function turnEndTs(turn: Turn): number {
  let max = 0;
  for (const group of turn.groups) max = Math.max(max, groupEndTs(group));
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
    case "sessionBoundary":
    case "expandedHeader":
      return null;
  }
}

// assignNativeAnchors maps each activity to the key of the transcript item it
// renders below: the last content item at or before its anchor timestamp. A
// settled card therefore appears where it settled, and a running card stays
// where it spawned. An activity before any content, or without a timestamp,
// falls back to the first anchorable item. The anchor rules are shared by every
// anchored card kind, so the entry type is the common timing shape rather than
// one activity type.
export function assignNativeAnchors<T extends { anchorTs: number }>(
  items: readonly MsgItem[],
  activities: readonly T[],
): Map<string, T[]> {
  const byAnchor = new Map<string, T[]>();
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
  const first = anchors[0];
  if (!first) return byAnchor;
  const fallback = first.key;
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

export interface BackgroundCommandActivity extends EventBackgroundCommand {
  source?: EventMessageSource;
  startedAt: number | null;
  endedAt: number | null;
  // anchorTs positions the card in the transcript: the observation that created
  // it or last changed its folded lifecycle status. A running card therefore
  // stays at its spawn while the command runs, and moves once when it settles.
  // 0 when no observation carried a timestamp.
  anchorTs: number;
}

export function backgroundTerminal(status: EventBackgroundCommand["status"]): boolean {
  return status === "completed" || status === "failed" || status === "interrupted";
}

// Missing terminal evidence is not success or interruption. A settled parent or
// incomplete historical window makes a running observation stale, not terminal.
export function backgroundCommandStatus(activity: { status: string }, settled: boolean): string {
  if (settled && activity.status === "running") return "Last observed running · outcome unknown";
  return activity.status.charAt(0).toUpperCase() + activity.status.slice(1);
}

export class BackgroundCommandTracker {
  private processed = 0;
  private firstEvent: EventMessage | null = null;
  private lastEvent: EventMessage | null = null;
  private byID = new Map<string, BackgroundCommandActivity>();
  private snapshot: BackgroundCommandActivity[] = [];

  derive(messages: readonly EventMessage[]): BackgroundCommandActivity[] {
    const reset =
      messages.length < this.processed ||
      (this.processed > 0 && (messages[0] !== this.firstEvent || messages[this.processed - 1] !== this.lastEvent));
    let changed = reset;
    if (reset) {
      this.byID.clear();
      this.processed = 0;
    }
    for (let i = this.processed; i < messages.length; i++) {
      const ev = messages[i];
      if (!ev) throw new Error("Missing timeline event");
      const s = ev.backgroundCommand;
      if (ev.kind !== "backgroundCommand" || !s?.id) continue;
      const old = this.byID.get(s.id);
      const status = old && (backgroundTerminal(old.status) || s.status === "running") ? old.status : s.status;
      const next: BackgroundCommandActivity = {
        ...s,
        source: old?.source ?? ev.source,
        toolUseID: old?.toolUseID || s.toolUseID,
        label: old?.label || s.label,
        // A terminal observation upgrades an interim result, but terminal
        // results do not overwrite each other.
        result:
          s.result && (!old?.result || (!backgroundTerminal(old.status) && backgroundTerminal(s.status)))
            ? s.result
            : old?.result || s.result,
        outputRef: old?.outputRef || s.outputRef,
        exitCode: old?.exitCode ?? s.exitCode,
        status,
        // Reposition only on creation or a folded status change, so repeated
        // running observations do not drag an active card toward newer content.
        anchorTs: !old || status !== old.status ? (ev.ts > 0 ? ev.ts : (old?.anchorTs ?? 0)) : old.anchorTs,
        startedAt: old?.startedAt ?? (s.status === "running" && ev.ts > 0 ? ev.ts : null),
        endedAt: old?.endedAt ?? (backgroundTerminal(s.status) && ev.ts > 0 ? ev.ts : null),
      };
      if (
        old &&
        Object.keys(next).every(
          (key) => next[key as keyof BackgroundCommandActivity] === old[key as keyof BackgroundCommandActivity],
        )
      )
        continue;
      this.byID.set(s.id, next);
      changed = true;
    }
    this.processed = messages.length;
    this.firstEvent = messages[0] ?? null;
    this.lastEvent = messages.at(-1) ?? null;
    if (changed) this.snapshot = [...this.byID.values()];
    return this.snapshot;
  }
}

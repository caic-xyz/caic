// Owns the data-anchor-key namespace and the lookups that keep a conversation scroll position stable.
//
// A row key must identify the same content before and after the change a
// correction spans: a canonical history replacement, a live append, or a pane
// resize. Message identities do that when the agent reports them. Tool groups
// carry one too. Text and thinking groups of agents that report no turn or item
// identity fall back to their first event, whose timestamp and kind are fixed
// for the record; two such groups can share that pair only when the same record
// also emitted a keyed row between them, and that costs one row of accuracy.
//
// Keys are data, never selectors: message identities contain quotes, brackets,
// and commas.

import type { EventMessage } from "@sdk/types.gen";
import type { MessageGroup } from "./grouping";

const ANCHOR_ATTRIBUTE = "data-anchor-key";

export function messageAnchor(messageKey: string): string {
  return `msg:${messageKey}`;
}

export function turnAnchor(turnKey: string): string {
  return `turn:${turnKey}`;
}

export function sessionAnchor(sessionID: string): string {
  return `session:${sessionID}`;
}

// Returns the row key for a rendered group, or undefined when the group has no
// identity to anchor on.
export function groupAnchor(group: MessageGroup): string | undefined {
  if (group.messageKey) return messageAnchor(group.messageKey);
  return eventAnchor(group.events[0]);
}

function eventAnchor(event: EventMessage | undefined): string | undefined {
  return event ? `ev:${event.ts}:${event.kind}` : undefined;
}

// Locates the row that anchors the visible content of a scrolling area.
export function anchorAt(area: HTMLElement): { key: string | null; top: number | null } {
  const bounds = area.getBoundingClientRect();
  const row = rows(area).find((candidate) => {
    const box = candidate.getBoundingClientRect();
    return box.bottom > bounds.top && box.top < bounds.bottom;
  });
  return { key: row?.dataset.anchorKey ?? null, top: row?.getBoundingClientRect().top ?? null };
}

// Locates the row with an exact key, which may have been rebuilt by a re-render.
export function anchorRow(area: HTMLElement, key: string | null): HTMLElement | null {
  if (!key) return null;
  return rows(area).find((row) => row.dataset.anchorKey === key) ?? null;
}

function rows(area: HTMLElement): HTMLElement[] {
  return [...area.querySelectorAll<HTMLElement>(`[${ANCHOR_ATTRIBUTE}]`)];
}

// Links turn-view spans to transcript items in both directions: span activation reveals an item; item hover marks spans.

import { createSignal, type Accessor } from "solid-js";

// TranscriptTarget locates one transcript item from a turn timeline span.
export interface TranscriptTarget {
  // toolKey is the toolIdentity of a tool call, or of the call that launched
  // a command.
  toolKey: string | null;
  // nativeID is the HistoryActivity whose card is the target.
  nativeID: string | null;
  // ownerAgentID names the HistoryActivity whose transcript holds the call.
  ownerAgentID: string | null;
  // at, in epoch milliseconds, places a target that no element identifies,
  // such as model time, and finds the archived context that holds a target.
  at: number | null;
}

// TranscriptRevealer is the conversation's side of the link.
export interface TranscriptRevealer {
  // visible reports whether the conversation is on screen beside the caller.
  visible: () => boolean;
  // reveal scrolls to the target and marks it; focus moves keyboard focus to
  // it. A conversation that is not visible yet waits until it is.
  reveal: (target: TranscriptTarget, focus: boolean) => void;
}

export interface TranscriptLinks {
  revealer: Accessor<TranscriptRevealer | null>;
  setRevealer: (revealer: TranscriptRevealer | null) => void;
  // hovered is the link key of the transcript item under the pointer or focus.
  hovered: Accessor<string | null>;
  setHovered: (key: string | null) => void;
}

export function toolLinkKey(toolKey: string): string {
  return `tool:${toolKey}`;
}

export function nativeLinkKey(id: string): string {
  return `native:${id}`;
}

// linkKeyAt is the link key of the innermost tool call or native card that
// contains el, or null.
export function linkKeyAt(el: Element | null): string | null {
  const item = el?.closest<HTMLElement>('[data-testid="tool-call"][data-tool-key], [data-native-id]');
  if (item?.dataset.toolKey) return toolLinkKey(item.dataset.toolKey);
  if (item?.dataset.nativeId) return nativeLinkKey(item.dataset.nativeId);
  return null;
}

export function createTranscriptLinks(): TranscriptLinks {
  const [revealer, setRevealer] = createSignal<TranscriptRevealer | null>(null);
  const [hovered, setHovered] = createSignal<string | null>(null);
  return { revealer, setRevealer, hovered, setHovered };
}

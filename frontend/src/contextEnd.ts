// Names how a context section ended, in the same words for its transcript icon and its Performance view.

import type { HistorySection } from "@sdk/types.gen";

// ContextEndCause is the event that closed a section. The server closes one
// only on compaction, a context clear, or an init with a new session ID.
export type ContextEndCause = "compaction" | "clear" | "session";

export const contextEndLabels: Record<ContextEndCause, string> = {
  compaction: "Ended by compaction",
  clear: "Ended by a context clear",
  session: "Ended by a new session",
};

// contextEndCause classifies the event that ended section; null for the
// current section, which has not ended.
export function contextEndCause(section: HistorySection): ContextEndCause | null {
  const end = section.endBoundary?.message;
  if (!end) return null;
  const subtype = end.system?.subtype;
  if (subtype === "compact_boundary") return "compaction";
  if (subtype === "context_cleared") return "clear";
  return "session";
}

// contextEndDetail names the ending and adds the context sizes compaction reported.
export function contextEndDetail(section: HistorySection): string {
  const cause = contextEndCause(section);
  if (cause === null) return "Current context";
  const label = contextEndLabels[cause];
  const system = section.endBoundary?.message.system;
  if (system?.subtype !== "compact_boundary") return label;
  const before = system.contextTokensBefore ?? 0;
  const after = system.contextTokensAfter ?? 0;
  if (before > 0 && after > 0) return `${label} from ${before.toLocaleString()} to ${after.toLocaleString()} tokens`;
  return before > 0 ? `${label} of ${before.toLocaleString()} tokens` : label;
}

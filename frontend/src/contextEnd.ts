// Names how a context section ended, in the same words for its transcript row and its Performance view.

import type { HistorySection } from "@sdk/types.gen";

// contextEndLabel names the event that ended section: compaction, a clear, or
// a new session; null for the current section, which has not ended.
export function contextEndLabel(section: HistorySection): string | null {
  const end = section.endBoundary?.message;
  if (!end) return null;
  const subtype = end.system?.subtype;
  if (subtype === "compact_boundary") return "Ended by compaction";
  if (subtype === "context_cleared") return "Ended by a context clear";
  if (end.init) return "Ended by a new session";
  return "Ended";
}

// contextEndDetail adds the context sizes compaction reported to the label.
export function contextEndDetail(section: HistorySection): string {
  const label = contextEndLabel(section);
  if (label === null) return "Current context";
  const system = section.endBoundary?.message.system;
  if (system?.subtype !== "compact_boundary") return label;
  const before = system.contextTokensBefore ?? 0;
  const after = system.contextTokensAfter ?? 0;
  if (before > 0 && after > 0) return `${label} from ${before.toLocaleString()} to ${after.toLocaleString()} tokens`;
  return before > 0 ? `${label} of ${before.toLocaleString()} tokens` : label;
}

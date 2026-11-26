// Bounds tooltip text so agent-supplied labels such as multi-line commands cannot grow a tooltip without limit.

const maxLines = 16;
const maxLineLength = 140;

/** boundTooltipText keeps at most 16 lines of at most 140 characters, marking each cut with "…". */
export function boundTooltipText(text: string): string {
  const lines = text.split("\n");
  const kept = lines
    .slice(0, maxLines)
    .map((line) => (line.length > maxLineLength ? `${line.slice(0, maxLineLength - 1)}…` : line));
  if (lines.length > maxLines) kept[maxLines - 1] = "…";
  return kept.join("\n");
}

/** firstLine keeps a multi-line label's first line, marking the cut with "…". */
export function firstLine(text: string): string {
  const end = text.indexOf("\n");
  return end < 0 ? text : `${text.slice(0, end).trimEnd()} …`;
}

// Diff parsing and moved-block annotation for unified diffs.

export interface FileDiff {
  path: string;
  content: string;
  lines: DiffLine[];
}

export type DiffLineKind =
  | "context"
  | "added"
  | "deleted"
  | "hunk"
  | "header"
  | "movedAdded"
  | "movedDeleted";

export interface DiffLine {
  text: string;
  kind: DiffLineKind;
  movedVariant?: 0 | 1;
  whitespaceOnly?: boolean;
}

interface ChangeLine {
  content: string;
  lineIndex: number;
}

interface MovedBlock {
  addedLineIndexes: number[];
  deletedLineIndexes: number[];
}

const minMovedBlockAlnumChars = 20;

/** Extract the file path from a single diff section. */
export function extractDiffPath(section: string): string {
  // Prefer +++ line: "+++ b/path" or "+++ path" (most reliable).
  const plus = section.match(/^\+\+\+ (?:[a-z]\/)?(.+)/m);
  if (plus && plus[1] !== "/dev/null") return plus[1];
  // Deleted files: use --- line.
  const minus = section.match(/^--- (?:[a-z]\/)?(.+)/m);
  if (minus && minus[1] !== "/dev/null") return minus[1];
  // Renames: "rename to <path>" gives the destination path.
  const renameTo = section.match(/^rename to (.+)/m);
  if (renameTo) return renameTo[1];
  // Last resort: diff --git header (binary/empty files). Handles both "a/b/" prefixed and no-prefix formats.
  const git = section.match(/^diff --git (?:[a-z]\/)?(.+?) (?:[a-z]\/)?(.+)$/m);
  if (git && git[1] === git[2]) return git[1];
  return "unknown";
}

/** Split and annotate a unified diff into per-file sections. */
export function splitDiff(raw: string): FileDiff[] {
  if (!raw.trim()) return [];
  const lines = annotateDiffLines(raw);
  const sections: DiffLine[][] = [];

  for (const line of lines) {
    if (line.text.startsWith("diff --git ") || sections.length === 0) {
      sections.push([]);
    }
    sections[sections.length - 1].push(line);
  }

  return sections
    .filter((section) => section.some((line) => line.text.trim()))
    .map((section) => {
      const content = section.map((line) => line.text).join("\n");
      return {
        path: extractDiffPath(content),
        content,
        lines: section,
      };
    });
}

/** Annotate diff lines, including zebra-style moved block classification. */
export function annotateDiffLines(diff: string): DiffLine[] {
  const rawLines = diff.split("\n");
  const lines: DiffLine[] = rawLines.map((text) => ({ text, kind: lineKind(text) }));
  const addedByLineIndex = new Map<number, ChangeLine>();
  const deletedByLineIndex = new Map<number, ChangeLine>();
  const addedLineIndexesByContent = new Map<string, number[]>();

  for (let i = 0; i < rawLines.length; i++) {
    const text = rawLines[i];
    if (isAddedLine(text)) {
      const change = { lineIndex: i, content: text.slice(1) };
      addedByLineIndex.set(i, change);
      const indexes = addedLineIndexesByContent.get(change.content) ?? [];
      indexes.push(i);
      addedLineIndexesByContent.set(change.content, indexes);
    } else if (isDeletedLine(text)) {
      deletedByLineIndex.set(i, { lineIndex: i, content: text.slice(1) });
    }
  }

  const movedBlocks = findMovedBlocks(deletedByLineIndex, addedByLineIndex, addedLineIndexesByContent);
  movedBlocks.forEach((block, blockIndex) => {
    const movedVariant = (blockIndex % 2) as 0 | 1;
    for (const lineIndex of block.addedLineIndexes) {
      lines[lineIndex] = { text: lines[lineIndex].text, kind: "movedAdded", movedVariant };
    }
    for (const lineIndex of block.deletedLineIndexes) {
      lines[lineIndex] = { text: lines[lineIndex].text, kind: "movedDeleted", movedVariant };
    }
  });

  markWhitespaceOnlyChanges(lines);

  return lines;
}

function lineKind(line: string): DiffLineKind {
  if (line.startsWith("@@")) return "hunk";
  if (line.startsWith("diff ") || line.startsWith("index ") || line.startsWith("---") || line.startsWith("+++")) {
    return "header";
  }
  if (line.startsWith("+")) return "added";
  if (line.startsWith("-")) return "deleted";
  return "context";
}

function isAddedLine(line: string): boolean {
  return line.startsWith("+") && !line.startsWith("+++");
}

function isDeletedLine(line: string): boolean {
  return line.startsWith("-") && !line.startsWith("---");
}

function markWhitespaceOnlyChanges(lines: DiffLine[]) {
  let index = 0;
  while (index < lines.length) {
    if (!isChangeLine(lines[index])) {
      index++;
      continue;
    }

    const deletedLineIndexes: number[] = [];
    const addedLineIndexes: number[] = [];
    while (index < lines.length && isChangeLine(lines[index])) {
      const line = lines[index];
      if (line.kind === "deleted") deletedLineIndexes.push(index);
      else if (line.kind === "added") addedLineIndexes.push(index);
      index++;
    }

    markWhitespaceOnlyChangeGroup(lines, deletedLineIndexes, addedLineIndexes);
  }
}

function isChangeLine(line: DiffLine): boolean {
  return line.kind === "added" || line.kind === "deleted";
}

function markWhitespaceOnlyChangeGroup(
  lines: DiffLine[],
  deletedLineIndexes: number[],
  addedLineIndexes: number[],
) {
  const usedAddedLineIndexes = new Set<number>();
  for (const deletedLineIndex of deletedLineIndexes) {
    const deletedContent = lines[deletedLineIndex].text.slice(1);
    const normalizedDeleted = removeWhitespace(deletedContent);
    const addedLineIndex = addedLineIndexes.find((candidate) => {
      if (usedAddedLineIndexes.has(candidate)) return false;
      const addedContent = lines[candidate].text.slice(1);
      return addedContent !== deletedContent && removeWhitespace(addedContent) === normalizedDeleted;
    });
    if (addedLineIndex === undefined) continue;

    usedAddedLineIndexes.add(addedLineIndex);
    lines[deletedLineIndex].whitespaceOnly = true;
    lines[addedLineIndex].whitespaceOnly = true;
  }
}

function removeWhitespace(text: string): string {
  return text.replace(/\s/g, "");
}

function findMovedBlocks(
  deletedByLineIndex: Map<number, ChangeLine>,
  addedByLineIndex: Map<number, ChangeLine>,
  addedLineIndexesByContent: Map<string, number[]>,
): MovedBlock[] {
  const blocks: MovedBlock[] = [];
  const usedAddedLineIndexes = new Set<number>();
  const usedDeletedLineIndexes = new Set<number>();
  const deletedLineIndexes = [...deletedByLineIndex.keys()].sort((a, b) => a - b);

  for (const deletedStart of deletedLineIndexes) {
    if (usedDeletedLineIndexes.has(deletedStart)) continue;
    const deleted = deletedByLineIndex.get(deletedStart);
    if (!deleted) continue;

    const addedCandidates = addedLineIndexesByContent.get(deleted.content) ?? [];
    let bestBlock: MovedBlock | undefined;
    let bestAlnumCount = 0;

    for (const addedStart of addedCandidates) {
      if (usedAddedLineIndexes.has(addedStart)) continue;
      const block = expandMovedBlock(
        deletedStart,
        addedStart,
        deletedByLineIndex,
        addedByLineIndex,
        usedDeletedLineIndexes,
        usedAddedLineIndexes,
      );
      const alnumCount = countMovedBlockAlnum(block, deletedByLineIndex);
      if (alnumCount >= minMovedBlockAlnumChars && alnumCount > bestAlnumCount) {
        bestBlock = block;
        bestAlnumCount = alnumCount;
      }
    }

    if (!bestBlock) continue;
    for (const lineIndex of bestBlock.addedLineIndexes) usedAddedLineIndexes.add(lineIndex);
    for (const lineIndex of bestBlock.deletedLineIndexes) usedDeletedLineIndexes.add(lineIndex);
    blocks.push(bestBlock);
  }

  return blocks;
}

function expandMovedBlock(
  deletedStart: number,
  addedStart: number,
  deletedByLineIndex: Map<number, ChangeLine>,
  addedByLineIndex: Map<number, ChangeLine>,
  usedDeletedLineIndexes: Set<number>,
  usedAddedLineIndexes: Set<number>,
): MovedBlock {
  const addedLineIndexes: number[] = [];
  const deletedLineIndexes: number[] = [];
  let deletedIndex = deletedStart;
  let addedIndex = addedStart;

  while (!usedDeletedLineIndexes.has(deletedIndex) && !usedAddedLineIndexes.has(addedIndex)) {
    const deleted = deletedByLineIndex.get(deletedIndex);
    const added = addedByLineIndex.get(addedIndex);
    if (!deleted || !added || deleted.content !== added.content) break;
    deletedLineIndexes.push(deletedIndex);
    addedLineIndexes.push(addedIndex);
    deletedIndex++;
    addedIndex++;
  }

  return { addedLineIndexes, deletedLineIndexes };
}

function countMovedBlockAlnum(block: MovedBlock, deletedByLineIndex: Map<number, ChangeLine>): number {
  let count = 0;
  for (const lineIndex of block.deletedLineIndexes) {
    const content = deletedByLineIndex.get(lineIndex)?.content ?? "";
    count += countAlnum(content);
  }
  return count;
}

function countAlnum(text: string): number {
  let count = 0;
  for (let i = 0; i < text.length; i++) {
    const c = text.charCodeAt(i);
    if (
      (c >= 48 && c <= 57) ||
      (c >= 65 && c <= 90) ||
      (c >= 97 && c <= 122)
    ) {
      count++;
    }
  }
  return count;
}

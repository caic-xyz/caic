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
  const plus = section.match(/^\+\+\+ (.+)/m);
  if (plus) {
    const path = normalizeDiffPath(plus[1], "b", section);
    if (path !== "/dev/null") return path;
  }
  // Deleted files: use --- line.
  const minus = section.match(/^--- (.+)/m);
  if (minus) {
    const path = normalizeDiffPath(minus[1], "a", section);
    if (path !== "/dev/null") return path;
  }
  // Renames: "rename to <path>" gives the destination path.
  const renameTo = section.match(/^rename to (.+)/m);
  if (renameTo) return normalizeDiffPath(renameTo[1], "b", section);
  // Binary lines carry paths even when no text hunk exists.
  const binary = section.match(/^Binary files (.+) and (.+) differ$/m);
  if (binary) {
    const newPath = normalizeDiffPath(binary[2], "b", section);
    if (newPath !== "/dev/null") return newPath;
    return normalizeDiffPath(binary[1], "a", section);
  }
  // Last resort: diff --git header (binary/empty files). Handles both "a/b/" prefixed and no-prefix formats.
  const git = section.match(/^diff --git (.+)$/m);
  const gitPaths = git ? splitDiffGitPaths(git[1]) : null;
  if (gitPaths) {
    const oldPath = normalizeDiffPath(gitPaths[0], "a", section);
    const newPath = normalizeDiffPath(gitPaths[1], "b", section);
    if (oldPath === newPath) return newPath;
  }
  return "unknown";
}

function normalizeDiffPath(raw: string, side: "a" | "b", section: string): string {
  const path = stripGitHeaderTab(raw);
  const usesSidePrefixes = diffHeaderUsesSidePrefixes(section);
  const sidePrefix = `${side}/`;
  if (path.startsWith(sidePrefix) && usesSidePrefixes) {
    return path.slice(sidePrefix.length);
  }
  const headerPath = diffGitSidePath(section, side, usesSidePrefixes);
  if (headerPath && headerPath !== path && headerPath.endsWith(`/${path}`)) {
    return headerPath;
  }
  return path;
}

function stripGitHeaderTab(path: string): string {
  const tab = path.indexOf("\t");
  return tab === -1 ? path : path.slice(0, tab);
}

function diffGitSidePath(section: string, side: "a" | "b", usesSidePrefixes: boolean): string | null {
  const git = section.match(/^diff --git (.+)$/m);
  const paths = git ? splitDiffGitPaths(git[1]) : null;
  if (!paths) return null;
  const path = stripGitHeaderTab(side === "a" ? paths[0] : paths[1]);
  const sidePrefix = `${side}/`;
  if (usesSidePrefixes && path.startsWith(sidePrefix)) return path.slice(sidePrefix.length);
  return path;
}

function splitDiffGitPaths(body: string): [string, string] | null {
  if (body.startsWith("a/")) {
    const sideSeparator = body.indexOf(" b/");
    if (sideSeparator !== -1) return [body.slice(0, sideSeparator), body.slice(sideSeparator + 1)];
  }
  for (let i = 0; i < body.length; i++) {
    if (body[i] === " " && body.slice(0, i) === body.slice(i + 1)) {
      return [body.slice(0, i), body.slice(i + 1)];
    }
  }
  const separator = body.indexOf(" ");
  if (separator === -1) return null;
  return [body.slice(0, separator), body.slice(separator + 1)];
}

function diffHeaderUsesSidePrefixes(section: string): boolean {
  const git = section.match(/^diff --git .+$/m);
  if (!git) return true;
  return git[0].startsWith("diff --git a/") && git[0].includes(" b/");
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
  let inHunk = false;
  const lines: DiffLine[] = rawLines.map((text) => {
    if (text.startsWith("diff --git ")) inHunk = false;
    const kind = lineKind(text, inHunk);
    if (kind === "hunk") inHunk = true;
    return { text, kind };
  });
  const addedByLineIndex = new Map<number, ChangeLine>();
  const deletedByLineIndex = new Map<number, ChangeLine>();
  const addedLineIndexesByContent = new Map<string, number[]>();

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    if (line.kind === "added") {
      const change = { lineIndex: i, content: line.text.slice(1) };
      addedByLineIndex.set(i, change);
      const indexes = addedLineIndexesByContent.get(change.content) ?? [];
      indexes.push(i);
      addedLineIndexesByContent.set(change.content, indexes);
    } else if (line.kind === "deleted") {
      deletedByLineIndex.set(i, { lineIndex: i, content: line.text.slice(1) });
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

function lineKind(line: string, inHunk: boolean): DiffLineKind {
  if (line.startsWith("@@")) return "hunk";
  const isFileHeader = !inHunk && (line.startsWith("index ") || line.startsWith("--- ") || line.startsWith("+++ "));
  if (line.startsWith("diff --git ") || isFileHeader) return "header";
  if (line.startsWith("+")) return "added";
  if (line.startsWith("-")) return "deleted";
  return "context";
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

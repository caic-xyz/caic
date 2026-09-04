// UnifiedDiffBlock renders annotated unified diffs with optional transport-header trimming.

import { createMemo, For } from "solid-js";

import { annotateDiffLines, type DiffLine } from "./diffLines";
import styles from "./UnifiedDiffBlock.module.css";

type Props = {
  compact?: boolean;
  hideFileHeader?: boolean;
  lineWrap?: boolean;
} & ({ diff: string; lines?: never } | { lines: DiffLine[]; diff?: never });

export default function UnifiedDiffBlock(props: Props) {
  const lines = createMemo(() => {
    const annotated = props.lines ?? annotateDiffLines(props.diff);
    return props.hideFileHeader
      ? annotated.filter((line) => line.kind !== "header")
      : annotated;
  });
  return (
    <pre
      class={styles.block}
      classList={{
        [styles.compact]: props.compact,
        [styles.lineWrap]: props.lineWrap,
      }}
    >
      <For each={lines()}>{(line) => <UnifiedDiffLine line={line} />}</For>
    </pre>
  );
}

function UnifiedDiffLine(props: { line: DiffLine }) {
  return (
    <div class={`${styles.line} ${diffLineClass(props.line)}`}>
      {props.line.text}
    </div>
  );
}

function diffLineClass(line: DiffLine): string {
  switch (line.kind) {
    case "added":
      return line.whitespaceOnly
        ? styles.lineAddedWhitespace
        : styles.lineAdded;
    case "deleted":
      return line.whitespaceOnly
        ? styles.lineDeletedWhitespace
        : styles.lineDeleted;
    case "hunk":
      return styles.lineHunk;
    case "header":
      return styles.lineHeader;
    case "movedAdded":
      return line.movedVariant === 1
        ? styles.lineMovedAddedAlt
        : styles.lineMovedAdded;
    case "movedDeleted":
      return line.movedVariant === 1
        ? styles.lineMovedDeletedAlt
        : styles.lineMovedDeleted;
    case "context":
      return "";
  }
}

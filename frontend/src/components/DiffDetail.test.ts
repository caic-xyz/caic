// Tests for DiffDetail diff parsing utilities.
import { describe, it, expect } from "vitest";
import { annotateDiffLines, extractDiffPath, splitDiff } from "./diffLines";

describe("extractDiffPath", () => {
  it("returns path from +++ line for modified file", () => {
    const section = `diff --git a/foo/bar.ts b/foo/bar.ts\nindex abc..def 100644\n--- a/foo/bar.ts\n+++ b/foo/bar.ts\n@@ -1 +1 @@\n-old\n+new\n`;
    expect(extractDiffPath(section)).toBe("foo/bar.ts");
  });

  it("returns path from --- line for deleted file", () => {
    const section = `diff --git a/foo/bar.ts b/foo/bar.ts\ndeleted file mode 100644\nindex abc..000 100644\n--- a/foo/bar.ts\n+++ /dev/null\n@@ -1 +0,0 @@\n-old\n`;
    expect(extractDiffPath(section)).toBe("foo/bar.ts");
  });

  it("returns path from diff --git line for binary/empty file", () => {
    const section = `diff --git a/img.png b/img.png\nnew file mode 100644\nindex 000..abc\nBinary files /dev/null and b/img.png differ\n`;
    expect(extractDiffPath(section)).toBe("img.png");
  });

  it("returns destination path for renamed file", () => {
    const section = `diff --git a/old/name#suffix b/new/name/suffix\nsimilarity index 100%\nrename from old/name#suffix\nrename to new/name/suffix\n`;
    expect(extractDiffPath(section)).toBe("new/name/suffix");
  });
});

describe("annotateDiffLines", () => {
  it("marks matching added and deleted blocks as moved", () => {
    const diff = [
      "diff --git a/src/main.ts b/src/main.ts",
      "--- a/src/main.ts",
      "+++ b/src/main.ts",
      "@@ -1,8 +1,8 @@",
      " const keep = true;",
      "-function movedExample() {",
      "-  return alphaBetaGammaDelta;",
      "-}",
      " const middle = true;",
      "+function movedExample() {",
      "+  return alphaBetaGammaDelta;",
      "+}",
    ].join("\n");

    const lines = annotateDiffLines(diff);

    expect(lines.filter((line) => line.kind === "movedDeleted")).toHaveLength(3);
    expect(lines.filter((line) => line.kind === "movedAdded")).toHaveLength(3);
  });

  it("does not mark matching blocks below Git's alphanumeric threshold", () => {
    const diff = [
      "diff --git a/src/main.ts b/src/main.ts",
      "--- a/src/main.ts",
      "+++ b/src/main.ts",
      "@@ -1,5 +1,5 @@",
      "-x = 1;",
      " context",
      "+x = 1;",
    ].join("\n");

    const lines = annotateDiffLines(diff);

    expect(lines.some((line) => line.kind === "movedDeleted" || line.kind === "movedAdded")).toBe(false);
  });

  it("alternates moved block variants", () => {
    const diff = [
      "diff --git a/src/main.ts b/src/main.ts",
      "--- a/src/main.ts",
      "+++ b/src/main.ts",
      "@@ -1,12 +1,12 @@",
      "-function firstMovedBlock() {",
      "-  return firstMovedValue;",
      "-}",
      " context",
      "-function secondMovedBlock() {",
      "-  return secondMovedValue;",
      "-}",
      " context",
      "+function firstMovedBlock() {",
      "+  return firstMovedValue;",
      "+}",
      " context",
      "+function secondMovedBlock() {",
      "+  return secondMovedValue;",
      "+}",
    ].join("\n");

    const movedDeleted = annotateDiffLines(diff).filter((line) => line.kind === "movedDeleted");

    expect(movedDeleted[0].movedVariant).toBe(0);
    expect(movedDeleted[3].movedVariant).toBe(1);
  });
});

describe("splitDiff", () => {
  it("preserves moved annotations across files", () => {
    const raw = [
      "diff --git a/old.ts b/old.ts",
      "--- a/old.ts",
      "+++ b/old.ts",
      "@@ -1,3 +0,0 @@",
      "-function movedBetweenFiles() {",
      "-  return movedBetweenFilesValue;",
      "-}",
      "diff --git a/new.ts b/new.ts",
      "--- a/new.ts",
      "+++ b/new.ts",
      "@@ -0,0 +1,3 @@",
      "+function movedBetweenFiles() {",
      "+  return movedBetweenFilesValue;",
      "+}",
    ].join("\n");

    const files = splitDiff(raw);

    expect(files).toHaveLength(2);
    expect(files[0].lines.filter((line) => line.kind === "movedDeleted")).toHaveLength(3);
    expect(files[1].lines.filter((line) => line.kind === "movedAdded")).toHaveLength(3);
  });
});

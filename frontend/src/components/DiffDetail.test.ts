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

  it("returns canonical multi-repo path from no-prefix binary diff headers", () => {
    const section = `diff --git caic/img file.png caic/img file.png\nBinary files caic/img file.png and caic/img file.png differ\n`;
    expect(extractDiffPath(section)).toBe("caic/img file.png");
  });

  it("returns destination path for renamed file", () => {
    const section = `diff --git a/old/name#suffix b/new/name/suffix\nsimilarity index 100%\nrename from old/name#suffix\nrename to new/name/suffix\n`;
    expect(extractDiffPath(section)).toBe("new/name/suffix");
  });

  it("returns canonical multi-repo paths for pure renames", () => {
    const section = `diff --git a/caic/old.txt b/caic/new.txt\nsimilarity index 100%\nrename from old.txt\nrename to new.txt\n`;
    expect(extractDiffPath(section)).toBe("caic/new.txt");
  });

  it("returns canonical multi-repo paths", () => {
    const section = `diff --git a/caic/main.go b/caic/main.go\nindex abc..def 100644\n--- a/caic/main.go\n+++ b/caic/main.go\n@@ -1 +1 @@\n-old\n+new\n`;
    expect(extractDiffPath(section)).toBe("caic/main.go");
  });

  it("keeps no-prefix paths that start with b", () => {
    const section = `diff --git b/main.go b/main.go\nindex abc..def 100644\n--- b/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-old\n+new\n`;
    expect(extractDiffPath(section)).toBe("b/main.go");
  });

  it("removes git's trailing tab marker from no-prefix paths", () => {
    const section = `diff --git path with space.txt path with space.txt\nindex abc..def 100644\n--- path with space.txt\t\n+++ path with space.txt\t\n@@ -1 +1 @@\n-old\n+new\n`;
    expect(extractDiffPath(section)).toBe("path with space.txt");
  });
});

describe("annotateDiffLines", () => {
  it("treats hunk content lines starting with file-header markers as changes", () => {
    const diff = [
      "diff --git a/README.md b/README.md",
      "--- a/README.md",
      "+++ b/README.md",
      "@@ -1,2 +1,2 @@",
      "--- old heading",
      "+++ new heading",
    ].join("\n");

    const lines = annotateDiffLines(diff);

    expect(lines.find((line) => line.text === "--- old heading")?.kind).toBe("deleted");
    expect(lines.find((line) => line.text === "+++ new heading")?.kind).toBe("added");
  });

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

  it("marks paired added and deleted lines that only change whitespace", () => {
    const diff = [
      "diff --git a/src/main.ts b/src/main.ts",
      "--- a/src/main.ts",
      "+++ b/src/main.ts",
      "@@ -1,3 +1,3 @@",
      " const keep = true;",
      "-const value = alpha + beta;",
      "+const value = alpha  + beta;",
      "-const changedText = alpha;",
      "+const changedText = beta;",
    ].join("\n");

    const lines = annotateDiffLines(diff);

    expect(lines.find((line) => line.text === "-const value = alpha + beta;")?.whitespaceOnly).toBe(true);
    expect(lines.find((line) => line.text === "+const value = alpha  + beta;")?.whitespaceOnly).toBe(true);
    expect(lines.find((line) => line.text === "-const changedText = alpha;")?.whitespaceOnly).toBeUndefined();
    expect(lines.find((line) => line.text === "+const changedText = beta;")?.whitespaceOnly).toBeUndefined();
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

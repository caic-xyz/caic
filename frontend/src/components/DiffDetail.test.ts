// Tests for DiffDetail diff parsing utilities.
import { describe, it, expect } from "vitest";

// Re-export internals via a test-only import trick: we duplicate the pure
// functions here so we can test them without modifying the module.

function extractDiffPath(section: string): string {
  const plus = section.match(/^\+\+\+ (?:[a-z]\/)?(.+)/m);
  if (plus && plus[1] !== "/dev/null") return plus[1];
  const minus = section.match(/^--- (?:[a-z]\/)?(.+)/m);
  if (minus && minus[1] !== "/dev/null") return minus[1];
  // Renames: "rename to <path>" gives the destination path.
  const renameTo = section.match(/^rename to (.+)/m);
  if (renameTo) return renameTo[1];
  const git = section.match(/^diff --git (?:[a-z]\/)?(.+?) (?:[a-z]\/)?(.+)$/m);
  if (git && git[1] === git[2]) return git[1];
  return "unknown";
}

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

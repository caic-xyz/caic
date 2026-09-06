// Tests for DiffDetail repository status rendering and diff parsing utilities.

import { fireEvent, render, screen } from "@solidjs/testing-library";
import { describe, it, expect, vi } from "vitest";

import type { DiffResp } from "@sdk/types.gen";

const { getTaskDiffMock } = vi.hoisted(() => ({
  getTaskDiffMock: vi.fn<() => Promise<DiffResp>>(),
}));

vi.mock("@solidjs/router", () => ({
  useNavigate: () => vi.fn(),
}));

vi.mock("../api", () => ({
  getTaskDiff: getTaskDiffMock,
}));

import { annotateDiffLines, extractDiffPath, splitDiff } from "./diffLines";
import DiffDetail, { elidePathAtBoundary } from "./DiffDetail";
import styles from "./DiffDetail.module.css";

describe("DiffDetail", () => {
  it("separates upstream commits from uncommitted files", async () => {
    getTaskDiffMock.mockResolvedValueOnce({
      diff: "",
      repositories: [
        {
          name: "caic",
          branch: "caic-42",
          upstream: "origin/main",
          ahead: 2,
          behind: 1,
          commits: [
            {
              sha: "1234567890abcdef",
              authoredDate: "2026-09-03",
              decorations: "HEAD -> caic-42, host/caic-42, tag: v1.2.3",
              subject: "Surface git status",
              stat: [
                {
                  path: "frontend/view.tsx",
                  added: 10,
                  deleted: 0,
                  binary: false,
                  diff: "@@ -1 +1 @@\n-old view\n+new view",
                },
              ],
            },
            {
              sha: "abcdef1234567890",
              authoredDate: "2026-09-02",
              subject: "Add status tests",
              stat: [
                {
                  path: "frontend/view.test.tsx",
                  added: 8,
                  deleted: 0,
                  binary: false,
                  diff: "@@ -0,0 +1 @@\n+new test",
                },
              ],
            },
          ],
          uncommitted: [
            {
              path: "frontend/working.tsx",
              worktreeStatus: "M",
              added: 2,
              deleted: 1,
              binary: false,
              diff: "@@ -1 +1,2 @@\n-old working\n+new working\n+line",
            },
            {
              path: "frontend/new.tsx",
              indexStatus: "A",
              added: 1,
              deleted: 0,
              binary: false,
              diff: "@@ -0,0 +1 @@\n+new file",
            },
            {
              path: "frontend/untracked.tsx",
              indexStatus: "?",
              worktreeStatus: "?",
              added: 1,
              deleted: 0,
              binary: false,
              diff: "@@ -0,0 +1 @@\n+untracked file",
            },
          ],
        },
      ],
    });

    render(() => <DiffDetail taskId="task-1" taskPath="/task/task-1" />);

    expect(await screen.findByText("origin/main")).toBeInTheDocument();
    expect(screen.getByText(/2 commits ahead/)).toHaveTextContent(
      "2 commits ahead · 1 behind",
    );
    expect(screen.getByText("Commits ahead (2)")).toBeInTheDocument();
    expect(screen.getByText("12345678")).toHaveClass(styles.commitSha);
    expect(screen.getByText("2026-09-03")).toHaveAttribute(
      "datetime",
      "2026-09-03",
    );
    expect(
      screen.getByText("HEAD -> caic-42, host/caic-42, tag: v1.2.3"),
    ).toHaveClass(styles.commitDecorations);
    expect(screen.getByText("Surface git status")).toBeInTheDocument();
    expect(screen.getByText("+10")).toHaveClass(styles.added);
    expect(screen.getAllByText("1 file changed")).toHaveLength(2);
    expect(screen.getByText("Uncommitted changes (3)")).toBeInTheDocument();
    expect(screen.getByText("staged: added")).toBeInTheDocument();
    expect(screen.getByText("untracked")).toBeInTheDocument();
    expect(screen.getByText("+2")).toHaveClass(styles.added);
    expect(screen.getByText("−1")).toHaveClass(styles.deleted);

    const committedFile = screen.getByRole("button", {
      name: /frontend\/view\.tsx/,
    });
    expect(committedFile).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(committedFile);
    expect(committedFile).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByText("+new view")).toBeInTheDocument();

    const workingFile = screen.getByRole("button", {
      name: /frontend\/working\.tsx/,
    });
    expect(workingFile).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(workingFile);
    expect(screen.getByText("+new working")).toBeInTheDocument();
  });

  it("retains complete long repository values", async () => {
    const branch = "feature/surface-complete-container-repository-status";
    const upstream = "origin/feature/with-a-very-long-upstream-branch-name";
    const subject =
      "Describe every committed file without truncating the complete commit subject";
    const committedPath =
      "frontend/src/components/repository-status/VeryLongCommittedFilename.tsx";
    const originalPath =
      "frontend/src/components/repository-status/VeryLongOriginalFilename.tsx";
    const uncommittedPath =
      "frontend/src/components/repository-status/VeryLongRenamedFilename.tsx";
    getTaskDiffMock.mockResolvedValueOnce({
      diff: "",
      repositories: [
        {
          name: "caic",
          branch,
          upstream,
          ahead: 1,
          behind: 0,
          commits: [
            {
              sha: "1234567890abcdef",
              authoredDate: "2026-09-03",
              subject,
              stat: [
                {
                  path: committedPath,
                  added: 1,
                  deleted: 0,
                  binary: false,
                  diff: "@@ -0,0 +1 @@\n+content",
                },
              ],
            },
          ],
          uncommitted: [
            {
              path: uncommittedPath,
              originalPath,
              worktreeStatus: "R",
              added: 0,
              deleted: 0,
              binary: false,
              diff: "",
            },
          ],
        },
      ],
    });

    render(() => <DiffDetail taskId="task-1" taskPath="/task/task-1" />);

    expect(await screen.findByText(branch)).toBeInTheDocument();
    expect(screen.getByText(upstream)).toBeInTheDocument();
    expect(screen.getByText(subject)).toBeInTheDocument();
    const committedFile = screen.getByTitle(committedPath);
    expect(committedFile).toHaveAttribute("title", committedPath);
    expect(committedFile).toHaveAccessibleName(committedPath);
    expect(
      committedFile.querySelector(`.${styles.pathValue}`),
    ).toHaveTextContent(committedPath);
    const renamedFile = screen.getByTitle(
      `${originalPath} → ${uncommittedPath}`,
    );
    expect(renamedFile).toHaveAttribute(
      "title",
      `${originalPath} → ${uncommittedPath}`,
    );
    expect(renamedFile.querySelectorAll(`.${styles.pathValue}`)).toHaveLength(2);
    expect(renamedFile).toHaveTextContent(
      `${originalPath} → ${uncommittedPath}`,
    );
  });
});

describe("elidePathAtBoundary", () => {
  const measureCharacters = (text: string) => text.length;

  it("removes complete directory segments before touching the filename", () => {
    expect(
      elidePathAtBoundary(
        "backend/internal/server/apiconv/oauth_handlers_test.go",
        33,
        measureCharacters,
      ),
    ).toBe("backend/…/oauth_handlers_test.go");
  });

  it("keeps the complete path when it fits", () => {
    expect(
      elidePathAtBoundary(
        "backend/internal/types.go",
        40,
        measureCharacters,
      ),
    ).toBe("backend/internal/types.go");
  });

  it("keeps the complete basename when only the basename fits", () => {
    expect(
      elidePathAtBoundary(
        "backend/internal/server/oauth_handlers_test.go",
        22,
        measureCharacters,
      ),
    ).toBe("oauth_handlers_test.go");
  });
});

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

    expect(lines.find((line) => line.text === "--- old heading")?.kind).toBe(
      "deleted",
    );
    expect(lines.find((line) => line.text === "+++ new heading")?.kind).toBe(
      "added",
    );
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

    expect(lines.filter((line) => line.kind === "movedDeleted")).toHaveLength(
      3,
    );
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

    expect(
      lines.some(
        (line) => line.kind === "movedDeleted" || line.kind === "movedAdded",
      ),
    ).toBe(false);
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

    expect(
      lines.find((line) => line.text === "-const value = alpha + beta;")
        ?.whitespaceOnly,
    ).toBe(true);
    expect(
      lines.find((line) => line.text === "+const value = alpha  + beta;")
        ?.whitespaceOnly,
    ).toBe(true);
    expect(
      lines.find((line) => line.text === "-const changedText = alpha;")
        ?.whitespaceOnly,
    ).toBeUndefined();
    expect(
      lines.find((line) => line.text === "+const changedText = beta;")
        ?.whitespaceOnly,
    ).toBeUndefined();
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

    const movedDeleted = annotateDiffLines(diff).filter(
      (line) => line.kind === "movedDeleted",
    );

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
    expect(
      files[0].lines.filter((line) => line.kind === "movedDeleted"),
    ).toHaveLength(3);
    expect(
      files[1].lines.filter((line) => line.kind === "movedAdded"),
    ).toHaveLength(3);
  });
});

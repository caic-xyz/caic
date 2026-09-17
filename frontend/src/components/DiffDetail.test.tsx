// Tests for DiffDetail repository status rendering and diff parsing utilities.

import { fireEvent, render, screen, waitFor } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, it, expect, vi } from "vitest";

import type { FileDiffResp, TaskDiffIndexResp } from "@sdk/types.gen";

const { getTaskDiffIndexMock, getTaskFileDiffMock } = vi.hoisted(() => ({
  getTaskDiffIndexMock: vi.fn<(id: string) => Promise<TaskDiffIndexResp>>(),
  getTaskFileDiffMock:
    vi.fn<
      (
        id: string,
        repository: string,
        commit: string,
        path: string,
        originalPath: string,
      ) => Promise<FileDiffResp>
    >(),
}));

vi.mock("@solidjs/router", () => ({
  useNavigate: () => vi.fn(),
}));

vi.mock("../api", () => ({
  getTaskDiffIndex: getTaskDiffIndexMock,
  getTaskFileDiff: getTaskFileDiffMock,
}));

import { annotateDiffLines, extractDiffPath, splitDiff } from "./diffLines";
import DiffDetail, { elidePathAtBoundary } from "./DiffDetail";
import { taskDiffCache } from "../diffCache";
import styles from "./DiffDetail.module.css";

describe("DiffDetail", () => {
  beforeEach(() => {
    getTaskDiffIndexMock.mockReset();
    getTaskFileDiffMock.mockReset();
    taskDiffCache.evictTask("task-1");
  });

  it("renders the index before an expanded patch resolves", async () => {
    let resolvePatch: (response: FileDiffResp) => void = () => undefined;
    const patch = new Promise<FileDiffResp>((resolve) => {
      resolvePatch = resolve;
    });
    getTaskDiffIndexMock.mockResolvedValueOnce(diffIndexFixture());
    getTaskFileDiffMock.mockReturnValueOnce(patch);

    render(() => <DiffDetail taskId="task-1" taskPath="/task/task-1" />);

    expect(await screen.findByText("Commits ahead (1)")).toBeInTheDocument();
    const row = screen.getByRole("button", { name: "committed.go" });
    expect(getTaskFileDiffMock).not.toHaveBeenCalled();

    fireEvent.click(row);

    expect(screen.getByRole("status")).toHaveTextContent(
      "Loading file diff...",
    );
    expect(screen.getByRole("status")).toHaveAttribute("aria-live", "polite");
    expect(screen.getByText("Commit subject")).toBeInTheDocument();
    expect(getTaskFileDiffMock).toHaveBeenCalledWith(
      "task-1",
      "0",
      "1234567890abcdef1234567890abcdef12345678",
      "committed.go",
      "",
    );
    resolvePatch({ diff: "@@ -1 +1 @@\n-old\n+new" });
    expect(await screen.findByText("+new")).toBeInTheDocument();
  });

  it("requests only the selector for the expanded row", async () => {
    getTaskDiffIndexMock.mockResolvedValueOnce(diffIndexFixture());
    getTaskFileDiffMock.mockResolvedValueOnce({
      diff: "@@ -1 +1 @@\n-old\n+new",
    });

    render(() => <DiffDetail taskId="task-1" taskPath="/task/task-1" />);
    const row = await screen.findByRole("button", {
      name: "old.go → working.go",
    });
    fireEvent.click(row);

    expect(await screen.findByText("+new")).toBeInTheDocument();
    expect(getTaskFileDiffMock).toHaveBeenCalledTimes(1);
    expect(getTaskFileDiffMock).toHaveBeenCalledWith(
      "task-1",
      "0",
      "",
      "working.go",
      "old.go",
    );
  });

  it("selects a file from the second repository by index", async () => {
    const index = diffIndexFixture();
    index.repositories.push({
      name: "tools",
      branch: "feature",
      ahead: 0,
      behind: 0,
      commits: [],
      uncommitted: [
        {
          path: "second.go",
          worktreeStatus: "M",
          added: 2,
          deleted: 0,
          binary: false,
        },
      ],
    });
    getTaskDiffIndexMock.mockResolvedValueOnce(index);
    getTaskFileDiffMock.mockResolvedValueOnce({
      diff: "@@ -0,0 +1 @@\n+second",
    });

    render(() => <DiffDetail taskId="task-1" taskPath="/task/task-1" />);
    fireEvent.click(await screen.findByRole("button", { name: "second.go" }));

    expect(await screen.findByText("+second")).toBeInTheDocument();
    expect(getTaskFileDiffMock).toHaveBeenCalledWith(
      "task-1",
      "1",
      "",
      "second.go",
      "",
    );
  });

  it("keeps retry focus during loading and restores it to the row", async () => {
    const user = userEvent.setup();
    let resolveRetry: (response: FileDiffResp) => void = () => undefined;
    const retry = new Promise<FileDiffResp>((resolve) => {
      resolveRetry = resolve;
    });
    getTaskDiffIndexMock.mockResolvedValueOnce(diffIndexFixture());
    getTaskFileDiffMock
      .mockRejectedValueOnce(new Error("patch unavailable"))
      .mockReturnValueOnce(retry);

    render(() => <DiffDetail taskId="task-1" taskPath="/task/task-1" />);
    const row = await screen.findByRole("button", { name: "committed.go" });
    fireEvent.click(row);

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "patch unavailable",
    );
    const retryButton = screen.getByRole("button", {
      name: "Retry diff for committed.go",
    });
    retryButton.focus();
    await user.keyboard("{Enter}");

    expect(retryButton).toBeEnabled();
    expect(retryButton).toHaveAttribute("aria-disabled", "true");
    expect(retryButton).toHaveFocus();
    expect(screen.getByRole("status")).toHaveTextContent(
      "Retrying file diff...",
    );
    resolveRetry({ diff: "@@ -1 +1 @@\n-old\n+retried" });
    expect(await screen.findByText("+retried")).toBeInTheDocument();
    await waitFor(() => expect(row).toHaveFocus());
    expect(getTaskFileDiffMock).toHaveBeenCalledTimes(2);
  });

  it("deduplicates an in-flight request when a row is reopened", async () => {
    getTaskDiffIndexMock.mockResolvedValueOnce(diffIndexFixture());
    getTaskFileDiffMock.mockReturnValueOnce(new Promise(() => undefined));

    render(() => <DiffDetail taskId="task-1" taskPath="/task/task-1" />);
    const row = await screen.findByRole("button", { name: "committed.go" });
    fireEvent.click(row);
    fireEvent.click(row);
    fireEvent.click(row);

    expect(screen.getByText("Loading file diff...")).toBeInTheDocument();
    expect(getTaskFileDiffMock).toHaveBeenCalledTimes(1);
  });

  it("reuses a successful patch when a row is reopened", async () => {
    getTaskDiffIndexMock.mockResolvedValueOnce(diffIndexFixture());
    getTaskFileDiffMock.mockResolvedValueOnce({
      diff: "@@ -1 +1 @@\n-old\n+loaded",
    });

    render(() => <DiffDetail taskId="task-1" taskPath="/task/task-1" />);
    const row = await screen.findByRole("button", { name: "committed.go" });
    fireEvent.click(row);
    expect(await screen.findByText("+loaded")).toBeInTheDocument();
    fireEvent.click(row);
    fireEvent.click(row);

    expect(screen.getByText("+loaded")).toBeInTheDocument();
    expect(getTaskFileDiffMock).toHaveBeenCalledTimes(1);
  });

  it("refreshes stale metadata while retaining an expanded loaded row", async () => {
    const refreshed = diffIndexFixture();
    refreshed.repositories[0].behind = 2;
    let resolveRefresh: (response: TaskDiffIndexResp) => void = () => undefined;
    getTaskDiffIndexMock
      .mockResolvedValueOnce(diffIndexFixture())
      .mockReturnValueOnce(
        new Promise((resolve) => {
          resolveRefresh = resolve;
        }),
      );
    getTaskFileDiffMock.mockResolvedValueOnce({
      diff: "@@ -1 +1 @@\n-old\n+loaded",
    });

    render(() => <DiffDetail taskId="task-1" taskPath="/task/task-1" />);
    const row = await screen.findByRole("button", { name: "committed.go" });
    fireEvent.click(row);
    expect(await screen.findByText("+loaded")).toBeInTheDocument();
    row.focus();

    taskDiffCache.invalidate("task-1");
    expect(screen.getByText("Updating diff...")).toBeInTheDocument();
    expect(screen.getByText("+loaded")).toBeInTheDocument();
    expect(row).toHaveAttribute("aria-expanded", "true");
    resolveRefresh(refreshed);

    expect(
      await screen.findByText(/1 commit ahead · 2 behind/),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "committed.go" }),
    ).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByText("+loaded")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "committed.go" })).toHaveFocus();
    expect(getTaskFileDiffMock).toHaveBeenCalledTimes(1);
  });

  it("keeps stale metadata without claiming a failed refresh is active", async () => {
    getTaskDiffIndexMock
      .mockResolvedValueOnce(diffIndexFixture())
      .mockRejectedValueOnce(new Error("refresh failed"));
    render(() => <DiffDetail taskId="task-1" taskPath="/task/task-1" />);
    expect(await screen.findByText("Commits ahead (1)")).toBeInTheDocument();

    taskDiffCache.invalidate("task-1");
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Diff may be out of date: refresh failed",
    );
    expect(screen.queryByText("Updating diff...")).not.toBeInTheDocument();
    expect(screen.getByText("Commit subject")).toBeInTheDocument();
  });

  it("clears repositories when a subscribed task cache is evicted", async () => {
    getTaskDiffIndexMock.mockResolvedValueOnce(diffIndexFixture());
    render(() => <DiffDetail taskId="task-1" taskPath="/task/task-1" />);
    expect(await screen.findByText("Commit subject")).toBeInTheDocument();

    taskDiffCache.evictTask("task-1");
    expect(await screen.findByRole("status")).toHaveTextContent(
      "Diff unavailable",
    );
    expect(screen.queryByText("Commit subject")).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "committed.go" }),
    ).not.toBeInTheDocument();
  });

  it("keeps an expanded working patch visible while its replacement loads", async () => {
    let resolveIndex: (response: TaskDiffIndexResp) => void = () => undefined;
    let resolvePatch: (response: FileDiffResp) => void = () => undefined;
    getTaskDiffIndexMock
      .mockResolvedValueOnce(diffIndexFixture())
      .mockReturnValueOnce(
        new Promise((resolve) => {
          resolveIndex = resolve;
        }),
      );
    getTaskFileDiffMock
      .mockResolvedValueOnce({ diff: "@@ -1 +1 @@\n-old\n+first" })
      .mockReturnValueOnce(
        new Promise((resolve) => {
          resolvePatch = resolve;
        }),
      );
    render(() => <DiffDetail taskId="task-1" taskPath="/task/task-1" />);
    const row = await screen.findByRole("button", {
      name: "old.go → working.go",
    });
    fireEvent.click(row);
    expect(await screen.findByText("+first")).toBeInTheDocument();

    taskDiffCache.invalidate("task-1");
    expect(screen.getByText("+first")).toBeInTheDocument();
    resolveIndex(diffIndexFixture());
    expect(
      await screen.findByText("Updating file diff..."),
    ).toBeInTheDocument();
    expect(screen.getByText("+first")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "old.go → working.go" })).toBe(
      row,
    );
    resolvePatch({ diff: "@@ -1 +1 @@\n-old\n+second" });
    expect(await screen.findByText("+second")).toBeInTheDocument();
    expect(screen.queryByText("+first")).not.toBeInTheDocument();
  });

  it("refreshes a working patch when metadata changes during its first load", async () => {
    let resolveFirstPatch: (response: FileDiffResp) => void = () => undefined;
    getTaskDiffIndexMock.mockResolvedValue(diffIndexFixture());
    getTaskFileDiffMock
      .mockReturnValueOnce(
        new Promise((resolve) => {
          resolveFirstPatch = resolve;
        }),
      )
      .mockResolvedValueOnce({ diff: "@@ -1 +1 @@\n-old\n+current" });
    render(() => <DiffDetail taskId="task-1" taskPath="/task/task-1" />);
    fireEvent.click(
      await screen.findByRole("button", { name: "old.go → working.go" }),
    );
    expect(getTaskFileDiffMock).toHaveBeenCalledTimes(1);

    taskDiffCache.invalidate("task-1");
    await vi.waitFor(() =>
      expect(getTaskDiffIndexMock).toHaveBeenCalledTimes(2),
    );
    expect(getTaskFileDiffMock).toHaveBeenCalledTimes(1);
    resolveFirstPatch({ diff: "@@ -1 +1 @@\n-old\n+obsolete" });

    expect(await screen.findByText("+current")).toBeInTheDocument();
    expect(screen.queryByText("+obsolete")).not.toBeInTheDocument();
    expect(getTaskFileDiffMock).toHaveBeenCalledTimes(2);
  });

  it("defers a queued working patch refresh while its row is collapsed", async () => {
    let resolveFirstPatch: (response: FileDiffResp) => void = () => undefined;
    const firstPatch = new Promise<FileDiffResp>((resolve) => {
      resolveFirstPatch = resolve;
    });
    getTaskDiffIndexMock.mockResolvedValue(diffIndexFixture());
    getTaskFileDiffMock
      .mockReturnValueOnce(firstPatch)
      .mockResolvedValueOnce({ diff: "@@ -1 +1 @@\n-old\n+current" });
    render(() => <DiffDetail taskId="task-1" taskPath="/task/task-1" />);
    const row = await screen.findByRole("button", {
      name: "old.go → working.go",
    });
    fireEvent.click(row);
    expect(getTaskFileDiffMock).toHaveBeenCalledTimes(1);

    taskDiffCache.invalidate("task-1");
    await vi.waitFor(() =>
      expect(getTaskDiffIndexMock).toHaveBeenCalledTimes(2),
    );
    fireEvent.click(row);
    expect(row).toHaveAttribute("aria-expanded", "false");
    resolveFirstPatch({ diff: "@@ -1 +1 @@\n-old\n+obsolete" });
    await firstPatch;
    await Promise.resolve();
    expect(getTaskFileDiffMock).toHaveBeenCalledTimes(1);

    fireEvent.click(row);
    expect(await screen.findByText("+current")).toBeInTheDocument();
    expect(getTaskFileDiffMock).toHaveBeenCalledTimes(2);
  });

  it("routes a file-patch 404 through task refresh handling", async () => {
    const err = Object.assign(new Error("task not found"), { status: 404 });
    const onTaskRefreshError = vi.fn(() => true);
    getTaskDiffIndexMock.mockResolvedValueOnce(diffIndexFixture());
    getTaskFileDiffMock.mockRejectedValueOnce(err);

    render(() => (
      <DiffDetail
        taskId="task-1"
        taskPath="/task/task-1"
        onTaskRefreshError={onTaskRefreshError}
      />
    ));
    fireEvent.click(
      await screen.findByRole("button", { name: "committed.go" }),
    );

    await vi.waitFor(() => {
      expect(onTaskRefreshError).toHaveBeenCalledWith("task-1", err);
      expect(screen.queryByRole("status")).not.toBeInTheDocument();
    });
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("separates upstream commits from uncommitted files", async () => {
    getTaskDiffIndexMock.mockResolvedValueOnce({
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
            },
            {
              path: "frontend/new.tsx",
              indexStatus: "A",
              added: 1,
              deleted: 0,
              binary: false,
            },
            {
              path: "frontend/untracked.tsx",
              indexStatus: "?",
              worktreeStatus: "?",
              added: 1,
              deleted: 0,
              binary: false,
            },
          ],
        },
      ],
    });
    getTaskFileDiffMock.mockImplementation(
      async (_id, _repository, _commit, path) => ({
        diff:
          path === "frontend/view.tsx"
            ? "@@ -1 +1 @@\n-old view\n+new view"
            : "@@ -1 +1,2 @@\n-old working\n+new working\n+line",
      }),
    );

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
    expect(await screen.findByText("+new view")).toBeInTheDocument();

    const workingFile = screen.getByRole("button", {
      name: /frontend\/working\.tsx/,
    });
    expect(workingFile).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(workingFile);
    expect(await screen.findByText("+new working")).toBeInTheDocument();
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
    getTaskDiffIndexMock.mockResolvedValueOnce({
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
    expect(renamedFile.querySelectorAll(`.${styles.pathValue}`)).toHaveLength(
      2,
    );
    expect(renamedFile).toHaveTextContent(
      `${originalPath} → ${uncommittedPath}`,
    );
  });
});

function diffIndexFixture(): TaskDiffIndexResp {
  return {
    repositories: [
      {
        name: "caic",
        branch: "feature",
        upstream: "origin/main",
        ahead: 1,
        behind: 0,
        commits: [
          {
            sha: "1234567890abcdef1234567890abcdef12345678",
            authoredDate: "2026-09-15",
            subject: "Commit subject",
            stat: [
              {
                path: "committed.go",
                added: 1,
                deleted: 1,
                binary: false,
              },
            ],
          },
        ],
        uncommitted: [
          {
            path: "working.go",
            originalPath: "old.go",
            worktreeStatus: "R",
            added: 1,
            deleted: 1,
            binary: false,
          },
        ],
      },
    ],
  };
}

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
      elidePathAtBoundary("backend/internal/types.go", 40, measureCharacters),
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

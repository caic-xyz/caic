// Tests for task URL path helpers.

import { describe, expect, it } from "vitest";

import { taskIdFromPath, taskPath, taskPathForTask } from "./taskPath";

describe("taskPath", () => {
  it("builds a task URL with ID and slug", () => {
    expect(taskPath("abc123", "owner/repo", "caic-1", "Fix bug")).toBe("/task/@abc123+repo-caic-1-fix-bug");
  });

  it("builds a task URL from task data", () => {
    expect(taskPathForTask({
      id: "abc123",
      repos: [{ name: "owner/repo", branch: "caic-1" }],
      title: "Fix bug",
    })).toBe("/task/@abc123+repo-caic-1-fix-bug");
  });
});

describe("taskIdFromPath", () => {
  it("extracts IDs from task subroutes", () => {
    expect(taskIdFromPath("/task/@abc123+repo-caic-1-fix-bug")).toBe("abc123");
    expect(taskIdFromPath("/task/@abc123+repo-caic-1-fix-bug/diff")).toBe("abc123");
    expect(taskIdFromPath("/task/@abc123+repo-caic-1-fix-bug/processes")).toBe("abc123");
    expect(taskIdFromPath("/task/@abc123+repo-caic-1-fix-bug/vnc")).toBe("abc123");
    expect(taskIdFromPath("/task/@abc123+repo-caic-1-fix-bug/info")).toBe("abc123");
  });

  it("extracts IDs from slugless task subroutes", () => {
    expect(taskIdFromPath("/task/@abc123/info")).toBe("abc123");
  });
});

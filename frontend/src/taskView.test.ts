// Tests task subview presentation URLs and rejects fullscreen flags outside supported workspace routes.
import { describe, it } from "node:test";
import { expect } from "@tests/expect";
import { taskViewMode } from "./taskView";

describe("taskViewMode", () => {
  it("requires a document route and a nonempty path", () => {
    expect(taskViewMode("/task/@abc", "?path=a.md&fullscreen=1")).toBe("closed");
    expect(taskViewMode("/settings", "?path=a.md&fullscreen=1")).toBe("closed");
    expect(taskViewMode("/task/@abc/view", "?fullscreen=1")).toBe("closed");
    expect(taskViewMode("/task/@abc/view", "?path=")).toBe("closed");
    expect(taskViewMode("/task/@abc/view", "?attachment=")).toBe("closed");
  });
  it("defaults to docked and accepts only the explicit fullscreen flag", () => {
    expect(taskViewMode("/task/@abc/view", "?path=notes.md")).toBe("docked");
    expect(taskViewMode("/task/@abc/view", "?attachment=" + "a".repeat(64))).toBe("docked");
    expect(taskViewMode("/task/@abc/view", "?path=notes.md&fullscreen=true")).toBe("docked");
    expect(taskViewMode("/task/@abc+slug/view", "?path=%2Fnotes.md&fullscreen=1")).toBe("fullscreen");
  });
  it("supports task tools without requiring a file path", () => {
    for (const path of ["diff", "info", "processes", "stats", "vnc"]) {
      expect(taskViewMode(`/task/@abc/${path}`, "")).toBe("docked");
      expect(taskViewMode(`/task/@abc+slug/${path}`, "?fullscreen=1")).toBe("fullscreen");
    }
    expect(taskViewMode("/task/@abc/unknown", "?fullscreen=1")).toBe("closed");
    expect(taskViewMode("/task/@abc/stats/nested", "?fullscreen=1")).toBe("closed");
  });
});

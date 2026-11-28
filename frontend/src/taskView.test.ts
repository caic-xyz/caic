// Tests supported fullscreen URLs and which Escape events can dismiss a task view.
import { describe, it } from "node:test";
import { expect } from "@tests/expect";
import { isTaskViewEscape, taskDetailFullscreen, taskFullscreen, taskViewMode } from "./taskView";

describe("task view Escape", () => {
  it("ignores held keys and Escape that ends text composition", () => {
    expect(isTaskViewEscape(new KeyboardEvent("keydown", { key: "Escape" }))).toBe(true);
    expect(isTaskViewEscape(new KeyboardEvent("keydown", { key: "Escape", repeat: true }))).toBe(false);
    expect(isTaskViewEscape(new KeyboardEvent("keydown", { key: "Escape", isComposing: true }))).toBe(false);
  });
});

describe("task fullscreen", () => {
  it("accepts the explicit flag on conversation routes", () => {
    expect(taskFullscreen("/task/@abc", "")).toBe(false);
    expect(taskFullscreen("/task/@abc", "?fullscreen=true")).toBe(false);
    expect(taskDetailFullscreen("/task/@abc+slug", "?fullscreen=1")).toBe(true);
    expect(taskFullscreen("/task/@abc/", "?fullscreen=1")).toBe(true);
  });

  it("leaves subview validation in effect", () => {
    expect(taskDetailFullscreen("/task/@abc/diff", "?fullscreen=1")).toBe(false);
    expect(taskFullscreen("/task/@abc/diff", "?fullscreen=1")).toBe(true);
    expect(taskFullscreen("/task/@abc/view", "?fullscreen=1")).toBe(false);
    expect(taskFullscreen("/settings", "?fullscreen=1")).toBe(false);
  });
});

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
    expect(taskViewMode("/task/@abc/view", "?image=%2Fapi%2Fimage")).toBe("docked");
    expect(taskViewMode("/task/@abc/view", "?attachment=2%2F0%2F0")).toBe("docked");
    expect(taskViewMode("/task/@abc/view", "?path=notes.md&fullscreen=true")).toBe("docked");
    expect(taskViewMode("/task/@abc+slug/view", "?path=%2Fnotes.md&fullscreen=1")).toBe("fullscreen");
  });
  it("supports task tools without requiring a file path", () => {
    for (const path of ["diff", "info", "perf", "perf/turns/3", "perf/contexts/2", "processes", "vnc"]) {
      expect(taskViewMode(`/task/@abc/${path}`, "")).toBe("docked");
      expect(taskViewMode(`/task/@abc+slug/${path}`, "?fullscreen=1")).toBe("fullscreen");
    }
    expect(taskViewMode("/task/@abc/unknown", "?fullscreen=1")).toBe("closed");
    expect(taskViewMode("/task/@abc/perf/nested", "?fullscreen=1")).toBe("closed");
    expect(taskViewMode("/task/@abc/perf/turns/3/nested", "?fullscreen=1")).toBe("closed");
    expect(taskViewMode("/task/@abc/perf/contexts/2/nested", "?fullscreen=1")).toBe("closed");
  });
});

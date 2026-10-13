// Tests document presentation URLs and rejects fullscreen flags outside an open file view.
import { describe, it } from "node:test";
import { expect } from "@tests/expect";
import { fileViewMode } from "./fileView";

describe("fileViewMode", () => {
  it("requires a document route and a nonempty path", () => {
    expect(fileViewMode("/task/@abc", "?path=a.md&fullscreen=1")).toBe("closed");
    expect(fileViewMode("/settings", "?path=a.md&fullscreen=1")).toBe("closed");
    expect(fileViewMode("/task/@abc/view", "?fullscreen=1")).toBe("closed");
    expect(fileViewMode("/task/@abc/view", "?path=")).toBe("closed");
  });
  it("defaults to docked and accepts only the explicit fullscreen flag", () => {
    expect(fileViewMode("/task/@abc/view", "?path=notes.md")).toBe("docked");
    expect(fileViewMode("/task/@abc/view", "?path=notes.md&fullscreen=true")).toBe("docked");
    expect(fileViewMode("/task/@abc+slug/view", "?path=%2Fnotes.md&fullscreen=1")).toBe("fullscreen");
  });
});

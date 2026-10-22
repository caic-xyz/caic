// Tests context-scoped skill identity, ambiguous enrichment, incremental updates, and replay resets.
import { describe, it } from "node:test";
import { expect } from "@tests/expect";
import type { EventMessage } from "@sdk/types.gen";
import { ContextSkillsTracker } from "./contextSkills";

const load = (skill: string, path?: string): EventMessage => ({ kind: "skillLoad", ts: 1, skillLoad: { skill, path } });

describe("ContextSkillsTracker", () => {
  it("ignores child loads and child compaction in the root context", () => {
    const source = { delegationToolUseID: "spawn" };
    const result = new ContextSkillsTracker().derive("task", 0, [
      load("root"),
      { ...load("child"), source },
      { kind: "system", ts: 2, source, system: { subtype: "compact_boundary" } },
    ]);
    expect(result.current.map((skill) => skill.skill)).toEqual(["root"]);
    expect(result.closed.size).toBe(0);
  });
  it("enriches one known path and preserves ambiguity when another path appears", () => {
    const tracker = new ContextSkillsTracker();
    const history = [load("review"), load("review", "/a/SKILL.md")];
    const first = tracker.derive("a", 0, history);
    expect(first.current).toEqual([{ skill: "review", path: "/a/SKILL.md" }]);
    expect(tracker.derive("a", 0, [...history, load("review", "/a/SKILL.md")])).toBe(first);
    history.push(load("review", "/a/SKILL.md"), load("review", "/b/SKILL.md"));
    expect(tracker.derive("a", 0, history).current).toEqual([
      { skill: "review", path: undefined },
      { skill: "review", path: "/a/SKILL.md" },
      { skill: "review", path: "/b/SKILL.md" },
    ]);
  });

  it("keeps a late pathless name unresolved when two paths are known and deduplicates paths across names", () => {
    const tracker = new ContextSkillsTracker();
    const skills = tracker.derive("a", 0, [
      load("review", "/a/SKILL.md"),
      load("review", "/b/SKILL.md"),
      load("review"),
      load("alias", "/a/SKILL.md"),
    ]).current;
    expect(skills).toEqual([
      { skill: "review", path: "/a/SKILL.md" },
      { skill: "review", path: "/b/SKILL.md" },
      { skill: "review", path: undefined },
    ]);
  });

  it("enriches a late name-only load when one path is known", () => {
    const tracker = new ContextSkillsTracker();
    expect(tracker.derive("a", 0, [load("review", "/a/SKILL.md"), load("review")]).current).toEqual([
      { skill: "review", path: "/a/SKILL.md" },
    ]);
  });

  for (const subtype of ["compact_boundary", "context_cleared"]) {
    it(`closes skills at ${subtype} and starts an empty section`, () => {
      const tracker = new ContextSkillsTracker();
      const boundary: EventMessage = { kind: "system", ts: 2, system: { subtype } };
      const history = [load("review", "/a/SKILL.md"), boundary];
      const closed = tracker.derive("a", 0, history);
      expect(closed.current).toEqual([]);
      expect(closed.closed.get(boundary)).toEqual([{ skill: "review", path: "/a/SKILL.md" }]);
      history.push(load("review", "/b/SKILL.md"));
      const next = tracker.derive("a", 0, history);
      expect(next.current).toEqual([{ skill: "review", path: "/b/SKILL.md" }]);
      expect(next.closed.get(boundary)).toBe(closed.closed.get(boundary));
    });
  }

  it("resets on task switch, replay epoch, and shortened history", () => {
    const tracker = new ContextSkillsTracker();
    tracker.derive("a", 0, [load("review")]);
    expect(tracker.derive("b", 0, [load("quality")]).current).toEqual([{ skill: "quality", path: undefined }]);
    expect(tracker.derive("b", 1, [load("design")]).current).toEqual([{ skill: "design", path: undefined }]);
    expect(tracker.derive("b", 1, []).current).toEqual([]);
  });
});

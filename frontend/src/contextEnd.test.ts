// Tests how a context end is classified and worded: compaction with and without sizes, a clear, a new session, and the current context.

import assert from "node:assert/strict";
import { describe, it } from "node:test";

import type { EventMessage, HistorySection } from "@sdk/types.gen";
import { contextEndCause, contextEndDetail } from "./contextEnd";

function ended(message: EventMessage | null): HistorySection {
  return {
    id: "1",
    endBoundary: message ? { recordID: 1, index: 0, message } : undefined,
    turnCount: 1,
    textCount: 0,
    toolCount: 0,
    durationMs: 0,
    skills: [],
  };
}

describe("contextEnd", () => {
  it("names compaction, with the sizes the harness reported", () => {
    const sized = ended({
      kind: "system",
      ts: 1,
      system: { subtype: "compact_boundary", contextTokensBefore: 150_000, contextTokensAfter: 20_000 },
    });
    assert.equal(contextEndCause(sized), "compaction");
    assert.equal(contextEndDetail(sized), "Compacted session · 150,000 → 20,000 tokens");
    const before = ended({
      kind: "system",
      ts: 1,
      system: { subtype: "compact_boundary", contextTokensBefore: 150_000 },
    });
    assert.equal(contextEndDetail(before), "Compacted session · from 150,000 tokens");
    const unsized = ended({ kind: "system", ts: 1, system: { subtype: "compact_boundary" } });
    assert.equal(contextEndDetail(unsized), "Compacted session");
  });

  it("names a clear and a new session", () => {
    const cleared = ended({ kind: "system", ts: 1, system: { subtype: "context_cleared" } });
    assert.equal(contextEndCause(cleared), "clear");
    assert.equal(contextEndDetail(cleared), "Cleared session");
    const session = ended({
      kind: "init",
      ts: 1,
      init: { reportedModel: "m", agentVersion: "v", sessionID: "next", tools: [], cwd: "", harness: "h" },
    });
    assert.equal(contextEndCause(session), "session");
    assert.equal(contextEndDetail(session), "Previous session");
  });

  it("leaves the current context without an end", () => {
    assert.equal(contextEndCause(ended(null)), null);
    assert.equal(contextEndDetail(ended(null)), "Current context");
  });
});

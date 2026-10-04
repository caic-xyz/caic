// Guard for the TaskListEvent SDK validator: proves structured warnings and settled-history status.
// variant survives validateTaskListEvent for a wire-format SSE payload. This is
// the exact path globalTaskEvents runs on every event, which the component tests
// bypass by mocking the api module. It fails if the status fields regress into
// the generator's discriminator switch (for example by re-adding omitempty to a
// non-kind field on the union), which is what made the feature inert on the real
// client.

import { describe, it } from "node:test";
import { expect } from "@tests/expect";
import { validateTaskListEvent } from "@sdk/validate.gen";

// Parse a raw wire payload the same way globalTaskEvents does before validation.
const wire = (json: string): unknown => JSON.parse(json);

describe("validateTaskListEvent settled status", () => {
  it("preserves loading=true on a kind=status event", () => {
    const ev = validateTaskListEvent(wire('{"kind":"status","status":{"loading":true,"error":""}}'));
    expect(ev.kind).toBe("status");
    expect(ev.status).toEqual({ loading: true, error: "" });
  });

  it("preserves the error on a failed kind=status event", () => {
    const ev = validateTaskListEvent(
      wire('{"kind":"status","status":{"loading":false,"error":"load purged tasks: boom"}}'),
    );
    expect(ev.kind).toBe("status");
    expect(ev.status?.loading).toBe(false);
    expect(ev.status?.error).toBe("load purged tasks: boom");
  });

  it("leaves status absent on a kind=snapshot event", () => {
    const ev = validateTaskListEvent(wire('{"kind":"snapshot","snapshot":[]}'));
    expect(ev.kind).toBe("snapshot");
    expect(ev.snapshot).toEqual([]);
    expect(ev.status).toBeUndefined();
  });
});

describe("validateTaskListEvent structured warnings", () => {
  it("preserves public runtime restoration warnings", () => {
    const ev = validateTaskListEvent(
      wire(
        '{"kind":"warning","warning":{"id":"restore-1","category":"runtime_restore_failed","message":"Some existing tasks could not be restored.","details":[]}}',
      ),
    );
    expect(ev.warning?.category).toBe("runtime_restore_failed");
    expect(ev.warning?.details).toEqual([]);
  });
  it("preserves identity, category, translated text, and repository diagnostics", () => {
    const ev = validateTaskListEvent(
      wire(
        '{"kind":"warning","warning":{"id":"episode-1","category":"ci_poll_failed","message":"Échec CI","details":[{"repo":"a","error":"timeout"}]}}',
      ),
    );
    expect(ev.warning).toEqual({
      id: "episode-1",
      category: "ci_poll_failed",
      message: "Échec CI",
      details: [{ repo: "a", error: "timeout" }],
    });
  });

  it("rejects warnings with malformed identity or details", () => {
    for (const warning of [
      { id: 42, category: "ci_poll_failed", message: "failure", details: [] },
      { id: "episode-1", category: "ci_poll_failed", message: "failure", details: [{ repo: "a", error: 42 }] },
    ]) {
      expect(() => validateTaskListEvent({ kind: "warning", warning })).toThrow();
    }
  });
});

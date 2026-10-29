// Tests session-bound CSRF requests and CSRF protection.

import { it } from "node:test";
import { expect, vi } from "@tests/expect";

import { api } from "./api";

it("keeps concurrent logout requests bound to their originating sessions with CSRF protection", async () => {
  const requests: Headers[] = [];
  const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (_input, init) => {
    requests.push(new Headers(init?.headers));
    return Response.json({ status: "ok" });
  });
  try {
    await Promise.all([api.logout("session-a"), api.logout("session-b")]);
    expect(requests.map((headers) => headers.get("X-Caic-Session"))).toEqual(["session-a", "session-b"]);
    expect(requests.map((headers) => headers.get("X-Caic-CSRF"))).toEqual(["1", "1"]);
  } finally {
    fetchMock.mockRestore();
  }
});

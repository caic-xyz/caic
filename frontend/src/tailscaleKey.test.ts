// Tests token loading, non-admin hiding, verified saves, superseded responses, and expiry wording.
import { afterEach, describe, it } from "node:test";
import { createRoot } from "solid-js";
import type { ISOTimestamp, TailscaleKeyResp } from "@sdk/types.gen";
import { expect } from "@tests/expect";
import { availableUntilMs, createTailscaleKey, formatRemaining } from "./tailscaleKey";

const original = globalThis.fetch;
afterEach(() => {
  globalThis.fetch = original;
});

type Handler = (method: string, url: string, body: unknown) => Promise<Response>;

function stubFetch(handler: Handler) {
  globalThis.fetch = ((input: RequestInfo | URL, init?: RequestInit) =>
    handler(
      init?.method ?? "GET",
      String(input),
      init?.body ? JSON.parse(String(init.body)) : undefined,
    )) as typeof fetch;
}

const json = (body: unknown, status = 200) =>
  Promise.resolve(new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } }));

const failure = (status: number, code: string, message: string) => json({ error: { code, message } }, status);

function setup() {
  const changes: TailscaleKeyResp[] = [];
  const key = createRoot(() => createTailscaleKey((s) => changes.push(s)));
  return { key, changes };
}

describe("createTailscaleKey", () => {
  it("loads the token for an administrator", async () => {
    const status: TailscaleKeyResp = {
      source: "config",
      keyID: "kCfg",
      expiresAt: "2099-01-01T00:00:00Z" as ISOTimestamp,
    };
    stubFetch(() => json(status));
    const { key, changes } = setup();
    await key.load();
    expect(key.status()).toEqual(status);
    expect(changes).toEqual([status]);
  });

  it("stays hidden from non-administrators without an error", async () => {
    stubFetch(() => failure(403, "FORBIDDEN", "administrator authority required"));
    const { key, changes } = setup();
    await key.load();
    expect(key.status()).toBeNull();
    expect(key.loadError()).toBe("");
    expect(changes).toEqual([]);
  });

  it("reports other load failures", async () => {
    stubFetch(() => failure(500, "INTERNAL_ERROR", "boom"));
    const { key } = setup();
    await key.load();
    expect(key.loadError()).toBe("boom");
  });

  it("saves a trimmed token and reports rejection", async () => {
    const saved: TailscaleKeyResp = { source: "saved", keyID: "kNew" };
    const bodies: unknown[] = [];
    stubFetch((method, _url, body) => {
      bodies.push(body);
      return method === "POST" && (body as { apiKey: string }).apiKey === "tskey-api-kNew-s"
        ? json(saved)
        : failure(400, "BAD_REQUEST", "unusable tailscale API access token: rejected");
    });
    const { key, changes } = setup();
    expect(await key.save("  tskey-api-kNew-s\n")).toBe(true);
    expect(bodies[0]).toEqual({ apiKey: "tskey-api-kNew-s" });
    expect(key.status()).toEqual(saved);
    expect(changes).toEqual([saved]);
    expect(key.pending()).toBeNull();

    expect(await key.save("tskey-api-kBad-s")).toBe(false);
    expect(key.actionError()).toBe("unusable tailscale API access token: rejected");
    expect(key.status()).toEqual(saved);
    key.clearActionError();
    expect(key.actionError()).toBe("");
  });

  it("drops a load answered after a newer save", async () => {
    let releaseLoad = (_: Response) => {};
    const saved: TailscaleKeyResp = { source: "saved", keyID: "kNew" };
    stubFetch((method) =>
      method === "GET"
        ? new Promise<Response>((resolve) => {
            releaseLoad = resolve;
          })
        : json(saved),
    );
    const { key, changes } = setup();
    const loading = key.load();
    await key.save("tskey-api-kNew-s");
    releaseLoad(new Response(JSON.stringify({ source: "none" }), { status: 200 }));
    await loading;
    expect(key.status()).toEqual(saved);
    expect(changes).toEqual([saved]);
  });
});

describe("availableUntilMs", () => {
  it("maps availability to a deadline", () => {
    expect(availableUntilMs(false, "2099-01-01T00:00:00Z")).toBeNull();
    expect(availableUntilMs(true, undefined)).toBe(Number.POSITIVE_INFINITY);
    expect(availableUntilMs(true, "2099-01-01T00:00:00Z")).toBe(Date.parse("2099-01-01T00:00:00Z"));
  });
});

describe("formatRemaining", () => {
  const now = Date.parse("2026-01-01T00:00:00Z");
  const hour = 3_600_000;
  it("uses the largest unit with at least two of it", () => {
    for (const [delta, want] of [
      [23 * 24 * hour, "in 23 days"],
      [36 * hour, "in 36 hours"],
      [90 * 60_000, "in 90 minutes"],
      [-3 * 24 * hour, "3 days ago"],
    ] as const) {
      expect(formatRemaining(now + delta, now)).toBe(want);
    }
  });
});

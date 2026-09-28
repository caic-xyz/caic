// Tests service-worker caching of immutable assets and restart fallback for icons.

import { describe, it } from "node:test";
import vm from "node:vm";
import { expect, vi } from "@tests/expect";

import workerSource from "../public/sw.js?raw";

type FetchHandler = (event: { request: Request; respondWith: (response: Promise<Response>) => void }) => void;

type CacheMock = {
  match: ReturnType<typeof vi.fn>;
  put: ReturnType<typeof vi.fn>;
};

function loadFetchHandler(
  cache: CacheMock,
  fetchMock: typeof fetch,
  workerURL = "https://quick.caic.xyz/sw.js?build=%2Fassets%2Findex-new.js",
) {
  const listeners = new Map<string, unknown>();
  const open = vi.fn(async () => cache);
  const remove = vi.fn(async () => true);
  const keys = vi.fn(async () => ["caic-assets-old", "caic-icons-v1"]);
  vm.runInNewContext(workerSource, {
    URL,
    caches: {
      delete: remove,
      keys,
      open,
    },
    console: { warn: vi.fn() },
    fetch: fetchMock,
    self: {
      addEventListener: (type: string, listener: unknown) => listeners.set(type, listener),
      location: { href: workerURL, origin: "https://quick.caic.xyz" },
      skipWaiting: vi.fn(),
    },
  });
  const handler = listeners.get("fetch");
  if (typeof handler !== "function") throw new Error("service worker did not register a fetch handler");
  return { handler: handler as FetchHandler, listeners, open, remove };
}

function fetchEvent(request: Request) {
  let response: Promise<Response> | undefined;
  return {
    event: {
      request,
      respondWith: (next: Promise<Response>) => {
        response = next;
      },
    },
    response: () => response,
  };
}

describe("service worker", () => {
  it("does not intercept personalized SPA documents", () => {
    const cache: CacheMock = { match: vi.fn(), put: vi.fn() };
    const fetchMock = vi.fn<typeof fetch>();
    const { handler } = loadFetchHandler(cache, fetchMock);
    const request = fetchEvent(new Request("https://quick.caic.xyz/task/@task-123"));

    handler(request.event);

    expect(request.response()).toBeUndefined();
    expect(fetchMock).not.toHaveBeenCalled();
    expect(cache.match).not.toHaveBeenCalled();
  });

  it("caches successful hashed assets", async () => {
    const cache: CacheMock = {
      match: vi.fn(async () => undefined),
      put: vi.fn(async () => undefined),
    };
    const fetchMock = vi.fn<typeof fetch>(async () => new Response("asset", { status: 200 }));
    const { handler, open } = loadFetchHandler(cache, fetchMock);
    const request = fetchEvent(new Request("https://quick.caic.xyz/assets/index-abc123.js"));

    handler(request.event);

    await expect(request.response()).resolves.toHaveProperty("status", 200);
    expect(fetchMock).toHaveBeenCalledWith(request.event.request);
    expect(cache.put).toHaveBeenCalledOnce();
    expect(open).toHaveBeenCalledWith("caic-assets-/assets/index-new.js");
  });

  it("serves a cached provider logo while the server is restarting", async () => {
    const cached = new Response("cached logo", { status: 200 });
    const cache: CacheMock = { match: vi.fn(async () => cached), put: vi.fn() };
    const fetchMock = vi.fn<typeof fetch>(async () => new Response("unavailable", { status: 503 }));
    const { handler, open } = loadFetchHandler(cache, fetchMock);
    const request = fetchEvent(new Request("https://quick.caic.xyz/logos/anthropic.svg"));

    handler(request.event);

    await expect(request.response()).resolves.toBe(cached);
    expect(fetchMock).toHaveBeenCalledWith(request.event.request, { cache: "no-cache" });
    expect(cache.match).toHaveBeenCalledWith(request.event.request);
    expect(cache.put).not.toHaveBeenCalled();
    expect(open).toHaveBeenCalledWith("caic-icons-v1");
  });

  it("refreshes a cached root icon after recovery", async () => {
    const cache: CacheMock = { match: vi.fn(), put: vi.fn(async () => undefined) };
    const fresh = new Response("fresh icon", { status: 200 });
    const fetchMock = vi.fn<typeof fetch>(async () => fresh);
    const { handler } = loadFetchHandler(cache, fetchMock);
    const request = fetchEvent(new Request("https://quick.caic.xyz/favicon.svg"));

    handler(request.event);

    await expect(request.response()).resolves.toBe(fresh);
    expect(cache.put).toHaveBeenCalledOnce();
    expect(cache.put).toHaveBeenCalledWith(request.event.request, expect.any(Response));
    expect(cache.match).not.toHaveBeenCalled();
  });

  it("falls back to a cached PWA icon when the network request fails", async () => {
    const cached = new Response("cached icon", { status: 200 });
    const cache: CacheMock = { match: vi.fn(async () => cached), put: vi.fn() };
    const fetchMock = vi.fn<typeof fetch>(async () => {
      throw new TypeError("network failed");
    });
    const { handler } = loadFetchHandler(cache, fetchMock);
    const request = fetchEvent(new Request("https://quick.caic.xyz/icon-192.png"));

    handler(request.event);

    await expect(request.response()).resolves.toBe(cached);
  });

  it("does not use a stale logo after a 404", async () => {
    const cache: CacheMock = { match: vi.fn(async () => new Response("old logo")), put: vi.fn() };
    const missing = new Response("missing", { status: 404 });
    const { handler } = loadFetchHandler(
      cache,
      vi.fn<typeof fetch>(async () => missing),
    );
    const request = fetchEvent(new Request("https://quick.caic.xyz/logos/removed.svg"));

    handler(request.event);

    await expect(request.response()).resolves.toBe(missing);
    expect(cache.match).not.toHaveBeenCalled();
  });

  it("retains the icon cache when the app build changes", async () => {
    const cache: CacheMock = { match: vi.fn(), put: vi.fn() };
    const { listeners, remove } = loadFetchHandler(cache, vi.fn<typeof fetch>());
    const activate = listeners.get("activate") as (event: { waitUntil: (promise: Promise<unknown>) => void }) => void;
    let pending: Promise<unknown> | undefined;

    activate({ waitUntil: (promise) => (pending = promise) });
    await pending;

    expect(remove).toHaveBeenCalledOnce();
    expect(remove).toHaveBeenCalledWith("caic-assets-old");
  });
});

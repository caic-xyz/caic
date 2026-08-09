// Tests that the service worker caches only immutable frontend assets.

import vm from "node:vm";
import { describe, expect, it, vi } from "vitest";

import workerSource from "../public/sw.js?raw";

type FetchHandler = (event: {
  request: Request;
  respondWith: (response: Promise<Response>) => void;
}) => void;

type CacheMock = {
  match: ReturnType<typeof vi.fn>;
  put: ReturnType<typeof vi.fn>;
};

function loadFetchHandler(cache: CacheMock, fetchMock: typeof fetch): FetchHandler {
  const listeners = new Map<string, unknown>();
  vm.runInNewContext(workerSource, {
    URL,
    caches: {
      delete: vi.fn(),
      keys: vi.fn(),
      open: vi.fn(async () => cache),
    },
    console: { warn: vi.fn() },
    fetch: fetchMock,
    self: {
      addEventListener: (type: string, listener: unknown) => listeners.set(type, listener),
      location: { origin: "https://quick.caic.xyz" },
      skipWaiting: vi.fn(),
    },
  });
  const handler = listeners.get("fetch");
  if (typeof handler !== "function") throw new Error("service worker did not register a fetch handler");
  return handler as FetchHandler;
}

function fetchEvent(request: Request) {
  let response: Promise<Response> | undefined;
  return {
    event: {
      request,
      respondWith: (next: Promise<Response>) => { response = next; },
    },
    response: () => response,
  };
}

describe("service worker", () => {
  it("does not intercept personalized SPA documents", () => {
    const cache: CacheMock = { match: vi.fn(), put: vi.fn() };
    const fetchMock = vi.fn<typeof fetch>();
    const handler = loadFetchHandler(cache, fetchMock);
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
    const handler = loadFetchHandler(cache, fetchMock);
    const request = fetchEvent(new Request("https://quick.caic.xyz/assets/index-abc123.js"));

    handler(request.event);

    await expect(request.response()).resolves.toHaveProperty("status", 200);
    expect(fetchMock).toHaveBeenCalledWith(request.event.request);
    expect(cache.put).toHaveBeenCalledOnce();
  });
});

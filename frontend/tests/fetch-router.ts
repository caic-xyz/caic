// Shared fetch seam for tests: components import the real api client, which calls
// globalThis.fetch, so tests stub the network here instead of mocking modules.
// installFetchRouter() replaces globalThis.fetch; routes are matched in registration
// order and unmatched requests get an ok empty-JSON response, matching the old
// permissive mockFetch default.
export interface FetchRoute {
  match: string | RegExp;
  method?: string;
  respond: (url: string, init?: RequestInit) => unknown;
}

export interface FetchRouter {
  routes: Array<FetchRoute>;
  /** Register a route; respond's return value is JSON-encoded with ok: true. */
  api: (method: string, match: string | RegExp, respond: (url: string, init?: RequestInit) => unknown) => void;
  apiGet: (match: string | RegExp, respond: (url: string) => unknown) => void;
  /** Register a route whose response is returned verbatim (e.g. ok: false). */
  raw: (route: { match: string | RegExp; method?: string; response: unknown }) => void;
  /** Every request seen so far, for call assertions. */
  calls: Array<{ url: string; init?: RequestInit }>;
  /** Clear routes and call history (call between tests). */
  reset: () => void;
  /** Restore the original globalThis.fetch. */
  restore: () => void;
}

export function installFetchRouter(): FetchRouter {
  const original = globalThis.fetch;
  const router = {
    routes: [] as Array<FetchRoute>,
    calls: [] as Array<{ url: string; init?: RequestInit }>,
    reset(): void {
      router.routes.length = 0;
      router.calls.length = 0;
    },
    restore(): void {
      globalThis.fetch = original;
    },
  } as FetchRouter;

  router.api = (method, match, respond) => router.routes.push({ method, match, respond });
  router.apiGet = (match, respond) => router.routes.push({ method: "GET", match, respond: (url) => respond(url) });
  router.raw = (route) =>
    router.routes.push({ method: route.method, match: route.match, respond: () => route.response });

  globalThis.fetch = ((request: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof request === "string" ? request : request instanceof URL ? request.href : request.url;
    router.calls.push({ url, init });
    for (const route of router.routes) {
      if (route.method && (init?.method ?? "GET") !== route.method) continue;
      if (typeof route.match === "string" ? url === route.match : route.match.test(url)) {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve(route.respond(url, init)),
        } as unknown as Response);
      }
    }
    return Promise.resolve({ ok: true, json: () => Promise.resolve({}) } as unknown as Response);
  }) as typeof fetch;

  return router;
}

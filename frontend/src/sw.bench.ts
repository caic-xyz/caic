// Benchmarks the service worker's request handling for hashed assets and icons.

import vm from "node:vm";
import { readFileSync } from "node:fs";
import { bench } from "@tests/bench";

const workerSource = readFileSync(new URL("../public/sw.js", import.meta.url), "utf8");

type FetchEvent = { request: Request; respondWith: (response: Promise<Response>) => void };
type FetchHandler = (event: FetchEvent) => void;

const listeners = new Map<string, unknown>();
const cached = new Response("asset");
const cache = { match: async () => cached, put: async () => undefined };
vm.runInNewContext(workerSource, {
  URL,
  caches: { delete: async () => true, keys: async () => [], open: async () => cache },
  console,
  fetch: async () => new Response("asset"),
  self: {
    addEventListener: (type: string, listener: unknown) => listeners.set(type, listener),
    location: { href: "https://quick.caic.xyz/sw.js?build=index", origin: "https://quick.caic.xyz" },
    skipWaiting: () => undefined,
  },
});
const handler = listeners.get("fetch") as FetchHandler | undefined;
if (!handler) throw new Error("service worker did not register a fetch handler");

const request = new Request("https://quick.caic.xyz/assets/index-abc123.js");
bench(
  "serves a cached service-worker asset",
  async () => {
    let response: Promise<Response> | undefined;
    handler({ request, respondWith: (next) => (response = next) });
    if ((await response) !== cached) throw new Error("cached asset not returned");
  },
  { time: 1_000, warmupTime: 200 },
);

const iconRequest = new Request("https://quick.caic.xyz/logos/anthropic.svg");
bench(
  "refreshes a service-worker icon",
  async () => {
    let response: Promise<Response> | undefined;
    handler({ request: iconRequest, respondWith: (next) => (response = next) });
    if (!(await response)?.ok) throw new Error("icon request failed");
  },
  { time: 1_000, warmupTime: 200 },
);

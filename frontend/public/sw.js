// Service worker for caic PWA.
// Network-first strategy: always prefer fresh content, fall back to cache.
// Hashed /assets/* files are cached aggressively (immutable).

const CACHE = "caic-v1";

self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (e) => {
  e.waitUntil(
    caches.keys().then((keys) =>
      Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k)))
    )
  );
});

self.addEventListener("fetch", (e) => {
  const url = new URL(e.request.url);

  // Never cache API calls or SSE streams.
  if (url.pathname.startsWith("/api/")) return;

  // Hashed assets: cache-first (immutable content).
  if (url.pathname.startsWith("/assets/")) {
    e.respondWith(
      caches.open(CACHE).then((cache) =>
        cache.match(e.request).then(
          (cached) => cached || fetch(e.request).then((resp) => {
            cache.put(e.request, resp.clone());
            return resp;
          })
        )
      )
    );
    return;
  }

  // Everything else: network-first, cache fallback.
  e.respondWith(
    fetch(e.request)
      .then((resp) => {
        const clone = resp.clone();
        caches.open(CACHE).then((cache) => cache.put(e.request, clone));
        return resp;
      })
      .catch(() => caches.match(e.request))
  );
});

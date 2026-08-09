// Service worker for caic PWA: caches only immutable hashed assets.
// SPA documents are personalized and must always come from the network.

const CACHE = "caic-assets-v2";

self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (e) => {
  e.waitUntil(
    caches.keys().then((keys) =>
      Promise.all(keys.filter((k) => k.startsWith("caic-") && k !== CACHE).map((k) => caches.delete(k)))
    )
  );
});

self.addEventListener("fetch", (e) => {
  if (e.request.method !== "GET") return;

  const url = new URL(e.request.url);
  if (url.origin !== self.location.origin || !url.pathname.startsWith("/assets/")) return;

  // Hashed assets are immutable, so they can be safely served cache-first.
  e.respondWith(
    caches.open(CACHE).then((cache) =>
      cache.match(e.request).then((cached) => {
        if (cached) return cached;
        return fetch(e.request).then((resp) => {
          if (!resp.ok) return resp;
          return cache.put(e.request, resp.clone()).then(
            () => resp,
            (error) => {
              console.warn("Could not cache caic asset", error);
              return resp;
            },
          );
        });
      })
    )
  );
});

// Service worker for caic PWA: caches hashed assets and keeps icons available during restarts.
// SPA documents are personalized and must always come from the network.

const entryAsset = new URL(self.location.href).searchParams.get("build") || "development";
const ASSET_CACHE = `caic-assets-${entryAsset}`;
const ICON_CACHE = "caic-icons-v1";
const ROOT_ICONS = new Set(["/favicon.svg", "/icon-192.png", "/icon-512.png"]);

self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (e) => {
  e.waitUntil(
    caches
      .keys()
      .then((keys) =>
        Promise.all(
          keys
            .filter((k) => k.startsWith("caic-") && k !== ASSET_CACHE && k !== ICON_CACHE)
            .map((k) => caches.delete(k)),
        ),
      ),
  );
});

self.addEventListener("fetch", (e) => {
  if (e.request.method !== "GET") return;

  const url = new URL(e.request.url);
  if (url.origin !== self.location.origin) return;

  if (ROOT_ICONS.has(url.pathname) || url.pathname.startsWith("/logos/")) {
    // Stable URLs are refreshed when online; retain the last successful icon
    // across app builds so a restart can still render the previous version.
    e.respondWith(
      caches.open(ICON_CACHE).then(async (cache) => {
        let resp;
        try {
          resp = await fetch(e.request, { cache: "no-cache" });
        } catch (error) {
          const cached = await cache.match(e.request);
          if (cached) return cached;
          throw error;
        }
        if (resp.ok) {
          try {
            await cache.put(e.request, resp.clone());
          } catch (error) {
            console.warn("Could not cache caic icon", error);
          }
          return resp;
        }
        if (resp.status >= 500) return (await cache.match(e.request)) ?? resp;
        return resp;
      }),
    );
    return;
  }

  if (!url.pathname.startsWith("/assets/")) return;

  // Hashed assets are immutable, so they can be safely served cache-first.
  e.respondWith(
    caches.open(ASSET_CACHE).then((cache) =>
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
      }),
    ),
  );
});

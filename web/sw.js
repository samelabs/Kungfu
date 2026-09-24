// sw.js — Kungfu.md service worker.
//
// Cache policy (deployment correctness first):
//   - scripts & styles: NETWORK-FIRST. The network response always
//     wins when reachable and updates the cache; the cached copy is
//     only a fallback for offline. A deployed JS/CSS change is picked
//     up by a plain online refresh — no manual cache clearing.
//   - images & fonts: cache-first (content-stable assets).
//   - navigations: network with offline fallback.
//   - /api/*: never handled here.
//   - /sw.js itself: served no-cache/no-store by the server.
//
// Version history:
//   v3 — cache-first scripts/styles (stale-code bug), migrated away.
//   v4 — network-first scripts/styles; old v3 caches deleted on
//        activate so they cannot keep polluting pages.
//   v5 — pages reference fingerprinted asset URLs (?v=<content hash>);
//        v4 caches (which could hold year-long proxy-cached copies)
//        are dropped, and the shell is fetched bypassing the HTTP cache.

const SW_VERSION = 'kungfu-pwa-v5';
const SHELL_CACHE = `${SW_VERSION}-shell`;
const RUNTIME_CACHE = `${SW_VERSION}-runtime`;

const SHELL_ASSETS = [
  '/',
  '/manifest.webmanifest',
  '/assets/site.css',
  '/assets/home.css',
  '/assets/owner.css',
  '/assets/icons/app-icon.svg',
  '/assets/icons/app-icon-192.png',
  '/assets/icons/app-icon-512.png',
  '/assets/icons/app-icon-maskable-512.png',
  '/assets/icons/apple-touch-icon.png',
  '/assets/icons/favicon-32.png',
  '/assets/icons/favicon-16.png',
  '/llms.txt',
  '/openai.json'
];

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(SHELL_CACHE).then((cache) =>
      cache.addAll(SHELL_ASSETS.map((url) => new Request(url, {cache: 'reload'}))))
  );
  self.skipWaiting();
});

self.addEventListener('activate', (event) => {
  event.waitUntil((async () => {
    const names = await caches.keys();
    await Promise.all(
      names
        // Delete every cache not of the current version — including
        // the old kungfu-pwa-v3 runtime cache that pinned stale
        // JS/CSS.
        .filter((name) => !name.startsWith(SW_VERSION))
        .map((name) => caches.delete(name))
    );
    await self.clients.claim();
  })());
});

self.addEventListener('fetch', (event) => {
  if (event.request.method !== 'GET') return;

  const url = new URL(event.request.url);
  if (url.origin !== self.location.origin) return;

  if (url.pathname.startsWith('/api/')) {
    return;
  }

  if (event.request.mode === 'navigate') {
    event.respondWith((async () => {
      try {
        const network = await fetch(event.request);
        const cache = await caches.open(RUNTIME_CACHE);
        cache.put(event.request, network.clone());
        return network;
      } catch (error) {
        return (await caches.match(event.request)) || (await caches.match('/'));
      }
    })());
    return;
  }

  const dest = event.request.destination;

  if (dest === 'script' || dest === 'style') {
    // NETWORK-FIRST: never serve stale code while online.
    event.respondWith((async () => {
      const cache = await caches.open(RUNTIME_CACHE);
      try {
        const network = await fetch(event.request);
        if (network && network.ok) {
          cache.put(event.request, network.clone());
        }
        return network;
      } catch (error) {
        const cached = await cache.match(event.request);
        if (cached) return cached;
        throw error;
      }
    })());
    return;
  }

  if (dest === 'image' || dest === 'font') {
    // Content-stable assets keep the existing cache-first policy.
    event.respondWith((async () => {
      const cache = await caches.open(RUNTIME_CACHE);
      const cached = await cache.match(event.request);
      const networkPromise = fetch(event.request)
        .then((response) => {
          cache.put(event.request, response.clone());
          return response;
        })
        .catch(() => null);

      return cached || (await networkPromise) || fetch(event.request);
    })());
  }
});

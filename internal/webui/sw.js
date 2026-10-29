// Service worker for the homelab dashboard.
//
// The only job here is making the app shell openable when the network is not
// there — which, for a tool you reach for when the homelab is broken, is the
// case that matters. It caches the page, the manifest and the icon, and
// nothing else.
//
// Deliberately NOT cached: every API response. The dashboard exists to say what
// is true right now, and a stale alert list is worse than no alert list — it
// would show a green page for a fleet that is on fire. If the network is down,
// the page loads and its fetches fail visibly, which is the honest outcome.

const CACHE = 'homelab-inventory-shell-v1';
const SHELL = ['/', '/manifest.webmanifest', '/icon.svg'];

self.addEventListener('install', (event) => {
  event.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)));
  // Take over as soon as the new worker is ready: a dashboard that keeps
  // serving last week's shell after a deploy is its own bug.
  self.skipWaiting();
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return;

  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return;
  if (!SHELL.includes(url.pathname)) return;   // API responses are never cached

  // Network first, cache as the fallback. The other way round would serve a
  // stale page after every deploy, and the shell is small enough that fetching
  // it fresh costs nothing when the network is up.
  event.respondWith(
    fetch(req)
      .then((res) => {
        const copy = res.clone();
        caches.open(CACHE).then((c) => c.put(req, copy));
        return res;
      })
      .catch(() => caches.match(req).then((hit) => hit || Response.error()))
  );
});

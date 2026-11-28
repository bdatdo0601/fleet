const CACHE_NAME = "fleet-console-v14";
const APP_SHELL = [
  "/",
  "/manifest.webmanifest",
  "/favicon.ico",
  "/apple-touch-icon.png",
  "/assets/app.css",
  "/assets/app.js",
  "/assets/lib.js",
  "/assets/overview.js",
  "/assets/devices.js",
  "/assets/agents.js",
  "/assets/memory.js",
  "/assets/slacker.js",
  "/assets/system.js",
  "/assets/usage.js",
  "/assets/chat.js",
  "/assets/chat_markdown.js",
  "/assets/chat_oauth.js",
  "/assets/chat_settings.js",
  "/assets/skills.js",
  "/assets/favicon.svg",
  "/assets/icon-192.png",
  "/assets/icon-512.png",
  "/assets/apple-touch-icon.png",
];

self.addEventListener("install", (event) => {
  event.waitUntil(
    caches
      .open(CACHE_NAME)
      .then((cache) => cache.addAll(APP_SHELL))
      .then(() => self.skipWaiting()),
  );
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE_NAME).map((k) => caches.delete(k))))
      .then(() => self.clients.claim()),
  );
});

self.addEventListener("fetch", (event) => {
  const req = event.request;
  if (req.method !== "GET") {
    return;
  }
  const url = new URL(req.url);
  if (url.origin !== self.location.origin) {
    return;
  }

  // Never intercept SSE streams or cache /v1/* API data.
  if (url.pathname === "/v1/assistant/stream" || (req.headers.get("accept") || "").includes("text/event-stream")) {
    return;
  }
  if (url.pathname.startsWith("/v1/")) {
    event.respondWith(
      fetch(req).catch(
        () =>
          new Response(JSON.stringify({ error: "Offline: unable to reach fleet lighthouse" }), {
            status: 503,
            headers: { "Content-Type": "application/json; charset=utf-8" },
          }),
      ),
    );
    return;
  }

  // Network-first for app shell and static assets so binary upgrades take effect immediately.
  event.respondWith(
    fetch(req)
      .then((resp) => {
        if (resp && resp.ok) {
          const copy = resp.clone();
          event.waitUntil(
            caches.open(CACHE_NAME).then((cache) => cache.put(req, copy)).catch(() => {}),
          );
        }
        return resp;
      })
      .catch(async () => {
        const cached = await caches.match(req);
        if (cached) {
          return cached;
        }
        if (req.mode === "navigate" && url.pathname !== "/assistant-widget") {
          const shell = await caches.match("/");
          if (shell) {
            return shell;
          }
        }
        return new Response("Offline", { status: 503, headers: { "Content-Type": "text/plain; charset=utf-8" } });
      }),
  );
});

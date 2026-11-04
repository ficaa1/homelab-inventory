/* Read-only demo of the homelab-inventory dashboard.
 * The page is the real dashboard. Every request it makes is answered from a recording of an invented
 * six-host fleet (data.js); timestamps move forward so "seen 2m ago" stays true. Nothing is sent anywhere. */
(function () {
  "use strict";
  var D = window.__DEMO__, shift = Date.now() - Date.parse(D.recordedAt);

  /* Storage: a sandboxed frame has no storage, so fall back to memory. */
  function mem() {
    var m = {};
    return { getItem: function (k) { return k in m ? m[k] : null; }, setItem: function (k, v) { m[k] = String(v); },
      removeItem: function (k) { delete m[k]; }, clear: function () { m = {}; }, key: function (i) { return Object.keys(m)[i] || null; },
      get length() { return Object.keys(m).length; } };
  }
  ["localStorage", "sessionStorage"].forEach(function (n) {
    var ok = false;
    try { var s = window[n]; s.setItem("__t", "1"); s.removeItem("__t"); ok = true; } catch (e) { }
    if (!ok) { try { Object.defineProperty(window, n, { value: mem(), configurable: true }); } catch (e) { } }
  });
  try {
    sessionStorage.setItem("homelab-inventory.token", "demo");
    var p = new URLSearchParams(location.search).get("theme");
    if (p) localStorage.setItem("homelab-inventory.theme", p);
  } catch (e) { }

  /* The page around the demo sends its theme. */
  addEventListener("message", function (e) {
    var t = e.data && e.data.hiTheme;
    if (t !== "light" && t !== "dark") return;
    document.documentElement.setAttribute("data-theme", t);
    try { localStorage.setItem("homelab-inventory.theme", t); } catch (x) { }
  });

  /* No service worker for the demo. */
  try { Object.defineProperty(navigator, "serviceWorker", { value: { register: function () { return Promise.resolve(); } }, configurable: true }); } catch (e) { }

  var ISO = /"((?:19|20)\d\d-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?)(Z|[+-]\d\d:\d\d)"/g;
  function moved(text) {
    return text.replace(ISO, function (all, t, z) {
      var d = new Date(Date.parse(t + z) + shift);
      return isNaN(d) ? all : '"' + d.toISOString() + '"';
    });
  }
  function bytes(b64) {
    var s = atob(b64), a = new Uint8Array(s.length);
    for (var i = 0; i < s.length; i++) a[i] = s.charCodeAt(i);
    return a;
  }
  function json(status, obj) {
    return new Response(JSON.stringify(obj), { status: status, headers: { "content-type": "application/json" } });
  }
  function key(input) {
    var u = new URL(typeof input === "string" ? input : input.url, location.href);
    return u.pathname.replace(/^.*?(?=\/(?:hosts|alerts|overview|services|ports|storage|plugins|icons|images|capabilities|settings|setup|auth|agent|actions|silences|uploads|dusk|healthz|vendor)\b)/, "") + u.search;
  }

  var realFetch = window.fetch.bind(window);
  window.fetch = function (input, init) {
    var method = ((init && init.method) || (input && input.method) || "GET").toUpperCase();
    var k = key(input);
    if (/^\/vendor\//.test(k)) return realFetch("vendor/" + k.split("/").pop(), init);
    if (method !== "GET" && method !== "HEAD") return Promise.resolve(json(403, { error: "This is a read-only demo." }));
    if (k === "/healthz") return Promise.resolve(json(200, { status: "ok" }));
    var r = D.responses[k] || D.responses[k.split("?")[0]];
    if (!r) return Promise.resolve(json(404, { error: "Not part of the demo." }));
    var body = r.enc ? bytes(r.b) : (/json/.test(r.t) ? moved(r.b) : r.b);
    /* chart series in the network plugin are [unix seconds, value] pairs */
    if (!r.enc && /^\/plugins\/network\/api\//.test(k)) body = body.replace(/\[(1[78]\d{8})(\.\d+)?,/g, function (a, t) { return "[" + (+t + Math.round(shift / 1000)) + ","; });
    return new Promise(function (res) {
      setTimeout(function () { res(new Response(body, { status: r.s, headers: { "content-type": r.t || "application/octet-stream" } })); }, 60);
    });
  };

  /* Uploads use XHR for progress; refuse them politely. */
  var send = XMLHttpRequest.prototype.send;
  XMLHttpRequest.prototype.send = function () {
    var x = this;
    setTimeout(function () {
      try { Object.defineProperty(x, "status", { value: 403 }); Object.defineProperty(x, "readyState", { value: 4 }); } catch (e) { }
      x.dispatchEvent(new Event("error")); x.dispatchEvent(new Event("loadend"));
    }, 50);
    return void send;
  };
})();

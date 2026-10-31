// The Network page: speed test, VPN, WAN, connection quality, traffic, routes
// and seeding, read from the network plugin's own API. Each source loads and
// fails on its own; a failed refresh keeps the last answer, dimmed, with its age.
(function () {
  "use strict";
  var ID = "network";
  var REFRESH = 60000;
  var VIEW_LOADS = {
    overview: ["summary", "metrics", "routes"],
    vpn: ["summary", "metrics"],
    traffic: ["metrics", "traffic"],
    routes: ["routes"],
    seeding: ["summary", "torrents"],
    speedtest: []
  };
  var SPEED_VIEWS = { overview: true, speedtest: true };
  var PHASES = { latency: "Measuring ping", download: "Downloading", upload: "Uploading" };

  var open = {};   // details kept open across re-renders, by key
  var s = null;    // the shown page's state; null while hidden
  var rootEl = null;

  // ------------------------------------------------------------ formatting

  function num(v) { return typeof v === "number" && isFinite(v); }
  function bps(v) {
    if (!num(v)) return "—";
    var u = ["b/s", "kb/s", "Mb/s", "Gb/s"], i = 0;
    while (Math.abs(v) >= 1000 && i < u.length - 1) { v /= 1000; i++; }
    return (v < 10 && i > 0 ? v.toFixed(1) : Math.round(v)) + " " + u[i];
  }
  function size(v) { return !num(v) ? "—" : v === 0 ? "0 B" : HI.bytes(v); }
  function bytesPs(v) { return !num(v) ? "—" : v === 0 ? "0 B/s" : HI.bytes(v) + "/s"; }
  function ms(v) { return !num(v) ? "—" : (v < 1 ? v.toFixed(2) : v < 10 ? v.toFixed(1) : Math.round(v)) + " ms"; }
  function pct(v) { return !num(v) ? "—" : (v > 0 && v < 0.1 ? "<0.1" : v < 10 && v % 1 ? v.toFixed(1) : Math.round(v)) + "%"; }
  function mbps(v) { return !num(v) ? "—" : v < 10 ? v.toFixed(1) : String(Math.round(v)); }
  function age(secs) {
    if (!num(secs)) return "—";
    secs = Math.max(0, secs);
    return secs < 90 ? Math.round(secs) + "s" : secs < 5400 ? Math.round(secs / 60) + "m" : secs < 172800 ? Math.round(secs / 3600) + "h" : Math.round(secs / 86400) + "d";
  }
  function since(iso) { var t = iso ? new Date(iso).getTime() : NaN; return isFinite(t) ? (Date.now() - t) / 1000 : null; }
  function span(secs) {
    if (!num(secs)) return "—";
    secs = Math.max(0, Math.floor(secs));
    var d = Math.floor(secs / 86400), h = Math.floor(secs % 86400 / 3600), m = Math.floor(secs % 3600 / 60);
    return d ? d + "d " + h + "h" : h ? h + "h " + m + "m" : m + "m";
  }
  function clock(t) { return new Date(t * 1000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }); }
  function state(status) {
    return status === "not configured" ? "Not configured" : status === "unavailable" ? "Unavailable" : status ? status.charAt(0).toUpperCase() + status.slice(1) : "Unavailable";
  }

  // ------------------------------------------------------------ building blocks
  // Class names and element structure are the Network panel's (net-*, ops-*),
  // so network.css carries its rules unchanged.

  function h(tag, attrs, kids) { return HI.h(tag, attrs, kids); }
  function chip(text, tone, title, icon) {
    return h("span", { class: "net-chip" + (tone ? " " + tone : ""), title: title || null }, [icon ? HI.iconNode(icon, "sm") : null, text]);
  }
  function ageChip(secs) { return chip(age(secs), "", "Age", "clock"); }
  function muted(text) { return h("div", { class: "ops-muted", text: text }); }
  function skel(big) { return h("div", { class: "skel", role: "status", "aria-label": "Loading" }, [h("span", { class: "sk" + (big ? " n" : "") })]); }
  function card(cls, icon, label, chips, body) {
    return h("section", { class: "ops-card net-card" + (cls ? " " + cls : "") }, [
      h("h2", {}, [HI.iconNode(icon, "sm"), label, h("span", { class: "net-chips" }, chips || [])])].concat(body));
  }
  function details(key, cls, summary, detail) {
    return h("details", { class: cls, "data-hn": key, open: !!open[key] }, [h("summary", {}, summary), h("div", { class: "ops-detail" }, detail)]);
  }
  function facts(rows) {
    var dl = h("dl", { class: "protection-facts" });
    rows.forEach(function (r) { dl.appendChild(h("dt", { text: r[0] })); dl.appendChild(h("dd", { text: r[1] })); });
    return dl;
  }
  function meter(v) {
    return h("div", { class: "ops-meter" }, [h("i", { style: "width:" + Math.max(0, Math.min(100, v)) + "%" })]);
  }
  function link(view, label) {
    return h("button", { class: "btn hn-link", type: "button", onclick: function (e) { e.preventDefault(); HI.go(HI.link(ID, view)); } }, [label]);
  }

  // Gaps (null) lift the pen: unknown is never drawn as zero.
  var NS = "http://www.w3.org/2000/svg";
  function svg(tag, attrs) {
    var el = document.createElementNS(NS, tag);
    Object.keys(attrs).forEach(function (k) { el.setAttribute(k, String(attrs[k])); });
    return el;
  }
  function path(points, w, hgt, lo, hi, t0, dt, pad) {
    var d = "", pen = false, last = null;
    points.forEach(function (p) {
      if (!num(p[1])) { pen = false; return; }
      var x = pad + (w - 2 * pad) * (p[0] - t0) / dt, y = hgt - pad - (hgt - 2 * pad) * (p[1] - lo) / (hi - lo);
      d += (pen ? "L" : "M") + x.toFixed(1) + " " + y.toFixed(1);
      pen = true; last = [x, y];
    });
    return { d: d, last: last };
  }
  function range(vals, zero) {
    var lo = Math.min.apply(null, vals), hi = Math.max.apply(null, vals);
    if (zero) lo = Math.min(0, lo);
    if (hi === lo) { if (zero && lo === 0) hi = 1; else { lo = hi - 1; hi = lo + 2; } }
    return [lo, hi];
  }
  function spark(points, opts) {
    opts = opts || {};
    var cls = "net-spark" + (opts.cls ? " " + opts.cls : "");
    var pts = (Array.isArray(points) ? points : []).filter(function (p) { return Array.isArray(p) && num(p[0]); });
    var vals = pts.filter(function (p) { return num(p[1]); }).map(function (p) { return p[1]; });
    if (vals.length < 2) return h("span", { class: cls, "aria-hidden": "true" });
    var w = opts.w || 64, hgt = opts.h || 20, r = range(vals, opts.zero);
    var t0 = pts[0][0], dt = Math.max(1, pts[pts.length - 1][0] - t0);
    var line = path(pts, w, hgt, r[0], r[1], t0, dt, 2);
    var el = svg("svg", { class: cls, viewBox: "0 0 " + w + " " + hgt, preserveAspectRatio: "none", "aria-hidden": "true" });
    el.appendChild(svg("path", { d: line.d, fill: "none", stroke: "currentColor", "stroke-width": 1.5, "stroke-linejoin": "round", "stroke-linecap": "round", "vector-effect": "non-scaling-stroke" }));
    if (opts.dot !== false && line.last) el.appendChild(svg("circle", { cx: line.last[0].toFixed(1), cy: line.last[1].toFixed(1), r: 2, fill: "currentColor" }));
    return el;
  }

  // A larger history chart for the Traffic view: shared scale, a dashed grid at
  // the halves, the peak and the window's ends as text outside the SVG.
  function chart(title, series, fmt) {
    var all = [], ts = [];
    series.forEach(function (x) { (x.points || []).forEach(function (p) { if (Array.isArray(p) && num(p[0])) { ts.push(p[0]); if (num(p[1])) all.push(p[1]); } }); });
    var legend = h("div", { class: "hn-legend" }, series.map(function (x) {
      var last = null;
      (x.points || []).forEach(function (p) { if (num(p[1])) last = p[1]; });
      return h("span", { class: "hn-key " + x.cls }, [h("i"), x.name, h("b", { text: fmt(last) })]);
    }));
    var head = h("div", { class: "hn-chart-head" }, [h("span", { class: "hn-chart-title", text: title }), legend]);
    if (all.length < 2) return h("div", { class: "hn-chart" }, [head, muted("No history yet")]);
    var w = 600, hgt = 120, r = range(all, true), t0 = Math.min.apply(null, ts), dt = Math.max(1, Math.max.apply(null, ts) - t0);
    var el = svg("svg", { class: "hn-plot", viewBox: "0 0 " + w + " " + hgt, preserveAspectRatio: "none", role: "img", "aria-label": title });
    [0.5, 1].forEach(function (f) {
      var y = (hgt - 2 - (hgt - 4) * f).toFixed(1);
      el.appendChild(svg("line", { x1: 0, x2: w, y1: y, y2: y, class: "hn-gl", "vector-effect": "non-scaling-stroke" }));
    });
    el.appendChild(svg("line", { x1: 0, x2: w, y1: hgt - 2, y2: hgt - 2, class: "hn-gl base", "vector-effect": "non-scaling-stroke" }));
    series.forEach(function (x) {
      var pts = (x.points || []).filter(function (p) { return Array.isArray(p) && num(p[0]); });
      var line = path(pts, w, hgt, r[0], r[1], t0, dt, 2);
      if (line.d) el.appendChild(svg("path", { d: line.d, class: "hn-line " + x.cls, fill: "none", "stroke-width": 1.75, "stroke-linejoin": "round", "vector-effect": "non-scaling-stroke" }));
    });
    return h("div", { class: "hn-chart" }, [head, h("div", { class: "hn-plotwrap" }, [h("span", { class: "hn-peak", text: fmt(r[1]) }), el]),
      h("div", { class: "hn-axis" }, [h("span", { text: clock(t0) }), h("span", { text: clock(t0 + dt) })])]);
  }

  // ------------------------------------------------------------ metrics helpers

  function report() {
    var m = s.data.metrics && s.data.metrics.panels ? s.data.metrics : s.data.summary && s.data.summary.metrics;
    return m && m.panels ? m : null;
  }
  function series(id) { var m = report(), p = m && m.panels[id]; return p && Array.isArray(p.series) ? p.series : []; }
  function has(id) { var m = report(); return !!(m && m.panels[id]); }
  function source(name) { var m = report(); return !!(m && m.sources && m.sources[name]); }
  function panelStale(id) {
    var m = report(), p = m && m.panels[id];
    return !p || !!p.stale || !!s.err.metrics || !Array.isArray(p.series) || !p.series.length || p.series.some(function (x) {
      var a = since(x.sampledAt);
      return a == null || a > 120 || a < -5;
    });
  }
  function sampleAge(ids) {
    var newest = null;
    ids.forEach(function (id) {
      series(id).forEach(function (x) { var a = since(x.sampledAt); if (a != null && (newest == null || a < newest)) newest = a; });
    });
    if (newest == null && s.err.metrics && s.when.metrics) newest = (Date.now() - s.when.metrics) / 1000;
    return newest;
  }
  function metricsStatus() {
    return s.data.metrics ? s.data.metrics.status : s.data.summary && s.data.summary.metrics && s.data.summary.metrics.status;
  }
  function metricsReady() { var st = metricsStatus(); return report() && (st === "ok" || st === "partial"); }
  function metricsPending(icon, label, cls) {
    var st = metricsStatus();
    return card(cls || "", icon, label, [], [s.data.metrics || s.data.summary || s.err.metrics ? muted(state(st)) : skel()]);
  }
  function ipKey(t) {
    var p = String(t).split(".").map(Number);
    var priv = p[0] === 10 || p[0] === 192 && p[1] === 168 || p[0] === 172 && p[1] >= 16 && p[1] < 32;
    return (priv ? 1 : 0) * 1e13 + (p.length === 4 && p.every(isFinite) ? ((p[0] * 256 + p[1]) * 256 + p[2]) * 256 + p[3] : 0);
  }

  // ------------------------------------------------------------ VPN

  function vpnCard(v, full) {
    var sum = s.data.summary, d = v && v.data, q = sum && sum.qbit, many = !!(sum.vpns && sum.vpns.length > 1);
    var vpnAge = since(v.inventoryAt);
    var stale = !!v.stale || !!s.err.summary || !d || vpnAge == null || vpnAge > 2700 || vpnAge < -15, hs = null, tone = "";
    if (d) {
      var at = v.inventoryAt ? new Date(v.inventoryAt).getTime() : NaN, hst = d.latestHandshake ? new Date(d.latestHandshake).getTime() : NaN;
      if (isFinite(at) && isFinite(hst)) hs = Math.max(0, (at - hst) / 1000);
      tone = !d.interfaceUp ? "crit" : hs == null ? "warn" : hs <= 180 ? "ok" : "warn";
    }
    var dot = h("span", { class: "net-dot " + (stale ? "" : tone), title: !d ? "No data" : !d.interfaceUp ? "Interface down" : hs == null ? "No handshake" : hs <= 180 ? "Up" : "Handshake stale" });
    var chips = [];
    if (many) chips.push(chip(v.host, "", "Host"));
    if (d && d.interfaceUp) chips.push(chip(age(hs), stale ? "" : tone, "Handshake", "refresh"));
    else if (d) chips.push(chip("down", stale ? "" : "crit", "Interface"));
    if (stale) chips.push(ageChip(vpnAge));
    var flow = h("span", { class: "net-flow" });
    if (source("vpn")) {
      var rx = series("vpnRx")[0], tx = series("vpnTx")[0];
      if (panelStale("vpnRx") || panelStale("vpnTx")) flow.className += " net-dim";
      flow.appendChild(h("span", { class: "net-cell" }, [h("span", { class: "net-dir", title: "Received", text: "↓" }), spark(rx && rx.points, { zero: true, dot: false }), h("b", { text: bps(rx && rx.value) })]));
      flow.appendChild(h("span", { class: "net-cell" }, [h("span", { class: "net-dir", title: "Sent", text: "↑" }), spark(tx && tx.points, { zero: true, dot: false }), h("b", { text: bps(tx && tx.value) })]));
    }
    if (q && q.status && q.status !== "not configured") {
      var qb = h("span", { class: "net-qbit" }, q.status === "ok"
        ? [chip("qB ↓ " + bytesPs(q.downloadBytesPerSecond), "", "qBittorrent download"), chip("qB ↑ " + bytesPs(q.uploadBytesPerSecond), "", "qBittorrent upload")]
        : [chip("qB —", "", state(q.status))]);
      qb.appendChild(h("button", { class: "net-seed", type: "button", onclick: function (e) { e.preventDefault(); e.stopPropagation(); openSeeding(); } }, ["Seeding"]));
      flow.appendChild(qb);
    }
    var where = d ? [d.country ? h("b", { text: d.country }) : null, d.city ? h("span", { text: d.city }) : null] : [];
    var summary = [
      h("span", { class: "net-vpn-head" }, [dot, h("span", { class: "ops-label" }, [HI.iconNode("shield", "sm"), "VPN"]), h("span", { class: "net-chips" }, chips)]),
      h("span", { class: "net-ip", text: d && d.exitIp || "—" }),
      h("span", { class: "net-where" }, where), flow];
    var rows = [["Endpoint", d && d.endpoint || "—"], ["Org", d && d.org || "—"],
      ["Received", d && d.rxBytes != null ? size(d.rxBytes) : "—"], ["Sent", d && d.txBytes != null ? size(d.txBytes) : "—"],
      ["Collected", v.inventoryAt ? HI.ago(v.inventoryAt) : "—"]];
    if (many) rows.unshift(["Host", v.host || "—"]);
    if (q && q.status === "ok") rows.push(["qBittorrent", (q.version || "—") + " · " + (q.connectionStatus || "—") + (q.dhtNodes != null ? " · DHT " + q.dhtNodes : "")]);
    var key = "vpn-" + (v.host || "");
    if (full && !(key in open)) open[key] = true;
    return details(key, "ops-card ops-wide net-card net-vpn" + (stale ? " net-stale" : ""), summary, [facts(rows)]);
  }
  function vpnSection(full) {
    var sum = s.data.summary;
    if (!sum) return card("ops-wide net-vpn", "shield", "VPN", [], [s.err.summary ? muted("Unavailable") : skel(true)]);
    var list = Array.isArray(sum.vpns) && sum.vpns.length ? sum.vpns : [sum.vpn || { stale: true }];
    if (!list[0].data && !list[0].inventoryAt) return card("ops-wide", "shield", "VPN", [], [muted("No host has reported tunnel facts")]);
    return full ? list.map(function (v) { return vpnCard(v, true); }) : [vpnCard(list[0], false)];
  }

  // ------------------------------------------------------------ WAN and quality

  function wanCard() {
    if (!metricsReady()) return metricsPending("trend", "WAN");
    var ids = ["wanDown", "wanUp", "wanDownUtilization", "wanUpUtilization", "wanState"].concat(has("ponRxPower") && series("ponRxPower").length ? ["ponRxPower"] : []);
    var stale = ids.some(panelStale), st = series("wanState")[0], pon = series("ponRxPower")[0];
    var sv = st && st.value, pv = pon && pon.value;
    var chips = [chip(sv === 1 ? "Up" : sv == null ? "—" : "Down", stale || sv == null ? "" : sv === 1 ? "ok" : "crit", "WAN state")];
    if (pon) chips.push(chip(num(pv) ? pv.toFixed(1) + " dBm" : "— dBm", stale || !num(pv) ? "" : pv < -27 || pv > -8 ? "warn" : "", "Optical receive power"));
    if (stale) chips.push(ageChip(sampleAge(ids.filter(panelStale))));
    function rate(id, util, arrow, label) {
      var x = series(id)[0], u = series(util)[0], uv = u && u.value;
      return h("div", { class: "net-rate" + (panelStale(id) ? " net-dim" : "") }, [
        h("span", { class: "ops-label", text: arrow + " " + label }),
        h("span", { class: "ops-number", text: bps(x && x.value) }),
        spark(x && x.points, { zero: true, cls: "wide", w: 160, h: 28, dot: false }),
        h("span", { class: "net-util" }, [h("span", { text: pct(uv) }), num(uv) ? meter(uv) : null])]);
    }
    var body = [h("div", { class: "net-pair" }, [rate("wanDown", "wanDownUtilization", "↓", "Down"), rate("wanUp", "wanUpUtilization", "↑", "Up")])];
    var sections = series("collectionSuccess");
    if (sections.length) {
      var ok = sections.filter(function (x) { return x.value === 1; }).length;
      body.push(details("collectors", "ops-row net-collectors", [h("span", { class: "ops-name", text: "Collectors" }), h("b", { text: ok + "/" + sections.length })],
        [h("span", { class: "net-chips" }, sections.map(function (x) {
          var name = (x.labels || {}).device || (x.labels || {}).section || "?";
          return chip((x.value === 1 ? "✓ " : x.value == null ? "— " : "✕ ") + name, x.value === 1 ? "ok" : x.value == null ? "" : "crit");
        }))]));
    }
    return card(stale ? "net-stale" : "", "trend", "WAN", chips, body);
  }

  function qualityCard() {
    if (!metricsReady()) return metricsPending("pulse", "Quality");
    var rtt = series("pingRtt"), loss = series("pingLoss"), dns = series("dnsLatency"), probes = series("probeUp"), dur = series("probeDuration");
    var by = function (list, label) { var o = {}; list.forEach(function (x) { o[(x.labels || {})[label]] = x; }); return o; };
    var rttBy = by(rtt, "target"), lossBy = by(loss, "target"), durBy = by(dur, "instance");
    var targets = {};
    rtt.concat(loss).forEach(function (x) { var t = (x.labels || {}).target; if (t) targets[t] = true; });
    var rs = source("quality") && panelStale("pingRtt"), ls = source("quality") && panelStale("pingLoss"), ds = source("quality") && panelStale("dnsLatency");
    var body = [];
    function sub(a, b, c) { return h("div", { class: "net-sub" }, [h("span", { text: a }), h("span", { text: b }), h("span", { text: c })]); }
    function row(name, a, b) { return h("div", { class: "net-row" }, [h("span", { class: "net-name", text: name, title: name }), a, b]); }
    function cell(dim, pts, value, opts, extra) {
      return h("span", { class: "net-cell" + (extra || "") + (dim ? " net-dim" : "") }, [spark(pts, opts), h("b", { text: value })]);
    }
    if (source("quality")) {
      body.push(sub("Ping", "RTT", "Loss"));
      var names = Object.keys(targets).sort(function (a, b) { return ipKey(a) - ipKey(b) || (a < b ? -1 : 1); });
      if (!names.length) body.push(muted("—"));
      names.forEach(function (t) {
        var r = rttBy[t], l = lossBy[t], lv = l && l.value, lossy = (l && l.points || []).some(function (p) { return num(p[1]) && p[1] > 0; });
        body.push(row(t, cell(rs, r && r.points, ms(r && r.value)),
          cell(ls, l && l.points, pct(lv), { zero: true, cls: "sm", w: 40 }, " loss" + (ls ? "" : num(lv) && lv > 0 ? " crit" : lossy ? " warn" : ""))));
      });
      body.push(sub("DNS", "", ""));
      if (!dns.length) body.push(muted("—"));
      dns.slice().sort(function (a, b) { return ipKey((a.labels || {}).server) - ipKey((b.labels || {}).server); }).forEach(function (x) {
        body.push(row((x.labels || {}).server || "?", cell(ds, x.points, ms(x.value)), h("span", { class: "net-cell loss" })));
      });
    }
    if (probes.length) {
      body.push(sub("Probes", "Duration", "State"));
      probes.slice(0, 20).forEach(function (x) {
        var inst = (x.labels || {}).instance || "?", d = durBy[inst], up = x.value === 1;
        body.push(row(inst, cell(false, d && d.points, ms(d && d.value)),
          h("span", { class: "net-cell loss" }, [chip(x.value == null ? "—" : up ? "up" : "down", x.value == null ? "" : up ? "ok" : "crit")])));
      });
    }
    if (!body.length) body.push(muted("No ping, DNS or probe metrics. Set the diagnostics prefix in the plugin settings, or run blackbox_exporter."));
    var stale = rs || ls || ds;
    return card(stale ? "net-stale" : "", "pulse", "Quality", stale ? [ageChip(sampleAge(["pingRtt", "pingLoss", "dnsLatency"].filter(panelStale)))] : [], body);
  }

  // ------------------------------------------------------------ routes

  // A route's target matches a catalog service by IPv4 address and port.
  function addrPort(u) {
    var m = /^(?:https?:\/\/)?((?:\d{1,3}\.){3}\d{1,3}):(\d{1,5})(?:[\/?#].*)?$/.exec(String(u || "").trim());
    return m ? m[1] + ":" + Number(m[2]) : null;
  }
  function routesCard(full) {
    var r = s.data.routes;
    if (!r) return card("ops-wide", "link", "Routes", [], [s.err.routes ? muted("Unavailable") : skel()]);
    var routes = Array.isArray(r.routes) ? r.routes : [], dns = Array.isArray(r.dns) ? r.dns : [], stale = !!r.stale || !!s.err.routes;
    var catalog = HI.services && HI.services(), list = catalog ? (catalog.groups || []).reduce(function (a, g) { return a.concat(g.services || []); }, []).concat(catalog.missing || []) : [];
    function targetService(target) {
      var want = addrPort(target);
      if (!want) return null;
      for (var i = 0; i < list.length; i++) {
        var urls = [list[i].url].concat(list[i].alt_urls || []);
        for (var j = 0; j < urls.length; j++) if (addrPort(urls[j]) === want) return list[i];
      }
      return null;
    }
    var dnsBy = Object.create(null); dns.forEach(function (d) { var k = String(d.name || "").toLowerCase().replace(/\.$/, ""); (dnsBy[k] || (dnsBy[k] = [])).push(d); });
    var dnsOK = r.cloudflareDns === "ok";
    var chips = [chip(r.status === "ok" ? String(routes.length) : "—")];
    if (!dnsOK && r.cloudflareDns) chips.push(chip(/^needs /.test(r.cloudflareDns) ? r.cloudflareDns : "DNS —", r.cloudflareDns === "not configured" ? "" : "warn", "Cloudflare DNS: " + state(r.cloudflareDns)));
    if (stale && r.status !== "not configured") chips.push(ageChip(since(r.generatedAt) != null && !s.err.routes ? since(r.generatedAt) : s.when.routes ? (Date.now() - s.when.routes) / 1000 : null));
    var sorted = routes.slice().sort(function (a, b) { return a.hostname < b.hostname ? -1 : a.hostname > b.hostname ? 1 : 0; });
    var shown = full ? sorted : sorted.slice(0, 12);
    var rows = shown.map(function (x) {
      var svc = targetService(x.target), records = dnsBy[String(x.hostname || "").toLowerCase()] || [], host = String(x.hostname || "");
      var mark = !dnsOK ? null : records.length ? h("span", { class: "net-mark ok", title: "DNS " + records.map(function (d) { return d.type; }).join(", ") }, [HI.iconNode("check", "sm")]) : h("span", { class: "net-mark", title: "No DNS record", text: "○" });
      var sso = x.sso === true ? h("span", { class: "net-mark ok", title: "SSO" }, [HI.iconNode("lock", "sm")]) : x.sso === false ? h("span", { class: "net-mark warn", title: "No SSO" }, [HI.iconNode("unlock", "sm")]) : null;
      var f = [];
      if (svc) f.push(["Target", String(x.target || "").replace(/^https?:\/\//, "")]);
      if (dnsOK) {
        if (!records.length) f.push(["DNS", "—"]);
        else records.forEach(function (d) { f.push(["DNS " + d.type, d.proxied ? "Proxied" : "DNS only"]); });
      }
      if (x.sso != null) f.push(["SSO", x.sso ? "Yes" : "No"]);
      var head = svc
        ? [HI.serviceIcon(svc), h("span", { class: "ops-name" }, [h("b", { text: svc.name }), h("span", { class: "ops-sub", text: host })])]
        : [h("span", { class: "ico net-glyph" }, [HI.iconNode("link")]), h("span", { class: "ops-name" }, [h("b", { text: host }), h("span", { class: "ops-sub", text: "→ " + x.target })])];
      return details("route-" + host + "-" + x.target, "ops-row net-route", head.concat([mark, sso]),
        [f.length ? facts(f) : null, /^[a-z0-9.-]+$/i.test(host) ? h("a", { href: "https://" + host, target: "_blank", rel: "noopener noreferrer", text: "Open ↗" }) : null]);
    });
    var loose = dns.filter(function (d) { return !d.matchesRoute; });
    var body = rows.length ? rows : [muted(r.status === "ok" ? "—" : state(r.status))];
    if (loose.length) body.push(details("dns-unmatched", "ops-more net-unmatched", [loose.length + " DNS"], loose.map(function (d) {
      return h("div", { class: "net-dnsrow" }, [h("span", { class: "ops-name", text: d.name }), chip(d.type || "DNS"), d.proxied ? chip("proxied") : null]);
    })));
    if (!full && routes.length > shown.length) body.push(h("div", { class: "hn-more" }, [link("routes", "All " + routes.length + " routes")]));
    return card("ops-wide" + (stale ? " net-stale" : ""), "link", "Routes", chips, body);
  }

  // ------------------------------------------------------------ seeding

  function seedingCard() {
    var list = s.data.torrents;
    if (!list) return card("ops-wide", "upload", "Seeding", [], [s.err.torrents ? muted("Unavailable") : skel()]);
    return card("ops-wide", "upload", "Seeding", list.status === "ok" ? [chip(String(list.count))] : [], [h("div", { class: "net-seeding hn-seedbox" }, seedingBody(list))]);
  }
  // The VPN card's Seeding button reads the list on open only;
  // an answer for a closed or superseded drawer is dropped.
  var seedEpoch = 0;
  function openSeeding() {
    var epoch = ++seedEpoch;
    function show(kids) { HI.openDrawer(h("div", { class: "net-seeding" }, [h("h3", { text: "Seeding" })].concat(kids)), "Seeding"); }
    show([h("div", { class: "empty", text: "Loading…" })]);
    HI.get(ID, "torrents").then(function (list) { if (epoch === seedEpoch) show(seedingBody(list)); },
      function () { if (epoch === seedEpoch) show([h("div", { class: "note", text: "Unavailable" })]); });
  }
  function seedingBody(list) {
    if (list.status !== "ok") return [muted(state(list.status))];
    var torrents = Array.isArray(list.torrents) ? list.torrents : [], need = list.requirementSeconds;
    var done = torrents.filter(function (t) { return t.done; }).length;
    var totals = h("div", { class: "seed-totals" }, [
      h("span", {}, [h("b", { text: String(list.count) }), " seeding"]), h("span", {}, [h("b", { text: size(list.totalSizeBytes) })]),
      num(need) ? h("span", {}, [h("b", { text: String(done) }), " done"]) : null,
      h("span", { text: num(need) ? "Target " + span(need) : "No seeding target set" })]);
    if (!torrents.length) return [totals, muted("Nothing seeding")];
    return [totals, h("ul", { class: "seed-list" }, torrents.slice(0, 500).map(function (t) {
      var p = num(need) && need > 0 ? t.seedingSeconds / need * 100 : null;
      return h("li", { class: "seed-row" }, [
        h("div", { class: "seed-head" }, [h("span", { class: "seed-name", text: t.name }),
          num(t.remainingSeconds) ? h("span", { class: "seed-left" + (t.done ? " done" : ""), text: t.done ? "Done" : span(t.remainingSeconds) + " left" }) : h("span", { class: "seed-left", text: bytesPs(t.uploadBytesPerSecond) })]),
        h("div", { class: "seed-facts" }, [h("span", { text: size(t.sizeBytes) }), h("span", { text: "Seeded " + span(t.seedingSeconds) }),
          t.category ? h("span", { text: t.category }) : null]),
        p == null ? null : meter(p)]);
    }))];
  }
  function qbitCard() {
    var q = s.data.summary && s.data.summary.qbit;
    if (!q) return card("", "download", "qBittorrent", [], [s.err.summary ? muted("Unavailable") : skel()]);
    if (q.status !== "ok") return card("", "download", "qBittorrent", [], [muted(state(q.status))]);
    return card("", "download", "qBittorrent", [chip(q.connectionStatus || "—", q.connectionStatus === "connected" ? "ok" : "warn", "Connection")], [
      h("div", { class: "net-pair" }, [
        h("div", { class: "net-rate" }, [h("span", { class: "ops-label", text: "↓ Download" }), h("span", { class: "ops-number", text: bytesPs(q.downloadBytesPerSecond) })]),
        h("div", { class: "net-rate" }, [h("span", { class: "ops-label", text: "↑ Upload" }), h("span", { class: "ops-number", text: bytesPs(q.uploadBytesPerSecond) })])]),
      h("div", { class: "ops-detail" }, [facts([["Version", q.version || "—"], ["DHT nodes", q.dhtNodes != null ? String(q.dhtNodes) : "—"]])])]);
  }

  // ------------------------------------------------------------ traffic

  function trafficSection() {
    var out = [], t = s.data.traffic;
    if (metricsReady() && source("wan")) {
      var down = series("wanDown")[0], up = series("wanUp")[0];
      out.push(card("ops-wide" + (panelStale("wanDown") ? " net-stale" : ""), "trend", "WAN", [], [chart("Internet line", [
        { name: "Down", cls: "c1", points: down && down.points }, { name: "Up", cls: "c2", points: up && up.points }], bps)]));
    }
    if (metricsReady() && source("vpn")) {
      var rx = series("vpnRx")[0], tx = series("vpnTx")[0];
      out.push(card("ops-wide" + (panelStale("vpnRx") ? " net-stale" : ""), "shield", "VPN tunnel", [], [chart("Container traffic", [
        { name: "Received", cls: "c1", points: rx && rx.points }, { name: "Sent", cls: "c2", points: tx && tx.points }], bps)]));
    }
    if (!t) { out.push(card("ops-wide", "network", "Hosts", [], [s.err.traffic ? muted("Unavailable") : skel()])); return out; }
    (t.notes || []).forEach(function (n) { out.push(HI.notice("info", n)); });
    if (t.status === "not configured") { out.push(HI.notice("info", "Prometheus is not configured. Set its URL in Admin → Plugins.")); return out; }
    var fleet = t.fleet || [[], []];
    if ((fleet[0] || []).length + (fleet[1] || []).length) {
      out.push(card("ops-wide", "network", "Fleet", [], [chart("All hosts", [{ name: "Receive", cls: "c1", points: fleet[0] }, { name: "Transmit", cls: "c2", points: fleet[1] }], bytesPs)]));
    }
    var hosts = t.hosts || [];
    if (hosts.length) {
      out.push(card("", "server", "Hosts", [chip(String(hosts.length))], hosts.map(function (x) {
        return details("host-" + x.name, "ops-row hn-host", [
          h("span", { class: "ops-name" }, [h("b", { text: x.name }), (num(x.errors) && x.errors > 0) || (num(x.drops) && x.drops > 0) ? h("span", { class: "ops-sub t-warn", text: (num(x.errors) ? x.errors.toFixed(2) : "0") + " err/s · " + (num(x.drops) ? x.drops.toFixed(2) : "0") + " drop/s" }) : null]),
          h("span", { class: "net-cell" }, [h("span", { class: "net-dir", text: "↓" }), spark(x.receive, { zero: true, dot: false }), h("b", { text: bytesPs(x.rx) })]),
          h("span", { class: "net-cell" }, [h("span", { class: "net-dir", text: "↑" }), spark(x.transmit, { zero: true, dot: false }), h("b", { text: bytesPs(x.tx) })])],
          [chart(x.name, [{ name: "Receive", cls: "c1", points: x.receive }, { name: "Transmit", cls: "c2", points: x.transmit }], bytesPs)]);
      })));
    }
    var cs = t.containers || [];
    if (cs.length) {
      var top = Math.max.apply(null, cs.map(function (c) { return num(c.bytesPerSecond) ? c.bytesPerSecond : 0; })) || 1;
      out.push(card("", "box", "Busiest containers", [], cs.map(function (c) {
        return h("div", { class: "hn-crow" }, [h("span", { class: "net-name", text: c.name, title: c.name }), meter((c.bytesPerSecond || 0) / top * 100), h("b", { text: bytesPs(c.bytesPerSecond) })]);
      })));
    }
    return out;
  }

  // ------------------------------------------------------------ speed test

  var SERVERS = { free: "Free", scaleway: "Scaleway", ovh: "OVH" };
  function server(r) {
    var down = r.downServer || "cloudflare", up = r.upServer || "cloudflare";
    var name = function (k) { return k === "cloudflare" ? "CF" + (r.colo ? " " + r.colo : "") : SERVERS[k] || k; };
    return down === up ? name(down) : "↓ " + name(down) + " · ↑ " + name(up);
  }
  function refusal(r) {
    if (!r || !r.uploadRefused) return "";
    var retry = r.uploadRetryAfter && new Date(r.uploadRetryAfter);
    return "Upload refused" + (retry && isFinite(retry.getTime()) ? " · Retry after " + retry.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }) : "");
  }
  function speedCell(label, value, unit, cls) {
    return h("div", { class: "net-speed-cell" + (cls ? " " + cls : "") }, [h("div", { class: "net-speed-label", text: label }),
      h("div", { class: "net-speed-num" }, [value, unit ? h("small", { text: unit }) : null])]);
  }
  function speedCard() {
    var btn = h("button", { class: "btn primary", type: "button", title: "Measures from the dashboard server: about 9 seconds of download and 3 of upload. At multi-gigabit speeds one run can move several GB.",
      onclick: startSpeed }, ["Run test"]);
    var body = h("div", { id: "net-speed-body", "aria-live": "polite" });
    s.speedBtn = btn; s.speedBody = body;
    var el = h("section", { class: "ops-card net-speed", "aria-labelledby": "net-speed-h" }, [
      h("div", { class: "net-speed-head" }, [h("h2", { id: "net-speed-h" }, [HI.iconNode("pulse", "sm"), "Internet speed"]), btn]), body]);
    renderSpeed();
    return el;
  }
  function renderSpeed() {
    if (!s || !s.speedBody) return;
    var sp = s.data.speed, body = s.speedBody, btn = s.speedBtn;
    var running = !!(sp && sp.running), cur = sp && sp.current, last = sp && sp.last;
    var hist = sp && Array.isArray(sp.history) ? sp.history : [];
    btn.disabled = running || s.starting || !!(sp && sp.configured === false);
    btn.textContent = running ? "Running…" : "Run test";
    var kids = [];
    if (!sp) {
      kids.push(muted(s.err.speed ? "Unavailable" : "Loading…"));
    } else if (sp.configured === false) {
      kids.push(muted(sp.message || "Speed test is not configured"));
    } else {
      var phase = running && cur ? cur.phase : "";
      var pick = function (name, val) { return phase === name ? { v: mbps(cur.mbps), cls: "live" } : { v: mbps(val), cls: running ? "stale" : "" }; };
      var down = pick("download", last && last.down_mbps), up = pick("upload", last && last.up_mbps);
      kids.push(h("div", { class: "net-speed-grid" }, [speedCell("Download", down.v, "Mbps", down.cls), speedCell("Upload", up.v, "Mbps", up.cls),
        speedCell("Ping", phase === "latency" ? "…" : ms(last && last.latency_ms), "", phase === "latency" ? "live" : running ? "stale" : ""),
        speedCell("Jitter", ms(last && last.jitter_ms), "", running ? "stale" : "")]));
      var meta = h("div", { class: "net-speed-meta" });
      if (running) meta.appendChild(chip(PHASES[phase] || "Starting"));
      else if (last) {
        meta.appendChild(chip(age(since(last.time)), "", "Age"));
        meta.appendChild(chip(server(last), "", "Download / upload server"));
        if (last.error) meta.appendChild(chip(last.error, "warn", "Last run"));
        if (last.uploadRefused) meta.appendChild(chip(refusal(last), "warn", last.uploadRetryAfter || ""));
        if (last.download && last.upload && num(last.download.bytes + last.upload.bytes)) meta.appendChild(chip(size(last.download.bytes + last.upload.bytes), "", "Payload used, including warm-up"));
      } else meta.appendChild(muted("No test run yet"));
      if (s.err.speed) meta.appendChild(chip("Unavailable", "warn"));
      kids.push(meta);
      if (hist.length >= 2) kids.push(h("div", {}, [spark(hist.slice().reverse().map(function (r, i) { return [i, r.down_mbps]; }), { w: 300, h: 28, cls: "net-speed-spark" })]));
      if (hist.length) {
        if (s.view === "speedtest" && !("speedtest" in open)) open.speedtest = true;
        var rows = h("div", { class: "ops-detail" });
        var row = function (values, head) {
          return h("div", { class: "net-speed-row" + (head ? " head" : "") }, values.map(function (v) { return h("span", { text: v, title: v }); }));
        };
        rows.appendChild(row(["When", "Down", "Up", "Ping", "Jitter", "Server", "Note"], true));
        hist.forEach(function (r) {
          var key = "speed-" + r.time;
          rows.appendChild(h("details", { class: "net-speed-entry", "data-hn": key, open: !!open[key] }, [
            h("summary", {}, [row([HI.ago(r.time), mbps(r.down_mbps), mbps(r.up_mbps), ms(r.latency_ms), ms(r.jitter_ms), server(r), r.error || refusal(r)])]),
            h("div", { class: "net-speed-extra", text: "Ping " + ms(r.latency_ms) + " · Jitter " + ms(r.jitter_ms) + " · " + server(r) + (r.error ? " · " + r.error : "") + (r.uploadRefused ? " · " + refusal(r) : "") })]));
        });
        kids.push(h("details", { class: "net-speed-hist", "data-hn": "speedtest", open: !!open.speedtest }, [h("summary", { text: "History (" + hist.length + ")" }), rows]));
      }
    }
    if (s.note) kids.push(muted(s.note));
    body.replaceChildren.apply(body, kids);
  }
  function startSpeed() {
    if (!s || s.starting || (s.data.speed && s.data.speed.running)) return;
    var mine = s;
    mine.starting = true; mine.note = "";
    renderSpeed();
    HI.send(ID, "speedtest", "POST", {}).then(function () { }, function (e) {
      if (e && e.unauthorized) return;
      if (e && e.status !== 409) mine.note = e.message || "Could not start the test";
    }).then(function () {
      mine.starting = false;
      if (s === mine) loadSpeed();
    });
  }

  // ------------------------------------------------------------ loading

  function loadSpeed() {
    if (!s) return;
    clearTimeout(s.speedTimer);
    if (!HI.visible() || s.speedCtrl) return;
    var mine = s, ctrl = mine.speedCtrl = new AbortController(), timeout = setTimeout(function () { ctrl.abort(); }, 5000);
    HI.get(ID, "speedtest", ctrl.signal).then(function (v) { mine.data.speed = v; delete mine.err.speed; },
      function (e) { if (!e || e.name !== "AbortError" || HI.visible()) mine.err.speed = "unavailable"; })
      .then(function () {
        clearTimeout(timeout);
        mine.speedCtrl = null;
        if (s !== mine) return;
        renderSpeed();
        if (HI.visible() && mine.data.speed && mine.data.speed.running) mine.speedTimer = setTimeout(loadSpeed, 1000);
      });
  }
  function load() {
    if (!s) return;
    clearTimeout(s.timer);
    if (!HI.visible() || s.loading) return;
    var mine = s, keys = VIEW_LOADS[mine.view] || [];
    var ctrl = mine.ctrl = new AbortController(), timeout = setTimeout(function () { ctrl.abort(); }, 25000);
    if (SPEED_VIEWS[mine.view]) loadSpeed();
    mine.loading = true;
    Promise.all(keys.map(function (key) {
      return HI.get(ID, key, ctrl.signal).then(function (v) { mine.data[key] = v; mine.when[key] = Date.now(); delete mine.err[key]; },
        function (e) { if (e && e.unauthorized) throw e; if (e && e.name === "AbortError" && !HI.visible()) return; mine.err[key] = (e && e.message) || "unavailable"; });
    })).catch(function () { }).then(function () {
      clearTimeout(timeout);
      mine.loading = false; mine.ctrl = null; mine.at = Date.now();
      if (s !== mine) return;
      render();
      if (HI.visible()) mine.timer = setTimeout(load, REFRESH);
    });
  }
  function onVisible() {
    if (!s) return;
    if (document.hidden) {
      clearTimeout(s.timer); clearTimeout(s.speedTimer);
      return;
    }
    load();
  }
  function onToggle(e) {
    var d = e.target;
    if (d && d.getAttribute && d.getAttribute("data-hn")) open[d.getAttribute("data-hn")] = d.open;
  }

  // ------------------------------------------------------------ rendering

  function stamp() {
    if (!s.at) return null;
    var failed = Object.keys(s.err).length > 0;
    return h("span", { class: "pv-stamp" + (failed ? " stale" : ""), title: new Date(s.at).toLocaleString() + " · refreshes every minute" },
      [(failed ? "Some sources failed · " : "Updated ") + new Date(s.at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })]);
  }
  function render() {
    if (!s) return;
    var main = [], v = s.view;
    if (v === "overview") {
      main = main.concat(vpnSection(false));
      if (!report() || source("wan")) main.push(wanCard());
      main.push(qualityCard());
      main.push(routesCard(true));
    } else if (v === "vpn") {
      main = main.concat(vpnSection(true));
      main.push(qbitCard());
    } else if (v === "traffic") {
      main = trafficSection();
    } else if (v === "routes") {
      main.push(routesCard(true));
    } else if (v === "seeding") {
      main.push(qbitCard());
      main.push(seedingCard());
    }
    s.main.replaceChildren.apply(s.main, main);
    if (v === "overview" && s.ports) HI.ports(s.ports);
    var head = v === "overview" ? h("div") : HI.head(ID, v, stamp());
    s.head.replaceWith(head);
    s.head = head;
  }

  (window.HI_PAGES = window.HI_PAGES || {}).network = {
    show: function (root, ctx) {
      var prev = s;
      if (prev) { clearTimeout(prev.timer); clearTimeout(prev.speedTimer); if (prev.ctrl) prev.ctrl.abort(); if (prev.speedCtrl) prev.speedCtrl.abort(); }
      var view = VIEW_LOADS[ctx.view] ? ctx.view : "overview";
      // Answers carry over between this page's views; each view refreshes its own.
      s = { view: view, data: prev ? prev.data : {}, err: {}, when: prev ? prev.when : {}, at: 0, timer: 0, speedTimer: 0, ctrl: null, speedCtrl: null, loading: false, starting: false, note: "" };
      if (rootEl !== root) {
        if (rootEl) rootEl.removeEventListener("toggle", onToggle, true);
        rootEl = root;
        root.addEventListener("toggle", onToggle, true);
      }
      if (!prev) document.addEventListener("visibilitychange", onVisible);
      s.head = h("div");
      s.main = h("div", { class: "net" + (view === "traffic" || view === "seeding" ? " hn-one" : "") });
      s.ports = h("div", { class: "hn-ports" });
      var kids = [s.head];
      if (SPEED_VIEWS[view]) kids.push(speedCard());
      kids.push(s.main);
      if (view === "overview") kids.push(s.ports);
      root.replaceChildren(h("section", { id: "panel-network" }, kids));
      if (view === "overview") HI.ports(s.ports);
      render();
      load();
    },
    hide: function () {
      if (!s) return;
      clearTimeout(s.timer); clearTimeout(s.speedTimer);
      if (s.ctrl) s.ctrl.abort();
      if (s.speedCtrl) s.speedCtrl.abort();
      document.removeEventListener("visibilitychange", onVisible);
      if (rootEl) rootEl.removeEventListener("toggle", onToggle, true);
      rootEl = null;
      s = null;
    }
  };
})();

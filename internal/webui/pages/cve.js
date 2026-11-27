// Vulnerabilities: CVEs matched against every host's and container image's
// installed packages by the cve plugin (OSV advisories, CISA KEV, EPSS). Three
// views share one summary: Vulnerabilities (one row per CVE), Packages (what to
// upgrade, biggest risk first) and Hosts (each host and image's exposure).
// Filters live in the route, so Back and shared links keep them. The server
// filters, sorts and pages; this page only asks and draws.
(function () {
  "use strict";
  var ID = "cve", LIMIT = 50, POLL = 60000, POLL_SCAN = 4000;
  var SEV = ["critical", "high", "medium", "low", "unknown"];
  var SEV_LABEL = { critical: "Critical", high: "High", medium: "Medium", low: "Low", unknown: "Unknown" };
  var SEV_GLYPH = { critical: "◆", high: "▲", medium: "▲", low: "●", unknown: "○" };
  var TRIAGE = { accepted: "Accepted risk", not_affected: "Not affected", false_positive: "False positive" };
  var PHASE = { inventory: "Reading inventories", matching: "Matching packages", details: "Fetching advisories", enrichment: "Fetching KEV and EPSS" };
  var VULN_SORTS = [["priority", "Priority"], ["severity", "Severity"], ["epss", "Exploit probability"], ["published", "Newest"], ["hosts", "Most hosts"], ["id", "CVE ID"]];
  var PKG_SORTS = [["priority", "Priority"], ["vulns", "Most CVEs"], ["hosts", "Most hosts"], ["name", "Name"]];
  var CVSS = {
    AV: ["Attack vector", { N: "Network", A: "Adjacent", L: "Local", P: "Physical" }],
    AC: ["Complexity", { L: "Low", H: "High" }],
    PR: ["Privileges", { N: "None", L: "Low", H: "High" }],
    UI: ["User interaction", { N: "None", R: "Required" }],
    S: ["Scope", { U: "Unchanged", C: "Changed" }],
    C: ["Confidentiality", { H: "High", L: "Low", N: "None" }],
    I: ["Integrity", { H: "High", L: "Low", N: "None" }],
    A: ["Availability", { H: "High", L: "Low", N: "None" }]
  };

  // Survives switching views; dropped with the page's memory.
  var st = { status: null, overview: null, hosts: null, list: null, listKey: "", listErr: "", loading: false, statusErr: "", wasScanning: false };
  var root = null, view = "vulns", f = {}, live = false, gen = 0;
  var ui = null;            // { view, wrap, search }
  var timer = null, ctrl = null, listCtrl = null, listGen = 0, searchTimer = null, drawerCtrl = null;

  function HI() { return window.HI; }
  function h(tag, attrs, kids) { return HI().h(tag, attrs, kids); }
  function clear(el) { while (el && el.firstChild) el.removeChild(el.firstChild); return el; }
  function n(v) { return typeof v === "number" && isFinite(v) ? v : 0; }
  function fmt(v) { return n(v).toLocaleString(); }
  function plural(v, one, many) { return fmt(v) + " " + (n(v) === 1 ? one : many || one + "s"); }
  function skel(rows) { var d = h("div"); d.innerHTML = HI().skeleton(rows); return d.firstChild; }
  function ext(href, text, cls) { return h("a", { href: href, target: "_blank", rel: "noopener noreferrer", class: cls || null }, [text]); }
  function day(iso) {
    if (!iso) return "—";
    var d = new Date(iso);
    return isFinite(d.getTime()) ? d.toLocaleDateString([], { year: "numeric", month: "short", day: "numeric" }) : String(iso);
  }
  // Published age in the coarse units a CVE list needs.
  function age(iso) {
    var t = iso ? new Date(iso).getTime() : NaN;
    if (!isFinite(t)) return "—";
    var d = Math.max(0, (Date.now() - t) / 86400000);
    return d < 1 ? "today" : d < 45 ? Math.round(d) + " d" : d < 545 ? Math.round(d / 30.4) + " mo" : (d / 365.25).toFixed(1).replace(/\.0$/, "") + " y";
  }
  // EPSS percentile in words; the upper half reads as "top N%" so a 0.997
  // percentile never rounds to "higher than 100%".
  function rank(v) {
    if (typeof v !== "number") return "";
    return v >= 0.5 ? "in the top " + Math.max(1, Math.ceil(100 * (1 - v))) + "% of CVEs" : "higher than " + Math.floor(100 * v) + "% of CVEs";
  }
  function pct(v) {
    if (typeof v !== "number") return "—";
    v *= 100;
    return (v < 0.1 ? "<0.1" : v < 10 ? v.toFixed(1) : Math.round(v)) + "%";
  }
  function scopeName(r) {
    if (!r || r.scope === "host") return "Host packages";
    return r.image_ref || String(r.scope || "").replace(/^image:(sha256:)?/, "").slice(0, 12);
  }
  function distroName(d) {
    if (!d || !d.id) return "Unknown distro";
    var name = { debian: "Debian", ubuntu: "Ubuntu", alpine: "Alpine", rocky: "Rocky Linux", almalinux: "AlmaLinux" }[d.id] || d.id;
    return name + (d.version_id ? " " + d.version_id : "") + (d.codename ? " (" + d.codename + ")" : "");
  }

  // ------------------------------------------------------------ route state

  function readFilters(p) {
    var g = function (k) { return (p && p.get(k)) || ""; };
    var sev = g("sev").split(",").filter(function (s) { return SEV.indexOf(s) >= 0; });
    return { q: g("q"), sev: sev, kev: g("kev") === "1", fix: g("fix") === "1", host: g("host"), set: g("set"),
      package: g("package"), ecosystem: g("ecosystem"), sup: g("sup") === "1" || g("sup") === "all" ? g("sup") : "",
      sort: g("sort"), order: g("order") === "asc" ? "asc" : "", page: Math.max(0, parseInt(g("page"), 10) || 0) };
  }
  function encode(x) {
    var p = new URLSearchParams();
    if (x.q) p.set("q", x.q);
    if (x.sev && x.sev.length) p.set("sev", x.sev.join(","));
    if (x.kev) p.set("kev", "1");
    if (x.fix) p.set("fix", "1");
    ["host", "set", "package", "ecosystem", "sup", "sort", "order"].forEach(function (k) { if (x[k]) p.set(k, x[k]); });
    if (x.page) p.set("page", String(x.page));
    return p.toString();
  }
  // Any filter change returns to the first page.
  function nav(v, changes, keepPage) {
    var x = {}, k;
    for (k in f) x[k] = f[k];
    if (v !== view) x = readFilters(null);
    for (k in changes) x[k] = changes[k];
    if (!keepPage) x.page = changes.page || 0;
    HI().go(HI().link(ID, v, encode(x)));
  }
  function apiQuery() {
    var p = new URLSearchParams();
    if (f.q) p.set("q", f.q);
    if (f.sev.length) p.set("severity", f.sev.join(","));
    if (f.kev) p.set("kev", "1");
    if (f.fix) p.set("fix", "1");
    if (f.host) p.set("host", f.host);
    if (view === "vulns") {
      if (f.set) p.set("set", f.set);
      if (f.package) p.set("package", f.package);
      if (f.ecosystem) p.set("ecosystem", f.ecosystem);
      p.set("suppressed", f.sup || "0");
    }
    if (f.sort) p.set("sort", f.sort);
    if (f.order) p.set("order", f.order);
    p.set("offset", String(f.page * LIMIT));
    p.set("limit", String(LIMIT));
    return p.toString();
  }

  // ------------------------------------------------------------ loading

  function scanning() { return !!st.status && st.status.state === "scanning"; }

  function schedule() {
    clearTimeout(timer);
    if (!live) return;
    timer = setTimeout(function () {
      if (!live) return;
      if (!HI().visible()) { schedule(); return; }
      refresh(false);
    }, scanning() ? POLL_SCAN : POLL);
  }

  // Status and overview on every tick; the view's own data when asked or
  // when a scan has just finished.
  function refresh(withList) {
    var my = ++gen;
    if (ctrl) ctrl.abort();
    var c = ctrl = new AbortController();
    var jobs = [
      HI().get(ID, "status", c.signal).then(function (v) { st.status = v; st.statusErr = ""; }, function (e) { if (!c.signal.aborted) st.statusErr = e.message || "unavailable"; }),
      HI().get(ID, "overview", c.signal).then(function (v) { st.overview = v; }, function () {})
    ];
    return Promise.all(jobs).then(function () {
      if (my !== gen || !live) return;
      var finished = st.wasScanning && !scanning();
      st.wasScanning = scanning();
      paintHead(); paintSummary(); paintFilters();
      if (withList || finished) return loadList(true);
      schedule();
    });
  }

  function loadList(force) {
    var key = view + "?" + (view === "hosts" ? "" : apiQuery());
    if (!force && st.listKey === key && st.list) { paintBody(); schedule(); return Promise.resolve(); }
    if (listCtrl) listCtrl.abort();
    var my = ++listGen, c = listCtrl = new AbortController();
    st.loading = true; st.listErr = "";
    paintBody();
    var path = view === "hosts" ? "hosts" : (view === "packages" ? "packages?" : "vulns?") + apiQuery();
    var lists = [HI().get(ID, path, c.signal)];
    // The host filter needs the host list; it changes only with a scan.
    if (view !== "hosts" && (!st.hosts || force)) lists.push(HI().get(ID, "hosts", c.signal).then(function (v) { st.hosts = v; }, function () {}));
    return Promise.all(lists).then(function (r) {
      if (my !== listGen || !live) return;
      st.list = r[0]; st.listKey = key; st.loading = false;
      if (view === "hosts") st.hosts = r[0];
    }, function (e) {
      if (my !== listGen || !live || c.signal.aborted) return;
      st.loading = false; st.listErr = e.message || "Could not load";
    }).then(function () {
      if (my !== listGen || !live) return;
      paintFilters(); paintBody(); schedule();
    });
  }

  function scanNow(btn) {
    btn.disabled = true;
    HI().send(ID, "scan", "POST", {}).then(function () {
      st.status = st.status || {};
      st.status.state = "scanning"; st.wasScanning = true;
    }, function (e) {
      if (!/already running/i.test(e.message || "")) window.alert("Scan did not start: " + (e.message || "error"));
    }).then(function () { if (live) refresh(false); });
  }

  // ------------------------------------------------------------ head

  function scanLine() {
    var s = st.status, box = h("div", { class: "cve-scan" });
    if (!s) { box.appendChild(h("span", { class: "pv-stamp", text: st.statusErr ? "Status unavailable" : "" })); return box; }
    if (s.state === "scanning") {
      var p = s.progress, frac = p && p.total ? Math.min(1, p.done / p.total) : null;
      box.appendChild(h("div", { class: "cve-progress", role: "status" }, [
        h("span", { class: "cve-spin", "aria-hidden": "true" }),
        h("span", { text: (PHASE[s.phase] || "Scanning") + (p && p.total ? " · " + fmt(p.done) + " / " + fmt(p.total) : "…") }),
        frac != null ? h("span", { class: "cve-pbar", "aria-hidden": "true" }, [h("i", { style: "width:" + (frac * 100).toFixed(1) + "%" })]) : null]));
      return box;
    }
    var last = s.last_scan, stale = last && last.finished_at && Date.now() - new Date(last.finished_at).getTime() > 3 * 86400000;
    var text = last && last.finished_at ? "Scanned " + HI().ago(last.finished_at) + (s.next_scan_at ? " · next " + HI().fromNow(s.next_scan_at) : "") : "Never scanned";
    box.appendChild(h("span", { class: "pv-stamp" + (stale || (last && last.error) ? " stale" : ""), title: last && last.finished_at ? new Date(last.finished_at).toLocaleString() : null, text: text }));
    var b = h("button", { class: "btn", type: "button", onclick: function () { scanNow(b); } }, [HI().iconNode("refresh", "sm"), "Scan now"]);
    box.appendChild(b);
    return box;
  }

  function paintHead() {
    if (!ui) return;
    var head = ui.wrap.querySelector(".cve-headslot");
    head.replaceChildren(HI().head(ID, view, scanLine()));
  }

  // ------------------------------------------------------------ summary

  function sevBar(counts, total, clickable) {
    var bar = h("div", { class: "cve-sevbar", role: "img", "aria-label": SEV.map(function (s) { return n(counts[s]) + " " + SEV_LABEL[s].toLowerCase(); }).join(", ") });
    SEV.forEach(function (s) {
      if (n(counts[s]) > 0) bar.appendChild(h("i", { class: "s-" + s, style: "flex-grow:" + n(counts[s]), title: fmt(counts[s]) + " " + SEV_LABEL[s] }));
    });
    if (!total) bar.appendChild(h("i", { class: "s-none", style: "flex-grow:1" }));
    var legend = h("div", { class: "cve-legend" }, SEV.map(function (s) {
      var c = n(counts[s]), on = f.sev.indexOf(s) >= 0 && view === "vulns";
      var kids = [h("span", { class: "cve-dot s-" + s, "aria-hidden": "true" }), h("b", { text: fmt(c) }), " " + SEV_LABEL[s]];
      return clickable && c ? h("button", { type: "button", class: "cve-leg", "aria-pressed": on ? "true" : "false", onclick: function () { nav("vulns", { sev: on ? [] : [s], sup: "" }); } }, kids)
        : h("span", { class: "cve-leg" + (c ? "" : " zero") }, kids);
    }));
    return [bar, legend];
  }

  function tile(label, value, sub, tone, onclick) {
    var t = h(onclick ? "button" : "div", { class: "tile cve-tile" + (onclick ? " cve-click" : ""), type: onclick ? "button" : null, onclick: onclick || null }, [
      h("div", { class: "label", text: label }),
      h("div", { class: "value" + (tone ? " " + tone : ""), text: value }),
      sub ? h("div", { class: "sub", text: sub }) : null]);
    return t;
  }

  function paintSummary() {
    if (!ui) return;
    var slot = clear(ui.wrap.querySelector(".cve-summary")), notes = clear(ui.wrap.querySelector(".cve-notes"));
    var o = st.overview, s = st.status;
    if (s && s.last_scan && s.last_scan.error) notes.appendChild(HI().notice("crit", "The last scan failed: " + s.last_scan.error + ". The figures below are from the scan before it."));
    if (s && s.sources) {
      [["osv", "OSV"], ["kev", "CISA KEV"], ["epss", "EPSS"]].forEach(function (x) {
        var src = s.sources[x[0]];
        if (src && src.enabled !== false && src.error) notes.appendChild(HI().notice("warn", x[1] + " could not be refreshed: " + src.error + (src.updated_at ? ". Using data from " + HI().ago(src.updated_at) + "." : ".")));
      });
    }
    if (o && o.eol && o.eol.length) notes.appendChild(eolNotice(o.eol));
    if (!o || !o.generated_at) {
      if (s && s.state === "scanning") slot.appendChild(HI().stateBox("First scan running", "Matching every host's and image's packages against OSV. The first scan downloads several thousand advisories and can take a few minutes."));
      else if (s || st.statusErr) {
        var b = h("button", { class: "btn primary", type: "button", onclick: function () { scanNow(b); } }, ["Scan now"]);
        slot.appendChild(HI().stateBox(st.statusErr ? "Vulnerability data unavailable" : "No scan yet", st.statusErr || "Scans run on a schedule. Start the first one now; package names and versions are sent to OSV.dev.", st.statusErr ? null : b, !!st.statusErr));
      } else slot.appendChild(skel(2));
      return;
    }
    var t = o.totals || {}, sev = t.severity || {}, cov = (s && s.coverage) || {};
    var lead = h("div", { class: "tile cve-tile cve-lead" }, [
      h("div", { class: "label" }, ["Open vulnerabilities", t.suppressed ? h("span", { class: "sub cve-supp", text: fmt(t.suppressed) + " suppressed" }) : null]),
      h("div", { class: "value", text: fmt(t.vulns) })].concat(sevBar(sev, n(t.vulns), true)));
    var unsupported = (cov.unsupported || []).length, skipped = (cov.skipped || []).length;
    slot.appendChild(h("div", { class: "cve-kpis" }, [
      lead,
      tile("Known exploited", fmt(t.kev), t.kev ? "In CISA's KEV catalog" : "None in CISA's KEV catalog", t.kev ? "crit" : "ok", t.kev ? function () { nav("vulns", { kev: true, sev: [], sup: "" }); } : null),
      tile("Fix available", fmt(t.fixable), t.vulns ? Math.round(100 * n(t.fixable) / n(t.vulns)) + "% of open CVEs" : "", "", t.fixable ? function () { nav("vulns", { fix: true, sup: "" }); } : null),
      tile("Affected", plural(t.hosts_affected, "host"), plural(t.sets_affected, "package set") + " · " + plural(t.packages_affected, "package"), "", function () { nav("hosts", {}); }),
      tile("Coverage", cov.sets ? fmt(cov.sets_scanned) + " / " + fmt(cov.sets) : "—",
        unsupported || skipped ? [unsupported ? plural(unsupported, "set") : "", skipped ? plural(skipped, "package") : ""].filter(Boolean).join(" and ") + " not checked · details" : plural(cov.packages, "package") + " checked",
        unsupported || skipped ? "warn" : "", unsupported || skipped ? openCoverage : null)
    ]));
  }

  // Folded by default: one line saying how many sets are past end of life,
  // the explanation and the list only when opened. The summary re-renders on
  // every poll, so the open state lives outside the element.
  var eolOpen = false;
  function eolNotice(list) {
    var rows = list.slice().sort(function (a, b) { return (a.eol_date || "").localeCompare(b.eol_date || "") || a.host.localeCompare(b.host); });
    var box = h("details", { class: "banner t-warn cve-eol" }, [
      h("summary", {}, [HI().iconNode("shield", "sm"), h("span", { class: "sr-only", text: "Warning: " }),
        h("span", { class: "cve-eol-title", text: plural(list.length, "package set") + " run" + (list.length === 1 ? "s" : "") + " an end-of-life distro" }),
        h("span", { class: "cve-eol-more", text: "Details" }), HI().iconNode("chevron", "sm")]),
      h("div", { class: "cve-eol-body" }, [
        h("p", { text: "These distros no longer get security fixes, so new CVEs in them go unpatched and are often never listed; their counts below understate the risk. Rebuild or upgrade them onto a supported release." }),
        h("table", { class: "cve-mini cve-eol-list" }, [
          h("thead", {}, [h("tr", {}, [h("th", { text: "Host" }), h("th", { text: "Set" }), h("th", { text: "Distro" }), h("th", { text: "End of life" })])]),
          h("tbody", {}, rows.map(function (r) {
            var set = scopeName(r);
            return h("tr", {}, [h("td", { class: "c-host", text: r.host }),
              h("td", { class: "c-set", title: set, text: r.scope === "host" ? "Host packages" : set.replace(/@sha256:[0-9a-f]{12,}$/, "") }),
              h("td", { class: "c-distro", text: r.distro }),
              h("td", { class: "c-eol" }, [r.eol_date || "—", r.eol_date ? h("span", { class: "sub", text: " · " + HI().ago(r.eol_date) }) : null])]);
          }))])])]);
    box.open = eolOpen;
    box.addEventListener("toggle", function () { eolOpen = box.open; });
    return box;
  }

  function detailTable(columns, children) {
    var table = h("table", { class: "cve-mini", "data-list": columns }, children);
    HI().prepareListTable(table);
    return h("div", { class: "scroll" }, [table]);
  }

  function openCoverage() {
    var cov = (st.status && st.status.coverage) || {}, rows = cov.unsupported || [], skipped = cov.skipped || [];
    var kids = [h("h3", { text: "Coverage" }),
      h("div", { class: "subtitle", text: "Only distro packages (dpkg, apk, rpm) on Debian, Ubuntu, Alpine, Rocky and Alma are checked. Language packages inside images, static binaries and vendor kernels such as Proxmox's are not." })];
    if (rows.length) kids.push(section("Package sets not checked", [detailTable("0,-1", [
      h("thead", {}, [h("tr", {}, [h("th", { text: "Host" }), h("th", { text: "Set" }), h("th", { text: "Reason" })])]),
      h("tbody", {}, rows.map(function (r) {
        return h("tr", {}, [h("td", { class: "cve-wrap", text: r.host }), h("td", { class: "cve-wrap", text: scopeName(r) }), h("td", { class: "cve-wrap sub", text: r.reason || "" })]);
      }))])]));
    if (skipped.length) kids.push(section("Packages OSV did not answer for", [
      h("p", { class: "sub", text: "These were too large for OSV to return in time and were left out of this scan; their CVEs are not counted. The next scan tries again." }),
      detailTable("0,2", [
        h("thead", {}, [h("tr", {}, [h("th", { text: "Package" }), h("th", { text: "Version" }), h("th", { text: "Sets" })])]),
        h("tbody", {}, skipped.map(function (r) {
          return h("tr", {}, [h("td", { class: "cve-wrap" }, [h("b", { text: r.package }), h("div", { class: "sub", text: r.ecosystem })]), h("td", { class: "cve-wrap" }, [h("code", { text: r.version })]), h("td", { class: "num", text: fmt(r.sets) })]);
        }))])]));
    HI().openDrawer(h("div", { class: "cve-drawer" }, kids), "Coverage");
  }

  // ------------------------------------------------------------ filters

  function chipToggle(label, on, onclick, count, cls) {
    return h("button", { type: "button", class: "cve-chip" + (cls ? " " + cls : ""), "aria-pressed": on ? "true" : "false", onclick: onclick }, [
      label, count != null ? h("span", { class: "cve-count", text: fmt(count) }) : null]);
  }

  function select(label, value, options, onchange) {
    var s = h("select", { class: "sel", "aria-label": label });
    options.forEach(function (o) { var opt = h("option", { value: o[0], text: o[1] }); if (o[0] === value) opt.selected = true; s.appendChild(opt); });
    s.addEventListener("change", function () { onchange(s.value); });
    return s;
  }

  function hostOptions() {
    var seen = {}, out = [["", "All hosts"]];
    ((st.hosts && st.hosts.items) || []).forEach(function (r) { if (!seen[r.host]) { seen[r.host] = 1; out.push([r.host, r.host]); } });
    if (f.host && !seen[f.host]) out.push([f.host, f.host]);
    return out;
  }

  function searchBox(placeholder) {
    if (ui.search && ui.search.view === view) return ui.search.el;
    var input = h("input", { type: "search", placeholder: placeholder, "aria-label": placeholder, value: f.q, autocomplete: "off", spellcheck: "false" });
    input.addEventListener("input", function () {
      clearTimeout(searchTimer);
      searchTimer = setTimeout(function () { if (input.value.trim() !== f.q) nav(view, { q: input.value.trim() }); }, 400);
    });
    var el = h("label", { class: "field cve-search" }, [HI().iconNode("search", "sm"), input]);
    ui.search = { view: view, el: el, input: input };
    return el;
  }

  function paintFilters() {
    if (!ui) return;
    var slot = ui.wrap.querySelector(".cve-filters");
    if (view === "hosts" || !st.overview || !st.overview.generated_at) { clear(slot); return; }
    if (ui.search && ui.search.view === view && document.activeElement !== ui.search.input && ui.search.input.value.trim() !== f.q) ui.search.input.value = f.q;
    var facets = (view === "vulns" && st.list && st.list.facets) || null, sev = (facets && facets.severity) || {};
    var row1 = h("div", { class: "filters cve-row" }, [
      searchBox(view === "vulns" ? "CVE, package or description" : "Package name"),
      select("Host", f.host, hostOptions(), function (v) { nav(view, { host: v, set: "" }); }),
      select("Sort by", f.sort || "priority", view === "vulns" ? VULN_SORTS : PKG_SORTS, function (v) { nav(view, { sort: v === "priority" ? "" : v }); })
    ]);
    var chips = SEV.map(function (s) {
      var on = f.sev.indexOf(s) >= 0;
      return chipToggle(SEV_LABEL[s], on, function () {
        var next = f.sev.filter(function (x) { return x !== s; });
        if (!on) next.push(s);
        next.sort(function (a, b) { return SEV.indexOf(a) - SEV.indexOf(b); });
        nav(view, { sev: next });
      }, facets ? n(sev[s]) : null, "s-" + s);
    });
    var row2 = h("div", { class: "cve-row cve-chips", role: "group", "aria-label": "Filters" }, chips.concat([
      h("span", { class: "cve-sep", "aria-hidden": "true" }),
      chipToggle("Known exploited", f.kev, function () { nav(view, { kev: !f.kev }); }, facets ? n(facets.kev) : null, "kev"),
      chipToggle("Fix available", f.fix, function () { nav(view, { fix: !f.fix }); }, facets ? n(facets.fixable) : null),
      view === "vulns" ? select("Triage", f.sup, [["", "Open"], ["1", "Suppressed" + (facets && facets.suppressed ? " (" + fmt(facets.suppressed) + ")" : "")], ["all", "Open and suppressed"]], function (v) { nav(view, { sup: v }); }) : null
    ]));
    var scoped = [];
    if (view === "vulns" && f.package) scoped.push(["Package " + f.package + (f.ecosystem ? " · " + f.ecosystem : ""), { package: "", ecosystem: "" }]);
    if (view === "vulns" && f.set) scoped.push([f.set === "host" ? "Host packages only" : "Image " + setLabel(f.host, f.set), { set: "" }]);
    var row3 = scoped.length ? h("div", { class: "cve-row cve-scoped" }, scoped.map(function (x) {
      return h("button", { type: "button", class: "chip", title: "Remove filter", onclick: function () { nav(view, x[1]); } }, [x[0], HI().iconNode("x", "sm")]);
    })) : null;
    slot.replaceChildren(row1, row2);
    if (row3) slot.appendChild(row3);
  }
  function setLabel(host, scope) {
    var r = ((st.hosts && st.hosts.items) || []).filter(function (x) { return x.host === host && x.scope === scope; })[0];
    return r ? scopeName(r) : scope.replace(/^image:(sha256:)?/, "").slice(0, 12);
  }

  // ------------------------------------------------------------ cells

  function sevCell(r) {
    var s = SEV_LABEL[r.severity] ? r.severity : "unknown", score = r.cvss && typeof r.cvss.score === "number" ? r.cvss.score.toFixed(1) : "";
    return h("span", { class: "cve-sev s-" + s, title: r.cvss ? "CVSS " + (r.cvss.version || "") + " " + score + (r.severity_source && r.severity_source !== "cvss_v3" ? " · rated by " + r.severity_source : "") : r.severity_source && r.severity_source !== "none" ? "Rated by " + r.severity_source : "No score" }, [
      h("span", { class: "g", "aria-hidden": "true", text: SEV_GLYPH[s] }), h("span", { class: "w", text: SEV_LABEL[s] }), score ? h("span", { class: "sc", text: score }) : null]);
  }
  function exploitCell(r) {
    var kids = [];
    if (r.kev) kids.push(h("span", { class: "pill critical cve-kev", title: "CISA KEV since " + r.kev.date_added + (r.kev.ransomware ? " · used in ransomware" : "") }, [h("span", { class: "glyph", "aria-hidden": "true", text: "◆" }), r.kev.ransomware ? "Exploited · ransomware" : "Exploited"]));
    if (r.epss) {
      var p = n(r.epss.score);
      kids.push(h("span", { class: "cve-epss", title: "EPSS " + pct(p) + " chance of exploitation in the next 30 days · " + rank(r.epss.percentile) }, [
        h("span", { class: "cve-ebar", "aria-hidden": "true" }, [h("i", { class: p >= 0.1 ? "hot" : p >= 0.01 ? "warm" : "", style: "width:" + Math.max(4, Math.min(100, Math.sqrt(p) * 100)).toFixed(0) + "%" })]),
        h("span", { text: pct(p) })]));
    }
    return kids.length ? h("div", { class: "cve-exploit" }, kids) : h("span", { class: "sub", text: "—" });
  }
  function fixCell(fix) {
    return fix === "all" ? HI().pill("Fix available", "ok") : fix === "some" ? HI().pill("Partial fix", "warn") : h("span", { class: "sub", text: "No fix yet" });
  }
  function counts(c) {
    return h("span", { class: "cve-counts" }, SEV.slice(0, 4).map(function (s) {
      return h("span", { class: "s-" + s + (n(c && c[s]) ? "" : " zero"), title: fmt(c && c[s]) + " " + SEV_LABEL[s].toLowerCase(), text: fmt(c && c[s]) });
    }));
  }
  function pager(list) {
    var total = n(list.total), from = total ? n(list.offset) + 1 : 0, to = Math.min(total, n(list.offset) + (list.items || []).length);
    if (total <= LIMIT && !f.page) return h("div", { class: "cve-pager sub", text: plural(total, view === "packages" ? "package" : "vulnerability", view === "packages" ? "packages" : "vulnerabilities") });
    return h("div", { class: "cve-pager" }, [
      h("span", { class: "sub", text: fmt(from) + "–" + fmt(to) + " of " + fmt(total) }),
      h("button", { class: "btn", type: "button", disabled: f.page <= 0, onclick: function () { nav(view, { page: f.page - 1 }, true); } }, ["Previous"]),
      h("button", { class: "btn", type: "button", disabled: to >= total, onclick: function () { nav(view, { page: f.page + 1 }, true); } }, ["Next"])]);
  }
  function rowLink(tr, fn, label) {
    tr.className = (tr.className ? tr.className + " " : "") + "clickable";
    tr.tabIndex = 0;
    tr.setAttribute("aria-label", label);
    tr.addEventListener("click", function (e) { if (!e.target.closest("a,button")) fn(); });
    tr.addEventListener("keydown", function (e) { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); fn(); } });
    return tr;
  }

  // ------------------------------------------------------------ views

  function paintBody() {
    if (!ui) return;
    var slot = ui.wrap.querySelector(".cve-body"), o = st.overview;
    if (!o || !o.generated_at) { clear(slot); return; }
    if (!st.list || st.listKey.split("?")[0] !== view) {
      if (st.listErr) slot.replaceChildren(HI().stateBox("Could not load", st.listErr, retryButton(), true));
      else slot.replaceChildren(skel(8));
      return;
    }
    slot.setAttribute("aria-busy", String(st.loading));
    if (st.listErr) { slot.replaceChildren(HI().notice("crit", "Could not refresh: " + st.listErr), current()); return; }
    slot.replaceChildren(current());
  }
  function retryButton() { return h("button", { class: "btn", type: "button", onclick: function () { loadList(true); } }, ["Try again"]); }
  function current() { return view === "packages" ? packagesView() : view === "hosts" ? hostsView() : vulnsView(); }
  function filtered() { return !!(f.q || f.sev.length || f.kev || f.fix || f.host || f.set || f.package || f.sup); }

  function vulnsView() {
    var list = st.list, items = list.items || [];
    if (!items.length) return HI().stateBox(filtered() ? "No vulnerabilities match" : f.sup === "1" ? "Nothing suppressed" : "No open vulnerabilities", filtered() ? "Clear a filter to see more." : "Every scanned package is at a version with no known advisory.", filtered() ? h("button", { class: "btn", type: "button", onclick: function () { nav("vulns", readFilters(null)); } }, ["Clear filters"]) : null);
    var body = h("tbody", {}, items.map(function (r) {
      var pk = r.packages || [];
      return rowLink(h("tr", { class: r.suppressed ? "cve-suppressed" : null }, [
        h("td", { class: "c-sev" }, [sevCell(r)]),
        h("td", { class: "c-id" }, [h("div", { class: "cve-idline" }, [h("b", { class: "cve-id", text: r.id }), r.suppressed ? HI().pill(TRIAGE[r.suppressed.state] || "Suppressed", "muted") : null]),
          h("div", { class: "sub cve-summary-text", text: r.summary || "No description" })]),
        h("td", { class: "c-pkg" }, [h("span", { class: "cve-pk", text: pk.slice(0, 2).join(", ") }), pk.length > 2 ? h("span", { class: "sub", text: " +" + (pk.length - 2) }) : null]),
        h("td", { class: "c-exp" }, [exploitCell(r)]),
        h("td", { class: "c-fix" }, [fixCell(r.fix)]),
        h("td", { class: "c-aff num" }, [h("span", { text: plural(r.hosts, "host") }), h("div", { class: "sub", text: plural(r.sets, "set") })]),
        h("td", { class: "c-age num sub", title: r.published ? day(r.published) : null, text: age(r.published) })
      ]), function () { openVuln(r.id); }, r.id + ", " + SEV_LABEL[r.severity] + ", open details");
    }));
    return h("div", {}, [
      h("div", { class: "scroll" }, [h("table", { class: "cve-table cve-vulns" }, [
        h("thead", {}, [h("tr", {}, [h("th", { text: "Severity" }), h("th", { text: "Vulnerability" }), h("th", { text: "Package" }), h("th", { text: "Exploitation" }), h("th", { text: "Fix" }), h("th", { class: "num", text: "Affected" }), h("th", { class: "num", text: "Published" })])]),
        body])]),
      pager(list)]);
  }

  function packagesView() {
    var list = st.list, items = list.items || [];
    var head = h("p", { class: "sub cve-lede", text: "Each row is one source package in one distro release. Upgrading it to the version shown clears every CVE that has a fix; the list is ordered by the risk those upgrades remove." });
    if (!items.length) return h("div", {}, [head, HI().stateBox(filtered() ? "No packages match" : "No vulnerable packages", filtered() ? "Clear a filter to see more." : "")]);
    var body = h("tbody", {}, items.map(function (r) {
      var inst = r.installed || [];
      return rowLink(h("tr", {}, [
        h("td", { class: "c-name" }, [h("b", { text: r.name }), h("div", { class: "sub", text: r.ecosystem })]),
        h("td", { class: "c-ver" }, [h("code", { text: inst[0] || "—" }), inst.length > 1 ? h("span", { class: "sub", text: " +" + (inst.length - 1) }) : null]),
        h("td", { class: "c-counts" }, [counts(r.vulns)]),
        h("td", { class: "c-exp" }, [r.kev ? HI().pill(fmt(r.kev) + " exploited", "crit") : h("span", { class: "sub", text: "—" })]),
        h("td", { class: "c-up" }, r.fix_version ? [h("code", { class: "cve-up", text: "≥ " + r.fix_version }), h("div", { class: "sub", text: "clears " + plural(r.fixes, "CVE") })] : [h("span", { class: "sub", text: "No fixed version yet" })]),
        h("td", { class: "c-aff num" }, [h("span", { text: plural(r.hosts, "host") }), h("div", { class: "sub", text: plural(r.sets, "set") })])
      ]), function () { openPackage(r); }, r.name + " " + r.ecosystem + ", open details");
    }));
    return h("div", {}, [head,
      h("div", { class: "scroll" }, [h("table", { class: "cve-table cve-pkgs" }, [
        h("thead", {}, [h("tr", {}, [h("th", { text: "Package" }), h("th", { text: "Installed" }), h("th", { text: "Critical · High · Medium · Low" }), h("th", { text: "Exploited" }), h("th", { text: "Upgrade to" }), h("th", { class: "num", text: "Affected" })])]),
        body])]),
      pager(list)]);
  }

  function hostsView() {
    var items = (st.list && st.list.items) || [];
    if (!items.length) return HI().stateBox("No package inventories", "Hosts report packages when their agent collects software.");
    var groups = [], by = {};
    items.forEach(function (r) { if (!by[r.host]) { by[r.host] = []; groups.push(r.host); } by[r.host].push(r); });
    var rows = [];
    groups.forEach(function (host) {
      var sets = by[host], hostRow = sets.filter(function (r) { return r.scope === "host"; })[0];
      var sum = { critical: 0, high: 0, medium: 0, low: 0, unknown: 0 }, kev = 0, fix = 0, images = 0;
      sets.forEach(function (r) { SEV.forEach(function (s) { sum[s] += n(r.vulns && r.vulns[s]); }); kev += n(r.kev); fix += n(r.fixable); if (r.scope !== "host") images++; });
      rows.push(rowLink(h("tr", { class: "cve-hostrow" }, [
        h("td", { class: "c-asset" }, [h("div", { class: "cve-idline" }, [HI().iconNode("server", "sm"), h("b", { text: host }), hostRow && hostRow.eol ? HI().pill("End of life", "warn") : null]),
          h("div", { class: "sub", text: (hostRow ? distroName(hostRow.distro) : "No host packages") + (images ? " · " + plural(images, "image") : "") })]),
        h("td", { class: "c-counts" }, [counts(sum)]),
        h("td", { class: "c-exp num" }, [kev ? HI().pill(fmt(kev), "crit") : h("span", { class: "sub", text: "0" })]),
        h("td", { class: "c-fixn num", text: fmt(fix) }),
        h("td", { class: "c-pk num sub", text: hostRow ? fmt(hostRow.packages) : "" }),
        h("td", { class: "c-age num sub", text: hostRow ? HI().ago(hostRow.collected_at) : "" })
      ]), function () { nav("vulns", readFilters(new URLSearchParams("host=" + encodeURIComponent(host)))); }, host + ", show vulnerabilities"));
      sets.filter(function (r) { return r.scope !== "host"; }).forEach(function (r) { rows.push(setRow(r)); });
    });
    return h("div", {}, [
      h("div", { class: "scroll" }, [h("table", { class: "cve-table cve-hosts" }, [
        h("thead", {}, [h("tr", {}, [h("th", { text: "Host or image" }), h("th", { text: "Critical · High · Medium · Low" }), h("th", { class: "num", text: "Exploited" }), h("th", { class: "num", text: "Fixable" }), h("th", { class: "num", text: "Packages" }), h("th", { class: "num", text: "Collected" })])]),
        h("tbody", {}, rows)])])]);
  }
  function setRow(r) {
    var ok = r.status === "scanned", cont = r.containers || [];
    var tr = h("tr", { class: "cve-setrow" + (ok ? "" : " cve-off") }, [
      h("td", { class: "c-asset" }, [h("div", { class: "cve-idline" }, [HI().iconNode("box", "sm"), h("span", { class: "cve-img", text: scopeName(r) }), r.eol ? HI().pill("End of life", "warn") : null]),
        h("div", { class: "sub", text: ok ? distroName(r.distro) + (cont.length ? " · " + cont.slice(0, 3).join(", ") + (cont.length > 3 ? " +" + (cont.length - 3) : "") : "") : "Not covered: " + (r.reason || r.status) })]),
      h("td", { class: "c-counts" }, ok ? [counts(r.vulns)] : []),
      h("td", { class: "c-exp num" }, ok ? [r.kev ? HI().pill(fmt(r.kev), "crit") : h("span", { class: "sub", text: "0" })] : []),
      h("td", { class: "c-fixn num", text: ok ? fmt(r.fixable) : "" }),
      h("td", { class: "c-pk num sub", text: fmt(r.packages) }),
      h("td", { class: "c-age num sub" })]);
    return ok ? rowLink(tr, function () { nav("vulns", readFilters(new URLSearchParams("host=" + encodeURIComponent(r.host) + "&set=" + encodeURIComponent(r.scope)))); }, scopeName(r) + " on " + r.host + ", show vulnerabilities") : tr;
  }

  // ------------------------------------------------------------ CVE drawer

  function openVuln(id) {
    if (drawerCtrl) drawerCtrl.abort();
    var c = drawerCtrl = new AbortController();
    HI().openDrawer(h("div", { class: "cve-drawer" }, [h("h3", { class: "cve-id", text: id }), skel(6)]), id);
    HI().get(ID, "vulns/" + encodeURIComponent(id), c.signal).then(function (v) {
      if (drawerCtrl === c) HI().openDrawer(vulnDetail(v), id);
    }, function (e) {
      if (drawerCtrl === c && !c.signal.aborted) HI().openDrawer(h("div", { class: "cve-drawer" }, [h("h3", { class: "cve-id", text: id }), HI().stateBox("Could not load", e.message || "", null, true)]), id);
    });
  }

  function section(title, kids) { return h("section", { class: "cve-sec" }, [h("h4", { text: title })].concat(kids)); }

  function vulnDetail(v) {
    var osv = v.osv_ids || [], isCVE = /^CVE-\d{4}-\d+$/.test(v.id);
    var top = h("div", { class: "cve-dhead" }, [sevCell(v), fixCell(v.fix), v.suppressed ? HI().pill(TRIAGE[v.suppressed.state] || "Suppressed", "muted") : null]);
    var facts = h("dl", { class: "dl cve-facts" });
    function fact(k, val) { if (val) { facts.appendChild(h("dt", { text: k })); facts.appendChild(h("dd", {}, [val])); } }
    fact("CVSS", v.cvss ? h("span", {}, [h("b", { text: v.cvss.score != null ? v.cvss.score.toFixed(1) : "—" }), " ", h("code", { class: "cve-vec", text: v.cvss.vector || "" })]) : null);
    fact("Rated by", v.severity_source && v.severity_source !== "none" && v.severity_source !== "cvss_v3" ? v.severity_source : null);
    fact("EPSS", v.epss ? pct(v.epss.score) + " chance of exploitation in 30 days · " + rank(v.epss.percentile) : null);
    fact("Published", v.published ? day(v.published) : null);
    fact("Weakness", (v.cwe_ids || []).length ? (v.cwe_ids || []).join(", ") : null);
    fact("Also known as", (v.aliases || []).concat(osv).filter(function (x, i, a) { return x !== v.id && a.indexOf(x) === i; }).slice(0, 8).join(", ") || null);

    var kids = [h("h3", { class: "cve-id", text: v.id }), top];
    if (v.summary) kids.push(h("p", { class: "cve-lede-d", text: v.summary }));
    if (v.kev) kids.push(h("div", { class: "cve-kevbox", role: "note" }, [
      h("b", { text: "Known exploited" + (v.kev.ransomware ? " · used in ransomware" : "") }),
      h("div", { text: (v.kev.name ? v.kev.name + ". " : "") + "In CISA's catalog since " + day(v.kev.date_added) + (v.kev.due_date ? "; US federal agencies must patch by " + day(v.kev.due_date) + "." : ".") })]));
    kids.push(facts);
    if (v.cvss && /^CVSS:3/.test(v.cvss.vector || "")) kids.push(vector(v.cvss.vector));
    kids.push(remediation(v.affected || []));
    kids.push(section("Affected", [affectedTable(v.affected || [])]));
    if (v.details && v.details !== v.summary) kids.push(section("Description", [h("p", { class: "cve-details", text: v.details })]));
    kids.push(section("Triage", [triage(v)]));
    var links = [];
    if (isCVE) links.push(ext("https://nvd.nist.gov/vuln/detail/" + v.id, "NVD"));
    if (osv.some(function (x) { return /^DEBIAN-/.test(x); }) && isCVE) links.push(ext("https://security-tracker.debian.org/tracker/" + v.id, "Debian tracker"));
    if (osv.some(function (x) { return /^UBUNTU-/.test(x); }) && isCVE) links.push(ext("https://ubuntu.com/security/" + v.id, "Ubuntu CVE"));
    if (osv.some(function (x) { return /^ALPINE-/.test(x); }) && isCVE) links.push(ext("https://security.alpinelinux.org/vuln/" + v.id, "Alpine secdb"));
    osv.slice(0, 4).forEach(function (x) { links.push(ext("https://osv.dev/vulnerability/" + encodeURIComponent(x), "OSV " + x)); });
    if (v.kev && isCVE) links.push(ext("https://www.cisa.gov/known-exploited-vulnerabilities-catalog?search_api_fulltext=" + v.id, "CISA KEV"));
    var refs = (v.references || []).filter(function (r) { return /^https?:\/\//.test(r.url || ""); }).slice(0, 12);
    kids.push(section("Links", [h("div", { class: "cve-links" }, links)].concat(refs.length ? [h("details", { class: "cve-refs" }, [
      h("summary", { text: plural(refs.length, "reference") }),
      h("ul", {}, refs.map(function (r) { return h("li", {}, [r.type ? h("span", { class: "sub", text: r.type.toLowerCase() + " " }) : null, ext(r.url, r.url, "cve-url")]); }))])] : [])));
    return h("div", { class: "cve-drawer" }, kids);
  }

  function vector(vec) {
    var parts = {};
    String(vec).split("/").slice(1).forEach(function (p) { var kv = p.split(":"); parts[kv[0]] = kv[1]; });
    return h("div", { class: "cve-vector" }, Object.keys(CVSS).filter(function (k) { return parts[k]; }).map(function (k) {
      var val = CVSS[k][1][parts[k]] || parts[k], bad = (k === "AV" && parts[k] === "N") || ((k === "PR" || k === "UI") && parts[k] === "N") || (k === "AC" && parts[k] === "L") || ((k === "C" || k === "I" || k === "A") && parts[k] === "H") || (k === "S" && parts[k] === "C");
      return h("div", { class: "cve-vm" + (bad ? " bad" : "") }, [h("span", { class: "sub", text: CVSS[k][0] }), h("b", { text: val })]);
    }));
  }

  // One line per source package and distro: the version that fixes it, where.
  function remediation(aff) {
    var by = {}, order = [];
    aff.forEach(function (a) {
      var k = a.ecosystem + "\x00" + a.package;
      if (!by[k]) { by[k] = { eco: a.ecosystem, pkg: a.package, fixed: {}, hosts: {}, open: {}, need: 0 }; order.push(k); }
      var g = by[k], where = a.scope === "host" ? a.host : scopeName(a) + " on " + a.host;
      if (a.fixable) { g.need++; g.fixed[a.fixed] = 1; g.hosts[where] = 1; }
      else if (!a.fixed) g.open[where] = 1;
    });
    var lines = order.map(function (k) {
      var g = by[k], fixed = Object.keys(g.fixed), hosts = Object.keys(g.hosts), open = Object.keys(g.open);
      if (fixed.length) return h("li", { class: "ok" }, [HI().iconNode("check", "sm"), h("span", {}, ["Upgrade ", h("b", { text: g.pkg }), " (" + g.eco + ") to ", h("code", { text: fixed.join(" / ") }), " on " + hosts.slice(0, 4).join(", ") + (hosts.length > 4 ? " +" + (hosts.length - 4) : "")])]);
      if (open.length) return h("li", {}, [HI().iconNode("clock", "sm"), h("span", {}, [h("b", { text: g.pkg }), " (" + g.eco + "): no fixed version published yet for " + plural(open.length, "set")])]);
      return null;
    }).filter(Boolean);
    return lines.length ? section("Remediation", [h("ul", { class: "cve-fixes" }, lines)]) : null;
  }

  function affectedTable(aff) {
    if (!aff.length) return h("div", { class: "sub", text: "No affected packages in the last scan." });
    var rows = aff.slice().sort(function (a, b) { return a.host < b.host ? -1 : a.host > b.host ? 1 : (a.scope === "host" ? -1 : b.scope === "host" ? 1 : 0); });
    return detailTable("0,-1", [
      h("thead", {}, [h("tr", {}, [h("th", { text: "Where" }), h("th", { text: "Package" }), h("th", { text: "Installed" }), h("th", { text: "Fixed in" })])]),
      h("tbody", {}, rows.map(function (a) {
        var bins = (a.binaries || []).map(function (b) { return b.name; }).filter(function (x) { return x !== a.package; });
        return h("tr", {}, [
          h("td", { class: "cve-wrap" }, [h("b", { text: a.host }), h("div", { class: "sub", text: a.scope === "host" ? "Host" : scopeName(a) })]),
          h("td", { class: "cve-wrap" }, [h("span", { text: a.package }), bins.length ? h("div", { class: "sub", text: bins.slice(0, 4).join(", ") + (bins.length > 4 ? " +" + (bins.length - 4) : "") }) : null]),
          h("td", { class: "cve-wrap" }, [h("code", { text: a.installed || "—" })]),
          h("td", { class: "cve-wrap" }, a.fixed ? [h("code", { class: a.fixable ? "cve-fixv" : "", text: a.fixed })] : [h("span", { class: "sub", text: a.vendor_status ? a.vendor_status : "Not yet" })])]);
      }))]);
  }

  // Suppress with a reason (Dependency-Track's analysis states), or reopen.
  function triage(v) {
    var box = h("div", { class: "cve-triage" }), err = h("div", { class: "cve-err", role: "alert" });
    if (v.suppressed) {
      var s = v.suppressed;
      var reopen = h("button", { class: "btn", type: "button" }, ["Reopen"]);
      reopen.addEventListener("click", function () {
        reopen.disabled = true;
        HI().send(ID, "suppressions/" + encodeURIComponent(v.id), "DELETE").then(function () { changed(v.id); }, function (e) { reopen.disabled = false; err.textContent = e.message || "Could not reopen"; });
      });
      box.append(h("p", {}, [h("b", { text: TRIAGE[s.state] || s.state }), " · " + (s.by || "someone") + ", " + day(s.at) + (s.until ? " · until " + day(s.until) : "")]),
        h("blockquote", { class: "cve-reason", text: s.reason || "" }), reopen, err);
      return box;
    }
    var state = select("Decision", "accepted", Object.keys(TRIAGE).map(function (k) { return [k, TRIAGE[k]]; }), function () {});
    var reason = h("textarea", { class: "cve-text", rows: "3", maxlength: "500", placeholder: "Why: not reachable, mitigated by…, scheduled for…", "aria-label": "Reason" });
    var until = h("input", { type: "date", class: "sel", "aria-label": "Until (optional)", min: new Date().toISOString().slice(0, 10) });
    var save = h("button", { class: "btn primary", type: "button" }, ["Suppress"]);
    save.addEventListener("click", function () {
      var why = reason.value.trim();
      if (!why) { err.textContent = "Give a reason."; reason.focus(); return; }
      save.disabled = true; err.textContent = "";
      var body = { state: state.value, reason: why };
      if (until.value) body.until = until.value;
      HI().send(ID, "suppressions/" + encodeURIComponent(v.id), "PUT", body).then(function () { changed(v.id); }, function (e) { save.disabled = false; err.textContent = e.message || "Could not save"; });
    });
    box.append(h("p", { class: "sub", text: "Suppressed CVEs leave the totals and alerts. They stay listed under Triage → Suppressed, and come back after the date if you set one." }),
      h("div", { class: "cve-trow" }, [h("label", { class: "cve-lab" }, [h("span", { text: "Decision" }), state]), h("label", { class: "cve-lab" }, [h("span", { text: "Until (optional)" }), until])]),
      reason, h("div", { class: "cve-trow" }, [save]), err);
    return box;
  }
  function changed(id) {
    st.listKey = "";
    refresh(true);
    openVuln(id);
  }

  // ------------------------------------------------------------ package drawer

  function openPackage(r) {
    var sets = r.set_refs || [];
    var cves = h("ul", { class: "cve-toplist" }, (r.top || []).map(function (id) {
      return h("li", {}, [h("button", { type: "button", class: "cve-linkbtn", onclick: function () { openVuln(id); } }, [id])]);
    }));
    HI().openDrawer(h("div", { class: "cve-drawer" }, [
      h("h3", { text: r.name }),
      h("div", { class: "subtitle", text: r.ecosystem + " · " + plural(r.sets, "package set") + " on " + plural(r.hosts, "host") }),
      h("div", { class: "cve-dhead" }, [counts(r.vulns), r.kev ? HI().pill(fmt(r.kev) + " exploited", "crit") : null]),
      r.fix_version ? h("ul", { class: "cve-fixes" }, [h("li", { class: "ok" }, [HI().iconNode("check", "sm"), h("span", {}, ["Upgrade to ", h("code", { text: r.fix_version }), " or later to clear " + plural(r.fixes, "CVE") + "."])])])
        : h("p", { class: "sub", text: "None of its CVEs has a fixed version in " + r.ecosystem + " yet." }),
      section("Installed", [h("div", { class: "cve-vers" }, (r.installed || []).map(function (x) { return h("code", { text: x }); }))]),
      section("Where", [h("ul", { class: "cve-where" }, sets.map(function (s) {
        return h("li", {}, [h("b", { text: s.host }), h("span", { class: "sub", text: " · " + (s.scope === "host" ? "Host" : scopeName(s)) })]);
      }))]),
      (r.top || []).length ? section("Highest priority", [cves]) : null,
      h("div", { class: "cve-trow" }, [h("button", { class: "btn primary", type: "button", onclick: function () { HI().closeDrawer(); nav("vulns", { package: r.name, ecosystem: r.ecosystem, q: "", sev: [], kev: false, fix: false, host: "", set: "", sup: "" }); } }, ["All vulnerabilities in " + r.name])])
    ]), r.name);
  }

  // ------------------------------------------------------------ page

  function build() {
    var wrap = h("div", { class: "cve" }, [h("div", { class: "cve-headslot" }), h("div", { class: "cve-notes" }), h("div", { class: "cve-summary" }), h("div", { class: "cve-filters" }), h("div", { class: "cve-body" })]);
    ui = { view: view, wrap: wrap, search: null };
    root.replaceChildren(wrap);
  }

  (window.HI_PAGES = window.HI_PAGES || {}).cve = {
    show: function (el, ctx) {
      var next = ctx.view === "packages" || ctx.view === "hosts" ? ctx.view : "vulns";
      var sameView = ui && root === el && ui.view === next && el.contains(ui.wrap);
      root = el; view = next; f = readFilters(ctx.params); live = true;
      if (!sameView) build();
      paintHead(); paintSummary(); paintFilters(); paintBody();
      if (!st.status || ctx.first) refresh(true);
      else loadList(false);
    },
    hide: function () {
      live = false; gen++; listGen++;
      clearTimeout(timer); clearTimeout(searchTimer);
      if (ctrl) ctrl.abort();
      if (listCtrl) listCtrl.abort();
      if (drawerCtrl) drawerCtrl.abort();
      ctrl = null; listCtrl = null; drawerCtrl = null; ui = null; root = null;
    }
  };
})();

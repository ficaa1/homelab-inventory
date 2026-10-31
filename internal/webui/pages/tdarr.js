// Tdarr: pick Library titles and seasons, review the exact files, confirm the
// reviewed plan, and watch the queue. The server rechecks everything on
// confirm; this page only collects the choice and shows what came back.
(function () {
  "use strict";
  var ID = "tdarr", LIMIT = 16, POLL = 30000;

  // Survives switching sections; dropped with the page's own memory.
  var st = {
    items: null, ready: false, loading: false, libError: "",
    status: null, statusLoading: null, statusCtrl: null, statusTimer: null,
    cands: null, candError: "", candQuery: "",
    selected: {}, query: "", filter: "all", pc: false, busy: false,
    notice: "", unknown: false, epoch: 0, expiry: null
  };
  var root = null, view = "", live = false;

  function HI() { return window.HI; }
  function h(tag, attrs, kids) { return HI().h(tag, attrs, kids); }
  function sz(n) { return n ? HI().bytes(n) : "0 B"; }
  function gb(n) { return typeof n === "number" ? (n < 10 ? n.toFixed(1) : Math.round(n)) + " GB" : "—"; }
  function num(n) { return n == null ? "—" : String(n); }
  function key(i) { return i.kind + ":" + i.id; }
  function tv(i) { return i.kind === "sonarr"; }
  function base(p) { return String(p || "").split("/").pop(); }
  function skel(n) { var d = h("div"); d.innerHTML = HI().skeleton(n); return d.firstChild; }
  function clear(el) { while (el && el.firstChild) el.removeChild(el.firstChild); return el; }
  function byId(id) { return root && root.querySelector("#" + id); }
  function archiveBusy(s) { return !!s && (s.status === "active" || s.status === "failed" || s.status === "pending_approval"); }
  function locked() { return !st.ready || st.busy || st.unknown; }

  function total(except) {
    return Object.keys(st.selected).reduce(function (t, k) {
      if (k !== except) { var p = st.selected[k].plan; t.count += p.paths.length; t.bytes += p.total_bytes; }
      return t;
    }, { count: 0, bytes: 0 });
  }

  // ------------------------------------------------------------ status

  function schedule() {
    clearTimeout(st.statusTimer);
    if (!live || !HI().visible()) { stopStatus(); return; }
    // A running batch holds the plugin; polling then would only queue behind it.
    if (!st.statusLoading) st.statusTimer = setTimeout(loadStatus, POLL);
  }

  function loadStatus() {
    if (st.statusLoading) return st.statusLoading;
    if (st.busy) { schedule(); return Promise.resolve(); }
    var ctrl = st.statusCtrl = new AbortController(), late = false;
    var timer = setTimeout(function () { late = true; ctrl.abort(); }, 10000);
    var p = st.statusLoading = HI().get(ID, "status", ctrl.signal).then(function (v) { st.status = v; }, function (e) {
      if (ctrl.signal.aborted && !late) return;   // left the page; not a Tdarr fault
      st.status = { status: "unavailable", error: late ? "Tdarr did not answer within 10 s" : e.message };
    }).then(function () {
      clearTimeout(timer);
      if (st.statusLoading !== p) return;
      st.statusLoading = null; st.statusCtrl = null;
      if (live) { renderStatus(); schedule(); }
    });
    return p;
  }

  function stopStatus() {
    clearTimeout(st.statusTimer);
    if (st.statusCtrl) st.statusCtrl.abort();
    st.statusLoading = null; st.statusCtrl = null;
  }

  function nodeState(n) {
    return !n.online ? "Offline" : n.paused == null ? "Online · pause unknown" : n.paused ? "Paused" : "Online";
  }
  function armed(n) {
    var w = n.workers || {};
    return !n.online || w.transcode_gpu == null || w.transcode_cpu == null ? null : w.transcode_gpu + w.transcode_cpu;
  }

  function statusCard(full) {
    var v = st.status, card = h("section", { class: "pv-card tdp-status", "aria-label": "Tdarr queue" });
    if (!v) { card.appendChild(h("div", { class: "tdp-pad" }, [skel(2)])); return card; }
    var q = v.queue || {};
    card.appendChild(h("div", { class: "tdp-metrics" }, [
      metric(q.queued, "queued"), metric(q.processing, "active"), metric(q.error, "errors", q.error > 0 ? "t-warn" : ""),
      full && v.server_version ? h("span", { class: "tdp-ver", text: "Tdarr " + v.server_version }) : null
    ]));
    if (v.status !== "ok") card.appendChild(h("div", { class: "tdp-pad" }, [HI().notice("warn",
      (v.status === "not_configured" ? "Tdarr is not configured" : "Status unavailable") + (v.error ? " · " + v.error : ""))]));
    (v.nodes || []).forEach(function (n) {
      var tone = !n.online ? "off" : n.paused ? "paused" : "on", w = n.workers || {};
      var row = h("div", { class: "tdp-node" }, [
        h("div", { class: "tdp-node-line" }, [
          h("span", { class: "tdp-dot " + tone, "aria-hidden": "true" }),
          h("b", { text: n.name || "Unknown node" }),
          h("span", { class: "sub", text: nodeState(n) }),
          h("span", { class: "tdp-armed", text: num(armed(n)) + " armed" })
        ])
      ]);
      if (full && n.online) row.appendChild(h("div", { class: "tdp-limits sub", text:
        "Transcode " + num(w.transcode_gpu) + " GPU · " + num(w.transcode_cpu) + " CPU   Health check " + num(w.healthcheck_gpu) + " GPU · " + num(w.healthcheck_cpu) + " CPU" }));
      (n.active || []).forEach(function (a) {
        var pct = typeof a.pct === "number" && isFinite(a.pct) ? Math.max(0, Math.min(100, a.pct)) : null;
        row.appendChild(h("div", { class: "tdp-active" }, [
          h("span", { class: "tdp-file", title: a.file || "", text: base(a.file) || "Current file" }),
          h("b", { text: pct == null ? "—" : Math.round(pct) + "%" + (typeof a.fps === "number" && full ? " · " + Math.round(a.fps) + " fps" : "") })
        ]));
        if (pct != null) row.appendChild(bar(pct));
      });
      card.appendChild(row);
    });
    if (full && v.last_batch) {
      var b = v.last_batch;
      card.appendChild(h("div", { class: "tdp-node sub", text: "Last batch: " + (b.state || "unknown") +
        (b.outcome ? " · " + b.outcome : "") + (b.finished_at ? " · finished " + HI().ago(b.finished_at) : "") }));
    }
    return card;
  }
  function metric(n, label, cls) { return h("span", { class: "tdp-metric" }, [h("b", { class: cls || "", text: num(n) }), " " + label]); }
  function bar(pct) {
    var fill = h("i"); fill.style.width = pct.toFixed(2) + "%";
    return h("span", { class: "pv-track tdp-bar", "aria-hidden": "true" }, [fill]);
  }

  function renderStatus() {
    var el = byId("tdp-status");
    if (el) el.replaceWith(withId(statusCard(view === "status"), "tdp-status"));
  }
  function withId(el, id) { el.id = id; return el; }

  // ------------------------------------------------------------ library

  function loadLibrary() {
    if (st.busy || st.loading) return;
    st.loading = true; st.ready = false; st.libError = "";
    renderRows();
    HI().get(ID, "batch/candidates").then(function (v) {
      if (v.status !== "ok" || !Array.isArray(v.items)) throw new Error(v.error || "Library unavailable");
      st.items = v.items; st.ready = true;
    }).catch(function (e) { st.libError = e.message; }).then(function () {
      st.loading = false;
      if (live && view === "library") { renderRows(); }
    });
  }

  function resolve(sel) {
    return HI().get(ID, "batch/candidates?kind=" + encodeURIComponent(sel.kind) + "&id=" + encodeURIComponent(sel.id) +
      (sel.seasons ? "&seasons=" + sel.seasons.join(",") : "")).then(function (v) {
      if (v.status !== "ok" || !v.plan) throw new Error(v.error || "Files unavailable");
      return v;
    });
  }

  function message(s) {
    st.notice = s;
    var el = byId("tdp-notice"); if (!el) return;
    clear(el);
    el.classList.toggle("hidden", !s);
    if (!s) return;
    el.appendChild(HI().notice(st.unknown ? "warn" : "info", s));
    if (st.unknown) el.appendChild(h("button", { class: "btn", type: "button", text: "Check Tdarr", onclick: function () {
      loadStatus(); var s2 = byId("tdp-status"); if (s2) s2.scrollIntoView({ block: "center" });
    } }));
  }

  function libraryView() {
    var wrap = h("div", { class: "tdp-lib" });
    wrap.appendChild(withId(statusCard(false), "tdp-status"));
    var seg = h("div", { class: "seg", role: "group", "aria-label": "Media type" }, ["all", "radarr", "sonarr"].map(function (k, i) {
      return h("button", { class: "btn", type: "button", "aria-pressed": st.filter === k ? "true" : "false", text: ["All", "Movies", "TV"][i],
        onclick: function () { st.filter = k; seg.querySelectorAll(".btn").forEach(function (b, j) { b.setAttribute("aria-pressed", j === i ? "true" : "false"); }); renderRows(); } });
    }));
    var search = h("input", { type: "search", placeholder: "Search titles…", "aria-label": "Search Library titles", value: st.query,
      oninput: function () { st.query = search.value; renderRows(); } });
    wrap.appendChild(h("div", { class: "filters tdp-tools" }, [
      seg, h("label", { class: "field" }, [HI().iconNode("search", "sm"), search]),
      h("button", { class: "btn", type: "button", text: "Refresh", onclick: function () { loadLibrary(); loadStatus(); } })
    ]));
    wrap.appendChild(h("div", { id: "tdp-notice", class: "tdp-notice hidden", role: "status" }));
    var pc = h("input", { id: "tdp-pc", type: "checkbox", checked: st.pc, onchange: function () { st.pc = pc.checked; } });
    wrap.appendChild(h("div", { class: "tdp-selbar" }, [
      h("span", { id: "tdp-total", role: "status" }),
      h("label", { class: "tdp-check", title: "Also arm the secondary worker for this batch" }, [pc, "Secondary worker"]),
      h("button", { id: "tdp-preview", class: "btn primary", type: "button", text: "Preview", onclick: preview })
    ]));
    if (!HI().operatorEnabled()) wrap.appendChild(HI().notice("info", "Preview and queue need the operator credential, and this server has none (OPERATOR_TOKEN). Browsing works; batches do not."));
    wrap.appendChild(h("div", { id: "tdp-rows", class: "pv-card tdp-rows" }));
    return wrap;
  }

  function renderRows() {
    var el = byId("tdp-rows"); if (!el) return;
    clear(el);
    if (!st.items) {
      if (st.libError) el.appendChild(HI().stateBox("Library unavailable", st.libError, h("button", { class: "btn", type: "button", text: "Try again", onclick: loadLibrary }), true));
      else el.appendChild(h("div", { class: "tdp-pad" }, [skel(4)]));
      updateTotal(); return;
    }
    var q = st.query.trim().toLowerCase();
    var rows = st.items.filter(function (r) {
      var i = r.entry;
      return (st.filter === "all" || i.kind === st.filter) && (i.title + " " + (i.year || "")).toLowerCase().indexOf(q) >= 0;
    }).slice().sort(function (a, b) { return b.entry.sizeBytes - a.entry.sizeBytes || a.entry.title.localeCompare(b.entry.title); });
    var max = Math.max.apply(null, [1].concat(rows.map(function (r) { return r.entry.sizeBytes; })));
    el.appendChild(h("h3", {}, ["Library", h("span", { class: "n", text: rows.length + (rows.length === st.items.length ? "" : " of " + st.items.length) })]));
    if (!rows.length) el.appendChild(h("div", { class: "pv-empty", text: q ? "No titles match." : "No titles." }));
    rows.forEach(function (r) { el.appendChild(row(r, max)); });
    if (st.libError) message(st.libError);
    updateTotal();
  }

  function row(r, max) {
    var i = r.entry, k = key(i), blocked = r.blocked_reason || "", chosen = st.selected[k];
    var busy = archiveBusy(i.archiveStatus) || (i.seasons || []).some(function (s) { return archiveBusy(s.archiveStatus); });
    var box = h("input", { type: "checkbox", "aria-label": "Select " + i.title, checked: !!chosen, disabled: !!blocked || locked(),
      onchange: function () { select(k, box); } });
    var meta = [tv(i) ? "TV" : "Movie"];
    if (tv(i)) meta.push(((i.seasons || []).filter(function (s) { return s.season > 0; }).length) + " seasons");
    else meta.push([i.resolution ? i.resolution + "p" : "", i.videoCodec || ""].filter(Boolean).join(" · "));
    if (chosen) meta.push(chosen.plan.paths.length + (chosen.plan.paths.length === 1 ? " file" : " files") + " picked");
    var size = h("div", { class: "tdp-size" }, [h("strong", { text: sz(i.sizeBytes) }), bar(i.sizeBytes / max * 100)]);
    var end;
    if (blocked) end = h("span", { title: blocked }, [HI().pill("Protected", "info")]);
    else if (tv(i)) end = h("button", { class: "btn", type: "button", disabled: locked(), onclick: function () { seasons(k); },
      text: "Seasons" + (chosen ? " · " + chosen.selection.seasons.map(function (n) { return n || "S"; }).join(", ") : "") });
    else end = h("span");
    return h("div", { class: "tdp-row" + (blocked ? " tdp-locked" : "") + (chosen ? " tdp-on" : "") }, [
      h("label", { class: "tdp-title" }, [box, h("span", { class: "tdp-name" }, [
        h("span", { class: "tdp-line" }, [h("b", { text: i.title }), i.year ? h("span", { class: "tdp-year", text: String(i.year) }) : null,
          busy ? HI().pill("Archive busy", "warn") : null]),
        h("span", { class: "sub", text: meta.filter(Boolean).join(" · ") })
      ])]),
      size, end
    ]);
  }

  function updateTotal() {
    var t = total(), el = byId("tdp-total");
    if (el) { clear(el); el.appendChild(h("b", { class: t.count > LIMIT ? "t-warn" : "", text: t.count + "/" + LIMIT })); el.appendChild(document.createTextNode(" files")); el.appendChild(h("span", { class: "sub", text: " · " + sz(t.bytes) + " selected" })); }
    var p = byId("tdp-preview"); if (p) p.disabled = locked() || t.count === 0 || t.count > LIMIT || !HI().operatorEnabled();
    var pc = byId("tdp-pc"); if (pc) pc.disabled = st.busy || st.unknown;
  }

  function select(k, box) {
    if (locked()) { box.checked = !!st.selected[k]; return; }
    var r = st.items.filter(function (x) { return key(x.entry) === k; })[0];
    if (!r || r.blocked_reason) return;
    if (st.selected[k]) { delete st.selected[k]; renderRows(); return; }
    if (tv(r.entry)) { box.checked = false; seasons(k); return; }
    st.busy = true; renderRows(); message("");
    var s = { kind: r.entry.kind, id: r.entry.id };
    resolve(s).then(function (v) {
      if (v.error) throw new Error(v.error);
      if (!v.plan.paths.length) throw new Error((v.plan.files[0] || {}).skip_reason || "No eligible files");
      if (total().count + v.plan.paths.length > LIMIT || Object.keys(st.selected).length >= LIMIT) throw new Error(LIMIT + "-file limit");
      st.selected[k] = { selection: s, plan: v.plan };
    }).catch(function (e) { message(r.entry.title + ": " + e.message); }).then(function () { st.busy = false; if (live) renderRows(); });
  }

  // ------------------------------------------------------------ drawers

  function drawer(label, kids) {
    HI().openDrawer('<div class="tdp-drawer"></div>', label);
    var box = document.querySelector("#drawer-body .tdp-drawer");
    kids.forEach(function (c) { if (c) box.appendChild(c); });
    return box;
  }
  function drawerOpen(box) {
    var d = document.getElementById("drawer");
    return !!box && box.isConnected && !!d && !d.classList.contains("hidden") && !d.motionOut;
  }

  function seasons(k) {
    if (locked()) return;
    var r = st.items.filter(function (x) { return key(x.entry) === k; })[0];
    if (!r || r.blocked_reason) return;
    var i = r.entry, chosen = st.selected[k], epoch = ++st.epoch, draft = null, seq = 0;
    var msg = h("div", { class: "tdp-msg", role: "status" });
    var save = h("button", { class: "btn primary", type: "button", text: "Save", disabled: true, onclick: function () {
      if (draft) st.selected[k] = draft; else delete st.selected[k];
      HI().closeDrawer(); renderRows();
    } });
    var boxes = (i.seasons || []).map(function (s) {
      var c = h("input", { type: "checkbox", value: String(s.season), checked: !!chosen && chosen.selection.seasons.indexOf(s.season) >= 0, onchange: inspect });
      return h("label", { class: "tdp-season" + (archiveBusy(s.archiveStatus) ? " tdp-locked" : "") }, [c,
        h("span", { text: s.season ? "Season " + s.season : "Specials" }),
        archiveBusy(s.archiveStatus) ? HI().pill("Archive busy", "warn") : null,
        h("span", { class: "tdp-season-size", text: sz(s.sizeBytes) })]);
    });
    drawer("Seasons for " + i.title, [
      h("div", { class: "tdp-eyebrow", text: "Tdarr · seasons" }), h("h3", { text: i.title }),
      boxes.length ? h("fieldset", { class: "tdp-seasons" }, [h("legend", { class: "sr-only", text: "Seasons" })].concat(boxes)) : h("div", { class: "pv-empty", text: "No seasons with files." }),
      msg,
      h("div", { class: "actions tdp-actions" }, [h("button", { class: "btn", type: "button", text: "Cancel", onclick: HI().closeDrawer }), save])
    ]);
    function inspect() {
      var run = ++seq, picked = boxes.map(function (l) { return l.querySelector("input"); }).filter(function (c) { return c.checked; }).map(function (c) { return Number(c.value); });
      draft = null; save.disabled = true;
      if (!picked.length) { msg.textContent = chosen ? "0 files · saving clears this title" : "0 files"; save.disabled = false; return; }
      msg.textContent = "Checking files…";
      var s = { kind: i.kind, id: i.id, seasons: picked };
      resolve(s).then(function (v) {
        if (epoch !== st.epoch || run !== seq) return;
        var count = total(k).count + v.plan.paths.length, skipped = v.plan.files.length - v.plan.paths.length;
        msg.textContent = count + "/" + LIMIT + " files · " + sz(v.plan.total_bytes) + (skipped ? " · " + skipped + " skipped" : "") + (v.error ? " · " + v.error : "") +
          (!v.plan.paths.length && v.plan.files.length ? " · " + (v.plan.files[0].skip_reason || "none eligible") : "");
        draft = { selection: s, plan: v.plan };
        save.disabled = !!v.error || !v.plan.paths.length || count > LIMIT || (!chosen && Object.keys(st.selected).length >= LIMIT);
      }).catch(function (e) { if (epoch === st.epoch && run === seq) msg.textContent = e.message; });
    }
    inspect();
  }

  function fileList(plan) {
    return h("div", { class: "tdp-files" }, plan.files.map(function (f) {
      var facts = [sz(f.size_bytes), f.codec, f.class, f.est_save_gb ? "≈" + gb(f.est_save_gb) + " back" : "", f.season ? "S" + f.season : ""].filter(Boolean).join(" · ");
      return h("details", { class: "tdp-file" + (f.skip_reason ? " tdp-skipped" : "") }, [
        h("summary", {}, [h("b", { text: base(f.path) }), h("span", { class: "sub", text: facts }),
          f.skip_reason ? HI().pill("Skipped", "info") : null]),
        h("div", { class: "tdp-path", text: f.path }),
        f.skip_reason ? h("div", { class: "tdp-why", text: f.skip_reason }) : null
      ]);
    }));
  }

  function waiting() {
    var q = st.status && st.status.queue;
    return q ? (q.queued || 0) + (q.processing || 0) : 0;
  }

  function busy(on) { st.busy = on; if (live && view === "library") renderRows(); }

  function preview() {
    if (locked() || !total().count) return;
    var epoch = ++st.epoch;
    busy(true);
    var box = drawer("Tdarr batch review", [h("h3", { text: "Reviewing batch" }), skel(3)]);
    var body = { selection: Object.keys(st.selected).map(function (k) { return st.selected[k].selection; }), withPc: st.pc };
    HI().send(ID, "batch/preview", "POST", body, { operator: true }).then(function (v) {
      if (epoch !== st.epoch || !drawerOpen(box)) return;
      if (!v || !v.plan) throw new Error((v && v.error) || "Preview unavailable");
      review(v, epoch);
    }).catch(function (e) {
      if (epoch !== st.epoch) return;
      if (e.canceled) { HI().closeDrawer(); return; }
      drawer("Tdarr batch review", [h("h3", { text: "Preview unavailable" }), HI().notice("warn", e.message)]);
    }).then(function () { busy(false); });
  }

  function review(v, epoch) {
    var n = v.plan.paths.length, ok = !!v.hash && !!v.confirmation_token && !v.error && n > 0 && n <= LIMIT;
    var msg = h("div", { class: "tdp-msg", role: "status", text: v.error || (n ? "" : "Nothing in this selection is eligible.") });
    var queue = h("button", { class: "btn primary", type: "button", text: "Queue " + n + (n === 1 ? " file" : " files"), disabled: !ok, onclick: run });
    var skipped = v.plan.files.length - n;
    var box = drawer("Tdarr batch review", [
      h("div", { class: "tdp-eyebrow", text: "Tdarr · secondary worker " + (v.plan.withPc ? "on" : "off") }),
      h("h3", { text: n + (n === 1 ? " file · " : " files · ") + sz(v.plan.total_bytes) }),
      h("div", { class: "sub", text: (skipped ? skipped + " skipped · " : "") + "Confirmation expires " + HI().fromNow(v.expires_at) + " and works once." }),
      waiting() ? HI().notice("warn", "Tdarr already has " + waiting() + " queued or active; the server refuses a batch until that queue is empty.") : null,
      fileList(v.plan), msg,
      h("div", { class: "actions tdp-actions" }, [h("button", { class: "btn", type: "button", text: "Cancel", onclick: HI().closeDrawer }), queue]),
      v.hash ? h("div", { class: "tdp-hash", text: "Plan " + v.hash.slice(0, 12) }) : null
    ]);
    function expired() { var t = Date.parse(v.expires_at); return !isFinite(t) || Date.now() >= t; }
    function again(text) {
      clearTimeout(st.expiry); msg.textContent = text;
      queue.textContent = "Preview again"; queue.disabled = false;
      queue.removeEventListener("click", run); queue.addEventListener("click", preview);
    }
    clearTimeout(st.expiry);
    if (v.confirmation_token) st.expiry = setTimeout(function () { if (epoch === st.epoch && drawerOpen(box)) again("Preview expired"); }, Math.max(0, Date.parse(v.expires_at) - Date.now()));
    function run() {
      if (st.busy || epoch !== st.epoch || !drawerOpen(box)) return;
      if (expired()) { again("Preview expired"); return; }
      clearTimeout(st.expiry); busy(true);
      queue.disabled = true; queue.textContent = "Queueing…";
      HI().send(ID, "batch/execute", "POST", { plan: v.plan, hash: v.hash, confirmation_token: v.confirmation_token, confirm: true }, { operator: true }).then(function (r) {
        st.selected = {}; st.notice = "Queued · " + num(r.queued_files) + " files";
      }, function (e) {
        if (e.canceled) { st.notice = ""; return "canceled"; }
        if (e.status === 409) return "again";
        if (e.status === 500) st.notice = e.message || "Queue failed · preview again";
        else if (e.status && e.status !== 502 && (e.status < 500 || e.status === 503)) st.notice = e.message || "Batch unavailable";
        else { st.unknown = true; st.notice = "Outcome unknown · inspect the sidecar and Tdarr before another batch"; }
      }).then(function (outcome) {
        st.busy = false;
        if (outcome === "canceled") { queue.disabled = false; queue.textContent = "Queue " + n + (n === 1 ? " file" : " files"); return; }
        if (outcome === "again") { if (epoch === st.epoch && drawerOpen(box)) again("Plan changed or expired"); return; }
        if (epoch === st.epoch) drawer("Tdarr batch", [h("h3", { text: st.notice }),
          h("div", { class: "actions tdp-actions" }, [h("button", { class: "btn", type: "button", text: "Close", onclick: HI().closeDrawer })])]);
        if (live) { render(); loadStatus(); }
      });
    }
  }

  // ------------------------------------------------------------ candidates

  function loadCands() {
    st.candError = "";
    HI().get(ID, "candidates").then(function (v) {
      if (v.status !== "ok" || !Array.isArray(v.items)) throw new Error(v.error || "Candidates unavailable");
      st.cands = v.items;
    }).catch(function (e) { st.candError = e.message; }).then(function () { if (live && view === "candidates") renderCands(); });
  }

  function candidatesView() {
    var wrap = h("div", { class: "tdp-cands" });
    var search = h("input", { type: "search", placeholder: "Search files…", "aria-label": "Search candidate files", value: st.candQuery,
      oninput: function () { st.candQuery = search.value; renderCands(); } });
    wrap.appendChild(h("div", { class: "filters tdp-tools" }, [h("label", { class: "field" }, [HI().iconNode("search", "sm"), search]),
      h("button", { class: "btn", type: "button", text: "Refresh", onclick: function () { st.cands = null; renderCands(); loadCands(); } })]));
    wrap.appendChild(h("div", { id: "tdp-cand-rows", class: "pv-card tdp-rows" }));
    return wrap;
  }

  var KIND = { video: ["Re-encode", "info"], bloat: ["Bloated HEVC", "warn"], "audio-only": ["Audio only", "info"] };
  function renderCands() {
    var el = byId("tdp-cand-rows"); if (!el) return;
    clear(el);
    if (!st.cands) {
      if (st.candError) el.appendChild(HI().stateBox("Candidates unavailable", st.candError, null, true));
      else el.appendChild(h("div", { class: "tdp-pad" }, [skel(4)]));
      return;
    }
    var q = st.candQuery.trim().toLowerCase();
    var rows = st.cands.filter(function (c) { return c.path.toLowerCase().indexOf(q) >= 0; });
    var save = rows.reduce(function (t, c) { return t + (c.est_save_gb || 0); }, 0);
    el.appendChild(h("h3", {}, ["Worth compressing", h("span", { class: "n", text: rows.length + " files · ≈" + gb(save) + " back" })]));
    if (!rows.length) el.appendChild(h("div", { class: "pv-empty", text: q ? "No files match." : "Nothing worth compressing." }));
    var max = Math.max.apply(null, [1].concat(rows.map(function (c) { return c.est_save_gb || 0; })));
    rows.forEach(function (c) {
      var k = KIND[c.kind] || [c.kind, "info"], dir = c.path.split("/").slice(-2, -1)[0] || "";
      el.appendChild(h("div", { class: "tdp-row tdp-cand" }, [
        h("div", { class: "tdp-name" }, [
          h("span", { class: "tdp-line" }, [h("b", { class: "tdp-file", title: c.path, text: base(c.path) }), HI().pill(k[0], k[1])]),
          h("span", { class: "sub", text: [dir, c.codec, c.resolution, c.reason].filter(Boolean).join(" · ") })
        ]),
        h("div", { class: "tdp-size" }, [h("strong", { text: "≈" + gb(c.est_save_gb) }), bar((c.est_save_gb || 0) / max * 100)]),
        h("span", { class: "sub tdp-of", text: "of " + gb(c.size_gb) })
      ]));
    });
  }

  // ------------------------------------------------------------ page

  function render() {
    if (!root) return;
    clear(root);
    var page = h("div", { class: "tdp" });
    root.appendChild(page);
    var refresh = view === "status" ? h("button", { class: "btn tdp-refresh", type: "button", text: "Refresh", onclick: loadStatus }) : null;
    page.appendChild(HI().head(ID, view, refresh));
    if (view === "status") {
      page.appendChild(withId(statusCard(true), "tdp-status"));
    } else if (view === "candidates") {
      page.appendChild(candidatesView());
      renderCands();
    } else {
      page.appendChild(libraryView());
      renderRows(); message(st.notice);
    }
  }

  (window.HI_PAGES = window.HI_PAGES || {}).tdarr = {
    show: function (el, ctx) {
      root = el; view = ctx.view || "library"; live = true;
      render();
      loadStatus();
      if (view === "library" && !st.items && !st.loading) loadLibrary();
      if (view === "candidates" && !st.cands) loadCands();
      document.removeEventListener("visibilitychange", onVisible);
      document.addEventListener("visibilitychange", onVisible);
    },
    hide: function () {
      live = false; st.epoch++;
      stopStatus(); clearTimeout(st.expiry);
      document.removeEventListener("visibilitychange", onVisible);
      root = null;
    }
  };
  function onVisible() { if (HI().visible() && !st.statusLoading) loadStatus(); else schedule(); }
})();

// Taste: rounds of recommendations to judge, the "Seen these?" wall of likely
// classics, everything the model believes a household has watched (each
// correctable), the Letterboxd watchlist, and import and sync status. Verdicts
// save on tap and the server rebuilds the model in the background.
(function () {
  "use strict";

  var ID = "taste";
  var PICK = [["seen", "Seen"], ["loved", "Loved"], ["liked", "Liked"], ["meh", "Meh"], ["pass", "Pass"]];
  var MORE = [["loved", "Loved"], ["liked", "Liked"], ["meh", "Meh"]];
  var ALL = [["loved", "Loved"], ["liked", "Liked"], ["seen", "Seen"], ["meh", "Meh"], ["disliked", "Disliked"], ["pass", "Pass"], ["ignore", "Not mine"]];
  var ADD = ALL.slice(0, 5);
  var HINT = { seen: "Seen it, no strong feelings", loved: "Seen it and loved it", liked: "Seen it and liked it", meh: "Seen it, didn't do much for me", disliked: "Seen it and disliked it", pass: "Not seen, not interested", ignore: "Watched for someone else; not my taste" };
  var SOURCE = { letterboxd: "Letterboxd", plex: "Plex", you: "You" };
  var EFFECT = { "strong like": "ok", like: "ok", dislike: "warn", "strong dislike": "crit" };
  var WHERE = { available: "In Plex", partial: "Partly in Plex", requested: "Requested" };
  var SYNC_WORD = { ok: "ok", partial: "partial", blocked: "blocked", invalid: "unreadable", pending: "not yet" };
  var SYNC_PARTS = [["activity", "Diary"], ["watchlist", "Watchlist"], ["ratings", "Ratings"], ["likes", "Likes"]];

  var HI, root = null, ctl = null, timer = 0;
  var urls = {}, posterQueue = [], posterActive = 0;
  // Page state survives view switches; a profile change resets the lists.
  var s = fresh();
  s.profiles = null; s.profile = ""; s.kind = "movie";

  function fresh() {
    return { pool: null, round: [], roundNo: 0, skipped: {}, mine: {}, known: null, filter: "all", q: "", searchQ: "",
      results: null, searching: false, status: null, error: "", familiar: null, wall: [], open: "", watchlist: null, imp: null, flash: null, busy: "" };
  }
  function keep(next) { next.profiles = s.profiles; next.profile = s.profile; next.kind = s.kind; next.view = s.view; return next; }

  function h(tag, attrs, kids) { return HI.h(tag, attrs, kids); }
  function tkey(t) { return t.kind + ":" + t.id; }
  function label(v) { var x = ALL.filter(function (a) { return a[0] === v; })[0]; return x ? x[1] : v; }
  function meta(t, extra) {
    return [t.year, t.kind === "tv" ? "Series" : "Film"].concat(extra || []).filter(Boolean).join(" · ");
  }
  function q(path) { return path + (path.indexOf("?") < 0 ? "?" : "&") + "profile=" + encodeURIComponent(s.profile); }

  // ---------------------------------------------------------------- posters

  function posterOf(t) {
    var f = t && t.poster ? String(t.poster).split("/").pop() : "";
    return /^[A-Za-z0-9]{10,64}\.(jpg|png)$/.test(f) ? f : "";
  }
  function posterNode(t, cls) {
    var f = posterOf(t), box = h("span", { class: "tposter" + (cls ? " " + cls : ""), "aria-hidden": "true" });
    var letter = h("span", { class: "tposter-none", text: (t.title || "?").slice(0, 1) });
    if (!f || urls[f] === null) { box.appendChild(letter); return box; }
    var img = h("img", { alt: "", decoding: "async" });
    if (urls[f]) img.src = urls[f];
    else { box.appendChild(letter); posterQueue.push({ f: f, img: img, box: box }); }
    if (urls[f]) box.appendChild(img);
    return box;
  }
  // Four at a time: the server answers a plugin's requests one by one, and a
  // verdict should not wait behind a wall of posters. Nodes wanting a poster
  // that is already on its way wait in parked.
  var pending = {}, parked = [];
  function pumpPosters() {
    while (posterActive < 4 && posterQueue.length && ctl) {
      var job = posterQueue.shift();
      if (!job.box.isConnected) continue;
      if (urls[job.f] !== undefined) { place(job); continue; }
      if (pending[job.f]) { parked.push(job); continue; }
      pending[job.f] = true; posterActive++;
      (function (job, signal) {
        HI.image(ID, "poster/" + job.f, signal).then(function (u) {
          if (signal.aborted) { URL.revokeObjectURL(u); return; }
          urls[job.f] = u;
        }, function () { if (!signal.aborted) urls[job.f] = null; }).then(function () {
          if (signal.aborted) return;
          posterActive--; delete pending[job.f];
          place(job);
          parked = parked.filter(function (j) { if (j.f !== job.f) return true; place(j); return false; });
          pumpPosters();
        });
      })(job, ctl.signal);
    }
  }
  function place(job) {
    if (!urls[job.f] || !job.box.isConnected) return;
    job.img.src = urls[job.f];
    job.box.replaceChildren(job.img);
  }

  // ---------------------------------------------------------------- data

  function load() {
    if (!root) return;
    var signal = ctl.signal;
    var fail = function (e) { if (signal.aborted) return; s.error = (e && e.message) || "failed"; render(); };
    if (!s.profiles) {
      HI.get(ID, "profiles", signal).then(function (d) {
        if (signal.aborted) return;
        s.profiles = (d && d.profiles) || [];
        var ids = s.profiles.map(function (p) { return p.id; });
        if (ids.indexOf(s.profile) < 0) s.profile = ids[0] || "";
        if (s.profile) load(); else render();
      }, fail);
      render();
      return;
    }
    s.error = "";
    if (s.view === "familiar") {
      HI.get(ID, q("familiar?kind=" + s.kind), signal).then(function (d) {
        if (signal.aborted) return;
        s.familiar = d.titles || []; s.status = d;
        if (!s.wall.length) nextWall();
        render();
      }, fail);
    } else if (s.view === "known") {
      HI.get(ID, q("known"), signal).then(function (d) { if (signal.aborted) return; s.known = d.titles || []; s.status = d; render(); }, fail);
    } else if (s.view === "watchlist") {
      HI.get(ID, q("watchlist"), signal).then(function (d) { if (signal.aborted) return; s.watchlist = d; render(); }, fail);
    } else if (s.view === "import") {
      HI.get(ID, q("import"), signal).then(function (d) { if (signal.aborted) return; s.imp = d; s.status = d; render(); }, fail);
    } else {
      HI.get(ID, q("picks?kind=" + s.kind), signal).then(function (d) {
        if (signal.aborted) return;
        s.pool = d.picks || []; s.status = d;
        if (!s.round.length) nextRound();
        render();
      }, fail);
    }
    render();
  }

  // A round is the next ten picks not yet answered or passed over.
  function nextRound() {
    s.round.forEach(function (t) { if (!s.mine[tkey(t)]) s.skipped[tkey(t)] = true; });
    s.round = (s.pool || []).filter(function (t) { return !s.skipped[tkey(t)] && !s.mine[tkey(t)]; }).slice(0, 10);
    s.roundNo++;
  }
  // The wall shows 24 likely-seen titles at a time; ones left untapped are
  // taken as not seen and stay in the picks.
  function nextWall() {
    s.wall.forEach(function (t) { if (!s.mine[tkey(t)]) s.skipped[tkey(t)] = true; });
    s.wall = (s.familiar || []).filter(function (t) { return !s.skipped[tkey(t)] && !s.mine[tkey(t)]; }).slice(0, 24);
    s.open = "";
  }

  function find(k) {
    var lists = [s.round, s.wall, s.pool || [], s.known || [], s.results || [], (s.watchlist && s.watchlist.titles) || []];
    for (var i = 0; i < lists.length; i++) for (var j = 0; j < lists[i].length; j++) if (tkey(lists[i][j]) === k) return lists[i][j];
    return null;
  }

  function setVerdict(k, verdict, toggle) {
    var t = find(k);
    if (!t) return;
    var before = s.mine[k];
    var current = before !== undefined ? before : (t.verdict || "");
    if (toggle && verdict && verdict === current) verdict = "";
    s.mine[k] = verdict;
    s.flash = null;
    render();
    var profile = s.profile;
    HI.send(ID, "verdict", "POST", { profile: profile, kind: t.kind, id: t.id, verdict: verdict, title: t.title, year: t.year || "" })
      .then(function () { if (profile !== s.profile) return; if (s.status) s.status.updating = true; render(); })
      .catch(function (e) {
        if (profile !== s.profile) return;
        if (before === undefined) delete s.mine[k]; else s.mine[k] = before;
        s.flash = ["crit", "Not saved: " + ((e && e.message) || "failed")];
        render();
      });
  }

  function post(path, busy, done) {
    s.busy = busy; s.flash = null; render();
    HI.send(ID, path, "POST", { profile: s.profile }).then(function () {
      s.busy = ""; s.flash = ["ok", done]; load();
    }, function (e) { s.busy = ""; s.flash = ["crit", (e && e.message) || "failed"]; render(); });
  }

  // ---------------------------------------------------------------- render

  function seg(labelText, items, current, pick, cls) {
    return h("div", { class: "seg" + (cls ? " " + cls : ""), role: "group", "aria-label": labelText }, items.map(function (v) {
      return h("button", { class: "btn", type: "button", "aria-pressed": current === v[0] ? "true" : "false",
        onclick: function () { pick(v[0]); } }, v.slice(1));
    }));
  }

  function verdictButtons(t, set, current) {
    return h("div", { class: "taste-verdicts", role: "group", "aria-label": "Your verdict on " + t.title }, set.map(function (v) {
      return h("button", { class: "btn", type: "button", title: HINT[v[0]], "aria-pressed": current === v[0] ? "true" : "false",
        onclick: function (e) { e.preventDefault(); setVerdict(tkey(t), v[0], true); } }, [v[1]]);
    }));
  }

  function statusLine() {
    var st = s.status || {}, parts = [];
    var answered = Object.keys(s.mine).filter(function (k) { return s.mine[k]; }).length;
    if (s.view === "picks") parts.push("Round " + Math.max(1, s.roundNo));
    if (answered) parts.push(answered + " answered this visit");
    parts.push(st.updating ? "updating your taste…" : st.built_at ? "taste updated " + HI.ago(st.built_at) : (st.model && st.model.built_at) ? "taste updated " + HI.ago(st.model.built_at) : "taste not built yet");
    return h("div", { class: "sub taste-status", role: "status", text: parts.join(" · ") });
  }

  function loading() {
    return h("div", { class: "skel", role: "status", "aria-label": "Loading" }, [0, 1, 2].map(function () { return h("span", { class: "sk bar" }); }));
  }
  function empty(title, detail) {
    return h("div", { class: "empty" }, [h("div", { class: "big", text: title }), detail || null]);
  }
  function failed(what) { return empty(what + " did not load.", s.error); }

  function renderPick() {
    if (s.error) return failed("Recommendations");
    if (!s.pool) return loading();
    if (!s.round.length) return empty("Nothing left to judge right now.", "Your answers are rebuilding the list; check back in a minute for a fresh round.");
    return h("div", {}, [
      h("p", { class: "subtitle" }, ["Tap what you have already seen, and how it went. ", h("b", { text: "Pass" }), " means not seen and not interested. Leave a title alone to keep it."]),
      h("div", { class: "taste-grid" }, s.round.map(function (t) {
        var v = s.mine[tkey(t)] || "";
        return h("article", { class: "card taste-card" + (v ? " judged" : "") }, [
          posterNode(t),
          h("div", { class: "taste-body" }, [
            h("h3", { text: t.title }),
            h("div", { class: "sub", text: meta(t, [t.rating ? "TMDB " + Number(t.rating).toFixed(1) : "", WHERE[t.availability] || ""]) }),
            t.reason ? h("div", { class: "taste-reason", text: t.reason }) : null]),
          verdictButtons(t, PICK, v)]);
      })),
      h("div", { class: "taste-next" }, [h("button", { class: "btn primary", type: "button", onclick: function () {
        nextRound(); render(); window.scrollTo(0, 0);
        var signal = ctl.signal;
        HI.get(ID, q("picks?kind=" + s.kind), signal).then(function (d) {
          if (signal.aborted) return;
          s.pool = d.picks || []; s.status = d;
          if (!s.round.length) { s.roundNo--; nextRound(); }
          render();
        }, function () {});
      } }, ["Next round"])])]);
  }

  function renderWall() {
    if (s.error) return failed("The list");
    if (!s.familiar) return loading();
    if (!s.wall.length) return empty("No more famous matches to check.", s.familiar.length ? "" : "This list needs the taste twins model on the server.");
    return h("div", {}, [
      h("p", { class: "subtitle", text: "Well-known " + (s.kind === "tv" ? "series" : "films") + " that match your taste. Tap every one you have already seen; the poster then offers Loved, Liked and Meh. Untapped means not seen, and it stays in your picks." }),
      h("div", { class: "taste-wall" }, s.wall.map(function (t) {
        var k = tkey(t), v = s.mine[k] || "", name = label(v);
        var hit = h("button", { class: "taste-hit", type: "button", "data-k": k, "aria-pressed": v ? "true" : "false",
          "aria-label": t.title + (t.year ? " (" + t.year + ")" : "") + (v ? ", marked " + name : ""),
          onclick: function () {
            // A first tap marks Seen and opens the choice on that poster; a
            // tap on a marked poster opens or closes it.
            if (!s.mine[k]) { s.open = k; setVerdict(k, "seen"); } else { s.open = s.open === k ? "" : k; render(); }
          } }, [posterNode(t), h("span", { class: "taste-tile-title", "aria-hidden": "true" }, [t.title + " ", h("span", { class: "sub", text: t.year || "" })])]);
        var kids = [hit];
        if (v) kids.push(h("span", { class: "taste-badge", "aria-hidden": "true", text: name }));
        if (v && s.open === k) kids.push(h("div", { class: "taste-choose", role: "group", "aria-label": "How was " + t.title + "?" },
          MORE.map(function (m) {
            // Tapping the current choice again goes back to plain Seen.
            return h("button", { class: "btn", type: "button", title: HINT[m[0]], "aria-pressed": v === m[0] ? "true" : "false",
              onclick: function () { s.open = ""; setVerdict(k, v === m[0] ? "seen" : m[0]); } }, [m[1]]);
          }).concat([h("button", { class: "btn", type: "button", title: "Not seen: remove the mark", onclick: function () { s.open = ""; setVerdict(k, ""); } }, ["Undo"])])));
        return h("div", { class: "taste-tile" }, kids);
      })),
      h("div", { class: "taste-next" }, [h("button", { class: "btn primary", type: "button", onclick: function () {
        nextWall(); render(); window.scrollTo(0, 0);
        var signal = ctl.signal;
        HI.get(ID, q("familiar?kind=" + s.kind), signal).then(function (d) {
          if (signal.aborted) return;
          s.familiar = d.titles || []; s.status = d;
          if (!s.wall.length) nextWall();
          render();
        }, function () {});
      } }, ["Next 24"])])]);
  }

  function search(e) {
    e.preventDefault();
    var text = s.searchQ.trim();
    if (!text || s.searching) return;
    s.searching = true; render();
    var signal = ctl.signal;
    HI.get(ID, q("search?q=" + encodeURIComponent(text)), signal).then(function (d) { s.results = d.titles || []; }, function () { s.results = []; })
      .then(function () { if (signal.aborted) return; s.searching = false; render(); });
  }

  function renderKnown() {
    if (s.error) return failed("The list");
    if (!s.known) return loading();
    var needle = s.q.toLowerCase();
    var src = function (t) { return t.verdict ? "you" : t.source; };
    var rows = s.known.filter(function (t) {
      return (s.filter === "all" || src(t) === s.filter) && (!needle || String(t.title).toLowerCase().indexOf(needle) >= 0);
    });
    var counts = { all: s.known.length };
    s.known.forEach(function (t) { counts[src(t)] = (counts[src(t)] || 0) + 1; });

    var add = h("details", { class: "card taste-add", open: !!s.results }, [
      h("summary", {}, [HI.iconNode("plus", "sm"), " Add a title it doesn't know"]),
      h("form", { id: "taste-search-form", class: "taste-search", onsubmit: search }, [
        h("input", { id: "taste-search", type: "search", placeholder: "Search films and series", autocomplete: "off", "aria-label": "Search films and series", value: s.searchQ,
          oninput: function (e) { s.searchQ = e.target.value; } }),
        h("button", { class: "btn", type: "submit" }, [s.searching ? "Searching…" : "Search"])]),
      s.results ? (s.results.length ? h("ul", { class: "taste-list" }, s.results.map(function (t) {
        return h("li", { class: "taste-row open" }, [posterNode(t, "sm"), h("div", { class: "taste-row-main" }, [
          h("b", { text: t.title }), " ", h("span", { class: "sub", text: meta(t) }), verdictButtons(t, ADD, s.mine[tkey(t)] || "")])]);
      })) : h("div", { class: "sub", text: "No matches." })) : null]);

    var tools = h("div", { class: "taste-tools" }, [
      seg("Source", [["all", "All"], ["letterboxd", "Letterboxd"], ["plex", "Plex"], ["you", "Yours"]].map(function (f) {
        return [f[0], f[1] + " ", h("span", { class: "sub", text: String(counts[f[0]] || 0) })];
      }), s.filter, function (v) { s.filter = v; render(); }),
      h("input", { id: "taste-filter", type: "search", placeholder: "Filter", autocomplete: "off", "aria-label": "Filter titles", value: s.q,
        oninput: function (e) { s.q = e.target.value; render(); } })]);

    var list = rows.length ? h("ul", { class: "taste-list" }, rows.slice(0, 400).map(function (t) {
      var k = tkey(t), v = s.mine[k] !== undefined ? s.mine[k] : (t.verdict || "");
      var why = v ? "Your verdict" : (t.label || SOURCE[t.source] || "");
      return h("li", { class: "taste-row" }, [h("details", {}, [
        h("summary", {}, [h("div", { class: "taste-row-main" }, [h("b", { text: t.title }), " ", h("span", { class: "sub", text: meta(t) }), h("div", { class: "sub", text: why })]),
          HI.pill(v ? label(v) : (t.effect || "seen"), v ? "info" : EFFECT[t.effect] || "muted")]),
        verdictButtons(t, ALL, v),
        v ? h("button", { class: "btn", type: "button", onclick: function () { setVerdict(k, ""); } }, ["Remove my verdict"]) : null])]);
    })) : empty("Nothing matches.");

    return h("div", {}, [
      h("p", { class: "subtitle", text: "Everything the model believes you have watched, and how it counts. Tap a title to correct it." }),
      add, tools, list,
      rows.length > 400 ? h("div", { class: "sub taste-more", text: "Showing 400 of " + rows.length + "; filter to narrow." }) : null]);
  }

  function renderWatchlist() {
    if (s.error) return failed("The watchlist");
    if (!s.watchlist) return loading();
    var rows = (s.watchlist.titles || []).filter(function (t) { return t.kind === s.kind; });
    if (!rows.length) return HI.stateBox("Nothing on the watchlist" + (s.kind === "tv" ? " for series." : " for films."), "Import a Letterboxd export or turn on sync to fill it.");
    return h("div", {}, [
      h("p", { class: "subtitle", text: rows.length + " on the Letterboxd watchlist. Availability comes from the media plugin's last check." }),
      h("div", { class: "taste-wall" }, rows.map(function (t) {
        var v = s.mine[tkey(t)] !== undefined ? s.mine[tkey(t)] : t.verdict;
        return h("div", { class: "taste-tile" }, [posterNode(t),
          h("span", { class: "taste-tile-title" }, [t.title + " ", h("span", { class: "sub", text: t.year || "" })]),
          WHERE[t.availability] ? h("span", { class: "taste-badge", text: WHERE[t.availability] }) : null,
          v ? h("span", { class: "sub", text: "You: " + label(v) }) : null]);
      }))]);
  }

  function dl(rows) {
    return h("dl", { class: "taste-dl" }, rows.filter(Boolean).reduce(function (out, r) {
      out.push(h("dt", { text: r[0] }), h("dd", { text: r[1] }));
      return out;
    }, []));
  }

  function renderImport() {
    if (s.error) return failed("Import status");
    if (!s.imp) return loading();
    var d = s.imp, lb = d.letterboxd, m = d.model;
    var file = h("input", { type: "file", accept: ".zip,application/zip", class: "taste-file", id: "taste-file", onchange: function (e) {
      var f = e.target.files && e.target.files[0];
      if (!f) return;
      if (!HI.upload(ID, d.upload_target, f)) { s.flash = ["warn", "Imports are not available for this profile right now. Reload the page and try again."]; render(); return; }
      s.flash = ["info", "Importing " + f.name + ". Matching titles can take a few minutes; this page updates when it lands."];
      render();
    } });
    var cards = [
      h("section", { class: "card taste-panel" }, [
        h("h3", { text: "Letterboxd export" }),
        lb && lb.films ? dl([["Imported", HI.ago(lb.imported_at)], ["Ratings", String(lb.ratings)], ["Watched", String(lb.watched)], ["Watchlist", String(lb.watchlist)], ["Likes", String(lb.likes)], ["Unmatched", String(lb.unmatched)]])
          : h("p", { class: "sub", text: "Nothing imported yet." }),
        h("p", { class: "sub", text: "Only ratings, watched, likes and watchlist are read from the ZIP. Reviews, comments and account data are ignored, and nothing is written to Letterboxd." }),
        h("div", { class: "actions" }, [file, h("label", { class: "btn primary", for: "taste-file" }, [HI.iconNode("upload", "sm"), "Import export ZIP"])])]),
      h("section", { class: "card taste-panel" }, [
        h("h3", { text: "Public sync" }),
        d.sync_configured ? h("table", { class: "taste-sync" }, [h("thead", {}, [h("tr", {}, ["", "State", "Checked", "Next"].map(function (x) { return h("th", { scope: "col", text: x }); }))]),
          h("tbody", {}, SYNC_PARTS.map(function (p) {
            var part = (lb && lb.sync && lb.sync[p[0]]) || {};
            var st = part.state || "pending";
            return h("tr", {}, [h("th", { scope: "row", text: p[1] }),
              h("td", {}, [HI.pill(SYNC_WORD[st] || st, st === "ok" ? "ok" : st === "partial" ? "info" : st === "pending" ? "muted" : "warn")]),
              h("td", { text: part.checked_at ? HI.ago(part.checked_at) : "—" }), h("td", { text: part.next_at ? HI.fromNow(part.next_at) : "—" })]);
          }))])
          : h("p", { class: "sub", text: "Not configured for this profile. Set a public username in the profile bindings to follow the diary, watchlist, ratings and likes." }),
        d.sync_configured ? h("div", { class: "actions" }, [h("button", { class: "btn", type: "button", disabled: !!s.busy, onclick: function () { post("sync", "sync", "Letterboxd checked."); } }, [s.busy === "sync" ? "Syncing…" : "Sync now"])]) : null]),
      h("section", { class: "card taste-panel" }, [
        h("h3", { text: "Taste model" }),
        m ? dl([["Built", HI.ago(m.built_at)], ["Recommendations", String(m.picks)], ["Watched titles", String(m.known)], ["Seen these?", String(m.familiar || 0)], ["Your verdicts", String(d.verdicts || 0)], ["State", d.updating ? "rebuilding with newer answers" : "current"]])
          : h("p", { class: "sub", text: "Not built yet. It builds in the background once the media plugin answers." }),
        h("div", { class: "actions" }, [h("button", { class: "btn", type: "button", disabled: !!s.busy, onclick: function () { post("refresh", "refresh", "Rebuild requested; it runs in the background."); } }, [s.busy === "refresh" ? "Asking…" : "Rebuild now"])])])];
    return h("div", {}, [h("p", { class: "subtitle", text: "Where this profile's taste comes from, and when it last changed." }), h("div", { class: "taste-panels" }, cards)]);
  }

  function render() {
    if (!root) return;
    var focus = document.activeElement && document.activeElement.id;
    var trailing = null;
    if (s.profiles && s.profiles.length > 1) {
      trailing = h("label", { class: "taste-profile" }, [h("span", { class: "sub", text: "Profile" }),
        h("select", { class: "sel", "aria-label": "Household profile", onchange: function (e) { s = keep(fresh()); s.profile = e.target.value; load(); } },
          s.profiles.map(function (p) { return h("option", { value: p.id, selected: p.id === s.profile, text: p.id }); }))]);
    }
    var kids = [HI.head(ID, s.view, trailing)];
    if (s.profiles && !s.profiles.length) {
      kids.push(HI.stateBox("No household profiles yet.", "Add profile bindings in Admin → Plugins → Taste."));
    } else {
      var bar = [];
      if (s.view === "picks" || s.view === "familiar" || s.view === "watchlist") bar.push(seg("Films or series", [["movie", "Films"], ["tv", "Series"]], s.kind, function (v) {
        if (s.kind === v) return;
        s.kind = v; s.pool = null; s.familiar = null; s.round = []; s.wall = []; s.roundNo = 0; s.open = "";
        if (s.view === "watchlist") render(); else load();
      }));
      if (s.view !== "watchlist") bar.push(statusLine());
      kids.push(h("div", { class: "taste-bar" }, bar));
      if (s.flash) kids.push(HI.notice(s.flash[0], s.flash[1]));
      kids.push(!s.profiles ? (s.error ? failed("Taste") : loading()) : s.view === "known" ? renderKnown() : s.view === "familiar" ? renderWall() : s.view === "watchlist" ? renderWatchlist() : s.view === "import" ? renderImport() : renderPick());
    }
    root.replaceChildren(h("div", { class: "taste-page" }, kids));
    pumpPosters();
    if (focus === "taste-filter" || focus === "taste-search") {
      var f = document.getElementById(focus);
      if (f) { f.focus(); f.setSelectionRange(f.value.length, f.value.length); }
    }
    if (s.open) {
      // Escape closes the open choice and returns focus to its poster.
      var tile = root.querySelector('.taste-hit[data-k="' + CSS.escape(s.open) + '"]');
      if (tile && tile.parentNode) tile.parentNode.addEventListener("keydown", function (e) {
        if (e.key !== "Escape") return;
        var k = s.open; s.open = ""; render();
        var t = root && root.querySelector('.taste-hit[data-k="' + CSS.escape(k) + '"]');
        if (t) t.focus();
      });
    }
  }

  // Import and sync status move on their own; poll them while shown.
  function schedule() {
    clearTimeout(timer);
    timer = setTimeout(function () {
      if (root && s.view === "import" && HI.visible() && !s.busy) load();
      if (root) schedule();
    }, 15000);
  }

  (window.HI_PAGES = window.HI_PAGES || {}).taste = {
    show: function (el, ctx) {
      HI = window.HI;
      if (ctl) ctl.abort();
      ctl = new AbortController();
      posterQueue = []; parked = []; pending = {}; posterActive = 0;
      root = el;
      var want = ctx.params.get("profile");
      if (want && want !== s.profile) { s = keep(fresh()); s.profile = want; if (s.profiles && !s.profiles.some(function (p) { return p.id === want; })) s.profiles = null; }
      if (ctx.view !== s.view) { s.error = ""; s.flash = null; s.view = ctx.view; }
      load();
      schedule();
    },
    hide: function () {
      if (ctl) ctl.abort();
      ctl = null; root = null;
      clearTimeout(timer);
      posterQueue = []; parked = []; pending = {}; posterActive = 0;
      Object.keys(urls).forEach(function (f) { if (urls[f]) URL.revokeObjectURL(urls[f]); });
      urls = {};
    }
  };
})();

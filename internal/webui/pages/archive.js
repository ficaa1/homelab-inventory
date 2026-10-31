// The Archive plugin's page: the media-policy service's running job, its
// queues, every tagged title with the releases its last search found, the
// completed replacements, the library, the seeding mover and the scoring
// settings. Every request goes through the plugin's own API
// (internal/plugins/archive/api.go), which owns the service URL and token.
// Writes confirm in the side sheet, carry the service's plan hash where it
// has one, and are never retried by the page.
(function () {
  "use strict";
  var ID = "archive";
  var HI = null;
  var GLYPH = { critical: "◆", warning: "▲", info: "●", good: "●" };
  var FAST = 5000, IDLE = 30000, MAX = 120000, LINGER = 10000, FADE = 600;
  var STEPS = [["grab_intent", "Grab"], ["downloading", "Download"], ["import_intent", "Import copy"], ["verifying", "Verify"], ["complete", "Done"]];
  var SEED_ACTIVE = ["queued", "library_move", "linking", "moving", "rechecking", "rolling_back", "cleaning"];
  var SEED_QUEUE_MAX = 50;
  var SEED_PHASE = { queued: "Waiting", library_move: "Preparing", linking: "Preparing", moving: "Moving", rechecking: "Checking", rolling_back: "Restoring", rolled_back: "Restored", cleaning: "Cleaning up", complete: "Complete", halted: "Stopped", failed: "Stopped", stopped: "Stopped" };

  // Page state survives switching between the page's own views; hide() ends
  // polling and drops in-flight answers.
  var st = { root: null, body: null, strip: null, tools: null, view: "status", params: null, gen: 0 };
  var status = null, lastStatus = null, library = null, libraryActivity = null, seeding = null, selection = null;
  var notice = {}, open = {}, request = 0, ownerBusy = false, ownerNotice = "";
  var libFilter = "all", libQuery = "", libExpanded = {}, libNotice = "", libLoading = false;
  var seedLoading = false;
  var draft = null, preview = null, scBusy = false;
  var poll = { timer: null, last: 0, busy: false, again: false, fails: 0, ran: false };
  var liveText = null;
  var stripOpen = true, linger = {}, goTargets = {}, lingerTimer = null, pending = null;

  // ------------------------------------------------------------ small helpers

  function esc(s) { return HI.esc(s); }
  function $(id) { return document.getElementById(id); }
  function drawerOpen() { var d = $("drawer"); return d && !d.classList.contains("hidden"); }
  function sheet(html, label) { HI.openDrawer('<div class="arc-page arc-sheet">' + html + "</div>", label || "Archive"); }
  function msg(id, text, bad) { var el = $(id); if (el) { el.textContent = text; el.classList.toggle("arc-bad", !!bad); } }
  function row(k, v) { return "<dt>" + esc(k) + "</dt><dd>" + esc(v == null || v === "" ? "—" : v) + "</dd>"; }
  function real(iso) { return iso && new Date(iso).getFullYear() > 1; }
  function date(iso) { return real(iso) ? new Date(iso).toLocaleString() : "—"; }
  function clock(iso, zone) { return real(iso) ? new Date(iso).toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", timeZone: zone || undefined }) : "—"; }
  function elapsed(iso) { return real(iso) ? HI.dur(Math.max(1, (Date.now() - new Date(iso).getTime()) / 1000)) : "not recorded"; }
  function gb(n) { return n == null ? "—" : (n / 1e9).toFixed(n >= 1e11 ? 0 : 1) + " GB"; }
  function size(n) { var tb = n >= 1e12; return ((n || 0) / (tb ? 1e12 : 1e9)).toFixed(1) + (tb ? " TB" : " GB"); }
  function metric(n) { var p = size(n).split(" "); return esc(p[0]) + "<small>" + esc(p[1]) + "</small>"; }
  function pad(n) { return String(n).padStart(2, "0"); }
  function title(t) { return (t.title || "Untitled") + (t.kind === "sonarr" ? " S" + pad(t.season) : ""); }
  function archiveKey(t) { return t.kind + ":" + t.id + ":" + (t.season || 0); }
  function fileBytes(t) { return (t.files || []).reduce(function (n, f) { return n + f.size; }, 0); }
  function pill(word, level) {
    return '<span class="pill ' + esc(level || "") + '"><span class="glyph" aria-hidden="true">' + GLYPH[level || "info"] + "</span>" + esc(word) + "</span>";
  }
  function bar(pct, mode, label) {
    return '<div class="arc-bar ' + (mode || "") + '" role="progressbar" aria-label="' + esc(label) + '" aria-valuemin="0" aria-valuemax="100"' +
      (pct == null ? "" : ' aria-valuenow="' + pct.toFixed(0) + '"') + '><i style="width:' + (pct == null ? 30 : pct) + '%"></i></div>';
  }
  function fail(err) { return err && err.unauthorized; }
  function active(a) { return a && a.job && ["complete", "abandoned", "failed"].indexOf(a.job.phase) < 0; }
  function failedJobs(a) { return (a.failedJobs || []).concat(a.job && a.job.phase === "failed" ? [a.job] : []); }
  function jobID(j) { return archiveKey(j.target) + "@" + new Date(j.started).toISOString().slice(0, 19).replace(/[-:]/g, "") + "Z"; }

  // Replace el's children with html, keeping every node that still matches,
  // so a poll keeps scroll, focus and open details. Children pair by data-key
  // when they carry one, otherwise by tag and position.
  function morph(el, html) {
    var t = document.createElement("template"), focus = document.activeElement;
    t.innerHTML = html;
    if (st.strip && el === st.body && st.strip.parentNode === el) st.root.insertBefore(st.strip, el);
    morphKids(el, t.content);
    if (focus && focus !== document.activeElement && focus.isConnected && el.contains(focus)) focus.focus({ preventScroll: true });
  }
  function morphKey(n, count) {
    var k = n.nodeType === 1 && n.getAttribute("data-key");
    if (k) return "k:" + k;
    count[n.nodeName] = (count[n.nodeName] || 0) + 1;
    return n.nodeName + "#" + count[n.nodeName];
  }
  function morphKids(a, b) {
    var ak = Array.prototype.slice.call(a.childNodes), bk = Array.prototype.slice.call(b.childNodes), old = {}, c1 = {}, c2 = {};
    ak.forEach(function (n) { old[morphKey(n, c1)] = n; });
    var want = bk.map(function (n) {
      var k = morphKey(n, c2), o = old[k];
      if (!o) return n;
      delete old[k];
      return morphNode(o, n);
    });
    Object.keys(old).forEach(function (k) { a.removeChild(old[k]); });
    var at = a.firstChild;
    want.forEach(function (n) { if (n === at) at = at.nextSibling; else a.insertBefore(n, at); });
  }
  function morphNode(a, b) {
    if (a.nodeType !== b.nodeType || a.nodeName !== b.nodeName) { a.parentNode.replaceChild(b, a); return b; }
    if (a.nodeType !== 1) { if (a.nodeValue !== b.nodeValue) a.nodeValue = b.nodeValue; return a; }
    var details = a.nodeName === "DETAILS", input = a.nodeName === "INPUT";
    Array.prototype.slice.call(a.attributes).forEach(function (at) { if (!b.hasAttribute(at.name) && !(details && at.name === "open")) a.removeAttribute(at.name); });
    Array.prototype.forEach.call(b.attributes, function (at) {
      if (details && at.name === "open") return;
      if (input && at.name === "value") return; // never overwrite what is being typed
      if (a.getAttribute(at.name) !== at.value) a.setAttribute(at.name, at.value);
    });
    morphKids(a, b);
    return a;
  }

  // ------------------------------------------------------------ status view

  function jobReason(m) {
    var rules = [[/unrelated|extra|pack/i, "Extra pack files"], [/probe unavailable/i, "Probe unavailable"], [/quiet playback/i, "Waiting for quiet playback"], [/owner/i, "Needs owner approval"], [/timeout|timed out/i, "Job timed out"], [/another download|queue changed/i, "Queue changed"], [/tag removed/i, "Tag removed"], [/tags unavailable/i, "Tag check unavailable"], [/selection settings/i, "Settings invalid"], [/policy or mode/i, "Policy changed"], [/identify.*download|download unidentified/i, "Download unknown"], [/non-ranking|import rejection/i, "Import rejected"], [/stereo|audio|channel/i, "Audio rejected"], [/recovery.*unproven/i, "Recovery proof missing"], [/lifecycle.*unproven/i, "Download proof missing"], [/verification/i, "Verification failed"], [/evidence conflicts/i, "Grab evidence conflicts"], [/import/i, "Import needs review"], [/library.*change/i, "Library change unproven"]];
    for (var i = 0; i < rules.length; i++) if (rules[i][0].test(m || "")) return rules[i][1];
    return "Needs review";
  }
  function reasonLabel(r) {
    var rules = [[/needs attention|check status/i, "Needs attention"], [/download disk|free space|disk minimum/, "Disk space"], [/copy|import|verif/, "Import"], [/rate limit|lookup limit/, "Search limit"], [/watched|recent play/, "Recent play"], [/Plex sessions|streaming/, "Streaming"], [/queue/, "Queued"], [/score|no release good enough/, "Low score"], [/size|larger|smaller|GB|half/, "Size"], [/seed/, "Seeds"], [/audio|channel|stereo|lossless/, "Audio"], [/HDR|Dolby|DV/, "HDR"], [/resolution|2160|1080|4K/, "Resolution"], [/pack|season/, "Season pack"], [/nightly|window/, "Nightly"], [/\blease|busy|Busy|using this title/, "Busy"], [/disabled/, "Disabled"], [/Plex/, "Plex"], [/restored/, "Restored"], [/candidate|replacement|release/, "No match"]];
    for (var i = 0; i < rules.length; i++) if (rules[i][0].test(r)) return rules[i][1];
    return "Details";
  }
  // Full reasons stay one tap away in a chip.
  function reason(r, label) {
    if (!r) return "";
    return '<details class="arc-reason"><summary title="' + esc(r) + '">' + esc(label || reasonLabel(r)) + "</summary><div>" + esc(r) + "</div></details>";
  }
  function waitText(r) {
    if (/Plex.*sessions/i.test(r || "")) return "Media streams active";
    if (r && r.trim().split(/\s+/).length <= 5 && !/[—;]/.test(r)) return r;
    return "Import queued";
  }
  function ownerButton(path, label, fields, off) {
    return '<button class="btn" type="button" data-owner-path="' + esc(path) + '" data-owner-fields="' + esc(JSON.stringify(fields)) + '"' + (off || ownerBusy ? " disabled" : "") + ">" + esc(label) + "</button>";
  }
  function ownerOrder(path, fields, i, n) {
    return '<span class="arc-order">' + ownerButton(path, "Up", Object.assign({}, fields, { position: i - 1 }), i === 0) +
      ownerButton(path, "Down", Object.assign({}, fields, { position: i + 1 }), i === n - 1) + "</span>";
  }

  function nowCard(v) {
    var a = v.archive || {}, info = v.info || {}, j = a.job, isActive = active(a), html = '<div class="arc-now">';
    if (isActive) {
      var p = j.progress || {}, phase = j.phase, idx = STEPS.map(function (s) { return s[0]; }).indexOf(phase);
      html += '<div class="arc-eyebrow">' + pill(a.paused ? "held" : "running", a.paused ? "warning" : "good") +
        '<span>' + (j.manuallyApproved ? "Approved" : "Automatic") + ' · started ' + esc(elapsed(j.started)) + ' ago</span></div>' +
        '<h3>' + esc(title(j.target)) + '</h3><div class="arc-code">' + esc((j.release || {}).title) + '</div>';
      if (phase === "downloading" && p.bytesTotal > 0) {
        html += '<div class="arc-figs"><span><b>' + esc(gb(p.bytesTotal - (p.bytesLeft || 0))) + '</b> of ' + esc(gb(p.bytesTotal)) + '</span></div>';
      }
      if (phase !== "needs_approval" && a.paused) html += reason(a.paused) + '<div class="arc-figs">Retry ' + esc(HI.fromNow(info.nextCycle)) + '</div>';
      if (phase === "needs_approval") {
        html += '<div class="note" title="' + esc(j.approvalReason || j.message || '') + '">' + pill(j.forceImport ? 'Approved' : 'Needs approval','warning') + ' ' + esc(j.forceImport ? waitText(a.paused) : jobReason(j.approvalReason || j.message)) + '</div><div class="actions">' +
          ownerButton('job/approve','Import anyway',{id:jobID(j)},j.forceImport || v.dryRun) +
          ownerButton('job/drop','Drop',{id:jobID(j)},v.dryRun) + '</div>';
      } else if (p.error || j.message) html += reason(p.error || j.message);
      if (phase !== 'needs_approval') html += '<ol class="arc-steps">' + STEPS.map(function (s, i) {
        return '<li class="' + (i < idx ? 'done' : i === idx ? 'on' : '') + '"' + (i === idx ? ' aria-current="step"' : '') + '>' + esc(s[1]) + '</li>';
      }).join('') + '</ol>';
    } else {
      var queued = (a.approvals || []).filter(function (q) { return !q.blocked; }).length;
      html += '<h3>' +
        (queued ? 'Starting' : 'Idle') + '</h3><div class="idle-line">' +
        (queued ? esc(a.paused || 'Queued') + ' · ' + esc(HI.fromNow(info.nextCycle)) :
          'Nightly ' + esc(clock(info.windowStart, info.timezone)) + '–' + esc(clock(info.windowEnd, info.timezone)) +
          ' · ' + esc(info.timezone || '') + ' · next ' + esc(HI.fromNow(info.nextCycle))) + '</div>';
    }
    failedJobs(a).forEach(function (failed) {
      html += '<div class="arc-queue"><div class="arc-eyebrow">' + pill(failed.nonHalting ? 'failed' : 'halted', failed.nonHalting ? 'warning' : 'critical') + '</div><h3>' + esc(title(failed.target)) + '</h3>' +
        '<div class="note" title="' + esc(failed.message || '') + '">' + esc(jobReason(failed.message)) + '</div>' +
        '<div class="actions"><button class="btn" style="min-height:44px;min-width:44px" type="button" data-archive-action="clear-job" data-archive-id="' + esc(jobID(failed)) + '">Clear</button></div></div>';
    });
    var queue = a.approvals || [];
    if (queue.length) {
      html += '<div class="arc-queue"><h4>' + (isActive ? 'Up next' : 'Approvals') + '</h4>' + queue.map(function (q, i) {
        var d = (a.report || []).filter(function (r) { return archiveKey(r.target) === archiveKey(q); })[0], c = d && (d.candidates || []).filter(function (x) { return x.id === q.releaseId; })[0];
        return '<div class="arc-q"><span class="n">' + (i + 1) + '.</span><span class="what"><b>' + esc(d ? title(d.target) : q.kind + ' ' + q.id) + '</b> · ' +
          (c ? '<span class="arc-code">' + esc(c.title) + '</span> · ' + esc(gb(c.size)) : 'release from the last search') +
          (q.message ? reason(q.message, q.blocked ? 'Blocked' : 'Details') : '') +
          '</span>' + ownerOrder('approvals/reorder', {key:archiveKey(q)},i,queue.length) + '<button class="btn" type="button" data-archive-action="clear" data-archive-id="' + i + '">Remove</button></div>';
      }).join('') + '</div>';
    }
    var searches = (v.archiveSearches || []).filter(function(q) { return q.state === 'queued'; });
    if (searches.length) html += '<div class="arc-queue"><h4>Search queue</h4>' + searches.map(function(q,i) {
      return '<div class="arc-q"><span class="n">' + (i+1) + '.</span><span class="what"><b>' + esc(q.name + (q.title.kind === 'sonarr' ? ' S' + String(q.title.season).padStart(2,'0') : '')) + '</b></span>' + ownerOrder('search/reorder',{key:archiveKey(q.title)},i,searches.length) + '</div>';
    }).join('') + '</div>';
    return html + '</div>';
  }

  function tiles(v) {
    var a = v.archive || {}, info = v.info || {}, saved = 0, done = 0, waiting = 0;
    (a.history || []).forEach(function (h) {
      if (h.status !== "restored" && h.job.actualBytes > 0) { saved += fileBytes(h.job.target) - h.job.actualBytes; done++; }
    });
    (a.report || []).forEach(function (d) {
      var key = archiveKey(d.target), busy = active(a) && archiveKey(a.job.target) === key;
      if (!d.release && (d.candidates || []).length && !busy && !(a.approvals || []).some(function (q) { return archiveKey(q) === key; })) waiting++;
    });
    var free = info.downloadFreeBytes, min = info.downloadMinFreeBytes, diskKnown = !info.downloadFreeError && info.downloadChecked && new Date(info.downloadChecked).getFullYear() > 1;
    function tile(label, value, sub, cls) {
      return '<div class="tile"><div class="label">' + esc(label) + '</div><div class="value' + (cls ? ' ' + cls : '') + '">' + value + '</div><div class="sub">' + esc(sub) + '</div></div>';
    }
    return '<div class="kpis arc-kpis">' +
      tile("Space saved", esc((saved / 1e9).toFixed(0)) + '<small>GB</small>', done + ' replacement' + (done === 1 ? '' : 's')) +
      tile("Your pick", esc(waiting), waiting ? 'titles' : '—', waiting ? 'warn' : '') +
      tile("Download disk", diskKnown ? esc((free / 1e9).toFixed(0)) + '<small>GB free</small>' : '—', diskKnown ? 'Minimum ' + gb(min) : (info.downloadFreeError || 'Unknown'), diskKnown && free < min ? 'warn' : '') +
      tile("Tracker searches", esc(info.searchesLastMinute || 0) + '<small>/ ' + esc(info.searchesPerMinute || 5) + ' per min</small>', (info.searchesUsed || 0) + ' tonight') +
      '</div>';
  }

  function standing(v, d) {
    var a = v.archive || {}, key = archiveKey(d.target), n = (d.candidates || []).length;
    var queued = (a.approvals || []).filter(function (q) { return archiveKey(q) === key; })[0];
    if (active(a) && archiveKey(a.job.target) === key) return [pill("replacing", "good"), ""];
    if (queued) return [pill(queued.blocked ? "blocked" : "queued", queued.blocked ? "warning" : "info"), queued.blocked ? queued.message : ""];
    if (d.reason === "already archived") return [pill("archived", "good"), ""];
    if (d.release && !d.skipped) return [pill("auto pick", "good"), ""];
    if (/watched within idle window/.test(d.skipped || d.reason || "")) return [pill("watched recently, your pick", "warning"), ""];
    if (d.release) return [pill("waiting", "info"), d.skipped || d.reason];
    if (n) {
      var why = d.skipped || d.reason || "none passed the automatic rules";
      return [pill("your pick", "warning"), why];
    }
    if (d.searchedAt && new Date(d.searchedAt).getFullYear() > 1) return [pill("none found", "info"), d.reason];
    return [pill("not searched", "info"), d.reason];
  }

  function breakdown(c) {
    var s = c.score || {}, n = function(v) { return Number(v || 0).toFixed(1).replace(/\.0$/, ''); };
    if (!s.mode) return '<div class="src">Details unavailable</div>';
    var remaining = 100, segments = [s.sizeFit, s.savings, s.seeders, s.group, s.features].map(function(v) {
      var width = Math.max(0, Math.min(remaining, Number(v) || 0)); remaining -= width;
      return '<i style="width:' + width + '%"></i>';
    }).join('');
    var row = function(label, value, detail) { return '<div>' + label + (detail ? '<small>' + esc(detail) + '</small>' : '') + '</div><b>' + value + '</b>'; };
    return '<div class="arc-parts"><div>' + esc(s.mode) + ' · ' + esc(s.codec) + '</div><b>' + n(s.total) + '/100</b></div>' +
      '<div class="arc-stack" role="img" aria-label="Score ' + n(s.total) + ' of 100, minimum ' + n(s.minScore) + '">' + segments +
      (s.audioPenalty ? '<span class="penalty" title="Unknown audio −5" style="left:' + Math.max(0,Math.min(100,Number(s.total))) + '%;width:' + Math.max(0,Math.min(Math.abs(s.audioPenalty),100-Number(s.total))) + '%"></span>' : '') +
      '<span class="minimum" style="left:' + Math.max(0,Math.min(100,Number(s.minScore))) + '%"><span style="transform:translateX(' + (s.minScore > 90 ? '-100%' : s.minScore < 10 ? '0' : '-50%') + ')">Min ' + n(s.minScore) + '</span></span></div>' +
      '<div class="arc-parts">' + row('Size fit', n(s.sizeFit) + '/50', n(s.gbPerHour) + ' GB/h, target ' + n(s.target)) +
      row('Range', n(s.floor) + '–' + n(s.cap) + ' GB/h') +
      row('Savings', n(s.savings) + '/20') + row('Seeders', n(s.seeders) + '/15') +
      row('Group', n(s.group) + '/10', s.trusted ? 'Trusted' : 'Unlisted') +
      row('Features', '+' + n(s.features), (s.featureNames || []).join(' · ') || 'None') +
      (s.audioPenalty ? row('Unknown audio', '−' + n(Math.abs(s.audioPenalty))) : '') + '</div>';
  }

  function releases(v, d, i) {
    var a = v.archive || {}, key = archiveKey(d.target), before = fileBytes(d.target);
    var activeHere = active(a) && archiveKey(a.job.target) === key;
    var blocked = v.dryRun || !v.archiveEnabled || activeHere;
    var queued = (a.approvals || []).filter(function (q) { return archiveKey(q) === key; })[0];
    var showAll = notice["all:" + key], list = d.candidates || [], shown = showAll ? list : list.slice(0, 8);
    var searched = d.searchedAt && new Date(d.searchedAt).getFullYear() > 1;
    var html = '<div class="arc-body"><div class="arc-tools"><span>' + (searched ? 'Searched ' + esc(HI.ago(d.searchedAt)) : 'Not searched') +
      ' · ' + esc(d.mode === "2160p" || d.target.fourK ? "4K HDR" : "1080p or 4K HDR") + '</span>' +
      '<button class="btn" type="button" data-archive-action="search" data-archive-id="' + i + '"' + (v.archiveEnabled ? '' : ' disabled') + '>Search</button>' +
      (notice[key] ? '<span role="status">' + esc(notice[key]) + '</span>' : '') + '</div>';
    if (!list.length) return html + (d.rejected && d.rejected["not freeleech"] ? '<div class="arc-v">' + esc(d.rejected["not freeleech"]) + ' non-freeleech</div>' : '') + '</div>';
    html += '<div class="arc-rel head"><span>Release</span><span>Size</span><span>GB/h</span><span>Seeders</span><span>Score</span><span>Auto pick</span><span></span></div>';
    html += shown.map(function (c, ci) {
      var pick = d.release && c.title === d.release.title && !c.rejection, mine = queued && queued.releaseId === c.id;
      var verdict = pick ? '<span class="arc-v ok">✓ auto pick</span>' : !c.rejection ? '<span class="arc-v ok">passes</span>' : reason(c.rejection);
      var detailID = "score-" + encodeURIComponent(key + ":" + c.indexerId + ":" + c.id), expanded = !!notice[detailID];
      return '<div class="arc-release" data-key="' + esc(detailID) + '"><div class="arc-rel' + (mine ? ' queued' : pick ? ' pick' : '') + '"><div class="t">' + esc(c.title) + '<small>' + esc(c.group || 'unknown group') + ' · ' + esc((c.score || {}).codec || '?') + (c.audioUnverified ? ' · audio unverified' : '') + '</small></div>' +
        '<div class="c" data-k="size"><b>' + esc(gb(c.size)) + '</b><span class="sub">−' + (before > 0 ? Math.round((1 - c.size / before) * 100) : 0) + '%</span></div>' +
        '<div class="c" data-k="GB/h">' + Number((c.score || {}).gbPerHour).toFixed(1) + '</div>' +
        '<div class="c" data-k="seeders">' + esc(c.seeders) + '</div>' +
        '<div class="c" data-k="score"><button class="btn arc-score-toggle" type="button" aria-expanded="' + expanded + '" aria-controls="' + detailID + '" data-score-toggle="' + detailID + '">Score ' + Number((c.score || {}).total).toFixed(0) + '</button></div>' +
        '<div class="c">' + verdict + '</div>' +
        '<div class="a">' + (mine ? '<span class="arc-v">queued</span>' : '<button class="btn' + (pick ? ' primary' : '') + '" type="button" data-archive-action="approve" data-archive-id="' + i + ':' + ci + '"' + (blocked ? ' disabled' : '') + '>Approve</button>') + '</div></div><div id="' + detailID + '" class="arc-breakdown' + (expanded ? '' : ' hidden') + '">' + breakdown(c) + '</div></div>';
    }).join('');
    if (list.length > shown.length) html += '<div class="arc-more"><button class="btn" type="button" data-archive-action="all" data-archive-id="' + i + '">All ' + list.length + '</button></div>';
    return html + '</div>';
  }

  function titles(v) {
    var a = v.archive || {}, rows = (a.report || []).slice();
    if (!rows.length) return '<div class="empty">No titles</div>';
    var rank = function (d) { var s = standing(v, d)[0]; return /replacing|queued|blocked/.test(s) ? 0 : /your pick|auto pick/.test(s) ? 1 : /archived/.test(s) ? 3 : 2; };
    rows = rows.map(function (d, i) { return {d: d, i: i}; }).sort(function (x, y) { return rank(x.d) - rank(y.d) || fileBytes(y.d.target) - fileBytes(x.d.target); });
    return rows.map(function (r) {
      var d = r.d, s = standing(v, d), key = "archive:title:" + archiveKey(d.target);
      var isOpen = open.hasOwnProperty(key) ? open[key] : false;
      return '<details class="arc-title" data-key="' + esc(key) + '" data-group="' + esc(key) + '"' + (isOpen ? ' open' : '') + '><summary><span class="name">' + esc(title(d.target)) +
        '<span class="size">' + esc(gb(fileBytes(d.target))) + (d.target.fourK ? ' · 4K' : '') + ((d.candidates || []).length ? ' · ' + d.candidates.length + ' releases' : '') + '</span></span><span>' + s[0] + '</span></summary>' +
        (isOpen ? reason(s[1]) + releases(v, d, r.i) : '<div class="arc-body hidden"></div>') + '</details>';
    }).join('');
  }

  function refusal(v, h, action) {
    var info = v.info || {};
    if (!v.reachable || !v.archiveEnabled) return "Archive is unavailable or disabled.";
    if (h.status !== "available") return "Already " + h.status + ".";
    if (Date.now() > new Date(h.undoUntil).getTime()) return "Past the recycle cleanup; the originals are gone.";
    if (h.inRecycle === false) return "The original is no longer in the recycle bin.";
    if (!info.restoreConfigured) return "archive.restoreMounts is not configured; there is no writable view to restore or delete through";
    if (action === "restore" && !info.quarantineConfigured) return "archive.quarantineDir is not configured; the replacement has nowhere to go";
    if (active(v.archive) && v.archive.job.target.kind === h.job.target.kind && v.archive.job.target.id === h.job.target.id) return "An archive job for this title is in progress.";
    return "";
  }

  function history(v) {
    var rows = (v.archive || {}).history || [];
    if (!rows.length) return '<div class="empty">No replacements</div>';
    return '<div class="scroll" tabindex="0" aria-label="Completed"><table class="arc-history"><thead><tr>' +
      '<th>Title</th><th>Size</th><th>Undo</th><th>Status</th></tr></thead><tbody>' +
      rows.map(function (h) {
        var j = h.job, before = fileBytes(j.target), after = j.actualBytes > 0 ? j.actualBytes : null, reasons = [];
        var actions = ["restore", "purge"].map(function (action) {
          var reason = refusal(v, h, action);
          if (reason && reasons.indexOf(reason) < 0) reasons.push(reason);
          return reason ? '' : '<button class="btn" type="button" data-archive-action="' + action + '" data-archive-id="' + esc(h.id) + '">' + (action === "restore" ? 'Restore' : 'Purge') + '</button>';
        }).join('');
        return '<tr><td><b>' + esc(title(j.target)) + '</b><span class="arc-code">' + esc((j.release || {}).title) + '</span><span class="sub">' +
          (j.manuallyApproved ? 'Approved · ' : '') + esc(j.sidecarsCarried || 0) + ' subtitle file' + (j.sidecarsCarried === 1 ? '' : 's') + ' carried' + (j.sidecarMessage ? ' · ' + esc(j.sidecarMessage) : '') + '</span></td><td>' +
          esc(gb(before)) + ' → <b>' + esc(gb(after)) + '</b><div class="sub">' + (after == null ? 'not verified' : esc(gb(before - after)) + ' saved') + '</div></td><td>' +
          (h.status === 'ended' ? 'Library unchanged' : esc(date(h.undoUntil)) + '<div class="sub">original ' + (h.inRecycle == null ? 'unknown' : h.inRecycle ? 'in recycle bin' : 'gone') + '</div>') + '</td><td>' +
          (h.status === 'ended' ? '<div class="sub" title="' + esc(j.message || '') + '">' + esc(jobReason(j.message)) + '</div>' : '') + pill(h.status, h.status === 'available' ? 'good' : /failed/.test(h.status) ? 'critical' : 'info') + (h.note ? '<div class="sub">' + esc(h.note) + '</div>' : '') +
          (actions ? '<div class="acts">' + actions + '</div>' : '') + (h.status === 'available' ? reasons.map(function (r) { return reason(r); }).join('') : '') + '</td></tr>';
      }).join('') + '</tbody></table></div>';
  }

  function purgeAllButton(v) {
    var n = ((v.archive || {}).history || []).filter(function (h) { return !refusal(v, h, "purge"); }).length;
    return n ? '<span class="spacer"></span><button class="btn" type="button" data-archive-action="purge-all">Purge all (' + n + ")</button>" : "";
  }

  function archiveLibrarySwitch(noPause) {
    var v = status || {}, paused = v.archivePaused;
    return (noPause ? "" : ownerButton("pause", paused ? "Resume" : "Pause", { paused: !paused }, v.dryRun)) +
      '<div class="library-nav" role="group" aria-label="Media views">' + HI.mediaSections().map(function (section) {
        var on = section.plugin === ID && section.view === st.view;
        return '<button class="btn' + (on ? ' primary' : '') + '" type="button" data-library-link="' + esc(HI.link(section.plugin, section.view)) + '" aria-pressed="' + on + '">' + esc(section.title) + '</button>';
      }).join("") + '</div>';
  }

  function renderStatus() {
    var v = status;
    if (!v) { morph(st.body, archiveLibrarySwitch() + HI.skeleton(3)); return; }
    if (!v.reachable) { morph(st.body, archiveLibrarySwitch() + '<div class="empty"><div class="big">Media policy did not answer.</div>' + esc(v.error || "Try refresh.") + '</div>'); return; }
    var a = v.archive || {}, info = v.info || {};
    morph(st.body, '<div class="arc-head"><h2>Archive</h2><span class="pill" title="Originals kept in the recycle bin">' + HI.iconNode("clock", "sm").outerHTML + ' undo ' + esc(info.undoDays || 7) + 'd</span><span class="spacer"></span>' +
      archiveLibrarySwitch() + '<button class="btn" type="button" data-archive-action="run"' + (!v.archiveEnabled || (info.runNow || {}).pending ? ' disabled' : '') + '>Check now</button></div>' +
      (!v.archiveEnabled ? '<div class="banner warn"><div>' + pill('disabled', 'warning') + '</div></div>' : '') +
      (v.archivePaused ? '<div class="banner warn"><div>' + pill('Paused','warning') + '</div></div>' : '') +
      (ownerNotice ? '<div class="note" role="status">' + esc(ownerNotice) + '</div>' : '') +
      (v.dryRun ? '<div class="banner warn"><div>' + pill('dry run', 'warning') + '</div></div>' : '') +
      (v.error || a.error ? '<div class="banner warn"><div>' + pill('error', 'critical') + ' ' + esc(v.error || a.error) + '</div></div>' : '') +
      (a.tagRemovalError ? '<div class="note">' + esc(a.tagRemovalError) + '</div>' : '') +
      nowCard(v) + tiles(v) +
      ((a.abandonedDownloads || []).length ? '<div class="arc-queue"><h4>Rejected downloads</h4>' + a.abandonedDownloads.map(function(d) { return '<div class="arc-q"><span class="what"><b>' + esc(d.title) + '</b><br><span class="arc-code">' + esc(d.release) + '</span><br>Removes ' + esc(new Date(d.removeAfter).getFullYear() > 1 ? new Date(d.removeAfter).toLocaleDateString() : 'Awaiting completion') + '</span></div>'; }).join('') + '</div>' : '') +
      (a.cleanupError ? '<div class="arc-msg warn">' + esc(a.cleanupError) + '</div>' : '') +
      '<div class="arc-section"><h3>Tagged titles</h3></div>' + titles(v) +
      '<div class="arc-section"><h3>Completed</h3>' + purgeAllButton(v) + '</div>' + history(v) +
      (a.notificationError ? '<div class="freebar">' + esc(a.notificationError) + '</div>' : ''));
    focusPending();
  }

  // Inline actions that need no confirmation: searching, showing every
  // release and removing a queued approval are all undoable.
  function inline(action, id) {
    var a = (status || {}).archive || {};
    if (action === "all") { notice["all:" + archiveKey(a.report[Number(id)].target)] = true; render(); return true; }
    if (action === "search") {
      var t = a.report[Number(id)].target, k = archiveKey(t);
      notice[k] = "searching…";
      render();
      HI.send(ID, "search", "POST", { title: { kind: t.kind, id: t.id, season: t.season || 0 } }).then(function (r) {
        notice[k] = (r && r.message) || "done";
      }).catch(function (err) { if (!fail(err)) notice[k] = err.message; }).then(pollNow);
      return true;
    }
    if (action === "clear") {
      var q = a.approvals[Number(id)];
      HI.send(ID, "approve", "POST", { approval: { kind: q.kind, id: q.id, season: q.season || 0, releaseId: q.releaseId, indexerId: q.indexerId, clear: true } })
        .catch(function () {}).then(pollNow);
      return true;
    }
    return false;
  }

  // Only previews are bounded: abandoning an apply does not establish whether
  // the service performed it.
  function plan(path, id) {
    var timer;
    var deadline = new Promise(function (_, reject) {
      timer = setTimeout(function () { reject(new Error("Loading plan timed out. Close this sheet and try again.")); }, 30000);
    });
    return Promise.race([HI.send(ID, path, "POST", { id: id }), deadline]).then(function (r) { clearTimeout(timer); return r; }, function (e) { clearTimeout(timer); throw e; });
  }

  function planMoves(label, moves) {
    return '<div class="arc-note"><b>' + esc(label) + "</b></div>" + (moves || []).map(function (m) {
      return '<dl class="dl">' + row("From", m.from) + row("To", m.to) + row("Size", gb(m.size)) + "</dl>";
    }).join("");
  }

  function openAction(action, id) {
    if (inline(action, id)) return;
    if (action === "purge-all") { confirmPurgeAll(); return; }
    var req = ++request, v = status || {}, a = v.archive || {};
    var head = action === "approve" ? "Approve release" : action === "run" ? "Check now" : action === "clear-job" ? "Clear" : action === "restore" ? "Restore" : "Purge";
    var approval, selected, decision;
    if (action === "approve") {
      var parts = id.split(":");
      decision = a.report[Number(parts[0])];
      selected = decision.candidates[Number(parts[1])];
      approval = { kind: decision.target.kind, id: decision.target.id, season: decision.target.season || 0, releaseId: selected.id, indexerId: selected.indexerId };
    }
    sheet("<h3>" + head + '</h3><div class="empty">Loading plan…</div>');
    function show(p) {
      if (req !== request || !drawerOpen()) return;
      var dry = v.dryRun && action !== "run" && action !== "clear-job", html = "<h3>" + head + "</h3>";
      if (approval) {
        var before = fileBytes(decision.target), s = selected.score || {};
        html += '<div class="subtitle">' + esc(title(decision.target)) + '</div><div class="arc-drawer-size">' + esc(gb(before)) + " → " + esc(gb(selected.size)) +
          "<small>−" + esc(gb(before - selected.size)) + '</small></div><p class="arc-code">' + esc(selected.title) + '</p><div class="arc-figs"><span>' + esc(selected.seeders) + " seeders</span><span>" +
          Number(s.gbPerHour || 0).toFixed(1) + " GB/h " + esc(s.codec || "") + "</span><span>score " + Number(s.total || 0).toFixed(0) + "</span></div>" +
          (selected.rejection ? '<div class="banner warn arc-gap"><div>' + pill("override", "warning") + " " + reason(selected.rejection) + "</div></div>" : "") +
          (active(a) ? '<div class="arc-note">Waits for the running job</div>' : "");
      } else if (action === "clear-job") {
        var job = failedJobs(a).filter(function (j) { return jobID(j) === id; })[0];
        html += '<div class="subtitle">' + esc(job ? title(job.target) : id) + "</div>";
      } else if (action === "run") {
        html += v.dryRun ? pill("dry run", "warning") : '<div class="subtitle">Refresh tags, eligibility and cached releases now.</div>';
      } else {
        html += '<div class="subtitle">' + esc(p.title) + "</div>";
        if (action === "restore") {
          html += reason("Existing files are not overwritten.", "Safety") +
            planMoves("Quarantine", (p.restore || {}).quarantine) + planMoves("Restore originals", (p.restore || {}).restore) +
            '<div class="arc-note">Rescan: ' + esc((p.restore || {}).rescan || "not recorded") + "</div>" +
            (p.sidecars || []).map(function (s) { return '<dl class="dl">' + row("Subtitle", s.path) + row("Staged", s.staged) + row("Note", s.note) + "</dl>"; }).join("");
        } else {
          html += '<div class="banner warn arc-gap"><div>' + pill("irreversible", "critical") + " " + esc(gb(p.bytes)) + " · Undo removed</div></div>" +
            (p.purge || []).map(function (f) { return '<dl class="dl">' + row("File", f.path) + row("Size", gb(f.size)) + "</dl>"; }).join("");
        }
      }
      html += '<div class="form">' + (action === "purge" ? '<div><label for="arc-confirm">Type ' + esc(p.confirm) + ' exactly</label><input id="arc-confirm" autocomplete="off" spellcheck="false"></div>' : "") +
        '<div class="actions"><button class="btn primary" id="arc-apply" type="button"' + (dry || action === "purge" ? " disabled" : "") + ">" + (approval ? "Approve" : head) + "</button>" +
        '<button class="btn" id="arc-cancel" type="button">Cancel</button></div><div id="arc-action-msg" role="status" class="arc-msg">' + (dry ? "Dry run" : "") + "</div></div>";
      sheet(html);
      $("arc-cancel").onclick = HI.closeDrawer;
      if (action === "purge") $("arc-confirm").oninput = function () { $("arc-apply").disabled = dry || this.value !== p.confirm; };
      $("arc-apply").onclick = function () {
        this.disabled = true;
        msg("arc-action-msg", "working…");
        var path = approval ? "approve" : action === "run" ? "run" : action === "clear-job" ? "clear" : action;
        var body = approval ? { approval: approval } : action === "run" ? {} : action === "clear-job" ? { id: id, apply: true } :
          { id: id, apply: true, hash: p.hash, confirm: action === "purge" ? $("arc-confirm").value : "" };
        HI.send(ID, path, "POST", body).then(function (r) {
          if (req !== request || !drawerOpen()) return;
          r = r || {};
          sheet("<h3>" + head + '</h3><div class="arc-note">' + esc(r.message || r.error || (r.applied ? "Applied." : "The service returned without applying the plan.")) + "</div>");
          pollNow();
        }).catch(function (err) {
          if (fail(err) || req !== request || !drawerOpen()) return;
          msg("arc-action-msg", err.message, true);
          pollNow();
        });
      };
    }
    if (action === "run" || action === "clear-job" || approval) { show({}); return; }
    plan(action, id).then(show).catch(function (err) {
      if (fail(err) || req !== request || !drawerOpen()) return;
      sheet("<h3>" + head + '</h3><div class="empty">' + esc(err.message) + "</div>");
    });
  }

  function purgeReason(r) {
    if (/busy|lease/i.test(r)) return "Title is busy";
    if (/changed|mismatch/i.test(r)) return "Plan changed";
    if (/missing|gone|not found/i.test(r)) return "Original unavailable";
    if (/audit|save|store/i.test(r)) return "Could not save";
    return "Purge failed";
  }
  // Counted from status; the sheet loads the service's own checked plan.
  function confirmPurgeAll() {
    var req = ++request;
    sheet('<h3>Purge all</h3><div class="empty">Loading plan</div>');
    HI.send(ID, "purge-all", "POST", {}).then(function (p) {
      if (req !== request) return;
      if (!p || !(p.entries || []).length) { sheet('<h3>Purge all</h3><div class="empty">No originals to purge</div>'); return; }
      sheet('<h3>Purge all</h3><ul class="arc-files">' + p.entries.map(function (e) {
        return "<li><b>" + esc(e.title) + '</b><span class="sub">' + esc(size(e.bytes)) + "</span></li>";
      }).join("") + '</ul><div class="arc-sum"><span class="sub">Total</span><strong>' + esc(size(p.bytes)) + "</strong></div>" +
        '<div class="form"><div><label for="arc-purge-all">Type purge all</label><input id="arc-purge-all" autocomplete="off" spellcheck="false"></div></div><div id="arc-purge-msg" class="arc-msg" role="status"></div>' +
        '<div class="actions"><button class="btn" id="arc-purge-cancel" type="button">Cancel</button><button class="btn primary" id="arc-purge-confirm" type="button" disabled>Purge all</button></div>');
      $("arc-purge-cancel").onclick = HI.closeDrawer;
      $("arc-purge-all").oninput = function () { $("arc-purge-confirm").disabled = this.value !== "purge all" || (status || {}).dryRun; };
      $("arc-purge-confirm").onclick = function () {
        this.disabled = true;
        HI.send(ID, "purge-all", "POST", { apply: true, hash: p.hash, confirm: $("arc-purge-all").value }).then(function (r) {
          if (req !== request) return;
          r = r || {};
          sheet('<h3>Purge all</h3><div class="arc-note">Purged ' + esc(r.purged) + '</div><ul class="arc-results">' + (r.entries || []).filter(function (e) { return e.error; }).map(function (e) {
            return '<li class="bad"><b>' + esc(e.title) + "</b> · " + esc(purgeReason(e.error)) + "</li>";
          }).join("") + "</ul>");
          pollNow();
        }).catch(function (err) { if (!fail(err) && req === request) msg("arc-purge-msg", "Check status", true); });
      };
    }).catch(function (err) { if (!fail(err) && req === request) sheet('<h3>Purge all</h3><div class="arc-note">Plan unavailable</div>'); });
  }

  function ownerControl(b) {
    if (ownerBusy) return;
    var path = b.getAttribute("data-owner-path"), fields = JSON.parse(b.getAttribute("data-owner-fields"));
    ownerBusy = true; b.disabled = true; ownerNotice = "";
    HI.send(ID, path, "POST", Object.assign(fields, { apply: true })).then(function () { ownerNotice = "Owner control saved"; }, function (err) { if (!fail(err)) ownerNotice = "Control not saved"; })
      .then(function () { ownerBusy = false; pollNow(); if (st.view === "seeding") loadSeeding(); });
  }

  // ------------------------------------------------------------ library view

  function libKey(item) { return item.kind + ":" + item.id; }
  var ACTIVITY_WORD = { downloading: "Downloading", paused: "Paused", importing: "Importing", waiting: "Waiting to import", queued: "Queued", attention: "Needs attention", failed: "Failed" };
  function activityTitle(item) {
    var a = libraryActivity;
    return a && (a.titles || []).filter(function (t) { return t.kind === item.kind && t.id === item.id; })[0] || null;
  }
  function activitySeason(item, season) {
    var t = activityTitle(item);
    return t && (t.seasons || []).filter(function (s) { return s.season === season; })[0] || null;
  }
  function activityETA(seconds) {
    if (seconds < 3600) return Math.max(1, Math.floor(seconds / 60)) + " min left";
    if (seconds < 86400) return Math.floor(seconds / 3600) + "h " + Math.floor((seconds % 3600) / 60) + "m left";
    return Math.floor(seconds / 86400) + " d left";
  }
  var libActivityLevels = {downloading:'info', paused:'warning', importing:'info', waiting:'info', queued:'info', attention:'warning', failed:'critical'};
  function libraryProgress(p) {
    if (!p || !ACTIVITY_WORD[p.state]) return '';
    var word = ACTIVITY_WORD[p.state], pct = null, sub = '';
    if (p.state === 'importing' && p.total) { word += ' ' + p.imported + '/' + p.total; pct = p.imported / p.total * 100; }
    else if ((p.state === 'downloading' || p.state === 'paused') && p.percent != null) { word += ' ' + p.percent + '%'; pct = p.percent; }
    if (p.state === 'downloading' && p.eta_seconds != null) sub = activityETA(p.eta_seconds);
    var bar = pct != null ? '<span class="library-bar" aria-hidden="true"><i style="width:' + pct.toFixed(2) + '%"></i></span>' : p.state === 'importing' ? '<span class="library-bar busy" aria-hidden="true"><i></i></span>' : '';
    return '<div class="library-progress">' + pill(word, libActivityLevels[p.state]) + bar + (sub ? '<div class="sub">' + esc(sub) + '</div>' : '') + '</div>';
  }
  function libraryProgressSlot(item, season) {
    return '<div data-library-progress="' + esc(libKey(item)) + '"' + (season == null ? '' : ' data-library-season="' + season + '"') + '>' + libraryProgress(season == null ? activityTitle(item) : activitySeason(item, season)) + '</div>';
  }
  function libPill(status) {
    var word = {none:'Not archived', tagged:'Tagged', shrunk:'Shrunk', pending_approval:'Pending approval', active:'In progress', failed:'Needs review'}[status.status] || 'Not archived';
    var cls = status.status === 'shrunk' ? 'good' : status.status === 'failed' ? 'critical' : status.status === 'pending_approval' ? 'warning' : 'info';
    return pill(word,cls) + (status.date ? '<div class="sub">' + esc(new Date(status.date).toLocaleDateString()) + ' · ' + esc(size(status.savedBytes || 0)) + ' saved</div>' : '');
  }

  function loadLibrary(refresh) {
    if (libLoading) return;
    libLoading = true;
    var gen = st.gen;
    if (!library && st.view === "library") morph(st.body, '<div class="empty" data-key="load">Loading the library…</div>');
    HI.get(ID, "library" + (refresh ? "?refresh=1" : "")).then(function (v) {
      library = v;
      if (gen === st.gen) render();
    }).catch(function (err) {
      if (fail(err) || gen !== st.gen || st.view !== "library") return;
      morph(st.body, '<div class="empty" data-key="err"><div class="big">Could not load the library</div>' + esc(err.message) + '<div class="actions arc-center"><button class="btn" type="button" data-library-refresh>Try again</button></div></div>');
    }).then(function () { libLoading = false; });
  }

  // Activity belongs to the optional read-only Media plugin. A disabled or
  // unavailable plugin is deliberately silent; archive rows still render.
  function loadLibraryActivity() {
    if (st.view !== "library") return;
    var gen = st.gen;
    HI.get("media", "activity").then(function (v) {
      if (gen !== st.gen || st.view !== "library") return;
      libraryActivity = v;
      if (library) renderLibrary();
    }, function () {});
  }

  function renderLibrary() {
    if (!library) return;
    var html = '<div class="arc-head library-heading"><h2>Library</h2><span class="spacer"></span>' + archiveLibrarySwitch() + '</div><div class="kpis arc-kpis library-kpis"><div class="tile" id="library-total"></div><div class="tile" id="library-top"></div><div class="tile" id="library-saved"></div></div>' +
      '<div class="library-tools"><div class="library-filters" role="group" aria-label="Media type">' + ['all','radarr','sonarr'].map(function(k,i) {
        return '<button type="button" data-library-filter="' + k + '" aria-pressed="' + (libFilter === k) + '">' + ['All','Movies','TV'][i] + '</button>';
      }).join('') + '</div><div class="library-find"><input id="library-search" type="search" placeholder="Search titles…" aria-label="Search library titles" value="' + esc(libQuery) + '"><button class="btn" data-library-refresh>Refresh</button></div></div>' +
      (library.dryRun || !library.archiveEnabled ? '<div class="note">' + (library.dryRun ? 'Dry run' : 'Disabled') + '</div>' : '') +
      '<div id="library-notice" class="note' + (libNotice ? '' : ' hidden') + '" role="status">' + esc(libNotice) + '</div>' +
      '<div class="arc-title library-card"><table class="library-table"><thead><tr><th>Title</th><th>Type</th><th>Disk usage ↓</th><th>Quality</th><th>Tags</th><th>Archive status</th><th>Action</th></tr></thead><tbody id="library-rows"></tbody></table></div>';
    morph(st.body, html);
    renderArchiveLibraryRows();
    focusPending();
  }
  function renderArchiveLibraryRows() {
    var all = (library.items || []).slice().sort(function(a,b) { return b.sizeBytes - a.sizeBytes || a.title.localeCompare(b.title); });
    var rows = all.filter(function(item) { return (libFilter === 'all' || item.kind === libFilter) && (item.title + ' ' + item.year).toLowerCase().includes(libQuery.toLowerCase()); });
    var total = rows.reduce(function(n,item) { return n + item.sizeBytes; },0);
    var top = rows.slice(0,20).reduce(function(n,item) { return n + item.sizeBytes; },0);
    $('library-total').innerHTML = '<div class="label">Stored media</div><div class="value">' + metric(total) + '</div><div class="sub">' + rows.length + ' titles</div>';
    $('library-top').innerHTML = '<div class="label">Largest ' + Math.min(20,rows.length) + '</div><div class="value">' + (total ? (top/total*100).toFixed(0) : '0') + '%</div><div class="sub">of total</div>';
    $('library-saved').innerHTML = '<div class="label">Space saved</div><div class="value">' + metric(rows.reduce(function(n,item) { return n + ((item.archiveStatus || {}).savedBytes || 0); },0)) + '</div><div class="sub">Verified</div>';
    var maxSize = Math.max(1, (all[0] || {}).sizeBytes || 0);
    $('library-rows').innerHTML = rows.map(function(item) {
      var key = libKey(item), tv = item.kind === 'sonarr', expanded = libExpanded[key];
      var status = item.archiveStatus || {status:'none'};
      if (status.status === 'none' && item.tags.archive) status = {status:'tagged'};
      var blocked = library.dryRun || !library.archiveEnabled || ['active','failed','pending_approval'].includes(status.status);
      var quality = [item.resolution ? item.resolution + 'p' : item.quality, item.dynamicRange, item.videoCodec].filter(Boolean).join(' · ');
      var title = '<b>' + esc(item.title) + '</b>' + (item.year ? '<span class="library-year">' + item.year + '</span>' : '');
      return '<tr class="library-row"><td class="library-title">' + (tv ? '<button type="button" class="library-expand" data-library-expand="' + key + '" aria-expanded="' + !!expanded + '" aria-controls="library-seasons-' + item.id + '"><span aria-hidden="true">' + (expanded ? '▾' : '▸') + '</span>' + title + '</button>' : title) +
        '<div class="sub">' + (tv ? (item.seasons || []).length + ' seasons' : (item.runtime ? item.runtime + ' min' : 'Movie')) + '</div></td><td class="library-type">' + (tv ? 'TV' : 'Movie') + '</td>' +
        '<td class="library-size"><strong>' + esc(size(item.sizeBytes)) + '</strong><span class="library-bar" aria-hidden="true"><i style="width:' + (item.sizeBytes/maxSize*100).toFixed(2) + '%"></i></span></td><td class="library-quality">' + esc(quality || '—') + '</td>' +
        '<td class="library-tags">' + (item.tags.archive ? '<span>archive</span>' : '') + (item.tags.fourK ? '<span>4k</span>' : '') + (!item.tags.archive && !item.tags.fourK ? '<span class="sub">—</span>' : '') + '</td>' +
        '<td class="library-status">' + libPill(status) + libraryProgressSlot(item) + '</td><td class="library-action">' + (item.tags.archive ? (tv ? '<button class="btn" type="button" data-library-action="' + esc(key) + '"' + (blocked ? ' disabled' : '') + '>Seasons</button> ' : '') +
          '<button class="btn" type="button" data-library-action="' + key + '|untag"' + (blocked ? ' disabled title="Busy: a job or approval is pending"' : '') + '>Un-tag</button>' :
          '<button class="btn" type="button" data-library-action="' + esc(key) + '"' + (blocked ? ' disabled title="Busy: a job or approval is pending"' : '') + '>Archive</button>') + '</td></tr>' +
        (tv ? '<tr id="library-seasons-' + item.id + '" class="library-season-row' + (expanded ? '' : ' hidden') + '"><td colspan="7"><div class="library-seasons">' + (item.seasons || []).map(function(s) {
          return '<div class="arc-rel library-season"><b>' + (s.season ? 'Season ' + s.season : 'Specials') + '</b><span>' + esc(size(s.sizeBytes)) + (s.selected ? ' · <b>to archive</b>' : '') + '</span>' + '<div>' + libPill(s.archiveStatus || {status:'none'}) + libraryProgressSlot(item, s.season) + '</div>' +
            (s.season > 0 && !s.selected && !blocked ? '<button class="btn" type="button" data-library-action="' + key + '|s' + s.season + '">Archive</button>' : '<span></span>') + '</div>';
        }).join('') + '</div></td></tr>' : '');
    }).join('') || '<tr><td colspan="7" class="empty">No matches</td></tr>';
  }

  function search(t) {
    return HI.send(ID, "search", "POST", { title: t });
  }

  function openLibraryAction(k) {
    var untag = /\|untag$/.test(k), only = (k.match(/\|s(\d+)$/) || [])[1];
    k = k.replace(/\|(untag|s\d+)$/, "");
    var it = (library.items || []).filter(function (i) { return libKey(i) === k; })[0];
    if (!it) return;
    var editing = it.tags.archive && !untag, req = ++request;
    var before = (it.seasons || []).filter(function (s) { return s.selected; }).map(function (s) { return s.season; });
    sheet('<div class="arc-eyebrow">' + (untag ? "ARCHIVE TAG" : editing ? "SEASONS" : "ARCHIVE") + "</div><h3>" + esc(it.title) + '</h3><div class="arc-drawer-size">' + esc(size(it.sizeBytes)) + (it.year ? " · " + esc(it.year) : "") + "</div>" +
      (untag ? "" : '<label class="arc-keep"><input id="arc-fourk" type="checkbox"' + (it.tags.fourK ? " checked" : "") + "> 4K HDR</label>" +
        (it.kind === "sonarr" ? '<fieldset class="arc-picker"><legend>Seasons</legend>' + (it.seasons || []).filter(function (s) { return s.season > 0; }).map(function (s) {
          return '<label><input type="checkbox" name="arc-season" value="' + s.season + '"' + (s.selected || String(s.season) === only || !it.tags.archive && !only ? " checked" : "") + "> Season " + s.season + "<span>" + esc(size(s.sizeBytes)) + "</span></label>";
        }).join("") + "</fieldset>" : "")) +
      '<div id="arc-lib-msg" class="arc-msg" role="status"></div><div class="actions"><button class="btn" id="arc-lib-cancel" type="button">Cancel</button><button class="btn primary" id="arc-lib-apply" type="button">' + (untag ? "Remove tag" : editing ? "Save" : "Archive") + "</button></div>");
    $("arc-lib-cancel").onclick = HI.closeDrawer;
    $("arc-lib-apply").onclick = function () {
      var fourK = untag ? it.tags.fourK : $("arc-fourk").checked;
      var chosen = it.kind === "radarr" ? [] : Array.prototype.map.call(document.querySelectorAll('[name="arc-season"]:checked'), function (el) { return Number(el.value); });
      if (!untag && it.kind === "sonarr" && !chosen.length) { msg("arc-lib-msg", "Choose a season", true); return; }
      // Search only seasons that were not already chosen.
      var seasons = it.kind === "radarr" ? [0] : chosen.filter(function (n) { return before.indexOf(n) < 0; });
      $("arc-lib-apply").disabled = true;
      var tagged = false, note = "";
      var body = it.kind === "sonarr" && !untag ? { kind: it.kind, id: it.id, archive: true, fourK: fourK, seasons: chosen } : { kind: it.kind, id: it.id, archive: !untag, fourK: fourK };
      var live = function () { return req === request && drawerOpen(); };
      HI.send(ID, "tag", "POST", body).then(function (r) {
        r = r || {};
        tagged = true;
        note = untag ? "Archive tag removed." : editing ? "Seasons saved." : "Tagged.";
        if (r.searchesQueued) {
          libNotice = untag ? "Archive tag removed" : editing ? "Seasons saved" : "Archiving " + it.title;
          if (live()) HI.closeDrawer();
          pollNow();
          return;
        }
        if (untag) return;
        // The tracker allows a few searches a minute: wait out the limit
        // instead of failing, one season at a time.
        var searched = 0, failures = [], i = 0;
        function next(attempt) {
          if (i >= seasons.length) return Promise.resolve();
          var season = seasons[i];
          if (live()) msg("arc-lib-msg", "Searching " + (it.kind === "sonarr" ? "season " + season + " " : "") + "(" + (i + 1) + " of " + seasons.length + ")" + (attempt ? ", waiting for the tracker limit…" : "…"));
          return search({ kind: it.kind, id: it.id, season: season }).then(function () { searched++; i++; return next(0); }, function (err) {
            if (fail(err)) throw err;
            if (/rate limit|cycle is running|Another archive or seeding job is running|Busy, try again shortly/.test(err.message) && attempt < 8) {
              return new Promise(function (res) { setTimeout(res, 15000); }).then(function () { return next(attempt + 1); });
            }
            failures.push((it.kind === "sonarr" ? "Season " + season + ": " : "") + err.message);
            i++;
            return next(0);
          });
        }
        return next(0).then(function () {
          if (searched) note += " " + searched + " search request" + (searched === 1 ? "" : "s") + " accepted.";
          if (failures.length) note += " " + failures.join(" · ");
          if (!seasons.length && !editing) note += " Search scheduled";
        });
      }).then(function () {
        if (!tagged) return;
        libNotice = libNotice || note;
        if (note) libNotice = note.trim();
        if (live()) sheet("<h3>" + esc(it.title) + '</h3><div class="arc-note" role="status">' + esc(libNotice) + "</div>");
      }).catch(function (err) {
        if (fail(err)) return;
        if (live()) { msg("arc-lib-msg", (tagged ? "Tags were updated. " : "") + err.message, true); $("arc-lib-apply").disabled = false; }
      }).then(function () { loadLibrary(true); });
    };
  }

  // ------------------------------------------------------------ seeding view

  function eligible(p) { return !p.refusal && p.planHash && (p.mode === "link" || p.mode === "move"); }
  function linkable(p) { return !p.refusal && p.planHash && p.mode === "link" && !p.nasBytesCost; }
  function seedQueue() { return (seeding && seeding.queue) || []; }
  function seedHolds(j) { return !!(j && j.phase !== "complete" && j.phase !== "rolled_back" && j.phase !== "stopped"); }
  function phaseLabel(p) { return SEED_PHASE[p] || String(p || "").replace(/_/g, " "); }

  // A torrent name read as a title: "Some.Show.S04.2160p.WEB" is "Some Show
  // S04" and "A.Film.2024.1080p.BluRay" is "A Film (2024)".
  function torrentTitle(name) {
    var raw = String(name || ""), s = raw.replace(/\.(mkv|mp4|avi|m4v)$/i, "").replace(/\[[^\]]*\]/g, " ").replace(/[._]+/g, " ").replace(/\s+/g, " ").trim();
    var p2 = function (n) { return pad(Number(n)); };
    var tv = /^(.+?)\s+S(\d{1,2})(?:\s?E(\d{1,3}))?(?:\s?-\s?S(\d{1,2}))?(?=\s|$|-)/i.exec(s);
    if (tv) return tv[1].replace(/[\s-]+$/, "") + " S" + p2(tv[2]) + (tv[3] ? "E" + p2(tv[3]) : "") + (tv[4] ? "-S" + p2(tv[4]) : "");
    var q = /\s(?:2160p|1080p|720p|576p|480p|4K|UHD|Blu-?Ray|BDRip|BRRip|WEB(?:-?DL|-?Rip)?|HDTV|DVDRip|REMUX|HDR|x26[45]|H ?26[45]|HEVC|AVC)(?=\s|$|-)/i.exec(" " + s);
    var headPart = (q ? s.slice(0, q.index) : s).trim();
    var movie = /^(.+\S)\s+\(?((?:19|20)\d\d)\)?$/.exec(headPart);
    if (movie) return movie[1] + " (" + movie[2] + ")";
    return headPart || raw;
  }

  function seedResults(q) {
    var rs = (seeding.queueResults || []), last = rs.length ? Date.parse(rs[rs.length - 1].at) : 0;
    if (!rs.length || (!q.length && Date.now() - last > 10 * 60000)) return '';
    var moved = rs.filter(function(r) { return r.result === 'done' && r.mode === 'move'; }).length;
    var done = rs.filter(function (r) { return r.result === 'done' && r.mode !== 'move'; }).length, bad = rs.filter(function (r) { return r.result !== 'done'; });
    return '<ul class="seed-results" data-key="seed-results">' + (done ? '<li>Linked ' + done + '</li>' : '') + (moved ? '<li>Moved ' + moved + '</li>' : '') + bad.map(function (r) {
      return '<li class="bad"><b>' + esc(torrentTitle(r.name)) + '</b> · ' + (r.result === 'failed' ? 'Failed' : 'Skipped') + (r.reason ? ' · ' + esc(r.reason) : '') + '</li>';
    }).join('') + '</ul>';
  }

  function seedPlans() {
    var j = seeding.job, q = seedQueue(), pos = {};
    q.forEach(function (e, i) { pos[e.hash] = i + 1; });
    var free = !q.length && !seedHolds(j);
    var links = seeding.items.filter(function (p) { return linkable(p) && !pos[p.hash]; }).slice(0, SEED_QUEUE_MAX);
    var moves = seeding.items.filter(function (p) { return eligible(p) && !pos[p.hash]; }).slice(0, SEED_QUEUE_MAX);
    return { j: j, q: q, pos: pos, links: links, moves: moves, canLink: links.length > 0 && free, canMove: moves.length > 0 && free };
  }

  function renderSeeding() {
    if(!seeding) return;
    var j=seeding.job, q=seedQueue(), pos={};
    q.forEach(function (e, i) { pos[e.hash] = i + 1; });
    var links=seeding.items.filter(function (p) { return linkable(p) && !pos[p.hash]; }).slice(0, SEED_QUEUE_MAX), canLink=links.length > 0 && !q.length && !seedHolds(j);
    var moves=seeding.items.filter(function(p) { return eligible(p) && !pos[p.hash]; }).slice(0, SEED_QUEUE_MAX), canMove=moves.length > 0 && !q.length && !seedHolds(j);
    var moving=j && SEED_ACTIVE.indexOf(j.phase) >= 0 ? j.plan.hash : '';
    morph(st.body,'<div class="arc-head"><h2>Seeding</h2><span class="spacer"></span>'+archiveLibrarySwitch()+'<button class="btn" data-seed-all'+(canLink?'':' disabled')+'>Link all ('+links.length+')</button><button class="btn" data-seed-move-all'+(canMove?'':' disabled')+'>Move all ('+moves.length+')</button>'+(q.length?'<button class="btn" data-seed-queue-cancel>Cancel queue</button>':'')+'<button class="btn" data-seed-refresh>Refresh</button></div>'+seedErrorText(j)+
      (moving ? '<div class="arc-now"><b>' + esc(torrentTitle(j.plan.name)) + '</b><div class="note">' + esc(j.stopRequested ? 'Stop requested' : phaseLabel(j.phase)) + '</div>' + ownerButton('seeding/stop','Stop',{hash:moving},j.stopRequested) + '</div>' : '') +
      (q.length ? '<div class="arc-queue"><h4>' + (q.some(function(e) { return e.mode === 'move'; }) ? 'Queue' : 'Link queue') + '</h4>' + q.map(function(e,i) { return '<div class="arc-q"><span class="n">' + (i+1) + '.</span><span class="what"><b>' + esc(torrentTitle(e.name)) + '</b></span>' + ownerOrder('seeding/queue/reorder',{hash:e.hash},i,q.length) + '</div>'; }).join('') + '</div>' : '') +
      seedResults(q) +
      (seeding.items.length ? seeding.items.map(function(p) {
        var k = pos[p.hash], sub = p.hash === moving ? phaseLabel(j.phase) : k ? 'Queued ' + k : p.refusal || 'Frees '+size(p.nvmeBytesFreed);
        return '<button class="seed-row" data-seed-hash="'+esc(p.hash)+'"'+(p.refusal||k?' disabled':'')+'><span class="seed-name"><b>'+esc(p.name)+'</b><span class="sub">'+esc(sub)+'</span></span><span class="seed-size">'+(p.mode?pill(p.mode==='link'?'Link':'Move',p.mode==='link'?'good':'info'):'')+'<span class="sub">'+esc(size(p.size))+'</span></span></button>';
      }).join(''):'<div class="empty">No torrents on NVMe</div>'));
    focusPending();
  }
  function seedErrorText(j) {
    return j && (j.phase === 'halted' || j.phase === 'failed' || j.phase === 'stopped') ? '<div id="seed-error" class="arc-msg warn" role="status" tabindex="-1"><b>'+esc(j.plan.name)+'</b> · '+esc(phaseLabel(j.phase))+(j.error?'<div class="sub">'+esc(j.error)+'</div>':'')+'</div>' : '';
  }

  function loadSeeding() {
    if (seedLoading) return;
    seedLoading = true;
    var gen = st.gen;
    HI.get(ID, "seeding").then(function (v) {
      seeding = v; seeding.items = seeding.items || [];
      if (gen === st.gen) render();
    }).catch(function (err) {
      if (fail(err) || gen !== st.gen || st.view !== "seeding") return;
      morph(st.body, '<div class="empty" data-key="err"><div class="big">Seeding mover unavailable</div>' + esc(err.message) + "</div>");
    }).then(function () { seedLoading = false; });
  }

  // Every seeding write confirms in one sheet: run posts, ok follows success.
  function seedSheet(headHTML, note, keep, label, run, ok, off) {
    var req = ++request;
    sheet(headHTML + '<div id="arc-seed-msg" class="arc-msg" role="status">' + note + '</div><div class="actions"><button class="btn" id="arc-seed-cancel" type="button">' + keep + '</button><button class="btn primary" id="arc-seed-confirm" type="button"' + (off ? " disabled" : "") + ">" + label + "</button></div>");
    $("arc-seed-cancel").onclick = HI.closeDrawer;
    $("arc-seed-confirm").onclick = function () {
      this.disabled = true;
      run().then(function (r) {
        if (req !== request) return;
        ok(r || {}); loadSeeding(); pollNow();
      }).catch(function (e) {
        if (fail(e) || req !== request || !$("arc-seed-msg")) return;
        msg("arc-seed-msg", e instanceof TypeError ? "Check status" : e.message, true);
        $("arc-seed-confirm").disabled = false;
      });
    };
  }

  function confirmAll(allowMove) {
    var label = allowMove ? "Move" : "Link", list = seeding.items.filter(allowMove ? eligible : linkable).slice(0, SEED_QUEUE_MAX);
    if (!list.length) return;
    var freed = list.reduce(function (n, p) { return n + p.nvmeBytesFreed; }, 0), cost = list.reduce(function (n, p) { return n + (p.nasBytesCost || 0); }, 0);
    seedSheet("<h3>" + label + ' all</h3><div class="arc-metrics"><div><span class="sub">Frees download disk</span><strong>' + esc(size(freed)) + '</strong></div><div><span class="sub">NAS cost</span><strong>' + esc(size(cost)) + "</strong></div></div>" +
      '<ul class="arc-files">' + list.map(function (p) { return "<li><b>" + esc(p.name) + "</b> " + pill(p.mode === "move" ? "Move" : "Link", p.mode === "move" ? "info" : "good") + '<span class="sub">' + esc(size(p.size)) + "</span></li>"; }).join("") + "</ul>",
      "One at a time", "Cancel", label + " " + list.length,
      function () {
        var body = { items: list.map(function (p) { return { hash: p.hash, planHash: p.planHash }; }), apply: true };
        if (allowMove) body.allowMove = true;
        return HI.send(ID, "seeding/queue", "POST", body).catch(function (e) {
          if (!allowMove || fail(e)) throw e;
          throw new Error(/plan changed/i.test(e.message) ? "Plan changed" : e instanceof TypeError ? "Check status" : "Move refused");
        });
      },
      function (r) { sheet("<h3>" + label + ' all</h3><div class="arc-note">Queued ' + esc(r.queued) + "</div>"); });
  }

  function confirmCancelQueue() {
    seedSheet('<h3>Cancel queue</h3><div class="arc-sum"><span class="sub">Waiting</span><strong>' + seedQueue().length + "</strong></div>",
      "The running move continues", "Keep", "Cancel queue",
      function () { return HI.send(ID, "seeding/queue/cancel", "POST", { apply: true }); }, HI.closeDrawer);
  }

  function previewSeeding(hash) {
    var req = ++request;
    sheet('<h3>Move seeding data</h3><div class="empty">Loading…</div>');
    HI.send(ID, "seeding/preview", "POST", { hash: hash }).then(function (p) {
      if (req !== request || !drawerOpen()) return;
      var moves = (p.libraryMoves || []).map(function (m) { return "<li>" + esc(m.from) + " → " + esc(m.to) + "</li>"; }).join("");
      seedSheet('<h3 class="arc-seed-title">' + esc(p.name) + "</h3>" + pill(p.mode === "link" ? "Link" : "Move", p.mode === "link" ? "good" : "info") +
        '<div class="arc-metrics"><div><span class="sub">Frees download disk</span><strong>' + esc(size(p.nvmeBytesFreed)) + '</strong></div><div><span class="sub">Costs NAS</span><strong>' + esc(size(p.nasBytesCost)) + "</strong></div></div>" +
        (moves ? '<h4>Move library first</h4><ul class="arc-files">' + moves + "</ul>" : "") +
        (p.originalDownloadPath ? '<div class="arc-note" title="Left empty after completion or rollback">Download path cleared</div>' : "") +
        '<ul class="arc-files">' + (p.files || []).map(function (f) { return "<li><b>" + esc(f.relative) + '</b><span class="sub">' + esc(size(f.size)) + " · " + (f.match ? "Library match" : "Copy") + "</span></li>"; }).join("") + "</ul>",
        p.dryRun ? "Dry run" : "", "Cancel", "Move to NAS",
        function () { return HI.send(ID, "seeding/move", "POST", { hash: p.hash, planHash: p.planHash, apply: true }); },
        function () { sheet("<h3>" + esc(p.name) + '</h3><div class="arc-note">Queued</div>'); }, p.dryRun);
    }).catch(function (e) { if (!fail(e) && req === request) sheet('<h3>Move seeding data</h3><div class="arc-note">' + esc(e.message) + "</div>"); });
  }

  // ------------------------------------------------------------ selection view

  function copy(v) { return JSON.parse(JSON.stringify(v)); }
  function num(id, label, value, min, max) {
    return '<div><label for="' + id + '">' + label + '</label><input id="' + id + '" type="number" inputmode="decimal" step="any" min="' + min + '"' + (max == null ? "" : ' max="' + max + '"') + ' value="' + esc(value) + '"></div>';
  }
  function groups(kind, label) {
    return '<fieldset><legend>' + label + '</legend><div class="arc-chips">' +
      draft[kind].map(function (g, i) { return '<button class="btn arc-chip" type="button" data-sc-group="' + kind + '" data-sc-index="' + i + '" aria-label="Remove ' + esc(g) + '">' + esc(g) + " ×</button>"; }).join("") +
      '</div><div class="arc-add"><input id="arc-sc-' + kind + '" aria-label="' + label + ' group" placeholder="Group" autocomplete="off"><button class="btn" type="button" data-sc-add="' + kind + '">Add</button></div></fieldset>';
  }
  function previewHTML() {
    if (!preview) return "";
    var pick = function (c) { return c ? esc(c.group || "Unlisted") + " · " + Number((c.score || {}).total || 0).toFixed(1) : "No pick"; };
    var full = function (c) { return c ? esc(c.title) : "No pick"; };
    return '<div class="arc-sc-preview"><b>Auto picks ' + esc(preview.currentAutoPicks) + " → " + esc(preview.autoPicks) + "</b>" +
      ((preview.titles || []).length ? preview.titles.map(function (t) {
        return '<details class="arc-sc-change"><summary>' + esc(t.target.title) + (t.target.season ? " S" + pad(t.target.season) : "") + '<div class="arc-sc-picks"><div><small>Current</small>' + pick(t.current) + "</div><div><small>New</small>" + pick(t.proposed) + "</div></div></summary>" +
          '<div class="arc-sc-picks"><div>' + full(t.current) + "</div><div>" + full(t.proposed) + "</div></div></details>";
      }).join("") : '<div class="arc-muted">No cached releases</div>') + "</div>";
  }

  function loadSelection() {
    var gen = st.gen;
    HI.get(ID, "selection").then(function (v) { selection = v; draft = null; preview = null; if (gen === st.gen) { if (st.view === "admin") renderSelection(); else render(); } }).catch(function (err) {
      if (fail(err) || gen !== st.gen || (st.view !== "selection" && st.view !== "admin")) return;
      morph(st.body, '<div class="empty" data-key="err"><div class="big">Scoring settings unavailable</div>' + esc(err.message) + "</div>");
    });
  }

  function renderSelection() {
    if (!selection) { morph(st.body, '<div data-key="load">' + HI.skeleton(4) + "</div>"); loadSelection(); return; }
    if (!draft) draft = copy(selection.effective);
    var d = draft, off = scBusy ? " disabled" : "";
    // A full replace keeps the form simple: typing never races a re-render
    // because number inputs update the draft without re-rendering.
    var inAdmin = st.view === "admin";
    st.body.innerHTML = '<section class="arc-sc' + (inAdmin ? "" : " card") + '" id="arc-sc">' +
      (inAdmin ? "" : '<p class="subtitle">How the service scores freeleech releases. Preview uses cached releases only; saving changes cached scores without starting a search or a grab.</p>') +
      (selection.warning ? '<div class="arc-msg warn" role="status">Invalid overrides · File policy</div>' : "") +
      '<div class="form">' + ["1080p", "2160p"].map(function (mode) {
        return '<fieldset><legend>' + mode + ' · GB/h</legend><div class="arc-sc-fields">' + ["floor", "target", "cap"].map(function (k) {
          return num("arc-sc-" + mode + "-" + k, k[0].toUpperCase() + k.slice(1), d.sizes[mode][k], 0.1, 100);
        }).join("") + "</div></fieldset>";
      }).join("") +
      '<fieldset><legend>Codecs</legend><div class="arc-sc-fields">' + ["hevc", "avc", "av1"].map(function (k) { return num("arc-sc-" + k, k.toUpperCase(), d.codecFactors[k], 0.1, 10); }).join("") + "</div></fieldset>" +
      groups("trustedGroups", "Trusted groups") + groups("deniedGroups", "Denied groups") +
      '<label for="arc-sc-min">Min score</label><input class="arc-sc-min" id="arc-sc-min" type="number" inputmode="decimal" step="any" min="0" max="100" value="' + esc(d.minScore) + '">' +
      '<div class="actions"><button class="btn" type="button" id="arc-sc-preview"' + off + '>Preview</button><button class="btn primary" type="button" id="arc-sc-save"' + (!preview || scBusy ? " disabled" : "") + '>Save</button><button class="btn" type="button" id="arc-sc-reset"' + off + ">Reset</button></div>" +
      '<div class="arc-msg" id="arc-sc-msg" role="status"></div><div id="arc-sc-results">' + previewHTML() + "</div></div></section>";
  }

  // Only fields that differ from the file are sent, so the rest keep
  // inheriting it.
  function readSelection() {
    var ok = true, d = draft;
    ["1080p", "2160p"].forEach(function (mode) {
      ["floor", "target", "cap"].forEach(function (k) {
        var input = $("arc-sc-" + mode + "-" + k); if (!input.value || !input.checkValidity()) ok = false;
        d.sizes[mode][k] = Number(input.value);
      });
      var b = d.sizes[mode]; if (!(b.floor > 0 && b.floor < b.target && b.target < b.cap)) ok = false;
    });
    ["hevc", "avc", "av1"].forEach(function (k) { var input = $("arc-sc-" + k); if (!input.value || !input.checkValidity() || Number(input.value) <= 0) ok = false; d.codecFactors[k] = Number(input.value); });
    var input = $("arc-sc-min"); if (!input.value || !input.checkValidity()) ok = false; d.minScore = Number(input.value);
    var file = selection.file, o = { sizes: {}, codecFactors: {} };
    ["1080p", "2160p"].forEach(function (mode) { var b = {}; ["floor", "target", "cap"].forEach(function (k) { if (d.sizes[mode][k] !== file.sizes[mode][k]) b[k] = d.sizes[mode][k]; }); if (Object.keys(b).length) o.sizes[mode] = b; });
    ["hevc", "avc", "av1"].forEach(function (k) { if (d.codecFactors[k] !== file.codecFactors[k]) o.codecFactors[k] = d.codecFactors[k]; });
    ["trustedGroups", "deniedGroups"].forEach(function (k) { if (JSON.stringify(d[k]) !== JSON.stringify(file[k] || [])) o[k] = d[k]; });
    if (d.minScore !== file.minScore) o.minScore = d.minScore;
    if (!ok) { msg("arc-sc-msg", "Check the values: floor < target < cap, factors above zero, score 0–100", true); return null; }
    return { overrides: o };
  }
  function selectionChanged() {
    preview = null;
    var save = $("arc-sc-save"); if (save) save.disabled = true;
    var res = $("arc-sc-results"); if (res) res.textContent = "";
    msg("arc-sc-msg", "Changed · preview before saving");
  }
  function addGroup(kind) {
    var input = $("arc-sc-" + kind), values = input.value.split(",").map(function (g) { return g.trim(); }).filter(Boolean);
    if (!values.length) return;
    values.forEach(function (g) { if (!draft[kind].some(function (x) { return x.toLowerCase() === g.toLowerCase(); })) draft[kind].push(g); });
    preview = null; renderSelection(); msg("arc-sc-msg", "Changed · preview before saving");
    var again = $("arc-sc-" + kind); if (again) again.focus();
  }
  function selectionWrite(reset) {
    var body = reset ? { reset: true } : readSelection(); if (!body) return;
    scBusy = true; renderSelection(); msg("arc-sc-msg", reset ? "Resetting" : "Saving");
    HI.send(ID, "selection", "PUT", body).then(function (r) {
      selection = r; draft = null; preview = null; scBusy = false; renderSelection(); msg("arc-sc-msg", reset ? "Reset" : "Saved");
    }, function (err) { scBusy = false; renderSelection(); if (!fail(err)) msg("arc-sc-msg", "Failed: " + err.message, true); });
  }
  function selectionPreview() {
    var body = readSelection(); if (!body) return;
    scBusy = true; renderSelection(); msg("arc-sc-msg", "Checking");
    HI.send(ID, "selection/preview", "POST", body).then(function (r) {
      preview = r; scBusy = false; renderSelection(); msg("arc-sc-msg", "Ready");
    }, function (err) { scBusy = false; renderSelection(); if (!fail(err)) msg("arc-sc-msg", "Failed: " + err.message, true); });
  }

  // ------------------------------------------------------------ activity strip
  //
  // One poll feeds the strip under the header, the job card and the queues.
  // It runs fast while anything moves, slow when idle, never overlaps itself,
  // backs off on errors and stops while the page is hidden.

  function secs(s) {
    var m = /^(?:(\d+)\.)?(\d+):(\d+):(\d+)$/.exec(s || "");
    return m ? Number(m[1] || 0) * 86400 + Number(m[2]) * 3600 + Number(m[3]) * 60 + Number(m[4]) : null;
  }
  function clamp(n) { return Math.max(0, Math.min(100, n)); }
  function eta(s) {
    if (s < 3600) return Math.max(1, Math.floor(s / 60)) + " min left";
    if (s < 86400) return Math.floor(s / 3600) + "h " + Math.floor((s % 3600) / 60) + "m left";
    return Math.floor(s / 86400) + " d left";
  }
  function empty() { return { running: [], queued: [], failed: [] }; }
  function merge(x, y) { return { running: x.running.concat(y.running), queued: x.queued.concat(y.queued), failed: x.failed.concat(y.failed) }; }

  function rowsArchive(v) {
    var out = empty();
    if (!v || !v.reachable) return out;
    var a = v.archive || {}, j = a.job, go = { view: "status" };
    if (active(a)) {
      var p = j.progress || {}, r = { key: "archive:" + jobID(j), title: title(j.target), pct: null, eta: null, bytes: null, go: go };
      if (j.phase === "downloading") {
        r.word = a.paused ? "Held" : "Downloading";
        if (p.bytesTotal > 0) { r.bytes = p.bytesTotal; r.pct = clamp((p.bytesTotal - (p.bytesLeft || 0)) / p.bytesTotal * 100); r.word += " " + Math.floor(r.pct) + "%"; }
        if (a.paused) r.level = "warning";
        else r.eta = real(p.eta) ? Math.max(0, (Date.parse(p.eta) - Date.now()) / 1000) : secs(p.timeLeft);
      } else r.word = j.phase === "needs_approval" ? (j.forceImport ? "Approved" : "Needs approval") : j.phase === "verifying" ? "Verifying" : j.phase === "import_intent" ? "Importing" : "Starting";
      out.running.push(r);
    }
    failedJobs(a).forEach(function (f) { out.failed.push({ key: "archive:" + jobID(f), title: title(f.target), word: "Stopped", level: "critical", go: go }); });
    (v.archiveSearches || []).forEach(function (q) {
      if (q.state === "done" || q.state === "failed" && q.finished && Date.now() - Date.parse(q.finished) > LINGER) return;
      var r = { key: "search:" + archiveKey(q.title), title: q.name + (q.title.kind === "sonarr" ? " S" + pad(q.title.season) : ""),
        word: q.state === "searching" ? "Searching" : q.state === "failed" ? "Failed" : "Queued", pct: null, eta: null, bytes: null, go: { view: "status", title: archiveKey(q.title) } };
      if (q.state === "failed") { r.level = "critical"; out.failed.push(r); } else if (q.state === "searching") out.running.push(r); else out.queued.push(r);
    });
    (a.approvals || []).forEach(function (q) {
      var d = (a.report || []).filter(function (r) { return archiveKey(r.target) === archiveKey(q); })[0], why = q.blocked ? reasonLabel(q.message || "") : "";
      out.queued.push({ key: "approval:" + archiveKey(q) + ":" + q.releaseId, title: d ? title(d.target) : q.kind === "sonarr" ? "Series" : "Movie",
        word: q.blocked ? "Blocked" + (why && why !== "Details" ? " · " + why : "") : "Approved", level: q.blocked ? "warning" : "", go: { view: "status", title: archiveKey(q) } });
    });
    return out;
  }

  function rowsSeeding(v) {
    var out = empty(), j = v && v.reachable ? v.seeding : null;
    (v && v.reachable ? v.seedingQueue || [] : []).forEach(function (e) {
      out.queued.push({ key: "seedq:" + e.hash, title: torrentTitle(e.name), name: e.name, word: "Queued", go: { view: "seeding", hash: e.hash } });
    });
    if (!j || !j.plan) return out;
    var k = "seed:" + j.plan.hash + ":" + j.started, go = { view: "seeding", hash: j.plan.hash }, t = torrentTitle(j.plan.name);
    if (SEED_ACTIVE.indexOf(j.phase) >= 0) {
      var r = { key: k, title: t, name: j.plan.name, word: phaseLabel(j.phase), pct: null, eta: null, bytes: null, go: go }, live = j.live;
      if (live && /^checking/.test(live.state) && (j.phase === "rechecking" || j.phase === "rolling_back")) {
        r.pct = clamp(live.progress * 100); r.bytes = j.plan.size > 0 ? j.plan.size : null; r.word = "Checking " + Math.floor(r.pct) + "%";
      }
      out.running.push(r);
    } else if (j.phase === "halted" || j.phase === "failed" || j.phase === "stopped") {
      out.failed.push({ key: k, title: t, name: j.plan.name, word: "Stopped", level: "critical", go: go });
    }
    return out;
  }

  function model() {
    var m = merge(rowsArchive(lastStatus), rowsSeeding(lastStatus)), now = Date.now(), seen = {};
    m.running.concat(m.queued, m.failed).forEach(function (r) { seen[r.key] = true; });
    Object.keys(linger).forEach(function (k) { if (linger[k].until <= now || seen[k]) delete linger[k]; });
    m.done = Object.keys(linger).map(function (k) { return linger[k].row; });
    return m;
  }

  // A running row that vanished with evidence it finished stays as Done for
  // a moment. One that vanished without evidence just goes.
  function lingerGone(before, after, finished) {
    var seen = {};
    after.running.concat(after.queued, after.failed).forEach(function (r) { seen[r.key] = true; });
    before.running.forEach(function (r) {
      if (seen[r.key] || !finished(r)) return;
      linger[r.key] = { until: Date.now() + LINGER, row: Object.assign({}, r, { word: "Done", pct: 100, eta: null, level: "", done: true }) };
    });
  }

  // Byte-weighted; a row of unknown size weighs the mean of the known ones.
  function aggregate(rows) {
    var known = rows.filter(function (r) { return r.pct != null; });
    if (!known.length) return null;
    var sized = known.filter(function (r) { return r.bytes > 0; });
    var mean = sized.length ? sized.reduce(function (n, r) { return n + r.bytes; }, 0) / sized.length : 1, total = 0, done = 0;
    known.forEach(function (r) { var w = r.bytes > 0 ? r.bytes : mean; total += w; done += w * r.pct / 100; });
    return done / total * 100;
  }

  function opHTML(r, bar) {
    goTargets[r.key] = r.go;
    var entry = linger[r.key], fading = r.done && entry && entry.until - Date.now() <= FADE;
    var meta = r.word + (r.eta > 0 ? ' · ' + activityETA(r.eta) : '');
    return '<button type="button" class="arc-op' + (r.level ? ' ' + r.level : '') + (r.done ? ' done' : '') + (fading ? ' fade' : '') + '" data-key="' + esc(r.key) + '" data-arc-go="' + esc(r.key) + '"' + (r.name ? ' title="' + esc(r.name) + '"' : '') + '>' +
      '<span class="arc-op-t">' + esc(r.title) + '</span><span class="arc-op-w">' + esc(meta) + '</span>' +
      (bar ? '<span class="library-bar' + (r.pct == null ? ' busy' : '') + '" role="progressbar" aria-label="' + esc(r.title) + '" aria-valuemin="0" aria-valuemax="100"' +
        (r.pct == null ? '' : ' aria-valuenow="' + Math.round(r.pct) + '"') + '><i style="width:' + (r.pct == null ? 35 : r.pct.toFixed(1)) + '%"></i></span>' : '') + '</button>';
  }

  function renderStrip() {
    if (!st.strip) return;
    var head = st.body.querySelector(".arc-head");
    if (head && head.nextSibling !== st.strip) st.body.insertBefore(st.strip, head.nextSibling);
    var el = st.strip, m = model(), live = el.querySelector('.arc-live');
    if (!m.running.length && !m.queued.length && !m.failed.length && !m.done.length) {
      el.hidden = true;
      setLive(live, '');
      return;
    }
    var pct = aggregate(m.running), n = m.running.length;
    if (pct != null) pct = Math.round(pct);
    var sum = n ? n + ' active' : m.failed.length ? m.failed.length + ' failed' : m.queued.length ? 'Queued ' + m.queued.length : 'Done';
    goTargets = {};
    morph(el.firstChild, '<button type="button" class="arc-strip-head" data-arc-toggle aria-expanded="' + stripOpen + '" aria-controls="arc-strip-body">' +
      '<span class="arc-strip-sum">' + esc(sum) + '</span>' + (n && m.failed.length ? '<span class="arc-strip-fail">' + m.failed.length + ' failed</span>' : '') +
      '<span class="spacer"></span><span class="arc-strip-pct">' + (pct == null ? '' : pct + '%') + '</span><span class="arc-strip-chev" aria-hidden="true">' + (stripOpen ? '▾' : '▸') + '</span></button>' +
      (n ? bar(pct, pct == null ? 'indeterminate' : '', 'Overall progress') : '') +
      '<div id="arc-strip-body" data-key="body" class="arc-strip-body' + (stripOpen ? '' : ' hidden') + '">' +
      m.failed.map(function (r) { return opHTML(r, false); }).join('') +
      m.running.map(function (r) { return opHTML(r, true); }).join('') +
      m.done.map(function (r) { return opHTML(r, true); }).join('') +
      // Queued alone is already the header, so its rows sit flat.
      (!m.queued.length ? '' : n || m.failed.length || m.done.length ? '<details class="arc-strip-q" data-key="queued"><summary>Queued ' + m.queued.length + '</summary>' + m.queued.map(function (r) { return opHTML(r, false); }).join('') + '</details>' : m.queued.map(function (r) { return opHTML(r, false); }).join('')) +
      '</div>');
    el.hidden = false;
    scheduleLinger();
    // Announce phases, never percentages, so a tick stays quiet.
    setLive(live, m.failed.concat(m.running, m.done).map(function (r) { return r.word.replace(/\s*[\d·].*$/, '') + ' ' + r.title; }).join('. '));
  }
  function setLive(el, text) {
    if (liveText === null) { liveText = text; return; }
    if (text === liveText) return;
    liveText = text;
    el.textContent = text;
  }

  function scheduleLinger() {
    clearTimeout(lingerTimer);
    var now = Date.now(), next = Infinity;
    Object.keys(linger).forEach(function (k) { var u = linger[k].until; next = Math.min(next, u - FADE > now ? u - FADE : u); });
    ((lastStatus || {}).archiveSearches || []).forEach(function (q) { var u = Date.parse(q.finished) + LINGER; if (q.state === "failed" && u > now) next = Math.min(next, u); });
    if (next < Infinity) lingerTimer = setTimeout(renderStrip, Math.max(0, next - now) + 20);
  }

  function applyStatus(v) {
    var last = lastStatus, sig = function (x) { if (!x) return ""; var c = Object.assign({}, x); delete c.fetched; return JSON.stringify(c); };
    var changed = sig(status) !== sig(v);
    status = v;
    if (v.reachable) {
      lastStatus = v;
      if (seeding) { seeding.job = v.seeding; seeding.queue = v.seedingQueue || []; seeding.queueResults = v.seedingQueueResults || []; }
      if (last) {
        var a = v.archive || {}, s = v.seeding;
        lingerGone(merge(rowsArchive(last), rowsSeeding(last)), merge(rowsArchive(v), rowsSeeding(v)), function (r) {
          if (r.key.indexOf("archive:") === 0) {
            var id = r.key.slice(8);
            return (a.job && a.job.phase === "complete" && jobID(a.job) === id) || (a.history || []).some(function (h) { return h.job && h.status !== "ended" && !/failed/.test(h.status) && jobID(h.job) === id; });
          }
          return !!(s && s.plan && s.phase === "complete" && r.key === "seed:" + s.plan.hash + ":" + s.started);
        });
        // A finished phase refreshes the list it changed, once.
        var was = active(last.archive) ? jobID(last.archive.job) : "", now = active(a) ? jobID(a.job) : "";
        if (was && was !== now && library) loadLibrary(true);
        var s0 = last.seeding, s1 = v.seeding, moved = s0 && SEED_ACTIVE.indexOf(s0.phase) >= 0 && !(s1 && SEED_ACTIVE.indexOf(s1.phase) >= 0 && s1.plan.hash === s0.plan.hash);
        if ((moved || (last.seedingQueue || []).length !== (v.seedingQueue || []).length) && seeding) loadSeeding();
      }
    }
    if (changed && (st.view === "status" || st.view === "seeding")) render();
    else renderTools();
    renderStrip();
  }

  function moving() {
    var m = model(), info = (status || {}).info || {};
    return m.running.length > 0 || m.done.length > 0 || m.queued.some(function (r) { return r.key.indexOf("search:") === 0; }) || !!(info.runNow || {}).pending;
  }

  function tick() {
    clearTimeout(poll.timer); poll.timer = null;
    if (!st.root || !HI.visible()) return;
    if (poll.busy) { poll.again = true; return; }
    poll.busy = true; poll.ran = true;
    var gen = st.gen;
    if (st.view === "library") loadLibraryActivity();
    HI.get(ID, "status").then(function (v) {
      if (gen !== st.gen) return;
      applyStatus(v);
      poll.fails = v.reachable === false ? poll.fails + 1 : 0;
    }, function (err) {
      if (gen !== st.gen) return;
      poll.fails++;
      if (!status && !fail(err) && st.view === "status") morph(st.body, '<div class="empty" data-key="err"><div class="big">Could not load the archive</div>' + esc(err.message) + "</div>");
    }).then(function () {
      poll.busy = false; poll.last = Date.now();
      if (gen !== st.gen) return;
      if (poll.again) { poll.again = false; tick(); } else schedule();
    });
  }

  // The next poll is due a fixed time after the last one finished.
  function schedule() {
    clearTimeout(poll.timer); poll.timer = null;
    if (!st.root || poll.busy) return;
    if (!HI.visible()) return;
    if (!poll.ran) { tick(); return; }
    var base = moving() ? FAST : IDLE;
    var due = poll.last + (poll.fails ? Math.min(MAX, base * Math.pow(2, poll.fails)) : base);
    poll.timer = setTimeout(tick, Math.max(0, due - Date.now()));
  }
  function pollNow() { if (poll.busy) poll.again = true; else tick(); }
  function onVisibility() { if (HI.visible()) pollNow(); else { clearTimeout(poll.timer); poll.timer = null; } }

  // Open what a strip row stands for, in its own view.
  function openTarget(t) {
    stripOpen = true;
    pending = t;
    var p = new URLSearchParams();
    if (t.title) p.set("title", t.title);
    if (t.hash) p.set("hash", t.hash);
    if (t.view === st.view) { focusPending(); return; }
    HI.go(HI.link(ID, t.view, p.toString()));
  }

  function focusPending() {
    var t = pending, el = null;
    if (!t || t.view !== st.view) return;
    if (t.view === "status" && t.title) {
      if (!status || !status.reachable) return;
      var g = "archive:title:" + t.title, d = st.body.querySelector('details.arc-title[data-group="' + CSS.escape(g) + '"]');
      if (d && !d.open) { open[g] = true; pending = null; renderStatus(); d = st.body.querySelector('details.arc-title[data-group="' + CSS.escape(g) + '"]'); }
      el = (d && d.querySelector("summary")) || st.body.querySelector(".arc-now");
    } else if (t.view === "status") el = st.body.querySelector(".arc-now");
    else if (t.view === "seeding") {
      if (!seeding) return;
      var rowEl = st.body.querySelector('[data-seed-hash="' + CSS.escape(t.hash || "") + '"]');
      el = (!rowEl || rowEl.disabled) && $("seed-error") ? $("seed-error") : rowEl;
    }
    pending = null;
    if (!el) return;
    el.scrollIntoView({ block: "center", behavior: matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth" });
    var f = el.matches("button:not([disabled]), summary, [tabindex='-1']") ? el : el.querySelector("button:not([disabled])");
    if (f) f.focus({ preventScroll: true });
  }

  // ------------------------------------------------------------ frame

  // The header's trailing tools change with the view and the pause state.
  function renderTools() { /* Tools live in the source's arc-head for each view. */ }

  function render() {
    if (!st.root) return;
    renderTools();
    if (st.view === "library") renderLibrary();
    else if (st.view === "seeding") renderSeeding();
    else if (st.view === "selection") renderSelection();
    else renderStatus();
    renderStrip();
  }

  function onClick(e) {
    var b = e.target.closest ? e.target.closest("button, [data-arc-go]") : null;
    if (!b || b.disabled || !st.root.contains(b)) return;
    var a = b.getAttribute.bind(b);
    if (a("data-library-link")) HI.go(a("data-library-link"));
    else if (a("data-owner-path")) ownerControl(b);
    else if (a("data-archive-action")) openAction(a("data-archive-action"), a("data-archive-id"));
    else if (a("data-score-toggle")) {
      var id = a("data-score-toggle"), panel = $(id), ex = a("aria-expanded") !== "true";
      notice[id] = ex; b.setAttribute("aria-expanded", String(ex)); if (panel) panel.classList.toggle("hidden", !ex);
    }
    else if (b.hasAttribute("data-library-refresh")) { libNotice = ""; loadLibrary(true); }
    else if (a("data-library-filter")) { libFilter = a("data-library-filter"); renderLibrary(); }
    else if (a("data-library-expand")) { var k = a("data-library-expand"); libExpanded[k] = !libExpanded[k]; renderLibrary(); }
    else if (a("data-library-action")) openLibraryAction(a("data-library-action"));
    else if (b.hasAttribute("data-seed-refresh")) loadSeeding();
    else if (a("data-seed-hash")) previewSeeding(a("data-seed-hash"));
    else if (b.hasAttribute("data-seed-all")) confirmAll(false);
    else if (b.hasAttribute("data-seed-move-all")) confirmAll(true);
    else if (b.hasAttribute("data-seed-queue-cancel")) confirmCancelQueue();
    else if (b.hasAttribute("data-arc-toggle")) { stripOpen = !stripOpen; renderStrip(); }
    else if (a("data-arc-go")) { var t = goTargets[a("data-arc-go")]; if (t) openTarget(t); }
    else if (a("data-sc-add")) addGroup(a("data-sc-add"));
    else if (a("data-sc-group")) { draft[a("data-sc-group")].splice(Number(a("data-sc-index")), 1); preview = null; renderSelection(); msg("arc-sc-msg", "Changed · preview before saving"); }
    else if (b.id === "arc-sc-preview") selectionPreview();
    else if (b.id === "arc-sc-save") { if (preview) selectionWrite(false); }
    else if (b.id === "arc-sc-reset") selectionWrite(true);
  }
  function onInput(e) {
    var t = e.target;
    if (t.id === "library-search") { libQuery = t.value; renderLibrary(); }
    else if ((st.view === "selection" || st.view === "admin") && t.type === "number" && /^arc-sc-/.test(t.id)) { readSelection(); selectionChanged(); }
  }
  function onKey(e) {
    var t = e.target;
    if (e.key === "Enter" && /^arc-sc-(trusted|denied)Groups$/.test(t.id || "")) { e.preventDefault(); addGroup(t.id.slice(7)); }
  }
  // Titles render their releases only when opened.
  function onToggle(e) {
    var el = e.target;
    if (el.classList && el.classList.contains("arc-title")) {
      open[el.getAttribute("data-group")] = el.open;
      if (el.open && el.querySelector(".arc-body.hidden")) renderStatus();
    }
  }

  (window.HI_PAGES = window.HI_PAGES || {}).archive = {
    show: function (root, ctx) {
      HI = window.HI;
      var views = ["status", "library", "seeding"];
      st.view = views.indexOf(ctx.view) >= 0 ? ctx.view : "status";
      st.params = ctx.params;
      st.gen++;
      document.addEventListener("visibilitychange", onVisibility);
      root.addEventListener("click", onClick);
      root.addEventListener("input", onInput);
      root.addEventListener("keydown", onKey);
      root.addEventListener("toggle", onToggle, true);
      st.root = root;
      root.textContent = "";
      root.className = "arc-page";
      st.tools = HI.h("div", { class: "arc-head-tools" });
      st.strip = HI.h("section", { class: "arc-strip", "aria-label": "Activity", hidden: true });
      st.body = HI.h("div", { id: st.view === "library" ? "archive-library" : st.view === "seeding" ? "archive-seeding" : "archive", class: "arc-view arc-view-" + st.view });
      st.strip.id = "arc-strip";
      st.strip.innerHTML = '<div class="arc-strip-card"></div><div class="arc-live" aria-live="polite"></div>';
      root.appendChild(st.strip);
      root.appendChild(st.body);
      var t = ctx.params && (ctx.params.get("title") || ctx.params.get("hash"));
      if (t && !pending) pending = { view: st.view, title: ctx.params.get("title"), hash: ctx.params.get("hash") };
      if (st.view === "seeding") { seeding = seeding || null; loadSeeding(); }
      if (st.view === "library") { loadLibrary(false); loadLibraryActivity(); }
      if (st.view === "selection") { selection = null; draft = null; preview = null; }
      render();
      poll.busy = false; poll.again = false;
      tick();
    },
    adminTitle: "Archive scoring",
    admin: function (root) {
      HI = window.HI;
      st.gen++;
      st.view = "admin";
      document.addEventListener("visibilitychange", onVisibility);
      st.root = root;
      st.body = root;
      st.strip = st.tools = null;
      root.className = "arc-page arc-admin";
      root.addEventListener("click", onClick);
      root.addEventListener("input", onInput);
      root.addEventListener("keydown", onKey);
      selection = null; draft = null; preview = null; scBusy = false;
      renderSelection();
      loadSelection();
    },
    hide: function () {
      st.gen++;
      clearTimeout(poll.timer); clearTimeout(lingerTimer);
      poll.timer = null; poll.busy = false; poll.ran = false;
      document.removeEventListener("visibilitychange", onVisibility);
      if (st.root) {
        st.root.removeEventListener("click", onClick);
        st.root.removeEventListener("input", onInput);
        st.root.removeEventListener("keydown", onKey);
        st.root.removeEventListener("toggle", onToggle, true);
        st.root.className = "";
      }
      st.root = st.body = st.strip = st.tools = null;
      request++;
    }
  };
})();

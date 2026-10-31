(function () {
  'use strict';
  var root = document.getElementById('backup-admin');
  var panel = document.getElementById('panel-admin');
  if (!panel) return;
  if (!root) { root = document.createElement('div'); root.id = 'backup-admin'; }
  panel.appendChild(root);
  root.className = 'setting';
  root.innerHTML = '<h4>Backup and migration</h4>' +
    '<p>Download an encrypted copy of inventory history, package sets, settings and application state. Keep the passphrase separately: it cannot be recovered.</p>' +
    '<p>Backups include credentials. External databases, media, upload files, signing keys and deployment mounts need separate backups. Creating or restoring a backup briefly pauses intake and background work.</p>' +
    '<div class="backup-controls"><label class="backup-field">Backup passphrase <input id="backup-pass" type="password" autocomplete="new-password" minlength="12" placeholder="At least 12 characters"></label> ' +
    '<button class="btn" type="button" id="backup-download">Download backup</button></div>' +
    '<div class="backup-controls backup-restore"><label class="backup-field">Restore file <input id="backup-file" type="file" accept=".hlb,application/octet-stream"></label> ' +
    '<button class="btn" type="button" id="backup-preview">Preview restore</button></div>' +
    '<details><summary>Private dashboard migration options</summary><p>Map owner-bound Taste history to a destination household profile ID (created if needed). Leave blank to use the source owner and destination profile home; the preview shows the proposed mapping.</p><div class="backup-options"><label class="backup-field">Household profile ID <input id="backup-profile" autocomplete="off"></label> <label class="backup-field">Source Plex owner ID <input id="backup-owner" type="number" min="1"></label></div><label class="backup-check"><input id="backup-legacy-agent" type="checkbox"> Keep existing agents posting with the old dashboard key (inventory only; no administrator or release access)</label></details>' +
    '<div id="backup-plan"></div><p id="backup-message" role="status" aria-live="polite"></p>';
  var busy = false, plan = null;
  function message(text) { document.getElementById('backup-message').textContent = text; }
  function headers() {
    var h = {'X-Requested-By': 'homelab-inventory'};
    var token = sessionStorage.getItem('homelab-inventory.token') || sessionStorage.getItem('homelab.token') || '';
    if (token) h.Authorization = 'Bearer ' + token;
    return h;
  }
  async function checked(path, options) {
    var paths = {export: '/admin/backup/export', preview: '/admin/backup/preview', restore: '/admin/backup/restore'};
    if (!Object.prototype.hasOwnProperty.call(paths, path)) throw new Error('Unknown backup operation');
    var target = paths[path];
    var response = await fetch(target, options);
    if (!response.ok) {
      var body = await response.json().catch(function () { return {}; });
      throw new Error(body.error || ('Backup request failed (' + response.status + ')'));
    }
    return response;
  }
  function passphrase() {
    var value = document.getElementById('backup-pass').value;
    if (value.length < 12) throw new Error('Enter a passphrase with at least 12 characters.');
    return value;
  }
  async function perform(action) {
    if (busy) return;
    busy = true;
    root.querySelectorAll('button').forEach(function (b) { b.disabled = true; });
    try { await action(); } catch (e) { message(e.message); }
    finally { busy = false; root.querySelectorAll('button').forEach(function (b) { b.disabled = false; }); }
  }
  document.getElementById('backup-download').onclick = function () { perform(async function () {
    var h = headers(); h['Content-Type'] = 'application/json';
    message('Preparing a consistent encrypted backup…');
    var response = await checked('export', {method: 'POST', headers: h, body: JSON.stringify({passphrase: passphrase()})});
    var url = URL.createObjectURL(await response.blob());
    var a = document.createElement('a'); a.href = url; a.download = 'inventory-backup.hlb'; a.click();
    setTimeout(function () { URL.revokeObjectURL(url); }, 60000);
    message('Backup downloaded. Keep its passphrase in a safe place.');
  }); };
  document.getElementById('backup-preview').onclick = function () { perform(async function () {
    plan = null; document.getElementById('backup-plan').replaceChildren();
    var file = document.getElementById('backup-file').files[0];
    if (!file) throw new Error('Select a backup file first.');
    var h = headers(); h['X-Backup-Passphrase'] = encodeURIComponent(passphrase()); h['X-Backup-Profile'] = document.getElementById('backup-profile').value.trim(); h['X-Backup-Owner'] = document.getElementById('backup-owner').value.trim(); h['X-Backup-Legacy-Agent'] = String(document.getElementById('backup-legacy-agent').checked); h['Content-Type'] = 'application/octet-stream';
    message('Uploading and validating backup. Active data is unchanged.');
    var response = await checked('preview', {method: 'POST', headers: h, body: file});
    plan = await response.json();
    var box = document.getElementById('backup-plan'); box.replaceChildren();
    var summary = document.createElement('p');
    summary.textContent = plan.source + ' · ' + plan.hosts + ' hosts · ' + plan.runs + ' runs · ' + plan.software_files + ' software files · ' + plan.package_sets + ' package sets · ' + plan.files + ' files (' + plan.bytes.toLocaleString() + ' bytes)';
    box.appendChild(summary);
    var digest = document.createElement('p'); digest.style.overflowWrap = 'anywhere'; digest.textContent = 'Verified plan: ' + plan.digest; box.appendChild(digest);
    var ul = document.createElement('ul'); (plan.issues || []).concat(plan.blockers || []).forEach(function (issue) { var li = document.createElement('li'); li.textContent = issue; ul.appendChild(li); }); box.appendChild(ul);
    var label = document.createElement('label'); label.className = 'backup-field backup-confirm'; label.textContent = 'Type REPLACE to replace this dataset: ';
    var input = document.createElement('input'); input.id = 'backup-confirm'; input.autocomplete = 'off'; label.appendChild(input); box.appendChild(label);
    var button = document.createElement('button'); button.type = 'button'; button.className = 'btn danger'; button.textContent = 'Restore this backup'; box.appendChild(button); if ((plan.blockers || []).length) button.hidden = true;
    var cancel = document.createElement('button'); cancel.type = 'button'; cancel.className = 'btn'; cancel.textContent = 'Cancel preview'; box.appendChild(cancel);
    cancel.onclick = function () { perform(async function () { await checked('preview', {method: 'DELETE', headers: headers()}); plan = null; box.replaceChildren(); message('Preview discarded.'); }); };
    button.onclick = function () { perform(async function () {
      if (input.value !== 'REPLACE') throw new Error('Type REPLACE to confirm the previewed replacement.');
      var hdr = headers(); hdr['Content-Type'] = 'application/json';
      message('Restoring backup. Keep this page open while application state is reopened.');
      await checked('restore', {method: 'POST', headers: hdr, body: JSON.stringify({id: plan.id, digest: plan.digest, confirm: input.value})});
      plan = null; box.replaceChildren(); document.getElementById('backup-pass').value = '';
      message('Backup restored. Reload the dashboard and sign in again if requested.');
    }); };
    message('Preview ready. No active data has changed. Plan expires in 30 minutes.');
  }); };
}());

const fs = require('node:fs'), assert = require('node:assert/strict');
const page = fs.readFileSync(process.argv[2] || require('node:path').join(__dirname, '..', 'index.html'), 'utf8');
const slice = (start, end) => page.slice(page.indexOf(start), page.indexOf(end));
const source = slice('  var pluginActionsPending', '  function renderPluginView(');
const dashboardRefresh = slice('  var loading = null', '  function scheduleRefresh(');
const pluginRefresh = slice('  function fetchPluginView(', '  // refresh_seconds');
const media = slice('  function mediaSections()', '  function groupOf(');
let form, focused, closed = 0, fetched = 0, rendered = 0, notification, response, submits = 0;
let drawerContent, drawerID = 0, drawerParent = null, drawerOpen = false, result, failure, toastPersistent = false;
const announcements = [], plugins = {};
const route = {view: 'plugin'};
const VIEW_NAME = /^[a-z][a-z0-9-]{0,31}$/, PLUGIN_ID = VIEW_NAME;
const pv = {plugin: 'fleet', view: 'host', key: 'fleet/host?name=host-a'};
const document = {activeElement: null}, toastAction = {};
const pluginBody = h('div'), refresh = h('button'), app = h('main'), drawerClose = h('button');
const drawerBody = {
  querySelector: (...args) => drawerContent && drawerContent.querySelector(...args),
  querySelectorAll: (...args) => drawerContent ? drawerContent.querySelectorAll(...args) : []
};

function h(tag, attrs, kids = []) {
  return {tag, ...attrs, kids, appendChild(k) { this.kids.push(k); }, focus() { focused = document.activeElement = this; },
    replaceChildren(...children) { this.kids = children; }, contains(el) { return flat(this).includes(el); },
    removeAttribute(k) { delete this[k]; }, setAttribute(k, v) { this[k] = v; }, getAttribute(k) { return this[k]; },
    querySelectorAll(selector) {
      return flat(this).filter(i => selector.startsWith('.') ?
        i.class && i.class.split(' ').includes(selector.slice(1)) : i[selector.slice(1, -1)] != null);
    },
    querySelector(selector) { return this.querySelectorAll(selector)[0]; },
    classList: {toggle() {}, contains() {return false;}}};
}
function $(id) { return {'plugin-body': pluginBody, refresh, app, 'drawer-close': drawerClose}[id]; }
function openDrawer(content) { drawerOpen = true; drawerContent = content; drawerClose.focus(); return ++drawerID; }
function openConfirmation(content, label, opener, cancel) {
  drawerParent = drawerOpen ? {content: drawerContent, focus: opener} : null;
  form = content;
  const id = openDrawer(content);
  flat(form).find(i => '#' + i.id === cancel).focus();
  return id;
}
function drawerExists(id) { return id === drawerID; }
function drawerIsOpen() { return drawerOpen; }
function closeDrawer() { drawerOpen = false; drawerParent = drawerContent = null; drawerID++; closed++; }
function sizeDrawer() {}
function schedulePluginRefresh() {}
function renderPluginView() { rendered++; }
function toast(title, opts) {
  if (toastPersistent && !opts.persistent) return;
  notification = [title, opts];
  toastPersistent = !!opts.persistent;
  if (opts.announcement !== '') announce(opts.announcement || title);
}
function announce(text) { announcements.push(text); }
function dismissToast() { toastPersistent = false; notification = null; }
function pluginInfo(id) { return plugins[id]; }
function pluginViews(info) { return info.tab.views; }
function api(path) {
  assert.equal(path, '/plugins/fleet/views/host');
  fetched++;
  return failure ? Promise.reject(failure) : Promise.resolve(result);
}
function load() { setRefreshError(''); }
function operatorWrite(path, method, body) {
  assert.equal(path, '/plugins/fleet/actions/run');
  assert.equal(method, 'POST');
  assert.deepEqual(body, {confirmed: true, values: {mode: 'user', enabled: false, count: 3, callback: 'synthetic'}});
  submits++;
  return response;
}
function flat(el) { return [el, ...(el.kids || []).filter(Boolean).flatMap(flat)]; }
const {openPluginAction, pluginActionBar, setRefreshError, fetchPluginView, mediaSections} = eval(
  '(() => {' + source + dashboardRefresh + pluginRefresh + media +
  '\nreturn {openPluginAction, pluginActionBar, setRefreshError, fetchPluginView, mediaSections};})()');
const settle = () => new Promise(setImmediate);

(async () => {
  const action = {id:'run', label:'Change mode', operator:true, destructive:true, result:'toast',
    values:{mode:'user', enabled:false}, fields:[
      {key:'mode', label:'Mode', type:'select', options:[{value:'user',label:'User only'}]},
      {key:'enabled', label:'Enabled', type:'bool'},
      {key:'count', label:'Count', type:'number', default:3},
      {key:'callback', label:'Address', type:'secret', required:true}]};
  const other = {...action, values:{mode:'model', enabled:false}};
  const bar = pluginActionBar(pv, [action, other]);
  pluginBody.kids = [bar];
  const [opener, otherOpener] = bar.kids;
  opener.onclick();
  const items = flat(form);
  assert(items.includes('User only') && items.includes('No'));
  assert(focused.kids.includes('Cancel'));
  const button = items.find(i => i.type === 'submit'), secret = items.find(i => i.type === 'password');
  const count = items.find(i => i.type === 'number');
  assert.equal(secret.autocomplete, 'off');
  assert.match(button.class, /primary destructive/);
  const submit = () => {
    flat(form).find(i => i.type === 'password').value = 'synthetic';
    form.onsubmit({preventDefault(){}});
  };
  let reject;
  response = new Promise((resolve, fail) => { reject = fail; });
  submit();
  assert.equal(secret.value, '');
  assert.equal(opener.disabled, true);
  assert.equal(opener.textContent, 'Working…');
  assert.equal(otherOpener.disabled, false);
  reject(Object.assign(new Error('Try again'), {fields: {callback: 'Paste the whole address'}}));
  await settle();
  assert.equal(closed, 0);
  assert.equal(secret.value, '');
  assert.equal(count.value, 3);
  assert(items.some(i => i.textContent === 'Paste the whole address'));

  const reply = {title: 'Reviewed', blocks: [{type:'notice', text:'Review the plan'}]};
  let resolve;
  response = new Promise(pass => { resolve = pass; });
  submit();
  const dismissed = form;
  closeDrawer();
  opener.onclick();
  openPluginAction(pv, action, opener);
  dismissed.onsubmit({preventDefault(){}});
  assert.equal(form, dismissed);
  assert.equal(submits, 2);
  assert.equal(opener.disabled, true);
  resolve(reply);
  result = reply;
  await settle();
  assert.equal(opener.disabled, false);
  assert.equal(opener.textContent, action.label);
  assert.deepEqual(notification, ['Reviewed', {kind:'ok', sub:'Review the plan'}]);
  assert.equal(fetched, 1);

  opener.onclick();
  assert.notEqual(form, dismissed);
  response = Promise.resolve(reply);
  failure = Object.assign(new Error('Fixture refresh unavailable'), {status: 503});
  submit();
  await settle();
  assert.equal(fetched, 2);
  assert.equal(pv.err, failure);
  assert.equal(notification[0], "Reviewed. Couldn't refresh the view.");
  assert.equal(notification[1].kind, 'error');
  assert.equal(notification[1].persistent, true);
  assert.equal(notification[1].action.label, 'Retry');
  setRefreshError('');
  assert.equal(toastPersistent, true);
  notification[1].action.fn();
  await settle();
  assert.equal(notification[0], "Reviewed. Couldn't refresh the view.");
  assert.deepEqual(announcements, ['Reviewed', 'Reviewed', "Couldn't refresh the view."]);

  opener.onclick();
  response = Promise.resolve({...reply, title: 'Changed.'});
  submit();
  await settle();
  assert.equal(notification[0], "Changed. Couldn't refresh the view.");
  assert.deepEqual(announcements, ['Reviewed', 'Reviewed', "Couldn't refresh the view.", 'Changed.']);
  failure = null;
  notification[1].action.fn();
  await settle();
  assert.equal(pv.err, null);
  assert.equal(toastPersistent, false);
  assert.equal(fetched, 5);

  delete action.result;
  response = Promise.resolve(reply);
  opener.onclick();
  submit();
  await settle();
  assert.equal(pv.data.title, 'Reviewed');
  assert.equal(pv.actionResult, true);
  assert.equal(fetched, 5);
  assert.equal(rendered, 6);

  const overwrites = Array.from({length: 20}, (_, i) => ({id: 'force_file', label: 'Overwrite file-' + i,
    destructive: true, values: {host: 'host-a', item: 'file-' + i}}));
  pv.data = {actions: overwrites};
  const overflow = pluginActionBar(pv, overwrites);
  assert.deepEqual(overflow.kids.map(button => button.kids[0]), ['All actions (20)']);
  overflow.kids[0].onclick();
  const choices = flat(drawerContent).filter(i => i.tag === 'button');
  assert.deepEqual(choices.map(button => button.kids[0]), overwrites.map(a => a.label));
  choices[3].focus();
  const updated = {...overwrites[3], label: 'Overwrite updated file'};
  result = {actions: [updated, overwrites[19]]};
  fetchPluginView();
  await settle();
  const currentChoices = flat(drawerContent).filter(i => i.tag === 'button');
  assert.deepEqual(currentChoices.map(button => button.kids[0]), [updated.label, overwrites[19].label]);
  assert.equal(focused, currentChoices[0]);
  const note = drawerContent.querySelector('.pv-action-list-status');
  choices[0].onclick();
  assert.equal(note.textContent, 'This action changed. Pick again.');
  assert.equal(drawerParent, null);
  currentChoices[1].onclick();
  assert.equal(note.textContent, '');
  assert.equal(flat(form).find(i => i.tag === 'h3').text, 'Overwrite file-19');
  result = {actions: [{...overwrites[19], label: 'Overwrite renamed file'}]};
  fetchPluginView();
  await settle();
  assert.equal(flat(form).find(i => i.tag === 'h3').text, 'Overwrite file-19');
  assert.deepEqual(flat(drawerParent.content).filter(i => i.tag === 'button').map(button => button.kids[0]), ['Overwrite renamed file']);
  assert.equal(drawerParent.focus.kids[0], 'Overwrite renamed file');
  const mixed = pluginActionBar(pv, [overwrites[0], {...action, destructive: false}, ...overwrites.slice(1, 3)]);
  assert.deepEqual(mixed.kids.map(button => button.kids[0]), ['Change mode', 'All actions (4)']);

  plugins.archive = {tab: {views: [{name: 'status'}, {name: 'library', hidden: true}]}};
  plugins.media = {tab: {views: [{name: 'library', title: 'Library'}, {name: 'detail', hidden: true}]}};
  assert.deepEqual(mediaSections().map(({key, view}) => [key, view]), [['archive-status', 'status'], ['archive-seeding', 'status']]);
  delete plugins.archive;
  assert.deepEqual(mediaSections().map(({key, view}) => [key, view]), [['media-library', 'library']]);
})().catch(e => { console.error(e); process.exitCode = 1; });

const fs = require('node:fs'), assert = require('node:assert/strict');
const page = fs.readFileSync(process.argv[2], 'utf8');
const slice = (start, end) => page.slice(page.indexOf(start), page.indexOf(end));
const source = slice('  var pluginActionsPending', '  function renderPluginView(');
const dashboardRefresh = slice('  var loading = null', '  function scheduleRefresh(');
const pluginRefresh = slice('  function fetchPluginView()', '  // refresh_seconds');
let form, focused, closed = 0, fetched = 0, rendered = 0, notification, response, submits = 0;
let drawerContent, drawerID = 0, drawerParent = null, result, failure, announcements = 0, toastPersistent = false;
const route = {view: 'plugin'};
const VIEW_NAME = /^[a-z][a-z0-9-]{0,31}$/, PLUGIN_ID = VIEW_NAME;
const pv = {plugin: 'fleet', view: 'host', key: 'fleet/host?name=host-a'};
const document = {activeElement: null}, toastAction = {};
const pluginBody = h('div'), refresh = h('button'), app = h('main');
const drawerBody = {querySelectorAll: (...args) => drawerContent ? drawerContent.querySelectorAll(...args) : []};

function h(tag, attrs, kids = []) {
  return {tag, ...attrs, kids, appendChild(k) { this.kids.push(k); }, focus() { focused = this; },
    removeAttribute(k) { delete this[k]; }, setAttribute(k, v) { this[k] = v; }, getAttribute(k) { return this[k]; },
    querySelectorAll(selector) { return flat(this).filter(i => i[selector.slice(1, -1)] != null); },
    classList: {toggle() {}, contains() {return false;}}};
}
function $(id) { return {'plugin-body': pluginBody, refresh, app}[id]; }
function openDrawer(content) { drawerContent = content; return ++drawerID; }
function openConfirmation(content, label, opener, cancel) {
  form = content;
  flat(form).find(i => '#' + i.id === cancel).focus();
  return openDrawer(content);
}
function drawerExists(id) { return id === drawerID; }
function drawerIsOpen() { return !!drawerContent; }
function closeDrawer() { drawerContent = null; drawerID++; closed++; }
function schedulePluginRefresh() {}
function renderPluginView() { rendered++; }
function toast(title, opts) {
  notification = [title, opts];
  if (opts.persistent) { toastPersistent = true; announcements++; }
}
function dismissToast() { toastPersistent = false; notification = null; }
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
const {openPluginAction, pluginActionBar, setRefreshError, fetchPluginView} = eval(
  '(() => {' + source + dashboardRefresh + pluginRefresh + '\nreturn {openPluginAction, pluginActionBar, setRefreshError, fetchPluginView};})()');
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
  assert.equal(notification[0], "Couldn't refresh · HTTP 503");
  assert.equal(notification[1].kind, 'error');
  assert.equal(notification[1].persistent, true);
  assert.equal(notification[1].action.label, 'Retry');
  setRefreshError('');
  assert.equal(toastPersistent, true);
  notification[1].action.fn();
  await settle();
  assert.equal(announcements, 1);
  failure = null;
  notification[1].action.fn();
  await settle();
  assert.equal(pv.err, null);
  assert.equal(toastPersistent, false);
  assert.equal(fetched, 4);

  delete action.result;
  opener.onclick();
  submit();
  await settle();
  assert.equal(pv.data.title, 'Reviewed');
  assert.equal(pv.actionResult, true);
  assert.equal(fetched, 4);
  assert.equal(rendered, 5);

  const overwrites = Array.from({length: 20}, (_, i) => ({id: 'force_file', label: 'Overwrite file-' + i,
    destructive: true, values: {host: 'host-a', item: 'file-' + i}}));
  const overflow = pluginActionBar(pv, overwrites);
  assert.deepEqual(overflow.kids.map(button => button.kids[0]), ['All actions (20)']);
  overflow.kids[0].onclick();
  const choices = flat(drawerContent).filter(i => i.tag === 'button');
  assert.deepEqual(choices.map(button => button.kids[0]), overwrites.map(a => a.label));
  choices[19].onclick();
  assert.equal(flat(form).find(i => i.tag === 'h3').text, 'Overwrite file-19');
  const mixed = pluginActionBar(pv, [overwrites[0], {...action, destructive: false}, ...overwrites.slice(1, 3)]);
  assert.deepEqual(mixed.kids.map(button => button.kids[0]), ['Change mode', 'All actions (4)']);
})().catch(e => { console.error(e); process.exitCode = 1; });

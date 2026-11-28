const fs = require('node:fs'), assert = require('node:assert/strict');
const page = fs.readFileSync(process.argv[2], 'utf8');
const source = page.slice(page.indexOf('  function openPluginAction('), page.indexOf('  function renderPluginView('));
let form, focused, closed = 0, fetched = 0, rendered = 0, notification, reject;
const pv = {plugin: 'example'}, drawer = {classList: {add() {}}};
function h(tag, attrs, kids = []) {
  return {tag, ...attrs, kids, appendChild(k) { this.kids.push(k); }, focus() { focused = this; },
    removeAttribute(k) { delete this[k]; }, setAttribute(k, v) { this[k] = v; }};
}
function openDrawer(content) { form = content; return 1; }
function drawerExists(id) { return id === 1; }
function closeDrawer() { closed++; }
function fetchPluginView() { fetched++; }
function renderPluginView() { rendered++; }
function schedulePluginRefresh() {}
function toast(title, opts) { notification = [title, opts]; }
async function operatorWrite(path, method, body) {
  assert.equal(path, '/plugins/example/actions/run');
  assert.equal(method, 'POST');
  assert.deepEqual(body.values, {mode: 'user', enabled: false, count: 3, callback: 'synthetic'});
  if (reject) throw Object.assign(new Error('Try again'), {fields: {callback: 'Paste the whole address'}});
  return {title: 'Reviewed', blocks: [{type:'notice', text:'Review the plan'}]};
}
function flat(el) { return [el, ...(el.kids || []).filter(Boolean).flatMap(flat)]; }
(async () => {
  const open = eval('(' + source.trim() + ')');
  const action = {id:'run', label:'Change mode', operator:true, destructive:true, result:'toast',
    values:{mode:'user', enabled:false}, fields:[
      {key:'mode', label:'Mode', type:'select', options:[{value:'user',label:'User only'}]},
      {key:'enabled', label:'Enabled', type:'bool'},
      {key:'count', label:'Count', type:'number', default:3},
      {key:'callback', label:'Address', type:'secret', required:true}]};
  const run = async () => { form.onsubmit({preventDefault(){}}); await new Promise(setImmediate); };
  open(action, {});
  const items = flat(form);
  assert(items.includes('User only') && items.includes('No'));
  assert(focused.kids.includes('Cancel'));
  const button = items.find(i => i.type === 'submit'), secret = items.find(i => i.type === 'password');
  assert.equal(secret.autocomplete, 'off');
  assert.match(button.class, /primary destructive/);
  secret.value = 'synthetic';
  reject = true;
  await run();
  assert.equal(closed, 0);
  assert.equal(secret.value, 'synthetic');
  assert(items.some(i => i.textContent === 'Paste the whole address'));
  reject = false;
  await run();
  assert.equal(closed, 1);
  assert.deepEqual(notification, ['Reviewed', {kind:'ok', sub:'Review the plan'}]);
  assert.equal(fetched, 1);
  delete action.result;
  open(action, {});
  flat(form).find(i => i.type === 'password').value = 'synthetic';
  await run();
  assert.equal(pv.data.title, 'Reviewed');
  assert.equal(rendered, 1);
  assert.equal(fetched, 1);
})().catch(e => { console.error(e); process.exitCode = 1; });

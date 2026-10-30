const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const page = fs.readFileSync(process.argv[2], 'utf8');
const start = page.indexOf('  var overviewOpen =');
const end = page.indexOf("  document.addEventListener('click'", start);
assert(start > 0 && end > start, 'overview rendering must exist');
const output = {innerHTML:''};
const context = {
  data: {}, $: () => output, countUp: () => {}, ic: () => '',
  ago: () => '1m ago', bytes: String, fromNow: () => 'in 4d',
  esc: value => String(value == null ? '' : value).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])),
  GLYPH: {good:'✓', warning:'▲'}
};
vm.createContext(context);
vm.runInContext(page.slice(start, end), context);
function render(fleet, attention = []) {
  context.data.overview = {fleet, attention, storage:[], zfs:[], backups:null, units_failed:[]};
  context.renderOverview();
  return output.innerHTML;
}
const counts = {critical:0, warning:0, info:0, silenced:0};
let html = render(null);
assert.equal((html.match(/class="ops-number">—/g) || []).length, 3, 'unknown summaries must not become zeros');
assert.doesNotMatch(html, /All hosts reporting|No outdated agents/);
html = render({status:'unavailable', error:'Synthetic timeout'});
assert.match(html, /Unavailable/);
assert.doesNotMatch(html, /All hosts reporting|No outdated agents/);
html = render({hosts_total:0, hosts_reporting:0, agents_outdated:[], alerts:counts});
assert.match(html, /No hosts collected/);
assert.match(html, /No outdated agents/);
assert.match(html, /class="ops-number">0</);
html = render({hosts_total:6, hosts_reporting:4, agents_outdated:['edge','apps'], alerts:{...counts, critical:1, warning:2, info:3}});
assert.match(html, /4<small> \/ 6</);
assert.match(html, /2 not reporting/);
assert.match(html, /class="ops-number crit">6</);
assert.match(html, /class="ops-number">2</);
assert.match(html, /href="#\/hosts"/);
html = render({hosts_total:1, hosts_reporting:1, agents_outdated:null, alerts:{status:'unavailable'}});
assert.match(html, /All hosts reporting/);
assert.match(html, /Unavailable/);
assert.doesNotMatch(html, /No outdated agents/);
const summary = 'Guest <img src=x> & backup failed';
html = render({hosts_total:1, hosts_reporting:1, agents_outdated:[], alerts:counts}, [
  {host:'atlas', subject:'204', rule:'backup', severity:'warning', summary}
]);
assert.match(html, /class="ops-name">Guest &lt;img src=x&gt; &amp; backup failed/);
assert.match(html, /class="sr-only">warning: /);
assert.doesNotMatch(html, /<img src=x>/);
assert.match(html, /data-ops-alert="0"/);
console.log('overview summaries, unknown versus empty data, stale hosts and escaped attention: PASS');

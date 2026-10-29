const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const page = fs.readFileSync(process.argv[2], 'utf8');
const start = page.indexOf('  var restartsDisabled =');
const end = page.indexOf('  function recentActionsHTML(', start);
assert(start > 0 && end > start);
const requests = [];
let status = 403, canceled = false, html = '', recoveryChecks = 0, gate = 0;
const context = {
  capabilities:{actions_enabled:true,actions_host:'host-a'}, token:'synthetic-bearer', OPERATOR_KEY:'synthetic-key', AbortController, setTimeout, clearTimeout,
  document:{addEventListener:()=>{}}, route:{view:'host',host:'host-b'},hostCache:{},
  operatorHeaders:()=>{ if (canceled) { const e=new Error('canceled');e.canceled=true;throw e; }return {Authorization:'Bearer synthetic-operator'}; },
  sessionStorage:{removeItem:()=>{}},
  esc:String, showGate:()=>gate++, renderHost:()=>{},
  fetch:async(path,opts)=>{ requests.push({path,opts});return {status,ok:status===200,json:async()=>({error:'disabled'})}; }
};
vm.createContext(context);
vm.runInContext(page.slice(start,end),context);
context.restartVisible=()=>true;
context.restartSheet=(target,content)=>{html=content;};
context.pollRestartHealth=()=>recoveryChecks++;
context.showRestartPreview=()=>{html='preview';};
const settle=()=>new Promise(resolve=>setImmediate(resolve));
(async()=>{
 assert.match(context.restartButton('container_restart','web'),/Restarts disabled/);
 context.route.host='host-a';context.ic=()=>'';
 assert.match(context.restartButton('container_restart','web'),/data-restart-kind/);
 context.capabilities.actions_enabled=false;
 assert.match(context.restartButton('container_restart','web'),/Restarts disabled/);
 context.capabilities.actions_enabled=true;
 await context.actionPost('/actions/preview',{host:'host-a'});
 assert.equal(requests.pop().opts.headers.Authorization,'Bearer synthetic-bearer');
 for (const denial of [403,404,503]) {
  status=denial;context.restartsDisabled=false;
  context.executeRestart({kind:'container_restart',target:'web',host:'host-b',container_set_hash:'synthetic'},1);
  await settle();
  assert.match(html,/Restarts are disabled/);
  assert.equal(context.restartsDisabled,true);
  const req=requests.at(-1);
  assert.equal(req.opts.headers.Authorization,'Bearer synthetic-operator');
  assert.equal(req.opts.headers['X-Operator-Token'],undefined);
  assert.equal(JSON.parse(req.opts.body).host,'host-b');
 }
 assert.equal(recoveryChecks,0,'denial does not imply an interrupted restart');
 assert.equal(requests.length,3,'never replay an execution');
 context.restartsDisabled=false;canceled=true;
 context.executeRestart({kind:'container_restart',target:'web',host:'host-b'},1);
 await settle();
 assert.equal(html,'preview');assert.equal(requests.length,3);
 canceled=false;status=401;
 context.executeRestart({kind:'container_restart',target:'web',host:'host-b'},1);
 context.closeDrawer=()=>{};
 await settle();assert.equal(gate,1);
 console.log('restart denials, host scoping, operator header, cancellation and no replay: PASS');
})().catch(err=>{console.error(err);process.exitCode=1;});

const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const page = fs.readFileSync(process.argv[2], 'utf8');
const start = page.indexOf('  var OPERATOR_KEY =');
const end = page.indexOf('  function applyCatalog(', start);
assert(start > 0 && end > start, 'write and operator helpers must exist');
const stored = new Map();
const requests = [];
let prompts = 0, answer = 'synthetic-operator', status = 200;
const context = {
  token: 'synthetic-bearer',
  sessionStorage: {
    getItem: key => stored.has(key) ? stored.get(key) : null,
    setItem: (key, value) => stored.set(key, value),
    removeItem: key => stored.delete(key)
  },
  prompt: () => { prompts++; return answer; },
  fetch: async (path, opts) => {
    requests.push({path, opts});
    return {status, ok: status === 200, json: async () => status === 200 ? {} : {error:'disabled'}};
  }
};
vm.createContext(context);
vm.runInContext(page.slice(start, end), context);
const fleet = {innerHTML:''};
Object.assign(context, {$:()=>fleet, esc:String, ic:()=>''});
vm.runInContext(page.slice(page.indexOf('  var capabilities ='), page.indexOf('  function refreshAgents()')), context);
context.agentStatus = {mode:'auto',sequence:1};
context.renderAgentFleet();
assert.match(fleet.innerHTML,/Promotion is disabled — set OPERATOR_TOKEN or use the signer CLI/);
assert.doesNotMatch(fleet.innerHTML,/data-agent-action/);
context.capabilities = {agent_offers_enabled:true};
context.renderAgentFleet();
assert.match(fleet.innerHTML,/data-agent-action="promote"/);
(async () => {
  await context.operatorWrite('/agent/offers', 'POST', {action:'promote'});
  await context.operatorWrite('/agent/offers', 'POST', {action:'hold'});
  assert.equal(prompts, 1, 'reuse token only within this session');
  assert.equal(requests[0].opts.headers['X-Operator-Token'], undefined);
  assert.equal(requests[0].opts.headers.Authorization, 'Bearer synthetic-operator');
  await context.write('/settings', 'PUT', {});
  assert.equal(requests[2].opts.headers.Authorization, 'Bearer synthetic-bearer', 'ordinary writes use ingest authority');
  status = 403;
  await assert.rejects(context.operatorWrite('/agent/offers', 'POST', {}), err => err.status === 403);
  assert.equal(stored.size, 0, 'discard a rejected operator token');
  answer = null;
  const before = requests.length;
  await assert.rejects(context.operatorWrite('/agent/offers', 'POST', {}), err => err.canceled === true);
  assert.equal(requests.length, before, 'canceling the prompt sends no request');
  assert.equal(stored.size, 0);
  answer = 'synthetic-operator';
  status = 401;
  await assert.rejects(context.operatorWrite('/agent/offers', 'POST', {}), err => err.unauthorized === true);
  console.log('operator prompt, session reuse, headers, cancellation and denials: PASS');
})().catch(err => { console.error(err); process.exitCode = 1; });

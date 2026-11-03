const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const page = fs.readFileSync(process.argv[2], 'utf8');
const start = page.indexOf('  function mintCode(');
const end = page.indexOf('  // Two reads per tick:', start);
assert(start > 0 && end > start);
const requests = [], changes = {};
let fail = false, expiry;
const fields = {};
for (const name of ['host','scheduler','state','replace','user','cmd','meta','label','fresh','note','left','cron']) {
 fields[name] = {value:'',innerHTML:'',textContent:'',classList:{toggle(){}},querySelector:()=>null};
}
Object.assign(fields.host,{value:'host-a'});
Object.assign(fields.scheduler,{value:'truenas-cron'});
Object.assign(fields.state,{value:'/mnt/pool/agent files'});
Object.assign(fields.replace,{value:'7'});
Object.assign(fields.user,{value:'root'});
const s = {root:{querySelectorAll:()=>[],contains:()=>true},q:name=>fields[name],dead:false,minting:false};
const context = {
 document:{addEventListener:(name,cb)=>{changes[name]=cb;}},
 enrollSession:s,
 esc:String, ic:()=>'', cmdBox:cmd=>cmd, announce:()=>{}, mmss:String,
 countdown:(when,tick,done)=>{expiry=done;return ()=>{};},
 pollEnroll:()=>{}, stopEnroll:()=>{}, showGate:()=>{},
 write:async(path,method,body)=>{
  requests.push({path,method,body});
  if(fail) throw new Error('no promoted baseline');
  return {id:'opaque',code:'synthetic-code',expires_at:'2099-01-01T00:00:00Z',signed_updates:true,http:false,commands:{root:'root install command',linux:'sudo install command'}};
 }
};
vm.createContext(context);
vm.runInContext(page.slice(start,end),context);
const settle = () => new Promise(resolve=>setImmediate(resolve));
(async()=>{
 context.mintCode(s,''); await settle();
 assert.deepEqual(JSON.parse(JSON.stringify(requests[0])),{path:'/setup/enroll',method:'POST',body:{hostname:'host-a',scheduler:'truenas-cron',state_dir:'/mnt/pool/agent files',replace_schedule:'7'}});
 assert.equal(fields.cmd.innerHTML,'root install command');
 // Root/sudo changes reuse this exact code; scheduler/host changes invalidate it.
 fields.user.value='linux';
 changes.change({target:Object.assign(fields.user,{closest:()=>true})});
 assert.equal(fields.cmd.innerHTML,'sudo install command');
 fields.host.value='host-b';
 changes.change({target:Object.assign(fields.host,{closest:()=>true})});
 assert.equal(s.commands,null);
 assert.match(fields.cmd.textContent,/Generate a new command/);
 fail=true;
 context.mintCode(s,''); await settle();
 assert.equal(s.commands,null,'failed request must not restore an older usable command');
 assert.match(fields.cmd.textContent,/Command unavailable/);
 assert.match(fields.meta.innerHTML,/no promoted baseline/);
 fail=false;
 context.mintCode(s,''); await settle();
 expiry();
 assert.equal(s.commands,null,'expired code must not be restored by changing root/sudo');
})().catch(err=>{console.error(err);process.exitCode=1;});

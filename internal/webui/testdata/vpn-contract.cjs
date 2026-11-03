const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const page = fs.readFileSync(process.argv[2], 'utf8');
const start = page.indexOf('  function vpnCard('), end = page.indexOf('  // ------------------------------------------------------------ WAN', start);
assert(start > 0 && end > start);
function h(tag, attrs = {}, kids = []) {
 return {tag, attrs, kids:kids.filter(Boolean), appendChild(child) {this.kids.push(child);}};
}
function text(n) { return typeof n === 'string' ? n : n ? [n.attrs.text || '', ...n.kids.map(text)].join(' ') : ''; }
function find(n, tag) { if (n.tag === tag) return n; for (const k of n.kids || []) { const v = typeof k === 'object' && find(k,tag); if (v) return v; } }
let reconnect;
const c = {
 h, s:{data:{summary:{vpns:[]}},err:{}},open:{}, Date,
 since:iso=>iso?(Date.now()-new Date(iso).getTime())/1000:null,
 num:v=>typeof v==='number'&&Number.isFinite(v),age:v=>Math.round(v)+'s',
 size:v=>v+' B',state:v=>v||'Unavailable',source:()=>false,series:()=>[],
 chip:(t,tone)=>h('chip',{text:t,tone}),ageChip:()=>h('chip',{text:'age'}),
 muted:t=>h('p',{text:t}),facts:rows=>h('dl',{},rows.map(r=>h('fact',{text:r.join(': ')}))),
 details:(key,cls,summary,body)=>h('details',{key,class:cls},[...summary,...body]),
 HI:{iconNode:()=>h('icon'),ago:()=> '1m ago',restartsEnabled:()=>true,vpnReconnect:(...args)=>{reconnect=args;}}
};
vm.createContext(c);vm.runInContext(page.slice(start,end),c);
const now=new Date().toISOString();
const vpn={host:'host-a',status:'degraded',reason:'Dependent disconnected',container:'vpn-demo',inventoryAt:now,handshakeAgeSeconds:0,
 data:{interfaceUp:true,latestHandshake:now,exitIp:'192.0.2.42',rxBytes:0,txBytes:null},reconnectAllowed:true,
 dependents:[{name:'worker-a',attached:true,state:'running'},{name:'worker-b',attached:false,state:'running'},{name:'<img src=x>',attached:null}],
 lastReconnect:{at:now,actor:'operator',result:'failed',reason:'Handshake timeout',exitIp:'192.0.2.42',country:'Example'},
 autofix:{reason:"internet unreachable from the dashboard host; automatic reconnect skipped",enabled:true,afterMinutes:10,state:'cooldown',cooldownUntil:new Date(Date.now()+600000).toISOString(),lastRun:now}};
let card=c.vpnCard(vpn,true), content=text(card);
assert.match(content,/Attached/);assert.match(content,/Cut off/);assert.match(content,/Attachment unknown/);
assert.match(content,/operator · failed/);assert.match(content,/Handshake timeout/);assert.match(content,/remaining/);
assert.match(content,/Recovery reason: internet unreachable/);assert.match(content,/Exit address: 192.0.2.42/);
assert.match(content,/Received: 0 B/);assert.match(content,/Sent: —/);assert.match(content,/Handshake: 0s ago/);
let btn=find(card,'button');assert.equal(btn.attrs.disabled,false);btn.attrs.onclick();assert.deepEqual(reconnect,['host-a','vpn-demo']);
assert.equal(find(card,'img'),undefined,'untrusted names are text, never markup');
card=c.vpnCard({...vpn,reconnectAllowed:false},true);assert.equal(find(card,'button').attrs.disabled,true);
c.s.err.summary='offline';card=c.vpnCard(vpn,true);assert.equal(find(card,'button').attrs.disabled,true);assert.match(card.attrs.class,/net-stale/);
c.s.err={};
card=c.vpnCard({...vpn,live:true,stale:true,inventoryAt:'2020-01-01T00:00:00Z',status:'cutoff'},true);
assert.doesNotMatch(card.attrs.class,/net-stale/,'current live probes are distinct from old inventory');
assert.match(text(card),/Live/);assert.match(text(card),/Inventory: Stale/);
card=c.vpnCard({...vpn,data:null,dependents:null,lastReconnect:null,autofix:null},true);content=text(card);
assert.match(content,/Dependent containers unavailable/);assert.match(content,/No reconnect recorded/);assert.match(content,/Automatic reconnect: Unavailable/);
console.log('VPN states, unknown attachment, zero counters, recovery history and reconnect gating: PASS');

const tunnel={...vpn,container:'tunnel',data:{...vpn.data,exitIp:'192.0.2.43'}};
c.s.data.summary.vpns=[vpn,tunnel];
for(const full of [false,true]) {
 const cards=c.vpnSection(full);
 assert.equal(cards.length,2);
 assert.notEqual(cards[0].attrs.key,cards[1].attrs.key,'same-host gateways need independent card state');
 assert.match(text(cards[0].kids[0]),/vpn-demo/);
 assert.match(text(cards[1].kids[0]),/tunnel/);
 assert.match(text(cards[0]),/Exit address: 192.0.2.42/);
 assert.match(text(cards[1]),/Exit address: 192.0.2.43/);
}

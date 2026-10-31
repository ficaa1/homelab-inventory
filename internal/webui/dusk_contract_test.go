package webui

import (
	"os/exec"
	"testing"
)

func TestDuskAdminContract(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	script := `
const fs = require('fs'), assert = require('assert');
const page = fs.readFileSync('index.html','utf8');
const start = page.indexOf('  function duskOptions('), end = page.indexOf('  function buildPlugRow(',start);
class Element {
 constructor(tag,attrs={},children=[]) { this.tag=tag; Object.assign(this,attrs); this.children=children.filter(Boolean); this.value=attrs.value||''; }
 replaceChildren(...children) { this.children=children; if(this.tag==='select') this.value=(children.find(c=>c.selected)||children[0]||{}).value||''; }
 addEventListener(type,fn) { this[type]=fn; }
 querySelectorAll(tag) { return flat(this).filter(c=>c.tag===tag); }
}
function flat(el) { return [el,...(el.children||[]).filter(c=>c&&typeof c==='object').flatMap(flat)]; }
function h(tag,attrs,children=[]) { const el=new Element(tag,attrs,children); if(tag==='select') el.replaceChildren(...children); return el; }
let calls=[]; function write(path,method,body) { calls.push({path,method,body}); return Promise.resolve({code:'ABCD-EFGH',expires_at:'2030-01-01'}); }
function showGate() { throw Error('unexpected auth'); } function loadDuskAdmin() { return Promise.resolve(); }
const window={confirm:()=>true};
eval(page.slice(start,end));
const state={content:h('div'),data:{profiles:[{id:'room/a',name:'<Household>'}],policies:{},enrichment:{'room/a':4},enrichment_options:[{id:4,name:'Viewer'}],devices:[],options:{requesters:[{id:3,name:'Viewer'}],servers:[{id:7,name:'Movies',kind:'movie',profiles:[{id:9,name:'Standard'}],roots:['/movies']}]}}};
renderDuskAdmin(state);
function button(text) {return flat(state.content).find(c=>c.tag==='button'&&c.text===text);}
const checks=flat(state.content).filter(c=>c.type==='checkbox');
assert.deepStrictEqual(checks.filter(c=>c.checked).map(c=>c.value),['media','upcoming','discover','search']);
button('Create pairing code').onclick();
assert.deepStrictEqual(calls[0].body,{profile_id:'room/a',scopes:['media','upcoming','discover','search']});
Promise.resolve().then(()=>{
 button('Save policy').onclick();
 assert.equal(calls[1].path,'/dusk/policies/room%2Fa');
 assert.equal(typeof calls[1].body.requester_id,'number');
 assert.equal(typeof calls[1].body.movie.server_id,'number');
 assert.ok(calls[1].body.tv);
 return Promise.resolve();
}).then(()=>{
 button('Save enrichment profile').onclick();
 assert.equal(calls[2].path,'/dusk/enrichment/room%2Fa');
 assert.deepStrictEqual(calls[2].body,{user_id:4});
 assert.ok(flat(state.content).some(c=>c.tag==='h3'&&c.text==='<Household>'));
});
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("Dusk UI contract: %v\n%s", err, out)
	}
}

func TestDuskAdminLoadsOptionsWithoutBlockingPairing(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	script := `
const fs=require('fs'), assert=require('assert'), page=fs.readFileSync('index.html','utf8');
const start=page.indexOf('  function loadDuskAdmin('), end=page.indexOf('  function duskOptions(',start);
let calls=[], failOptions=false, renders=0;
function api(path){calls.push(path);if(path==='/dusk/options')return failOptions?Promise.reject(new Error('Unavailable')):Promise.resolve({requesters:[{id:3,name:'Viewer'}],servers:[]});return Promise.resolve(path==='/dusk/admin'?{profiles:[{id:'room',name:'Room'}]}:[]);}
function renderDuskAdmin(){renders++;} function showGate(){throw Error('unexpected auth');}
eval(page.slice(start,end));
(async()=>{
 const state={content:{}};
 await loadDuskAdmin(state);
 assert.deepStrictEqual(calls,['/dusk/admin','/dusk/enrichment','/dusk/options']);
 assert.equal(state.data.options.requesters[0].id,3);
 assert.equal(state.loaded,true);
 failOptions=true;
 await loadDuskAdmin(state);
 assert.equal(state.loaded,true);
 assert.equal(state.data.profiles[0].id,'room');
 assert.equal(state.data.options_error,'Unavailable');
 assert.deepStrictEqual(state.data.options,{});
 assert.equal(renders,2);
})().catch(err=>{console.error(err);process.exitCode=1;});
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("Dusk options loading: %v\n%s", err, out)
	}
}

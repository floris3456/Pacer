package pacer

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// This is a deterministic source/DOM-contract test, not browser acceptance.
// It executes the real inline code with original frozen DTOs and deferred mock
// responses, checking logout/stale-response handling without network or sleeps.
func TestLegacyUISyntaxAndStateContracts(t *testing.T) {
	node := os.Getenv("PACER_NODE")
	if node == "" {
		var err error
		node, err = exec.LookPath("node")
		if err != nil {
			t.Skip("Node required; set PACER_NODE for source UI verification")
		}
	}
	html, err := os.ReadFile("../management/index.html")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(html), "<script>")
	if len(parts) != 2 {
		t.Fatal("expected one inline script")
	}
	script := strings.Split(parts[1], "</script>")[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	check := exec.CommandContext(ctx, node, "--check")
	check.Stdin = strings.NewReader(script)
	if out, err := check.CombinedOutput(); err != nil {
		t.Fatalf("Node syntax: %v\n%s", err, out)
	}
	cmd := exec.CommandContext(ctx, node, "-e", legacyUIHarness)
	cmd.Stdin = strings.NewReader(string(html))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Node source contracts: %v\n%s", err, out)
	} else {
		t.Log(string(out))
	}
}

const legacyUIHarness = `
const fs=require('node:fs'),vm=require('node:vm'),assert=require('node:assert/strict'),zlib=require('node:zlib');
const html=fs.readFileSync(0,'utf8'),nodes=new Map(),calls=[],timers=new Map(),urls=new Set(),downloads=[];
let clock=0,held=null,hold='',release=null;
class Element {
 constructor(tag='div',id=''){this.tag=tag;this.id=id;this.children=[];this.value='';this._text='';this.dataset={};this.disabled=false;this.hidden=false;this.checked=false;this.open=false}
 set innerHTML(v){throw Error('Unsafe HTML assignment')}
 set textContent(v){this._text=String(v??'');this.children=[]}
 get textContent(){return this._text+this.children.map(c=>c.textContent||'').join(' ')}
 append(...items){this.children.push(...items)}
 replaceChildren(...items){this._text='';this.children=items}
 get options(){return this.children}
 remove(i){this.children.splice(i,1)}
 querySelectorAll(){return this.id==='legacy-advanced'?[...nodes.values()].filter(n=>n.id.startsWith('legacy-')&&n.tag==='button'):[]}
 querySelector(){return new Element()}
 setAttribute(k,v){this[k]=v}
 showModal(){this.open=true}
 close(){this.open=false}
 reset(){const prefix=this.id==='legacy-form'?'legacy-':this.id==='legacy-scenario-form'?'legacy-scenario-':'legacy-forecast-';for(const n of nodes.values())if(n.id.startsWith(prefix)&&(n.tag==='input'||n.tag==='select')){n.value=n.options[0]?.value||'';n.checked=false}}
 click(){if(this.tag==='a')downloads.push({filename:this.download,url:this.href})}
}
for(const match of html.matchAll(/<([a-z]+)\b[^>]*\bid="([^"]+)"[^>]*>/g)){assert(!nodes.has(match[2]),'Duplicate DOM id '+match[2]);nodes.set(match[2],new Element(match[1],match[2]))}
const el=id=>{assert(nodes.has(id),'Missing DOM id '+id);return nodes.get(id)};
for(const match of html.matchAll(/<select\b[^>]*id="([^"]+)"[^>]*>([\s\S]*?)<\/select>/g)){for(const o of match[2].matchAll(/<option value="([^"]*)"[^>]*>([^<]*)<\/option>/g)){const n=new Element('option');n.value=o[1];n.textContent=o[2];el(match[1]).append(n)}el(match[1]).value=el(match[1]).options[0]?.value||''}
const cases=JSON.parse(zlib.gunzipSync(fs.readFileSync('../historyreports/testdata/historical-golden/historical-oracle.json.gz')));
const source=(view,name)=>structuredClone(cases.find(c=>c.View===view&&c.Name===name).Output);
const pin=source('profiles','v3').filter,events=[];
let ready=true;
function response(path,body){
 if(path.endsWith('/legacy/status'))return {historical_ready:ready,default_pin:pin};
 if(path.endsWith('/history/profiles'))return {profiles:[{name:'<img src=x onerror=alert(1)>',reference:'profile:test'}]};
 if(path.endsWith('/legacy/profiles')){const data=source('profiles','v3');data.config.profiles[0].name='<script>bad()</script>';return data}
 if(path.endsWith('/legacy/history'))return source('history','history-all');
 if(path.endsWith('/legacy/history-proof'))return source('history-proof','proof-all');
 if(path.endsWith('/legacy/profile-proof'))return source('profile-proof','v3-forecast-window');
 if(path.endsWith('/legacy/quota'))return body.query.pin?.capacity_ratio?source('quota','scenario'):source('quota','current');
 if(path.endsWith('/legacy/forecasts'))return {pin:body.query.pin,forecasts:source('forecasts','forecast-ledger'),total:2};
 if(path.endsWith('/legacy/export')||path.endsWith('/legacy/quota-export'))return {filename:'pacer-legacy-'+(body.view||'quota')+'.'+body.format,media_type:body.format==='csv'?'text/csv':'application/json',content:'exact source bytes\n'};
 throw Error('Unexpected request '+path)
}
const sandbox={console,Blob,Date,JSON,Map,Set,Number,Error,Array,Promise,Math,URL:{createObjectURL(){const u='blob:fixture-'+(++clock);urls.add(u);return u},revokeObjectURL(u){urls.delete(u)}},document:{getElementById:el,createElement:tag=>new Element(tag),createTextNode:text=>({textContent:text}),querySelectorAll:()=>[]},location:{origin:'https://offline.invalid'},setTimeout(fn){timers.set(++clock,fn);return clock},clearTimeout(id){timers.delete(id)},fetch:async(path,opts)=>{assert.equal(opts.redirect,'error');assert.equal(opts.cache,'no-store');assert.equal(opts.headers.Authorization,'Bearer synthetic-memory-key');const body=opts.body?JSON.parse(opts.body):undefined;calls.push({path,body});if(path.endsWith(hold)&&hold){await new Promise(resolve=>{release=resolve;held=true})}return {ok:true,json:async()=>response(path,body)}}};
sandbox.window={addEventListener(){}};sandbox.window.parent=sandbox.window;
Object.defineProperty(sandbox,'localStorage',{get(){throw Error('Persistent credentials forbidden')}});Object.defineProperty(sandbox,'sessionStorage',{get(){throw Error('Persistent credentials forbidden')}});
const hook='globalThis.ui={loadLegacy,loadLegacyForecasts,loadLegacyProof,applyLegacyScenario,legacyDownload,connect(){connected=true;generation++;key="synthetic-memory-key"},state(){return {legacyDefaultPin,legacyLoaded,legacyProof,legacyForecast,legacyBusy,key}},allowed};';
const script=html.split('<script>')[1].split('</script>')[0];assert(script.includes('  if(window.parent!==window)'));
vm.createContext(sandbox);vm.runInContext(script.replace('  if(window.parent!==window)',hook+'\n  if(window.parent!==window)'),sandbox,{timeout:5000});
const ui=sandbox.ui;
(async()=>{
 ui.connect();el('legacy-view').value='profiles';await ui.loadLegacy();
 assert.equal(ui.state().legacyLoaded.query.pin.through,240);assert(el('legacy-summary').textContent.includes('80 requests'));assert(el('legacy-profiles').children.length>0);assert(el('legacy-windows').children.length>0);assert(el('legacy-profile').textContent.includes('<img src=x'));assert(el('legacy-profiles').textContent.includes('<script>bad()'));
 el('legacy-model').value='should not override replay';await ui.loadLegacy(pin);assert.deepEqual(calls.at(-1).body.query,{pin});el('legacy-model').value='';
 el('legacy-view').value='quota';await ui.loadLegacy();assert(el('legacy-comparisons').children.length>0);assert(el('legacy-quota').textContent.includes('unavailable')||el('legacy-quota').textContent.includes('observed'));
 el('legacy-scenario-window').value='a-h';el('legacy-source-plan').value='claude-max-5';el('legacy-target-plan').value='claude-max-20';el('legacy-ratio').value='3';await ui.applyLegacyScenario();assert.equal(calls.at(-1).body.query.pin.capacity_ratio,3);assert.equal(calls.at(-1).body.query.pin.at,pin.at);assert(el('legacy-comparisons').textContent.includes('scenario'));
 await ui.loadLegacyForecasts('forecast-window',false);const fq=calls.at(-1).body;assert.equal(fq.query.pin.profile,'');assert(!('capacity_ratio' in fq.query.pin));assert(!('period' in fq.query.pin));assert.equal(el('legacy-forecasts').children.length,2);assert.equal(el('legacy-verifications').children.length,3);assert(el('legacy-verifications').textContent.includes('unscorable'));assert.equal(el('legacy-forecast-next').disabled,true);
 el('legacy-view').value='history';await ui.loadLegacy();assert.equal(calls.at(-1).body.query.pin.at,pin.at);assert.equal(ui.state().legacyLoaded.query.pin.through,240);assert(el('legacy-windows').children.length>0);await ui.loadLegacy(pin,'weekly');assert.equal(calls.at(-1).body.kind,'weekly');assert.equal(ui.state().legacyLoaded.kind,'weekly');
 await ui.loadLegacyProof('history-proof','forecast-window');assert.equal(el('legacy-proof-dialog').open,true);assert.equal(el('legacy-proof-totals').children.length,3);assert(el('legacy-proof-observations').children.length>0);
 await ui.legacyDownload('csv',true);assert.equal(downloads.at(-1).filename,'pacer-legacy-history-proof.csv');assert.equal(urls.size,1);
 // Deterministic pending network response, disconnect, then resolve. No sleep.
 hold='/legacy/forecasts';held=false;const pending=ui.loadLegacyForecasts('forecast-window',false);
 for(let i=0;i<20&&!held;i++)await Promise.resolve();assert(held);el('legacy-pin-input').value='saved private pin';el('disconnect').onclick();release();await pending;
 assert.equal(urls.size,0);assert.equal(ui.state().key,'');for(const k of ['legacyDefaultPin','legacyLoaded','legacyProof','legacyForecast'])assert.equal(ui.state()[k],null);assert.equal(ui.state().legacyBusy,false);assert.equal(el('legacy-pin-input').value,'');assert.equal(el('legacy-forecasts').children.length,0);assert.equal(el('legacy-windows').children.length,0);assert.equal(el('legacy-proof-dialog').open,false);assert.equal(el('legacy-profile').options.length,2);assert.equal(el('legacy-json').disabled,true);assert.equal(el('legacy-status').textContent,'Load a frozen view to check historical preparation.');
 // A stale export response cannot create a download after logout.
 hold='';ui.connect();el('legacy-view').value='profiles';await ui.loadLegacy();const count=downloads.length;hold='/legacy/export';held=false;const exportPending=ui.legacyDownload('json');for(let i=0;i<20&&!held;i++)await Promise.resolve();assert(held);el('disconnect').onclick();release();await exportPending;assert.equal(downloads.length,count);
 hold='';ready=false;ui.connect();el('legacy-view').value='profiles';await ui.loadLegacy();assert.equal(ui.state().legacyLoaded,null);assert(el('legacy-status').textContent.includes('offline re-preparation'));
 assert([...ui.allowed].filter(v=>v.includes('/history/legacy/')).length===9);assert(![...ui.allowed].some(v=>v.includes('/resource/')||v.includes('/prepare')));
 console.log('PASS: native inline syntax, structured DTO rendering, exact pin replay/scenarios, source-window forecasts, proof exports, readiness, text-only API content, logout clearing and stale-response/download guards. Browser acceptance remains pending.');
})().catch(error=>{console.error(error);process.exitCode=1});
`

// Source/fake-DOM contract only. Imports the unmodified inline module with fake
// transports; never starts a browser/server, evals code or contacts an endpoint.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import { test } from 'node:test';

const html=fs.readFileSync(new URL('../../internal/management/index.html',import.meta.url),'utf8');
const script=html.split('<script>')[1].split('</script>')[0];
const root='/v8/management/plugins/pacer/reports/forward/';
const version='pacer-forward-ledger-v1',hash='a'.repeat(64),nextHash='b'.repeat(64),revision='9007199254740993';
// Synthetic priced unit fixture only; it is not a native/browser pricing claim.
const token={uncached_input:0,cache_read:0,cache_write_5m:250000,cache_write_1h:750000,cache_write_unknown:0,output:0,reasoning:0};
const value={low_nano_usd:7250000000,high_nano_usd:7250000000,tokens:token,category_nano_usd:{},missing_price_requests:0,invalid_counter_requests:0};
const totals={source_record_count:1,canonical_rows:1,unique_attempts:1,valued_count:1,valued_subset:value,selection_value:value,reason_counts:{},coverage_reason_counts:{physical_coverage_unverified:1}};
const pin=filter=>({version,generation:hash,filter});
const report=p=>({pin:p,manifest:{version,report_at:'2026-10-08T01:00:03.000000001Z',profile_revision:revision,provenance:{reasons:['outside_usage_unknown']}},totals,capacity:'unavailable'});
const evidence=p=>({pin:p,id:'native:<img src=x>',account:'native-account:fixture',value,issues:[],coverage_reasons:['physical_coverage_unverified'],origins:[{id:'native:<img src=x>',source_id:'fixture-source',revision:'9223372036854775807',sequence:'9007199254740993',started_at:'2026-10-08T01:00:00.000000001Z',policy:{selected_ttl:'5m',effective_ttl:'1h'},attribution:{model:'claude-opus-5-5'},normalized_source_usage:{cache_create:1000000,cache_create_5m:250000,cache_create_1h:750000}}]});
const csv=Buffer.from('id,value\r\n"native:one\nline",7.25\r\n','utf8');
const exported=(format='csv')=>({filename:'pacer-forward.'+format,media_type:format==='csv'?'text/csv':'application/json',encoding:'base64',content:(format==='csv'?csv:Buffer.from('{"exact":true}\n')).toString('base64'),...(format==='csv'?{next_cursor:'csv-next'}:{})});
// Shape of the parent's saved readiness diagnostic, with synthetic identities.
// Tests do not open a live status URL, store, provider or mutable lab artifact.
const publisherStatus=()=>({version,state:'snapshot_available',feed_id:'c'.repeat(64),projected_through:'104',bootstrap_through:'104',latest_generation:hash,runtime:{state:'ready',target_through:'104'},publisher:{state:'ready',active:false,phase:'',source_head:'104',projected:'104',bootstrap_complete:true,latest_generation:hash,report_at:'2026-10-08T07:43:35.386974073Z',failures:'0',capacity:'unavailable'}});
let moduleSequence=0;
async function fixture(embedded=false){
  const nodes=new Map(),calls=[],downloads=[],urls=new Map(),revoked=[],timers=new Map(),listeners=new Map();let sequence=0;
  class Element{
    constructor(tag='div',id=''){this.tag=tag;this.id=id;this.children=[];this.dataset={};this.value='';this.checked=false;this.disabled=false;this.hidden=false;this.open=false;this.text='';}
    set innerHTML(_){throw Error('Unsafe HTML assignment')}
    set textContent(v){this.text=String(v??'');this.children=[]}
    get textContent(){return this.text+this.children.map(c=>c.textContent||'').join(' ')}
    append(...items){for(const item of items){this.children.push(item);if(item&&typeof item==='object')item.parent=this}}
    replaceChildren(...items){this.text='';this.children=[];this.append(...items)}
    get options(){return this.children}
    remove(index){if(typeof index==='number')this.children.splice(index,1);else if(this.parent)this.parent.children=this.parent.children.filter(x=>x!==this)}
    querySelectorAll(selector){return this.children.flatMap(c=>c instanceof Element?[...(c.tag===selector?[c]:[]),...c.querySelectorAll(selector)]:[])}
    querySelector(){return new Element()}
    setAttribute(k,v){this[k]=v}
    addEventListener(type,listener){this['on'+type]=listener}
    showModal(){this.open=true}close(){this.open=false}
    reset(){for(const n of this.querySelectorAll('input')){n.value='';n.checked=false}}
    click(){if(this.tag==='a')downloads.push({name:this.download,url:this.href,blob:urls.get(this.href)});else return this.onclick?.()}
  }
  for(const m of html.matchAll(/<([a-z]+)\b[^>]*\bid="([^"]+)"[^>]*>/g)){assert(!nodes.has(m[2]),'duplicate ID');nodes.set(m[2],new Element(m[1],m[2]))}
  const el=id=>{assert(nodes.has(id),'Missing '+id);return nodes.get(id)};
  for(const m of html.matchAll(/<select\b[^>]*id="([^"]+)"[^>]*>([\s\S]*?)<\/select>/g))for(const o of m[2].matchAll(/<option value="([^"]*)"[^>]*>([^<]*)<\/option>/g)){const n=new Element('option');n.value=o[1];n.textContent=o[2];el(m[1]).append(n)}
  let handle=async(path,body)=>{
    if(path==='/v0/management/plugins/pacer/state')return {routing:{mode:'off',modes:['off','on']},accounts:[]};
    assert(path.startsWith(root),'forward fallback or unrelated operation: '+path);
    const op=path.slice(root.length);
    if(op==='status')return {version,state:'snapshot_available',projected_through:revision,bootstrap_through:'0',latest_generation:hash,runtime:{state:'lagging',target_through:'9223372036854775807'}};
    if(op==='pin')return pin(body.filter);
    if(op==='report')return report(body.pin);
    if(op==='requests')return {pin:body.pin,rows:[evidence(body.pin)],next_cursor:body.cursor?'':'requests-next'};
    if(op==='evidence')return evidence(body.pin);
    if(op.startsWith('export-'))return exported(op.slice(7));
    if(op.startsWith('profiles?revision='))return {revision:op.split('=')[1],digest:hash,config:{revision,profiles:[{id:'work',name:'<script>text only</script>',provider:'claude',archived:true,memberships:[]}],bindings:[]}};
    if(op==='profiles')return {revision:'9007199254740994',publication:'pending',notification_enqueued:false};
    throw Error('Unexpected '+op);
  };
  const dispatch=async(path,body,method,signal)=>{calls.push({path,body:structuredClone(body),method,signal});return handle(path,body,method)};
  const loc={origin:'https://fixture.invalid'},window={addEventListener(name,fn){listeners.set(name,fn)}};
  const parent={postMessage(message,origin){assert.equal(origin,loc.origin);if(message.type==='pacer:hello')return;assert.equal(message.type,'pacer:request');Promise.resolve(dispatch(message.path,message.body,message.method)).then(data=>listeners.get('message')({source:parent,origin:loc.origin,data:{type:'pacer:response',version:1,id:message.id,ok:true,data}}),error=>listeners.get('message')({source:parent,origin:loc.origin,data:{type:'pacer:response',version:1,id:message.id,ok:false,status:error.status,error:'Fixed failure'}}))}};
  window.parent=embedded?parent:window;
  const originals=new Map();const set=(key,v)=>{originals.set(key,Object.getOwnPropertyDescriptor(globalThis,key));Object.defineProperty(globalThis,key,{value:v,configurable:true,writable:true})};
  set('window',window);set('location',loc);set('document',{getElementById:el,createElement:tag=>new Element(tag),createTextNode:text=>({textContent:text}),querySelectorAll:()=>[],querySelector:()=>null,addEventListener(){}});
  set('URL',{createObjectURL(blob){const url='blob:fixture-'+ ++sequence;urls.set(url,blob);return url},revokeObjectURL(url){urls.delete(url);revoked.push(url)}});
  set('setInterval',(fn,ms)=>{assert.equal(ms,30000);return ++sequence});set('clearInterval',()=>{});
  set('setTimeout',fn=>{timers.set(++sequence,fn);return sequence});set('clearTimeout',id=>timers.delete(id));
  set('fetch',async(path,opts)=>{assert.equal(opts.headers.Authorization,undefined);assert.equal(opts.credentials,'same-origin');assert.equal(opts.redirect,'error');assert.equal(opts.cache,'no-store');if(embedded)return {ok:false,status:401,json:async()=>({})};try{const data=await dispatch(path,opts.body?JSON.parse(opts.body):undefined,opts.method,opts.signal);return {ok:true,json:async()=>data}}catch(error){return {ok:false,status:error.status,json:async()=>({})}}});
  // Import the real script as an ES module: no source hooks, eval or vm code.
  await import('data:text/javascript;base64,'+Buffer.from(script+'\n// fixture '+ ++moduleSequence).toString('base64'));
  const submit=id=>el(id).onsubmit({preventDefault(){}});
  await new Promise(resolve=>setImmediate(resolve));
  if(embedded){listeners.get('message')({source:parent,origin:loc.origin,data:{type:'pacer:bridge-ready',version:1}});await new Promise(resolve=>setImmediate(resolve))}
  return {el,calls,downloads,urls,revoked,submit,setHandle(fn){const old=handle;handle=(...args)=>fn(...args,old)},async load(){await submit('forward-form')},send(data,source=parent,origin=loc.origin){listeners.get('message')({source,origin,data})},close(){for(const [k,d]of originals){if(d)Object.defineProperty(globalThis,k,d);else delete globalThis[k]}}};
}

test('forward source UI contracts (fake DOM/transport; no browser acceptance)',{timeout:10000},async t=>{
  await t.test('one pinned generation/filter survives status changes, editable filters, paging and raw CSV/JSON exports',async()=>{
    const f=await fixture(true);try{
      await f.load();assert.match(f.el('forward-summary').textContent,/\$7.25/);assert.match(f.el('forward-requests').textContent,/250000.*750000/);assert.match(f.el('forward-requests').textContent,/000000001 UTC/);const saved=f.el('forward-pin').textContent;
      f.el('forward-model').value='changed-control';f.setHandle((path,body,method,old)=>path.endsWith('/status')?{version,state:'snapshot_available',projected_through:'9223372036854775807',bootstrap_through:'0',latest_generation:nextHash,runtime:{state:'lagging',target_through:'9223372036854775807'}}:old(path,body,method));
      await f.el('forward-status-load').click();assert.match(f.el('forward-status').textContent,/9223372036854775807/);assert.equal(f.el('forward-pin').textContent,saved);
      await f.el('forward-next').click();const page=f.calls.findLast(c=>c.path.endsWith('/requests'));assert.deepEqual(page.body,{pin:JSON.parse(saved),limit:50,cursor:'requests-next'});
      await f.el('forward-csv').click();assert.deepEqual(Buffer.from(await f.downloads[0].blob.arrayBuffer()),csv);assert.equal(f.downloads[0].name,'pacer-forward.csv');assert.match(f.el('forward-download-status').textContent,/CSV page/);
      await f.el('forward-csv-next').click();assert.equal(f.calls.at(-1).body.cursor,'csv-next');assert.equal(f.calls.at(-1).body.limit,50);assert.deepEqual(f.calls.at(-1).body.pin,JSON.parse(saved));
      await f.el('forward-json').click();assert.equal(Buffer.from(await f.downloads.at(-1).blob.arrayBuffer()).toString(),'{"exact":true}\n');
      const evidenceButton=f.el('forward-requests').querySelectorAll('button')[0];await evidenceButton.click();assert(f.el('forward-evidence-dialog').open);assert.match(f.el('forward-evidence-body').textContent,/<img src=x>/);
      f.send({type:'pacer:bridge-disconnected',version:1});assert.equal(f.urls.size,0);assert.equal(f.el('forward-pin').textContent,'');assert(!f.el('forward-evidence-dialog').open);
    }finally{f.close()}
  });
  await t.test('explicit membership clearing uses loaded string CAS, never editable revision or omitted arrays',async()=>{
    const f=await fixture();try{await f.load();f.el('forward-profile-revision').value='1';f.el('forward-edit-id').value='work';f.el('forward-edit-name').value='Work';f.el('forward-edit-provider').value='claude';await f.submit('forward-profile-form');const body=f.calls.at(-1).body;assert.equal(body.expected_revision,revision);assert.deepEqual(body.profile.memberships,[]);assert.match(f.el('forward-profile-status').textContent,/publication pending/);assert.equal(JSON.parse(f.el('forward-pin').textContent).generation,hash);assert(f.el('forward-profile-save').disabled)}finally{f.close()}
  });
  await t.test('unsafe number, mismatched page pin and malformed download fail without rounded UI or click',async()=>{
    const f=await fixture();try{await f.load();f.setHandle((path,body,method,old)=>path.endsWith('/requests')?{pin:{...body.pin,generation:nextHash},rows:[]}:old(path,body,method));await f.el('forward-next').click();assert.match(f.el('forward-loaded').textContent,/changed the captured pin/);
      f.setHandle((path,body,method,old)=>path.endsWith('/export-json')?{...exported('json'),filename:'../../bad.json'}:old(path,body,method));await f.el('forward-json').click();assert.equal(f.downloads.length,0);
      f.setHandle((path,body,method,old)=>path.endsWith('/report')?{...report(body.pin),totals:{...totals,canonical_rows:9007199254740992}}:old(path,body,method));await f.load();assert.match(f.el('forward-loaded').textContent,/numeric precision/);assert.equal(f.el('forward-summary').textContent,'');assert(f.el('forward-json').disabled);
    }finally{f.close()}
  });
  await t.test('delayed disconnect/reload discard pending report and export results',async()=>{
    const f=await fixture(true);try{await f.load();let release,entered;const ready=new Promise(resolve=>entered=resolve);f.setHandle(async(path,body,method,old)=>{if(path.endsWith('/export-json')){entered();await new Promise(resolve=>release=resolve)}return old(path,body,method)});const work=f.el('forward-json').click();await ready;f.send({type:'pacer:bridge-disconnected',version:1});release();await work;await new Promise(resolve=>setImmediate(resolve));assert.equal(f.downloads.length,0);assert.equal(f.el('forward-pin').textContent,'');assert.equal(f.urls.size,0)}finally{f.close()}
  });
  await t.test('new load cancels the old epoch; late pin/report cannot overwrite the newly captured generation',async()=>{
    const f=await fixture();try{
      let release,entered,first=true;const ready=new Promise(resolve=>entered=resolve);
      f.setHandle(async(path,body,method,old)=>{if(path.endsWith('/report')&&first){first=false;entered();await new Promise(resolve=>release=resolve);return report(body.pin)}if(path.endsWith('/pin')&&!first)return {...pin(body.filter),generation:nextHash};return old(path,body,method)});
      const pending=f.load();await ready;const signal=f.calls.at(-1).signal;await f.load();assert(signal.aborted);assert.equal(JSON.parse(f.el('forward-pin').textContent).generation,nextHash);release();await pending;assert.equal(JSON.parse(f.el('forward-pin').textContent).generation,nextHash);
    }finally{f.close()}
  });
  await t.test('dated membership form preserves exact UTC millisecond bounds, archived options and fixed pinned reports',async()=>{
    const f=await fixture();try{
      await f.load();assert.match(f.el('forward-profile').textContent,/archived/);const saved=f.el('forward-pin').textContent;f.el('forward-edit-id').value='new';f.el('forward-edit-name').value='New';f.el('forward-edit-provider').value='claude';f.el('forward-membership-add').click();
      for(const input of f.el('forward-memberships').querySelectorAll('input'))input.value={account:'native-account:fixture',start_ms:'2026-10-08T00:00:00.001Z',end_ms:'2026-10-09T00:00:00Z'}[input.dataset.field];
      await f.submit('forward-profile-form');const body=f.calls.at(-1).body;assert.equal(body.expected_revision,revision);assert.deepEqual(body.profile.memberships,[{account:'native-account:fixture',start_ms:'1791417600001',end_ms:'1791504000000'}]);assert.equal(f.el('forward-pin').textContent,saved);
    }finally{f.close()}
  });
  await t.test('large bounded base64 bytes are decoded without text conversion; noncanonical padding and MIME fail',async()=>{
    const f=await fixture();try{
      await f.load();const large=Buffer.alloc(2*1024*1024,0x80);f.setHandle((path,body,method,old)=>path.endsWith('/export-csv')?{...exported(),content:large.toString('base64')}:old(path,body,method));await f.el('forward-csv').click();assert.deepEqual(Buffer.from(await f.downloads[0].blob.arrayBuffer()),large);
      for(const envelope of [{...exported(),content:'YR=='},{...exported(),media_type:'text/html'},{...exported(),content:'a==='}]){f.setHandle((path,body,method,old)=>path.endsWith('/export-csv')?envelope:old(path,body,method));await f.el('forward-csv').click();assert.equal(f.downloads.length,1);}
    }finally{f.close()}
  });
  await t.test('wrapped status uses exact quotes and keeps optional runtime fields conservative',async()=>{
    const f=await fixture();try{
      f.setHandle((path,body,method,old)=>path.endsWith('/status')?{status:{version,projected_through:revision,bootstrap_through:'0',latest_generation:hash},runtime:{state:'paused',source_head:'9223372036854775807',target_through:revision,target_at:'2026-10-08T01:00:03.000000001Z',other:'ignored',unknown_counter:9007199254740992}}:old(path,body,method));
      await f.el('forward-status-load').click();assert.match(f.el('forward-status').textContent,/Paused/);assert.match(f.el('forward-status').textContent,/9223372036854775807/);assert.match(f.el('forward-status').textContent,/000000001 UTC/);assert(!f.el('forward-status').textContent.includes('ignored'));
    }finally{f.close()}
  });
  await t.test('actual publisher DTO shape retains exact sealed receipt independently of non-atomic ledger/target metadata',async()=>{
    const f=await fixture(true);try{
      await f.load();const saved=f.el('forward-pin').textContent;let status=publisherStatus();
      f.setHandle((path,body,method,old)=>path.endsWith('/status')?status:old(path,body,method));
      await f.el('forward-status-load').click();let text=f.el('forward-status').textContent;
      assert.match(text,/Publisher snapshot: Ready · active no/);assert.match(text,/observed source head 104/);assert.match(text,/report T 2026-10-08 07:43:35\.386974073 UTC/);assert.match(text,/publisher target T Unknown/);
      assert.deepEqual(JSON.parse(f.el('forward-status-receipt').textContent),status);
      status={...status,projected_through:'9223372036854775807',bootstrap_through:revision,latest_generation:nextHash,runtime:{state:'lagging',target_through:'9223372036854775806',unknown_counter:9007199254740992},publisher:{...status.publisher,state:'lag',active:true,phase:'seal',source_head:'9223372036854775806',projected:revision,failures:'18446744073709551615',target_at:'2026-10-08T07:44:00.000000001Z',future_metadata:[{ignored:9007199254740992}]}};
      await f.el('forward-status-load').click();text=f.el('forward-status').textContent;const receipt=JSON.parse(f.el('forward-status-receipt').textContent);
      assert.match(text,/Ledger snapshot: .*projected sequence 9223372036854775807/);assert(text.split('\n').find(x=>x.startsWith('Ledger snapshot:')).endsWith(nextHash));
      assert(text.split('\n').find(x=>x.startsWith('Last sealed publication')).includes(hash));assert(!text.split('\n').find(x=>x.startsWith('Last sealed publication')).includes(nextHash));
      assert.match(text,/Publisher snapshot: Lagging · active yes · phase seal/);assert.match(text,/Next target metadata: adapter target head 9223372036854775806 · publisher target T 2026-10-08 07:44:00\.000000001 UTC/);
      assert.equal(receipt.publisher.failures,'18446744073709551615');assert.equal(receipt.publisher.report_at,'2026-10-08T07:43:35.386974073Z');assert.equal(receipt.publisher.target_at,'2026-10-08T07:44:00.000000001Z');assert.equal(receipt.publisher.projected,revision);assert(!('future_metadata' in receipt.publisher));assert(!('unknown_counter' in receipt.runtime));
      assert.match(text,/Separate, non-atomic/);assert.match(text,/not published watermarks or record totals/);assert.equal(f.el('forward-pin').textContent,saved);
      await f.el('forward-next').click();assert.deepEqual(f.calls.at(-1).body.pin,JSON.parse(saved));f.send({type:'pacer:bridge-disconnected',version:1});assert.equal(f.el('forward-status-receipt').textContent,'');
    }finally{f.close()}
  });
  await t.test('publisher states/phases and fixed errors keep the prior pin available; initialization invents no sealed receipt',async()=>{
    const f=await fixture();try{
      await f.load();const saved=f.el('forward-pin').textContent;let status=publisherStatus();f.setHandle((path,body,method,old)=>path.endsWith('/status')?status:old(path,body,method));
      for(const [state,label] of [['disabled','Disabled'],['initializing','Initializing'],['lag','Lagging'],['error','Error'],['ready','Ready'],['stopped','Paused / stopped']]){
        status.publisher={...status.publisher,state,active:state==='lag',phase:state==='lag'?'seal':'',...(state==='error'?{error_code:'forward_seal_failed',failures:'2'}:{error_code:'',failures:'0'})};
        await f.el('forward-status-load').click();const text=f.el('forward-status').textContent;assert(text.includes('Publisher snapshot: '+label));if(state==='error'){assert.match(text,/error forward_seal_failed/);assert(!text.includes('Publisher snapshot: Ready'))}
        assert.equal(f.el('forward-pin').textContent,saved);assert(!f.el('forward-json').disabled);assert(!f.el('forward-next').disabled);
      }
      for(const phase of ['bootstrap','projection','provenance_feed','provenance','seal']){status.publisher={...status.publisher,state:'lag',active:true,phase};await f.el('forward-status-load').click();assert(f.el('forward-status').textContent.includes('phase '+phase));}
      status.publisher={...status.publisher,state:'initializing',phase:'bootstrap',source_head:'0',projected:'0',bootstrap_complete:false};delete status.publisher.latest_generation;delete status.publisher.report_at;
      await f.el('forward-status-load').click();assert.match(f.el('forward-status').textContent,/Last sealed publication \(publisher\): generation Unknown · report T Unknown/);assert.match(f.el('forward-status').textContent,/bootstrap complete no/);assert.equal(f.el('forward-pin').textContent,saved);
    }finally{f.close()}
  });
  await t.test('known malformed/future publisher fields reject ready fallback, arrays and integer coercion while preserving the loaded pin',async()=>{
    const f=await fixture();try{
      await f.load();const saved=f.el('forward-pin').textContent;let status=publisherStatus();f.setHandle((path,body,method,old)=>path.endsWith('/status')?status:old(path,body,method));
      const badPublishers=[null,[],['ready'],{},...[
        {state:'future_ready'},{state:['ready']},{active:'false'},{active:undefined},{bootstrap_complete:'true'},{bootstrap_complete:undefined},{capacity:'available'},{capacity:undefined},{phase:'future_seal'},{phase:[]},
        {source_head:104},{source_head:'01'},{source_head:'9223372036854775808'},{projected:9007199254740992},{projected:'-1'},{failures:0},{failures:'18446744073709551616'},{failures:'00'},{failures:'1e3'},
        {latest_generation:[]},{latest_generation:'A'.repeat(64)},{report_at:1791445415386974073n.toString()},{report_at:'2026-02-30T01:00:00Z'},{report_at:'2026-10-08T07:43:35.3869740730Z'},{target_at:null},{error_code:'raw SQL private error'},
      ].map(p=>({...publisherStatus().publisher,...p}))];
      for(const publisher of badPublishers){status=publisherStatus();await f.el('forward-status-load').click();assert(f.el('forward-status-receipt').textContent);status={...status,publisher};await f.el('forward-status-load').click();assert.match(f.el('forward-status').textContent,/Unsupported forward publisher status/);assert(!f.el('forward-status').textContent.includes('Publisher snapshot: Ready'));assert.equal(f.el('forward-status-receipt').textContent,'');assert.equal(f.el('forward-pin').textContent,saved)}
      for(const malformed of [[],null,{...publisherStatus(),runtime:[]},{...publisherStatus(),status:[]},{...publisherStatus(),latest_generation:[]},{...publisherStatus(),projected_through:104},{...publisherStatus(),runtime:{state:'ready',target_through:104}}]){status=malformed;await f.el('forward-status-load').click();assert.match(f.el('forward-status').textContent,/Unsupported forward/);assert.equal(f.el('forward-pin').textContent,saved);assert.equal(f.el('forward-status-receipt').textContent,'')}
      await f.el('forward-json').click();assert.deepEqual(f.calls.at(-1).body.pin,JSON.parse(saved));assert.equal(f.downloads.length,1);
    }finally{f.close()}
  });
  await t.test('older runtime-only adapter does not fabricate publisher heads, sealed generation or dates',async()=>{
    const f=await fixture();try{
      const status=publisherStatus();delete status.publisher;status.runtime={state:'ready'};f.setHandle((path,body,method,old)=>path.endsWith('/status')?status:old(path,body,method));
      await f.el('forward-status-load').click();const text=f.el('forward-status').textContent;assert.match(text,/Adapter runtime: Ready · observed source head Unknown · target head Unknown · report T \(runtime only\) Unknown · target T Unknown/);assert.match(text,/Publisher metadata unavailable · last sealed generation \/ T: Unknown/);assert(!Object.hasOwn(JSON.parse(f.el('forward-status-receipt').textContent),'publisher'));
    }finally{f.close()}
  });
  await t.test('unvalued native-shaped records display unknown selection/row prices; the synthetic $7.25 case is not a native pricing expectation',async()=>{
    const f=await fixture();try{
      f.setHandle((path,body,method,old)=>{
        if(path.endsWith('/report'))return {...report(body.pin),totals:{...totals,valued_count:0,valued_subset:{...value,low_nano_usd:0,high_nano_usd:0},selection_value:null,reason_counts:{price_missing:1}}};
        if(path.endsWith('/requests'))return {pin:body.pin,rows:[{...evidence(body.pin),value:null,issues:['price_missing']}],next_cursor:''};
        return old(path,body,method);
      });await f.load();assert.match(f.el('forward-summary').textContent,/0 valued, 1 unvalued\. Value of the whole selection: Unknown/);assert.match(f.el('forward-requests').textContent,/Unknown/);assert(!f.el('forward-requests').textContent.includes('$'));assert.match(f.el('forward-reasons').textContent,/price missing/);
    }finally{f.close()}
  });
  await t.test('embedded unavailable never falls back and source/origin fences precede disconnect',async()=>{
    const f=await fixture(true);try{await f.load();f.send({type:'pacer:bridge-disconnected',version:1},{},'https://fixture.invalid');assert(f.el('forward-pin').textContent);f.send({type:'pacer:bridge-disconnected',version:1},undefined,'https://other.invalid');assert(f.el('forward-pin').textContent);
      const before=f.calls.length;f.setHandle(async()=>{throw Object.assign(Error('unsupported'),{status:404})});await f.load();assert.match(f.el('forward-loaded').textContent,/unsupported/);assert.equal(f.calls.length,before+1);assert.equal(f.calls.at(-1).path,root+'pin');f.send({type:'pacer:bridge-disconnected',version:1});assert(f.el('workspace').hidden);
    }finally{f.close()}
  });
});

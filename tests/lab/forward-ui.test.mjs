// Source/fake-DOM contract only. Imports the unmodified inline module with fake
// transports; never starts a browser/server, evals code or contacts an endpoint.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import { test } from 'node:test';

const html=fs.readFileSync(new URL('../../internal/management/index.html',import.meta.url),'utf8');
const script=html.split('<script>')[1].split('</script>')[0];
const root='/v8/management/plugins/pacer/reports/forward/';
const version='pacer-forward-ledger-v1',hash='a'.repeat(64),nextHash='b'.repeat(64),revision='9007199254740993';
const token={uncached_input:0,cache_read:0,cache_write_5m:250000,cache_write_1h:750000,cache_write_unknown:0,output:0,reasoning:0};
const value={low_nano_usd:7250000000,high_nano_usd:7250000000,tokens:token,category_nano_usd:{},missing_price_requests:0,invalid_counter_requests:0};
const totals={source_record_count:1,canonical_rows:1,unique_attempts:1,valued_count:1,valued_subset:value,selection_value:value,reason_counts:{},coverage_reason_counts:{physical_coverage_unverified:1}};
const pin=filter=>({version,generation:hash,filter});
const report=p=>({pin:p,manifest:{version,report_at:'2026-10-08T01:00:03.000000001Z',profile_revision:revision,provenance:{reasons:['outside_usage_unknown']}},totals,capacity:'unavailable'});
const evidence=p=>({pin:p,id:'native:<img src=x>',account:'native-account:fixture',value,issues:[],coverage_reasons:['physical_coverage_unverified'],origins:[{id:'native:<img src=x>',source_id:'fixture-source',revision:'9223372036854775807',sequence:'9007199254740993',started_at:'2026-10-08T01:00:00.000000001Z',policy:{selected_ttl:'5m',effective_ttl:'1h'},attribution:{model:'claude-opus-5-5'},normalized_source_usage:{cache_create:1000000,cache_create_5m:250000,cache_create_1h:750000}}]});
const csv=Buffer.from('id,value\r\n"native:one\nline",7.25\r\n','utf8');
const exported=(format='csv')=>({filename:'pacer-forward.'+format,media_type:format==='csv'?'text/csv':'application/json',encoding:'base64',content:(format==='csv'?csv:Buffer.from('{"exact":true}\n')).toString('base64'),...(format==='csv'?{next_cursor:'csv-next'}:{})});
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
    showModal(){this.open=true}close(){this.open=false}
    reset(){for(const n of this.querySelectorAll('input')){n.value='';n.checked=false}}
    click(){if(this.tag==='a')downloads.push({name:this.download,url:this.href,blob:urls.get(this.href)});else return this.onclick?.()}
  }
  for(const m of html.matchAll(/<([a-z]+)\b[^>]*\bid="([^"]+)"[^>]*>/g)){assert(!nodes.has(m[2]),'duplicate ID');nodes.set(m[2],new Element(m[1],m[2]))}
  const el=id=>{assert(nodes.has(id),'Missing '+id);return nodes.get(id)};
  for(const m of html.matchAll(/<select\b[^>]*id="([^"]+)"[^>]*>([\s\S]*?)<\/select>/g))for(const o of m[2].matchAll(/<option value="([^"]*)"[^>]*>([^<]*)<\/option>/g)){const n=new Element('option');n.value=o[1];n.textContent=o[2];el(m[1]).append(n)}
  let handle=async(path,body)=>{
    if(path==='/v0/management/plugins/pacer/state')return {routing:{mode:'off',can_activate:false},accounts:[]};
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
  set('window',window);set('location',loc);set('document',{getElementById:el,createElement:tag=>new Element(tag),createTextNode:text=>({textContent:text}),querySelectorAll:()=>[]});
  set('URL',{createObjectURL(blob){const url='blob:fixture-'+ ++sequence;urls.set(url,blob);return url},revokeObjectURL(url){urls.delete(url);revoked.push(url)}});
  set('setTimeout',fn=>{timers.set(++sequence,fn);return sequence});set('clearTimeout',id=>timers.delete(id));
  set('fetch',async(path,opts)=>{assert.equal(opts.headers.Authorization,'Bearer synthetic-memory-key');assert.equal(opts.redirect,'error');assert.equal(opts.cache,'no-store');try{return {ok:true,json:async()=>dispatch(path,opts.body?JSON.parse(opts.body):undefined,opts.method,opts.signal)}}catch(error){return {ok:false,status:error.status,json:async()=>({})}}});
  // Import the real script as an ES module: no source hooks, eval or vm code.
  await import('data:text/javascript;base64,'+Buffer.from(script+'\n// fixture '+ ++moduleSequence).toString('base64'));
  const submit=id=>el(id).onsubmit({preventDefault(){}});
  if(embedded){listeners.get('message')({source:parent,origin:loc.origin,data:{type:'pacer:bridge-ready',version:1}});await Promise.resolve();await Promise.resolve()}else{el('key').value='synthetic-memory-key';await submit('signin')}
  return {el,calls,downloads,urls,revoked,submit,setHandle(fn){const old=handle;handle=(...args)=>fn(...args,old)},async load(){await submit('forward-form')},send(data,source=parent,origin=loc.origin){listeners.get('message')({source,origin,data})},close(){for(const [k,d]of originals){if(d)Object.defineProperty(globalThis,k,d);else delete globalThis[k]}}};
}

test('forward source UI contracts (fake DOM/transport; no browser acceptance)',{timeout:10000},async t=>{
  await t.test('one pinned generation/filter survives status changes, editable filters, paging and raw CSV/JSON exports',async()=>{
    const f=await fixture();try{
      await f.load();assert.match(f.el('forward-summary').textContent,/\$7.25/);assert.match(f.el('forward-requests').textContent,/250000.*750000/);assert.match(f.el('forward-requests').textContent,/000000001 UTC/);const saved=f.el('forward-pin').textContent;
      f.el('forward-model').value='changed-control';f.setHandle((path,body,method,old)=>path.endsWith('/status')?{version,state:'snapshot_available',projected_through:'9223372036854775807',bootstrap_through:'0',latest_generation:nextHash,runtime:{state:'lagging',target_through:'9223372036854775807'}}:old(path,body,method));
      await f.el('forward-status-load').click();assert.match(f.el('forward-status').textContent,/9223372036854775807/);assert.equal(f.el('forward-pin').textContent,saved);
      await f.el('forward-next').click();const page=f.calls.findLast(c=>c.path.endsWith('/requests'));assert.deepEqual(page.body,{pin:JSON.parse(saved),limit:50,cursor:'requests-next'});
      await f.el('forward-csv').click();assert.deepEqual(Buffer.from(await f.downloads[0].blob.arrayBuffer()),csv);assert.equal(f.downloads[0].name,'pacer-forward.csv');assert.match(f.el('forward-download-status').textContent,/CSV page/);
      await f.el('forward-csv-next').click();assert.equal(f.calls.at(-1).body.cursor,'csv-next');assert.equal(f.calls.at(-1).body.limit,50);assert.deepEqual(f.calls.at(-1).body.pin,JSON.parse(saved));
      await f.el('forward-json').click();assert.equal(Buffer.from(await f.downloads.at(-1).blob.arrayBuffer()).toString(),'{"exact":true}\n');
      const evidenceButton=f.el('forward-requests').querySelectorAll('button')[0];await evidenceButton.click();assert(f.el('forward-evidence-dialog').open);assert.match(f.el('forward-evidence-body').textContent,/<img src=x>/);
      f.el('disconnect').click();assert.equal(f.urls.size,0);assert.equal(f.el('forward-pin').textContent,'');assert(!f.el('forward-evidence-dialog').open);
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
    const f=await fixture();try{await f.load();let release,entered;const ready=new Promise(resolve=>entered=resolve);f.setHandle(async(path,body,method,old)=>{if(path.endsWith('/export-json')){entered();await new Promise(resolve=>release=resolve)}return old(path,body,method)});const work=f.el('forward-json').click();await ready;const signal=f.calls.at(-1).signal;f.el('disconnect').click();assert(signal.aborted);release();await work;assert.equal(f.downloads.length,0);assert.equal(f.el('forward-pin').textContent,'');assert.equal(f.urls.size,0)}finally{f.close()}
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
  await t.test('wrapped status uses exact quotes, optional runtime fields stay conservative and no refresh interval is scheduled',async()=>{
    const f=await fixture();try{
      f.setHandle((path,body,method,old)=>path.endsWith('/status')?{status:{version,projected_through:revision,bootstrap_through:'0',latest_generation:hash},runtime:{state:'paused',source_head:'9223372036854775807',target_through:revision,target_at:'2026-10-08T01:00:03.000000001Z',other:'ignored',unknown_counter:9007199254740992}}:old(path,body,method));
      await f.el('forward-status-load').click();assert.match(f.el('forward-status').textContent,/Paused/);assert.match(f.el('forward-status').textContent,/9223372036854775807/);assert.match(f.el('forward-status').textContent,/000000001 UTC/);assert(!f.el('forward-status').textContent.includes('ignored'));
    }finally{f.close()}
  });
  await t.test('embedded unavailable never falls back and source/origin fences precede disconnect',async()=>{
    const f=await fixture(true);try{await f.load();f.send({type:'pacer:bridge-disconnected',version:1},{},'https://fixture.invalid');assert(f.el('forward-pin').textContent);f.send({type:'pacer:bridge-disconnected',version:1},undefined,'https://other.invalid');assert(f.el('forward-pin').textContent);
      const before=f.calls.length;f.setHandle(async()=>{throw Object.assign(Error('unsupported'),{status:404})});await f.load();assert.match(f.el('forward-loaded').textContent,/unsupported/);assert.equal(f.calls.length,before+1);assert.equal(f.calls.at(-1).path,root+'pin');f.send({type:'pacer:bridge-disconnected',version:1});assert(f.el('workspace').hidden);
    }finally{f.close()}
  });
});

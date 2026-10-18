// The Pacer page's routing settings in Chromium, against a stub of Pacer's
// state and settings routes: render, the ordered summary-model list with
// suggestions, saving, refusals and pausing an account for new chats.
import { chromium } from '/b/node_modules/playwright/index.mjs';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import http from 'node:http';

const html=fs.readFileSync(new URL('../../internal/management/index.html',import.meta.url));
const defaults={compact_on_switch:false,compaction_models:[],band_percent:90,new_chat_minimum_percent:15,short_floor_percent:2,long_floor_percent:1,warm_minutes:60,transient_retries:3,force_warning_percent:5,due_slack_hours:48,providers:['claude'],paused_accounts:[]};
let settings=structuredClone(defaults);
const patches=[];
const view=()=>({settings,seed:defaults,changed:Object.keys(settings).filter(k=>JSON.stringify(settings[k])!==JSON.stringify(defaults[k])),limits:{compaction_models:8}});
const models=['claude-opus-5-5','claude-sonnet-4-6','gemini-3.8-flash-high','gemini-opus-4-6-thinking'].map(id=>({channel:'claude',id,name:id,exposed:[id],enabled:true,upstream:id}));
models.push({channel:'antigravity',id:'claude-opus-5-5-high',name:'claude-opus-5-5-high',exposed:['claude-opus-5-5-high'],enabled:false,upstream:'claude-opus-5-5-high'});
const server=http.createServer(async(req,res)=>{
  if(req.url==='/'){res.setHeader('Content-Type','text/html');res.end(html);return}
  let raw='';for await(const part of req)raw+=part;
  const body=raw?JSON.parse(raw):{};
  let data,status=200;
  if(req.url.endsWith('/state'))data={routing:{mode:'on',modes:['off','on'],providers:settings.providers},settings:view(),accounts:[
    {id:'claude-jan.json',alias:'jan',account:'claude:jan',provider:'claude',state:'current',pace:0.5,paused:settings.paused_accounts.includes('claude:jan'),windows:[]},
    {id:'claude-ilse.json',alias:'ilse',account:'claude:ilse',provider:'claude',state:'current',pace:2.4,usd_per_percent:2.88,need_hours:6.2,due:true,paused:settings.paused_accounts.includes('claude:ilse'),windows:[]}],decisions:[]};
  else if(req.url.endsWith('/settings')&&req.method==='PATCH'){
    patches.push(body);
    if(body.band_percent!==undefined&&body.band_percent<50){status=400;data={error:'Settings not saved: band is 50 to 100 %'}}
    else{settings={...settings,...body};data=view()}
  }
  else if(req.url==='/v8/management/oauth/settings/models')data={models,'thinking-levels':[],'channel-info':[]};
  else {res.writeHead(404).end();return}
  res.writeHead(status,{'Content-Type':'application/json'});res.end(JSON.stringify(data));
});
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
const browser=await chromium.launch({headless:true});
try{
  const page=await browser.newPage({viewport:{width:1280,height:1100}});
  page.setDefaultTimeout(10000);
  const errors=[];page.on('pageerror',e=>errors.push(e.message));
  await page.goto(`http://127.0.0.1:${server.address().port}/`);
  await page.locator('#workspace').waitFor();
  await page.locator('#settings-form').waitFor();
  // Rendered from the state.
  assert.equal(await page.getByLabel('Compact on switch').isChecked(),false);
  assert.equal(await page.locator('#set-band').inputValue(),'90');
  assert.equal(await page.locator('#set-warm').inputValue(),'60');
  assert.equal(await page.locator('#set-retries').inputValue(),'3');
  assert.equal(await page.locator('#set-force-warning').inputValue(),'5');
  assert.equal(await page.locator('#set-due-slack').inputValue(),'48');
  // The measured value, the hours your use needs, and whether the account is due.
  assert.match(await page.locator('#accounts').innerText(),/2\.40 %\/h · \$2\.88 per 1% · your use needs 6 h · due/);
  assert.doesNotMatch(await page.locator('#accounts').innerText(),/0\.50 %\/h ·/);
  assert.equal(await page.locator('#set-claude').isChecked(),true);
  assert.equal(await page.locator('#set-models input').count(),1,'one empty field to start');
  // Suggestions: the models CPA serves, not disabled ones.
  await page.waitForFunction(()=>document.querySelectorAll('#model-options option').length===4);
  assert(!(await page.locator('#model-options').innerHTML()).includes('claude-opus-5-5-high'));
  // Each model filled in opens the next fallback field.
  await page.getByLabel('Compact on switch').check();
  await page.getByLabel('Summary model',{exact:true}).fill('claude-sonnet-4-6');
  assert.equal(await page.locator('#set-models input').count(),2);
  await page.getByLabel('Fallback model 1',{exact:true}).fill('gemini-opus-4-6-thinking');
  assert.equal(await page.locator('#set-models input').count(),3);
  await page.getByLabel('Fallback model 2',{exact:true}).fill('gemini-3.8-flash-high');
  await page.getByLabel('Fallback model 2',{exact:true}).blur();
  assert.equal(await page.locator('#set-models input').count(),4);
  // Reorder and remove.
  await page.getByRole('button',{name:'Move up gemini-3.8-flash-high'}).click();
  assert.deepEqual(await page.locator('#set-models input').evaluateAll(i=>i.map(x=>x.value)),['claude-sonnet-4-6','gemini-3.8-flash-high','gemini-opus-4-6-thinking','']);
  await page.getByRole('button',{name:'Remove gemini-opus-4-6-thinking'}).click();
  assert.equal(await page.locator('#set-models input').count(),3);
  await page.locator('#set-warm').fill('5');
  await page.locator('#set-retries').fill('2');
  await page.locator('#set-force-warning').fill('7.5');
  await page.locator('#set-due-slack').fill('36');
  await page.locator('#set-codex').check();
  // An auto-refresh while editing keeps the edits.
  await page.evaluate(()=>document.activeElement.blur());
  await page.locator('#save-mode').evaluate(b=>b.focus());
  await page.getByRole('button',{name:'Save settings'}).click();
  await page.waitForFunction(()=>document.getElementById('message').textContent.startsWith('Settings saved'));
  assert.deepEqual(patches.at(-1),{compact_on_switch:true,compaction_models:['claude-sonnet-4-6','gemini-3.8-flash-high'],providers:['claude','codex'],band_percent:90,new_chat_minimum_percent:15,short_floor_percent:2,long_floor_percent:1,warm_minutes:5,transient_retries:2,force_warning_percent:7.5,due_slack_hours:36});
  assert.match(await page.locator('#settings-status').innerText(),/compact on switch, summary models, warm for, retries, forced move warning, slack before a reset, providers/);
  // A name CPA does not list is saved with a note.
  await page.getByLabel('Fallback model 2',{exact:true}).fill('my-private-model');
  await page.getByRole('button',{name:'Save settings'}).click();
  await page.waitForFunction(()=>document.getElementById('message').textContent.includes('Not in CPA’s model list: my-private-model'));
  // Refusals: browser validation, then Pacer's.
  const before=patches.length;
  await page.locator('#set-band').fill('120');
  await page.getByRole('button',{name:'Save settings'}).click();
  await page.waitForFunction(()=>document.getElementById('message').className.includes('error'));
  assert.equal(patches.length,before,'invalid band not sent');
  await page.locator('#set-band').fill('40');
  await page.locator('#set-band').evaluate(i=>{i.min='0'});
  await page.getByRole('button',{name:'Save settings'}).click();
  await page.waitForFunction(()=>document.getElementById('message').textContent.includes('band is 50 to 100'));
  await page.getByRole('button',{name:'Revert unsaved changes'}).click();
  assert.equal(await page.locator('#set-band').inputValue(),'90');
  // Pause an account for new chats.
  await page.getByLabel('New chats on ilse').uncheck();
  await page.waitForFunction(()=>document.getElementById('message').textContent.startsWith('Paused for new chats: ilse'));
  assert.deepEqual(patches.at(-1),{paused_accounts:['claude:ilse']});
  assert.equal(await page.getByLabel('New chats on ilse').isChecked(),false);
  assert.match(await page.locator('#accounts').innerText(),/Paused/);
  fs.mkdirSync('/evidence',{recursive:true});
  await page.locator('#pacer').screenshot({path:'/evidence/settings-wide.png'});
  await page.setViewportSize({width:390,height:844});
  assert(await page.evaluate(()=>document.documentElement.scrollWidth-window.innerWidth)<=1,'no horizontal scroll');
  await page.locator('#pacer').screenshot({path:'/evidence/settings-narrow.png'});
  assert.deepEqual(errors,[]);
  console.log('PASS: settings render; summary-model list opens a field per model, suggests CPA models, reorders/removes; save; refusals; revert; pause for new chats; narrow layout.');
}finally{await browser.close();await new Promise(resolve=>server.close(resolve))}

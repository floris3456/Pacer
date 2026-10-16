// Production UI in Chromium, with deterministic CPA/usage responses.
import { chromium } from '/b/node_modules/playwright/index.mjs';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import http from 'node:http';

const html=fs.readFileSync(new URL('../../internal/management/index.html',import.meta.url));
const start=Date.parse('2026-10-11T09:00:00Z'),week=7*86400000;
const rate={status:'available',usd_per_percent:{low:4,high:4},start_percent:0,end_percent:100,delta_pp:100,samples:2};
const window=(id,scope,from,end,used,status='closed')=>({id,kind:'weekly',scope,label:'Test account',start_ms:from,end_ms:end,reset_ms:end,status,used_percent:used,rate:{...rate,end_percent:used,delta_pp:used},selected:{requests:1200,value_low_nano_usd:400e9,value_high_nano_usd:400e9},models:[{model:'gpt-6-astra',share_percent:100}]});
const current=window('current','codex-main',start,start+week,4,'current');
const previous=window('previous','codex-main',start-week,start,100);
const spark=window('spark','model:gpt-5.3-codex-spark',start-week,start,50);
const profile={reference:'profile:test',name:'ChatGPT Pro 20x',provider:'codex',credentials:1,current:[],pools:[{kind:'weekly',scope:'codex-main',main:true,ended:false,windows:2,latest:current,average:rate},{kind:'weekly',scope:spark.scope,label:'Spark',main:false,ended:true,windows:1,latest:spark,average:rate}]};
const model={channel:'claude',source:'account',upstream:'claude-fable-5',id:'claude-fable-5',name:'claude-fable-5',enabled:false,display_name:'Claude Fable 5',kind:'chat',setting:null,supports:{'max-output-tokens':true,'keep-original':true}};
const patches=[];
const polling=[];
profile.polling_seconds=1800;profile.polling_revision=0;
profile.credentials=2;
profile.current=[
  {account:'a',label:'Account A',kind:'weekly',scope:'codex-main',used_percent:100},
  {account:'a',label:'Account A',kind:'5h',scope:'codex-main',used_percent:0},
  {account:'b',label:'Account B',kind:'weekly',scope:'codex-main',used_percent:25},
  {account:'b',label:'Account B',kind:'5h',scope:'codex-main',used_percent:50},
  {account:'b',label:'Account B',kind:'monthly',scope:'codex-main',used_percent:10},
];
const server=http.createServer(async(req,res)=>{
  if(req.url==='/'){res.setHeader('Content-Type','text/html');res.end(html);return}
  let raw='';for await(const part of req)raw+=part;
  const body=raw?JSON.parse(raw):{};
  let data;
  if(req.url.endsWith('/state'))data={routing:{mode:'off',modes:['off','on']},accounts:[]};
  else if(req.url==='/v8/management/config/access/api-keys')data=['synthetic-secret-one','synthetic-secret-two'];
  else if(req.url.endsWith('/keys'))data={keys:[],groups:[],revision:0,editable:false};
  else if(req.url==='/v8/management/oauth/settings/models')data={models:[model],'thinking-levels':['low','medium','high','xhigh','max'],'channel-info':[{id:'claude',label:'Anthropic'}]};
  else if(req.url==='/v8/management/routing/model-definitions/claude')data={models:[{id:'claude-fable-5',display_name:'Claude Fable 5',context_length:1000000,max_completion_tokens:128000,thinking:{levels:['low','medium','high','xhigh','max']}}]};
  else if(req.url==='/v8/management/models/config'){patches.push(body);model.setting=body.setting;data={status:'ok'}}
  else if(req.url.endsWith('/models/prices'))data={models:[],sync:{}};
  else if(req.url.endsWith('/usage/profiles'))data={profiles:[profile],api_keys:[],query:body,frozen_at_ms:start+3600000};
  else if(req.url.endsWith('/usage/windows'))data={windows:[current,previous,spark]};
  else if(req.url.endsWith('/usage/polling')){polling.push(body);profile.polling_seconds=body.seconds;profile.polling_revision++;data={seconds:body.seconds,revision:profile.polling_revision}}
  else {res.writeHead(404).end();return}
  res.writeHead(200,{'Content-Type':'application/json'});res.end(JSON.stringify(data));
});
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
const browser=await chromium.launch({headless:true});
try{
  const page=await browser.newPage({viewport:{width:1440,height:1100}});
  page.setDefaultTimeout(10000);
  const errors=[];page.on('pageerror',e=>errors.push(e.message));
  await page.goto(`http://127.0.0.1:${server.address().port}/`);
  await page.locator('#workspace').waitFor();
  await page.getByRole('button',{name:'Keys & Groups',exact:true}).click();
  await page.getByText('CPA currently authorizes 2 API keys.',{exact:false}).waitFor();
  assert.equal(await page.locator('#key-rows tr').count(),2);
  assert(!(await page.locator('#key-rows').innerText()).includes('synthetic-secret'));
  await page.getByRole('button',{name:'Model Settings',exact:true}).click();
  await page.locator('#catalogue tr').getByRole('button',{name:'Edit',exact:true}).click();
  assert.equal(await page.locator('#model-context').inputValue(),'1000000');
  assert.equal(await page.locator('#model-output').inputValue(),'128000');
  assert.equal(await page.locator('#model-display').inputValue(),'Claude Fable 5');
  assert.equal(await page.locator('#model-thinking').inputValue(),'low, medium, high, xhigh, max');
  assert.equal(await page.locator('#model-alias').inputValue(),'claude-fable-5');
  await page.locator('#model-dialog').getByRole('button',{name:'Save',exact:true}).click();
  await page.locator('#model-dialog').waitFor({state:'hidden'});
  assert.equal(patches[0].setting,null,'Unchanged defaults must not become overrides');
  assert.equal(patches[0].alias,null);
  await page.locator('#catalogue tr').getByRole('button',{name:'Edit',exact:true}).click();
  await page.locator('#model-context').fill('800000');
  await page.locator('#model-dialog').getByRole('button',{name:'Save',exact:true}).click();
  await page.locator('#model-dialog').waitFor({state:'hidden'});
  assert.deepEqual(patches[1].setting,{'max-context-length':800000});
  await page.getByRole('button',{name:'Usage Quota',exact:true}).click();
  const picker=page.getByLabel('ChatGPT Pro 20x window',{exact:true});
  await picker.locator('option[value=previous]').waitFor({state:'attached'});
  assert.equal(await picker.inputValue(),'');
  assert.equal(await page.locator('.usage-pool-title').count(),0,'Ended Spark is hidden for latest');
  assert.equal(await page.locator('.usage-col .usage-rate-big').first().innerText(),'96% left');
  assert.equal(await page.locator('.usage-meter-pair').count(),2);
  assert.equal(await page.locator('.usage-meter-pair').first().locator('.usage-meter-mini').first().innerText(),'7 day\n0% left');
  assert.equal(await page.locator('.usage-meter-pair').first().locator('.usage-meter-mini').last().innerText(),'5 hour\n100% left');
  assert.equal(await page.locator('.usage-bar.blocked>span').evaluate(e=>e.style.width),'100%');
  assert.equal(await page.locator('.usage-bar.blocked>span').evaluate(e=>getComputedStyle(e).backgroundColor),'rgb(133, 133, 133)');
  assert.equal(await page.locator('.usage-meter-pair').last().locator('.usage-bar>span').first().evaluate(e=>getComputedStyle(e).backgroundColor),'rgb(105, 185, 130)');
  await page.getByLabel('ChatGPT Pro 20x polling minutes').fill('15');
  await page.locator('.usage-polling').getByRole('button',{name:'Save',exact:true}).click();
  await page.waitForFunction(()=>document.querySelector('.usage-polling input').value==='15'&&!document.querySelector('.usage-polling button').disabled);
  assert.deepEqual(polling,[{profile:'profile:test',seconds:900,revision:0}]);
  await picker.selectOption('previous');
  await page.locator('.usage-pool-title').getByText('Spark',{exact:true}).waitFor();
  assert.equal(await page.locator('.usage-col .usage-rate-big').first().innerText(),'0% left');
  await page.getByRole('button',{name:'Average',exact:true}).click();
  assert.equal(await picker.count(),0);
  assert.equal(await page.locator('.usage-pool-title').innerText(),'Spark');
  assert(!(await page.locator('#usage-grid').innerText()).includes('Ended limits'));
  await page.getByRole('button',{name:'Single window',exact:true}).click();
  await picker.selectOption('');
  fs.mkdirSync('/evidence',{recursive:true});
  await page.screenshot({path:'/evidence/usage-wide.png',fullPage:true});
  await page.setViewportSize({width:390,height:844});
  assert(await page.evaluate(()=>document.documentElement.scrollWidth-window.innerWidth)<=1);
  await page.screenshot({path:'/evidence/usage-narrow.png',fullPage:true});
  assert.deepEqual(errors,[]);
  console.log('PASS: CPA keys; model defaults; window selection; compact paired remaining meters, green/blocked-gray fills, monthly meter, saved polling frequency; narrow/wide layouts.');
}finally{await browser.close();await new Promise(resolve=>server.close(resolve))}

// Browser acceptance against the real isolated preview, with no API fixtures.
import { chromium } from '/b/node_modules/playwright/index.mjs';
import assert from 'node:assert/strict';
import fs from 'node:fs';

const url=process.env.PACER_PREVIEW_URL;
assert(url&&['host.docker.internal','127.0.0.1','localhost'].some(host=>url.startsWith('http://'+host+':18329/pacer-dev/')),'Use the owned loopback preview tunnel');
// Map a loopback origin to the owned SSH tunnel on the Docker host. Localhost
// retains secure-context APIs (fingerprinting); the public preview uses HTTPS.
const browser=await chromium.launch({headless:true,args:['--host-resolver-rules=MAP localhost host.docker.internal']});
try{
  const page=await browser.newPage({viewport:{width:1440,height:1100}});
  page.setDefaultTimeout(30000);
  const errors=[];page.on('pageerror',e=>errors.push(e.message));
  page.on('response',r=>{if(r.status()>=400)console.log('HTTP',r.status(),new URL(r.url()).pathname)});
  await page.goto(url.replace('host.docker.internal','localhost'));
  await page.locator('#workspace').waitFor();
  assert((await page.locator('body').innerText()).includes('DEVELOPMENT · isolated copy'));
  await page.getByRole('button',{name:'Usage Quota',exact:true}).click();
  await page.locator('.usage-card').first().waitFor();
  assert(await page.locator('.usage-card').count()>=5);
  assert(await page.locator('.usage-meter-mini').count()>0);
  assert.equal(await page.getByRole('button',{name:'Refresh view',exact:true}).count(),0);
  assert.equal(await page.getByRole('button',{name:'Disconnect',exact:true}).count(),0);
  assert.equal(await page.getByRole('button',{name:'Single window',exact:true}).count(),0);
  const gemini=page.locator('.usage-card').filter({has:page.getByText('Gemini Pro',{exact:true})});
  const profileResponse=await page.request.post(url+'v0/management/plugins/pacer/usage/profiles',{data:{latest_only:true}});
  const profiles=(await profileResponse.json()).profiles;
  const gp=profiles.find(p=>p.name==='Gemini Pro');assert(gp.active_credentials>1,'Snapshot must exercise multiple accounts');
  const geminiMeters=gp.active_credentials*gp.pools.filter(p=>!p.ended).length;
  await page.waitForFunction(expected=>{const c=[...document.querySelectorAll('.usage-card')].find(c=>c.querySelector('.usage-profile-name')?.textContent==='Gemini Pro');return c?.querySelectorAll('.usage-meter-mini').length===expected},geminiMeters,{timeout:65000});
  assert.equal(await gemini.locator('.usage-meter-mini').count(),geminiMeters);
  assert.equal(await gemini.locator('.usage-meter-name').count(),new Set(gp.current.map(m=>m.account+'|'+m.scope)).size);
  assert(!(await gemini.locator('.usage-meters').innerText()).includes('@'));
  const claude=page.locator('.usage-card').filter({has:page.getByText('Claude Pro',{exact:true})});
  const cp=profiles.find(p=>p.name==='Claude Pro');
  for(const scope of new Set(cp.current.map(m=>m.scope))){
    if(scope==='all'&&cp.pools.some(p=>p.scope===scope&&p.kind==='5h'&&!p.ended)){
      const scoped=claude.locator('.usage-meter-group[data-scope="all"] .usage-meter-table');
      assert.equal(await scoped.count(),1,'Reported 5-hour limit remains visible without a current reading');
      assert.equal(await scoped.getByText('5-hour',{exact:true}).count(),1);
      const kinds=new Set([...cp.current.filter(m=>m.scope===scope).map(m=>m.kind),...cp.pools.filter(p=>p.scope===scope&&!p.ended).map(p=>p.kind)]);
      const accounts=new Set(cp.current.filter(m=>m.scope===scope).map(m=>m.account));
      assert.equal(await scoped.locator('.usage-meter-mini').count(),kinds.size*accounts.size);
      if(!cp.current.some(m=>m.scope===scope&&m.kind==='5h'))assert.equal(await scoped.locator('.usage-percent').filter({hasText:'—'}).count(),accounts.size,'Missing balances remain unknown');
    }
  }
  const weekly=gp.current.filter(m=>m.scope==='gemini'&&m.kind==='weekly');
  const combined=weekly.reduce((sum,m)=>sum+100-m.used_percent,0)/gp.active_credentials;
  const geminiPool=gemini.locator('.usage-col').filter({has:page.getByText('Gemini models',{exact:true})});
  assert(Math.abs(Number(await geminiPool.getByRole('meter',{name:'Gemini Pro 7-day combined percent left',exact:true}).getAttribute('aria-valuenow'))-combined)<.01);
  const three=page.locator('.usage-card').filter({has:page.getByText('Claude Max 5x',{exact:true})});
  const rowTops=await three.locator('.usage-col').evaluateAll(nodes=>nodes.map(n=>Math.round(n.getBoundingClientRect().top)));
  assert.equal(rowTops.length,3);assert.equal(new Set(rowTops).size,1,'Three summaries share one row');
  const fourTops=await gemini.locator('.usage-col').evaluateAll(nodes=>nodes.map(n=>Math.round(n.getBoundingClientRect().top)));
  assert.equal(fourTops.length,4);assert.equal(new Set(fourTops).size,2,'Four summaries use two rows');
  assert.equal(await page.locator('details.usage-details').count(),0);
  assert(await three.locator('.usage-average').first().isVisible());
  assert.equal(await page.locator('.usage-window-picker summary').count(),0);
  await gemini.locator('.usage-window-category').nth(3).waitFor();
  const headings=await gemini.locator('.usage-window-category h4').evaluateAll(nodes=>nodes.map(n=>Math.round(n.getBoundingClientRect().top)));
  assert.equal(headings.length,4);assert.equal(new Set(headings).size,1,'All four selector categories share one row');
  assert((await gemini.locator('.usage-window-change').first().innerText()).includes('→'));
  const options=gemini.locator('.usage-window-options');
  assert.equal(await options.evaluate(n=>getComputedStyle(n).transitionDuration),'0.9s');
  const chevron=gemini.locator('.usage-window-expand span');
  assert.equal(await chevron.innerText(),'');
  assert.deepEqual(await chevron.evaluate(n=>{const s=getComputedStyle(n);return [s.width,s.height,s.transitionDuration]}),['8px','8px','0.9s']);
  assert.equal(Math.round((await options.boundingBox()).height),95);
  const controls=await gemini.locator('.usage-window-controls').evaluate(n=>[...n.children].map(c=>{const r=c.getBoundingClientRect();return r.top+r.height/2}));
  assert(Math.max(...controls)-Math.min(...controls)<2,'Selection and polling controls share a row');
  const heights=await gemini.locator('.usage-window-option').evaluateAll(nodes=>nodes.map(n=>n.getBoundingClientRect().height));
  assert(heights.every(h=>h===34),'Latest marker adds no row height');
  await gemini.getByRole('button',{name:'Show more windows',exact:true}).click();
  await page.waitForFunction(()=>{const n=[...document.querySelectorAll('.usage-card')].find(c=>c.querySelector('.usage-profile-name')?.textContent==='Gemini Pro')?.querySelector('.usage-window-options');return n&&Math.round(n.getBoundingClientRect().height)===197});
  assert(await options.evaluate(n=>{n.scrollTop=80;return n.scrollTop>0&&getComputedStyle(n).overflowY==='auto'}),'Expanded list scrolls through older windows');
  await options.evaluate(n=>{n.scrollTop=0});
  await page.screenshot({path:'/evidence/preview-windows-expanded.png',fullPage:true});
  await gemini.getByRole('button',{name:'Show fewer windows',exact:true}).click();
  await page.waitForFunction(()=>{const n=[...document.querySelectorAll('.usage-card')].find(c=>c.querySelector('.usage-profile-name')?.textContent==='Gemini Pro')?.querySelector('.usage-window-options');return n&&Math.round(n.getBoundingClientRect().height)===95});
  await page.screenshot({path:'/evidence/preview-window-picker.png',fullPage:true});
  const chat=page.locator('.usage-card').filter({has:page.getByText('ChatGPT Pro 20x',{exact:true})});
  assert.equal(await chat.locator('.usage-meters').count(),0);
  const boxes=chat.locator('.usage-window-option input');
  await boxes.first().waitFor();
  // Without a selection of its own the profile counts its latest windows; they are ordinary choices.
  assert.equal(await chat.locator('.usage-window-option input:disabled').count(),0);
  const latest=await chat.locator('.usage-window-option input:checked').count();
  assert(latest>0,'Latest windows start selected');
  const checked=n=>page.waitForFunction(n=>{const c=[...document.querySelectorAll('.usage-card')].find(c=>c.textContent.includes('ChatGPT Pro 20x'));return c&&c.querySelectorAll('.usage-window-option input:checked').length===n},n);
  const total=await boxes.count();
  const changedWindows=page.waitForResponse(r=>r.url().endsWith('/usage/profiles')&&r.request().postDataJSON()?.selected_windows&&Object.values(r.request().postDataJSON().selected_windows).some(v=>v.length));
  await chat.getByRole('button',{name:'Show more windows',exact:true}).click();
  await chat.locator('.usage-window-option input:not(:checked)').first().check();assert.equal((await changedWindows).status(),200);
  await chat.getByRole('button',{name:'Select all',exact:true}).click();await checked(total);
  await chat.getByRole('button',{name:'Deselect all',exact:true}).click();await checked(0);
  // Select latest drops the profile's own selection, so it follows its latest windows again.
  const followLatest=page.waitForResponse(r=>r.url().endsWith('/usage/profiles')&&!Object.values(r.request().postDataJSON()?.selected_windows||{}).length);
  await chat.getByRole('button',{name:'Select latest',exact:true}).click();
  assert.equal((await followLatest).status(),200);await checked(latest);
  const polling=page.getByLabel('ChatGPT Pro 20x polling minutes');
  const original=await polling.inputValue();
  const changed=original==='60'?'120':'60';
  try{
    await polling.fill(changed);
    const response=page.waitForResponse(r=>r.url().endsWith('/usage/polling')&&r.request().method()==='PATCH');
    await chat.locator('.usage-polling button').click();
    assert.equal((await response).status(),200);
    await page.reload();await page.locator('#workspace').waitFor();
    await page.getByRole('button',{name:'Usage Quota',exact:true}).click();
    await polling.waitFor();assert.equal(await polling.inputValue(),changed);
  }finally{
    await polling.fill(original);
    const restored=page.waitForResponse(r=>r.url().endsWith('/usage/polling')&&r.request().method()==='PATCH');
    await chat.locator('.usage-polling button').click();assert.equal((await restored).status(),200);
  }
  await page.getByRole('button',{name:'Keys & Groups',exact:true}).click();
  try{await page.locator('#key-rows tr').nth(1).waitFor()}catch(e){
    console.log('Key diagnostics',await page.locator('#key-status').innerText(),await page.locator('#message').count()?await page.locator('#message').innerText():'',errors,await page.evaluate(()=>({secure:isSecureContext,crypto:!!crypto.subtle})));
    throw e;
  }
  assert.equal(await page.locator('#key-rows tr').count(),2);
  assert(!(await page.locator('#key-rows').innerText()).includes('pacer-preview-client-'));
  await page.getByRole('button',{name:'Model Settings',exact:true}).click();
  await page.locator('#catalogue tr').first().waitFor();
  assert(await page.locator('#catalogue tr').count()>1);
  await page.getByRole('button',{name:'Usage Quota',exact:true}).click();
  await page.locator('.usage-card').first().waitFor();
  const nextRefresh=page.waitForResponse(r=>r.url().endsWith('/usage/profiles'),{timeout:40000});
  await page.locator('h2').filter({hasText:'Usage Quota'}).click();
  assert.equal((await nextRefresh).status(),200);
  fs.mkdirSync('/evidence',{recursive:true});
  await page.screenshot({path:'/evidence/preview-live.png',fullPage:true});
  await page.getByRole('button',{name:'Accounts & profiles',exact:true}).click();
  const dialog=page.locator('#usage-manage-dialog');
  await dialog.locator('.manage-profile-row').first().waitFor();
  await page.screenshot({path:'/evidence/preview-profiles.png'});
  await dialog.getByRole('button',{name:'Create profile',exact:true}).click();
  await dialog.locator('.manage-create-form').waitFor();
  await page.screenshot({path:'/evidence/preview-create.png'});
  const form=dialog.locator('.manage-create-form');
  const testName='Preview check '+Date.now();await form.getByLabel('Name',{exact:true}).fill(testName);
  await form.getByLabel('Provider',{exact:true}).selectOption('antigravity');
  await form.locator('input[type=checkbox]').first().check();
  const created=page.waitForResponse(r=>r.url().endsWith('/usage/profile')&&r.request().postDataJSON()?.op==='create');
  await form.getByRole('button',{name:'Create profile',exact:true}).click();assert.equal((await created).status(),200);
  await dialog.getByText('Accounts in '+testName,{exact:true}).waitFor();
  assert.equal(await dialog.getByText('Active',{exact:true}).count(),1);
  const archived=page.waitForResponse(r=>r.url().endsWith('/usage/profile')&&r.request().postDataJSON()?.op==='archive');
  await dialog.getByRole('button',{name:'Archive',exact:true}).click();assert.equal((await archived).status(),200);
  await dialog.getByRole('button',{name:'Accounts & logins',exact:true}).click();
  await dialog.getByText('Link a replacement login',{exact:true}).first().waitFor();
  await page.screenshot({path:'/evidence/preview-accounts.png'});
  await page.locator('#usage-manage-close').click();
  await page.setViewportSize({width:390,height:844});
  assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
  await page.screenshot({path:'/evidence/preview-mobile.png',fullPage:true});
  assert.deepEqual(errors,[]);
  console.log(`PASS: real preview; ${geminiMeters} Gemini meters across ${gp.active_credentials} accounts; combined arithmetic; single-account rows hidden; latest-pinned multi-selection; automatic refresh; persisted polling; dummy keys/models; atomic profile creation with an account; profile/login navigation; mobile fit.`);
}finally{await browser.close()}

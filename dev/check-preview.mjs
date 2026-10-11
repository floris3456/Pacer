// Browser acceptance against the real isolated preview, with no API fixtures.
import { chromium } from '/b/node_modules/playwright/index.mjs';
import assert from 'node:assert/strict';
import fs from 'node:fs';

const url=process.env.PACER_PREVIEW_URL;
assert(url?.startsWith('http://host.docker.internal:18329/pacer-dev/'),'Use the owned loopback preview tunnel');
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
  await page.waitForFunction(()=>{const c=[...document.querySelectorAll('.usage-card')].find(c=>c.querySelector('.usage-profile-name')?.textContent==='Gemini Pro');return c?.querySelectorAll('.usage-meter-mini').length===12},{},{timeout:65000});
  assert.equal(await gemini.locator('.usage-meter-mini').count(),12);
  assert.equal(await gemini.locator('.usage-meter-name').count(),6);
  assert(!(await gemini.locator('.usage-meters').innerText()).includes('@'));
  const profileResponse=await page.request.post(url+'v0/management/plugins/pacer/usage/profiles',{data:{latest_only:true}});
  const profiles=(await profileResponse.json()).profiles;
  const gp=profiles.find(p=>p.name==='Gemini Pro');assert.equal(gp.active_credentials,3);
  const weekly=gp.current.filter(m=>m.scope==='gemini'&&m.kind==='weekly');
  const combined=weekly.reduce((sum,m)=>sum+100-m.used_percent,0)/3;
  const geminiPool=gemini.locator('.usage-pool').filter({has:page.getByText('Gemini models',{exact:true})});
  assert(Math.abs(Number(await geminiPool.getByRole('meter',{name:'Gemini Pro 7-day combined percent left',exact:true}).getAttribute('aria-valuenow'))-combined)<.01);
  const chat=page.locator('.usage-card').filter({has:page.getByText('ChatGPT Pro 20x',{exact:true})});
  assert.equal(await chat.locator('.usage-meters').count(),0);
  await chat.locator('.usage-window-picker summary').click();
  const boxes=chat.locator('.usage-window-option input');
  await boxes.first().waitFor();
  assert(await chat.locator('.usage-window-option input:disabled:checked').count()>0);
  assert.equal(await chat.locator('.usage-window-option input:enabled:checked').count(),0);
  const changedWindows=page.waitForResponse(r=>r.url().endsWith('/usage/profiles')&&r.request().postDataJSON()?.selected_windows&&Object.values(r.request().postDataJSON().selected_windows).some(v=>v.length));
  await chat.locator('.usage-window-option input:enabled').first().check();assert.equal((await changedWindows).status(),200);
  await chat.getByRole('button',{name:'Select all',exact:true}).click();
  await page.waitForFunction(()=>{const c=[...document.querySelectorAll('.usage-card')].find(c=>c.textContent.includes('ChatGPT Pro 20x'));return c&&c.querySelectorAll('.usage-window-option input:enabled:not(:checked)').length===0});
  await chat.getByRole('button',{name:'Deselect all',exact:true}).click();
  await page.waitForFunction(()=>{const c=[...document.querySelectorAll('.usage-card')].find(c=>c.textContent.includes('ChatGPT Pro 20x'));return c&&c.querySelectorAll('.usage-window-option input:enabled:checked').length===0});
  assert(await chat.locator('.usage-window-option input:disabled:checked').count()>0);
  await chat.locator('.usage-window-picker summary').click();
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
  console.log('PASS: real preview; 12 Gemini meters across three accounts; combined arithmetic; single-account rows hidden; latest-pinned multi-selection; automatic refresh; persisted polling; dummy keys/models; atomic profile creation with an account; profile/login navigation; mobile fit.');
}finally{await browser.close()}

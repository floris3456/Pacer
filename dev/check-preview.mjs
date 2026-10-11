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
  const chat=page.locator('.usage-card').filter({has:page.getByText('ChatGPT Pro 20x',{exact:true})});
  const selector=chat.locator('.usage-window-picker select');
  await selector.locator('option').nth(1).waitFor({state:'attached'});
  const option=await selector.locator('option').nth(1).getAttribute('value');
  await selector.selectOption(option);
  assert.equal(await selector.inputValue(),option);
  await selector.selectOption('');
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
  fs.mkdirSync('/evidence',{recursive:true});
  await page.screenshot({path:'/evidence/preview-live.png',fullPage:true});
  assert.deepEqual(errors,[]);
  console.log('PASS: actual preview HTML/backend; copied profiles and meters; historical window selection; isolated persisted polling edit; dummy keys; model catalogue.');
}finally{await browser.close()}

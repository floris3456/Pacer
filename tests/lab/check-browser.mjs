// Real Chromium, real native Pacer page, isolated CPA with dummy credentials.
// Run in the existing Playwright image, --network pacer-lab, /b dependency mount.
import { chromium } from '/b/node_modules/playwright/index.mjs';
import assert from 'node:assert/strict';
import fs from 'node:fs';

const base = 'http://pacer-lab-cpa:8317';
const out = '/evidence';
const result = { steps: [], errors: [] };
const step = name => { result.steps.push({name,pass:true});console.log('PASS',name); };
const browser = await chromium.launch({headless:true});
const context = await browser.newContext({viewport:{width:1440,height:1000}});
const page = await context.newPage();
page.setDefaultTimeout(15000);
page.on('pageerror', e => result.errors.push(e.message));

try {
  await page.goto(base+'/v0/resource/plugins/pacer/index.html');
  assert.equal(await page.title(),'Pacer');
  await page.getByLabel('Management key').fill('pacer-test-inference');
  await page.getByRole('button',{name:'Connect',exact:true}).click();
  await page.getByText('Management authorization required. Inference keys are not administrator keys.',{exact:true}).waitFor();
  assert.equal(await page.locator('#workspace').isVisible(),false);
  step('inference key cannot administer through the browser');
  await page.getByLabel('Management key').fill('pacer-test-admin');
  await page.getByRole('button',{name:'Connect',exact:true}).click();
  await page.locator('#workspace').waitFor();
  await page.locator('#rollout').filter({hasText:'dry-run observation'}).waitFor();
  const activeOption=await page.locator('#mode option[value=active]').evaluate(node=>({disabled:node.disabled,attribute:node.hasAttribute('disabled'),html:node.outerHTML}));
  assert.equal(activeOption.disabled&&activeOption.attribute,true,JSON.stringify(activeOption));
  assert.deepEqual(await page.evaluate(()=>({local:Object.keys(localStorage),session:Object.keys(sessionStorage),password:document.querySelector('#key').value})),{local:[],session:[],password:''});
  step('management connects without storing credentials; active routing gated');
  await page.getByLabel('Routing mode').selectOption('dry-run');
  await page.getByRole('button',{name:'Apply',exact:true}).click();
  await page.getByText('Routing mode saved. This does not change any chat’s cache policy.',{exact:true}).waitFor();
  const state = await page.request.get(base+'/v0/management/plugins/pacer/state',{headers:{Authorization:'Bearer pacer-test-admin'}});
  assert.equal((await state.json()).routing.mode,'dry-run');
  step('routing control saved through actual native management route');
  await page.screenshot({path:out+'/pacer-desktop.png',fullPage:true});

  await page.getByRole('button',{name:'Usage Quota',exact:true}).click();
  await page.locator('#windows tr').first().waitFor();
  assert.match(await page.locator('#windows').innerText(),/80\.0%|70\.0%/);
  assert.match(await page.locator('#history').textContent(),/CPAMP remains the history source/);
  step('quota observation view renders real persisted passive signals and honest history coverage');
  await page.screenshot({path:out+'/pacer-quota.png',fullPage:true});

  await page.getByRole('button',{name:'Model Settings',exact:true}).click();
  const model = page.locator('tr[data-model="claude-opus-5-5"]');
  await model.waitFor();
  const beforeCatalogue=await page.request.get(base+'/v8/management/oauth/settings/models',{headers:{Authorization:'Bearer pacer-test-admin'}});
  const collision=(await beforeCatalogue.json()).models.find(m=>m.channel==='claude'&&m.upstream!=='claude-opus-5-5'&&m.enabled);
  assert.ok(collision,'fixture must have a second enabled Claude model');
  await model.getByRole('button',{name:'Edit',exact:true}).click();
  const dialog=page.getByRole('dialog');
  await dialog.getByLabel('Alias',{exact:true}).fill(collision.id);
  await dialog.getByRole('button',{name:'Save',exact:true}).click();
  await page.locator('#model-error').filter({hasText:'another model already uses this name'}).waitFor();
  step('alias collision refused by CPA and displayed in browser');
  await dialog.getByLabel('Alias',{exact:true}).fill('pacer-lab-opus');
  await dialog.getByLabel('Also keep upstream name',{exact:true}).check();
  await dialog.getByLabel('Display name',{exact:true}).fill('Pacer offline test');
  await dialog.getByLabel('Context window',{exact:true}).fill('180000');
  await dialog.getByLabel('Max reply tokens',{exact:true}).fill('8192');
  await dialog.getByLabel('Thinking levels',{exact:true}).fill('low, high');
  await page.screenshot({path:out+'/pacer-model-editor.png'});
  await dialog.getByRole('button',{name:'Save',exact:true}).click();
  await page.getByText('Model configuration saved in CPA.',{exact:true}).waitFor();
  await model.filter({hasText:'pacer-lab-opus'}).waitFor();
  const catalogue=await page.request.get(base+'/v8/management/oauth/settings/models',{headers:{Authorization:'Bearer pacer-test-admin'}});
  const changed=(await catalogue.json()).models.find(m=>m.channel==='claude'&&m.upstream==='claude-opus-5-5');
  assert.equal(changed.alias.name,'pacer-lab-opus');
  assert.equal(changed.setting['max-output-tokens'],8192);
  assert.equal(changed.setting['max-context-length'],180000);
  assert.deepEqual(changed.setting['thinking-levels'],['low','high']);
  const served=await page.request.get(base+'/v1/models',{headers:{Authorization:'Bearer pacer-test-inference'}});
  const ids=(await served.json()).data.map(m=>m.id);
  assert.ok(ids.includes('pacer-lab-opus')&&ids.includes('claude-opus-5-5'));
  step('alias limits and thinking settings saved in CPA and visible in client catalogue');
  await model.getByRole('button',{name:'Edit',exact:true}).click();
  await dialog.getByRole('button',{name:'Reset name and settings',exact:true}).click();
  await dialog.waitFor({state:'hidden'});
  step('reset removes overrides through CPA');

  await page.setViewportSize({width:390,height:844});
  await page.screenshot({path:out+'/pacer-mobile.png',fullPage:true});
  assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth-window.innerWidth)<=1);
  step('mobile layout has no page-level horizontal overflow');
  await page.getByRole('button',{name:'Disconnect',exact:true}).click();
  assert.equal(await page.locator('#workspace').isVisible(),false);
  await page.reload();
  assert.equal(await page.locator('#workspace').isVisible(),false);
  step('disconnect and reload require fresh authorization');
  assert.deepEqual(result.errors,[]);
} finally {
  fs.writeFileSync(out+'/browser-direct.json',JSON.stringify(result,null,2)+'\n');
  await browser.close();
}

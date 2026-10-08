// Narrow real-Chromium diagnostic; native HTML/data, dummy lab management only.
import { chromium } from '/b/node_modules/playwright/index.mjs';
import assert from 'node:assert/strict';
import fs from 'node:fs';
const browser = await chromium.launch({headless:true});
const result = {scope:'Direct native mobile diagnostic; not full browser acceptance'};
try {
  const context = await browser.newContext({viewport:{width:1440,height:1000}});
  const page = await context.newPage();
  page.setDefaultTimeout(15000);
  await page.goto('http://pacer-lab-cpa:8317/v0/resource/plugins/pacer/index.html');
  await page.getByLabel('Management key').fill('pacer-test-admin');
  await page.getByRole('button',{name:'Connect',exact:true}).click();
  await page.locator('#workspace').waitFor();
  await page.getByRole('button',{name:'Usage Quota',exact:true}).click();
  await page.locator('#legacy-advanced > summary').click();
  await page.locator('#legacy-view').selectOption('quota');
  await page.locator('#legacy-period').selectOption('history');
  await page.locator('#legacy-load').click();
  await page.locator('#legacy-quota tr').first().waitFor();
  const dimensions = () => page.evaluate(() => ({width:innerWidth,scroll:document.documentElement.scrollWidth,
    overflow:[...document.querySelectorAll('body *')].filter(e => !e.closest('.table-wrap') && e.getBoundingClientRect().right > innerWidth+1)
      .slice(0,40).map(e => ({tag:e.tagName,id:e.id,classes:String(e.className).slice(0,100),width:e.getBoundingClientRect().width,
        right:e.getBoundingClientRect().right,overflow:getComputedStyle(e).overflowX,display:getComputedStyle(e).display}))}));
  result.desktop=await dimensions();
  await page.setViewportSize({width:390,height:844});
  await page.evaluate(() => new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve))));
  result.mobile=await dimensions();
  await page.locator('#legacy-scenario-window').scrollIntoViewIfNeeded();
  await page.screenshot({path:'/evidence/mobile-before.png',fullPage:false,mask:[page.locator('textarea:visible,pre:visible')]});
  if (process.env.PACER_DIAGNOSTIC_CSS) {
    await page.addStyleTag({content:process.env.PACER_DIAGNOSTIC_CSS});
    result.injectedCSSDiagnosticOnly=true;
    result.after=await dimensions();
    await page.screenshot({path:'/evidence/mobile-diagnostic-css.png',fullPage:false,mask:[page.locator('textarea:visible,pre:visible')]});
    assert.ok(result.after.scroll<=result.after.width+1,'Candidate diagnostic CSS still overflows');
  }
  if (process.env.PACER_LAYOUT_EXPECT_FIT==='1') {
    assert.ok(!process.env.PACER_DIAGNOSTIC_CSS,'Loaded-product acceptance must not inject CSS');
    assert.ok(result.mobile.scroll<=result.mobile.width+1,'Loaded native mobile page overflows');
    result.loadedMobileFit=true;
  }
} finally {
  await browser.close();
  fs.writeFileSync('/evidence/dimensions.json',JSON.stringify(result,null,2)+'\n');
}

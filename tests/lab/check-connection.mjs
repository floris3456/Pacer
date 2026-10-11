// Real Chromium and the production page, embedded by a panel without a legacy bridge.
// Run in the Playwright image with its matching package installed under /b.
import { chromium } from '/b/node_modules/playwright/index.mjs';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import http from 'node:http';

const html = fs.readFileSync(new URL('../../internal/management/index.html', import.meta.url));
let authorized = false;
const requests = [];
const server = http.createServer((req, res) => {
  if (req.url === '/panel') {
    res.setHeader('Content-Type', 'text/html');
    res.end('<iframe src="/pacer" title="Pacer"></iframe>');
    return;
  }
  if (req.url === '/pacer') {
    res.setHeader('Content-Type', 'text/html');
    res.setHeader('Content-Security-Policy', "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'self'");
    res.end(html);
    return;
  }
  if (req.url === '/v0/management/plugins/pacer/state') {
    requests.push(req.headers);
    res.writeHead(authorized ? 200 : 401, {'Content-Type': 'application/json'});
    res.end(JSON.stringify(authorized ? {routing: {mode: 'dry-run', can_activate: false}, accounts: []} : {}));
    return;
  }
  res.writeHead(404).end();
});
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
const browser = await chromium.launch({headless: true});
try {
  const page = await browser.newPage();
  page.setDefaultTimeout(10000);
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  const base = `http://127.0.0.1:${server.address().port}`;
  await page.goto(base + '/panel');
  const frame = page.frameLocator('iframe');
  await frame.getByText('Your session is not authorized. Reload this page to sign in.', {exact: true}).waitFor();
  assert.equal(await frame.locator('#workspace').isVisible(), false);
  assert.equal(await frame.locator('input[type=password]').count(), 0);
  assert.equal(requests.length, 1);
  authorized = true;
  await frame.getByRole('button', {name: 'Retry connection', exact: true}).click();
  await frame.locator('#workspace').waitFor();
  assert.equal(requests.length, 2);
  await page.reload();
  await frame.locator('#workspace').waitFor();
  assert.equal(requests.length, 3);
  await page.goto(base + '/pacer');
  await page.locator('#workspace').waitFor();
  assert.equal(requests.length, 4);
  assert(requests.every(headers => !headers.authorization));
  assert.deepEqual(errors, []);
  console.log('PASS: Chromium embedded authorization refusal, session retry, automatic embedded connection, standalone connection; no key prompt or Authorization header.');
} finally {
  await browser.close();
  await new Promise(resolve => server.close(resolve));
}

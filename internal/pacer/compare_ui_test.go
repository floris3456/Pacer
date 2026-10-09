package pacer

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
)

func TestCompareUISyntaxAndStateContracts(t *testing.T) {
	node := os.Getenv("PACER_NODE")
	if node == "" {
		var err error
		node, err = exec.LookPath("node")
		if err != nil {
			t.Skip("Node required; set PACER_NODE for source UI verification")
		}
	}
	html, err := os.ReadFile("../management/index.html")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(html), "<script>")
	if len(parts) != 2 {
		t.Fatal("expected one inline script")
	}
	script := strings.Split(parts[1], "</script>")[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	check := exec.CommandContext(ctx, node, "--check")
	check.Stdin = strings.NewReader(script)
	if out, err := check.CombinedOutput(); err != nil {
		t.Fatalf("Node syntax: %v\n%s", err, out)
	}

	p := legacyHTTPFixture(t)
	respCompare := compareHTTP(p, "compare", `{}`)
	if respCompare.StatusCode != 200 {
		t.Fatal("compare failed", respCompare.StatusCode, string(respCompare.Body))
	}
	var view historyreports.CompareView
	if err := json.Unmarshal(respCompare.Body, &view); err != nil {
		t.Fatal(err)
	}

	respKeyFiltered := compareHTTP(p, "compare", `{"key":"unattributed"}`)
	if respKeyFiltered.StatusCode != 200 {
		t.Fatal("compare key-filtered failed", respKeyFiltered.StatusCode, string(respKeyFiltered.Body))
	}

	var pool *historyreports.ComparePool
	var profileRef string
	for _, cp := range view.Profiles {
		if len(cp.Pools) > 0 {
			pool = &cp.Pools[0]
			profileRef = cp.Reference
			break
		}
	}
	if pool == nil {
		t.Fatal("no pool found")
	}
	bodyWindows, _ := json.Marshal(historyreports.CompareWindowsQuery{Profile: profileRef, Kind: pool.Kind, Scope: pool.Scope})
	respWindows := compareHTTP(p, "compare-windows", string(bodyWindows))
	if respWindows.StatusCode != 200 {
		t.Fatal("compare-windows failed", respWindows.StatusCode, string(respWindows.Body))
	}

	tmp := t.TempDir()
	compareFile := filepath.Join(tmp, "compare.json")
	keyFile := filepath.Join(tmp, "compare_key.json")
	windowsFile := filepath.Join(tmp, "windows.json")

	if err := os.WriteFile(compareFile, respCompare.Body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, respKeyFiltered.Body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(windowsFile, respWindows.Body, 0600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(ctx, node, "-e", compareUIHarness)
	cmd.Stdin = strings.NewReader(string(html))
	cmd.Env = append(os.Environ(),
		"COMPARE_DATA="+compareFile,
		"COMPARE_KEY_DATA="+keyFile,
		"COMPARE_WINDOWS_DATA="+windowsFile,
		"PROFILE_REF="+profileRef,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Node compare UI contracts: %v\n%s", err, out)
	} else {
		t.Log(string(out))
	}
}

const compareUIHarness = `
const fs = require('node:fs'), vm = require('node:vm'), assert = require('node:assert/strict');
const html = fs.readFileSync(0, 'utf8'), nodes = new Map(), calls = [], timers = new Map();
let clock = 0, held = false, hold = '', release = null;

class Element {
  constructor(tag = 'div', id = '') {
    this.tag = tag;
    this.id = id;
    this.children = [];
    this.value = '';
    this._text = '';
    this.dataset = {};
    this.disabled = false;
    this.hidden = false;
    this.checked = false;
    this.open = false;
    this.attributes = {};
  }
  set innerHTML(v) { throw Error('Unsafe HTML assignment'); }
  set textContent(v) { this._text = String(v ?? ''); this.children = []; }
  get textContent() { return this._text + this.children.map(c => c.textContent || '').join(' '); }
  append(...items) { this.children.push(...items); }
  replaceChildren(...items) { this._text = ''; this.children = items.filter(Boolean); }
  get options() { return this.children; }
  remove(i) { this.children.splice(i, 1); }
  showModal() { this.open = true; }
  close() { this.open = false; }
  reset() {}
  setAttribute(k, v) { this[k] = v; this.attributes[k] = v; }
  getAttribute(k) { return this.attributes[k]; }
  querySelectorAll(sel) {
    const res = [];
    const walk = el => {
      for (const c of el.children) {
        if (!c || typeof c !== 'object') continue;
        if (sel.startsWith('.') && c.className && c.className.split(' ').includes(sel.slice(1))) res.push(c);
        else if (sel.startsWith('#') && c.id === sel.slice(1)) res.push(c);
        else if (c.tag === sel) res.push(c);
        walk(c);
      }
    };
    walk(this);
    return res;
  }
  querySelector(sel) {
    const all = this.querySelectorAll(sel);
    return all[0] || null;
  }
}

for (const match of html.matchAll(/<([a-z0-9-]+)\b[^>]*\bid="([^"]+)"[^>]*>/g)) {
  assert(!nodes.has(match[2]), 'Duplicate DOM id ' + match[2]);
  nodes.set(match[2], new Element(match[1], match[2]));
}
const el = id => { assert(nodes.has(id), 'Missing DOM id ' + id); return nodes.get(id); };

for (const match of html.matchAll(/<select\b[^>]*id="([^"]+)"[^>]*>([\s\S]*?)<\/select>/g)) {
  for (const o of match[2].matchAll(/<option value="([^"]*)"[^>]*>([^<]*)<\/option>/g)) {
    const n = new Element('option');
    n.value = o[1];
    n.textContent = o[2];
    el(match[1]).append(n);
  }
  el(match[1]).value = el(match[1]).options[0]?.value || '';
}

function response(path, body) {
  if (path.endsWith('/history/compare')) {
    if (body?.key === 'unattributed') {
      const d = JSON.parse(fs.readFileSync(process.env.COMPARE_KEY_DATA, 'utf8'));
      d.query = body;
      d.key_filtered = true;
      return d;
    }
    const d = JSON.parse(fs.readFileSync(process.env.COMPARE_DATA, 'utf8'));
    d.query = body || {};
    return d;
  }
  if (path.endsWith('/history/compare-windows')) {
    return JSON.parse(fs.readFileSync(process.env.COMPARE_WINDOWS_DATA, 'utf8'));
  }
  if (path.endsWith('/state')) {
    return { routing: { mode: 'off' }, accounts: [] };
  }
  throw Error('Unexpected request ' + path);
}

const sandbox = {
  console,
  Blob,
  Date,
  JSON,
  Map,
  Set,
  Number,
  Error,
  Array,
  Promise,
  Math,
  URL: {
    createObjectURL() { return 'blob:fixture-' + (++clock); },
    revokeObjectURL() {}
  },
  document: {
    getElementById: el,
    createElement: tag => new Element(tag),
    createTextNode: text => ({ textContent: text }),
    querySelectorAll: () => []
  },
  location: { origin: 'https://offline.invalid' },
  setTimeout(fn) {
    const id = ++clock;
    timers.set(id, fn);
    return id;
  },
  clearTimeout(id) { timers.delete(id); },
  fetch: async (path, opts) => {
    assert.equal(opts.redirect, 'error');
    assert.equal(opts.cache, 'no-store');
    assert.equal(opts.headers.Authorization, 'Bearer synthetic-memory-key');
    const body = opts.body ? JSON.parse(opts.body) : undefined;
    calls.push({ path, body });
    if (hold && path.endsWith(hold)) {
      await new Promise(resolve => { release = resolve; held = true; });
    }
    return {
      ok: true,
      json: async () => response(path, body)
    };
  }
};
sandbox.window = { addEventListener() {} };
sandbox.window.parent = sandbox.window;
Object.defineProperty(sandbox, 'localStorage', { get() { throw Error('Persistent credentials forbidden'); } });
Object.defineProperty(sandbox, 'sessionStorage', { get() { throw Error('Persistent credentials forbidden'); } });

const hook = 'globalThis.ui={loadCompare,renderCompare,clearCompare,connect(){connected=true;generation++;key="synthetic-memory-key"},state(){return {currentCompareData,compareMode,compareGeneration,compareCache,connected,key}},allowed};';
const script = html.split('<script>')[1].split('</script>')[0];
assert(script.includes('  if(window.parent!==window)'));
vm.createContext(sandbox);
vm.runInContext(script.replace('  if(window.parent!==window)', hook + '\n  if(window.parent!==window)'), sandbox, { timeout: 5000 });
const ui = sandbox.ui;

(async () => {
  // 1. Assert allowed set contains both routes
  assert(ui.allowed.has('/v0/management/plugins/pacer/history/compare' ? 'POST /v0/management/plugins/pacer/history/compare' : ''), 'allowed set must contain POST compare');
  assert(ui.allowed.has('POST /v0/management/plugins/pacer/history/compare-windows'), 'allowed set must contain POST compare-windows');

  // 2. Assert no innerHTML
  assert(!script.includes('.innerHTML'), 'script must never use innerHTML');

  // Connect and verify initial load
  ui.connect();
  assert.equal(calls.length, 0);

  // 3. Load default compare view and assert all profiles rendered once
  await ui.loadCompare();
  assert.equal(calls.length, 1, 'Initial load should make exactly 1 request');
  assert(calls[0].path.endsWith('/history/compare'));
  assert.deepEqual(calls[0].body, {});

  const compareData = JSON.parse(fs.readFileSync(process.env.COMPARE_DATA, 'utf8'));
  const grid = el('compare-grid');
  const cards = grid.children.filter(c => c.className === 'compare-card');
  assert.equal(cards.length, compareData.profiles.length, 'All profiles must be rendered once');

  // Verify archived ordering and badge
  const nonArchivedCount = compareData.profiles.filter(p => !p.archived).length;
  for (let i = 0; i < compareData.profiles.length; i++) {
    const card = cards[i];
    if (i < nonArchivedCount) {
      assert(!card.textContent.includes('Archived'), 'Active profile card must not have Archived badge');
    } else {
      assert(card.textContent.includes('Archived'), 'Archived profile card must have Archived badge');
    }
  }

  // 4. Latest vs Average toggle changes shown numbers without a new API call
  const callsBeforeToggle = calls.length;
  const firstCard = cards[0];
  const latestText = firstCard.textContent;

  // Toggle to average
  el('compare-mode-average').onclick();
  assert.equal(calls.length, callsBeforeToggle, 'Toggle to Average must NOT make an API call');
  assert.equal(el('compare-mode-latest').getAttribute('aria-pressed'), 'false');
  assert.equal(el('compare-mode-average').getAttribute('aria-pressed'), 'true');
  assert.equal(ui.state().compareMode, 'average');

  const avgText = grid.children[0].textContent;
  assert.notEqual(latestText, avgText, 'Average mode must change shown numbers');
  assert(avgText.includes('measured') || avgText.includes('windows measured'), 'Average mode shows measured windows count');

  // Toggle back to latest
  el('compare-mode-latest').onclick();
  assert.equal(calls.length, callsBeforeToggle, 'Toggle to Latest must NOT make an API call');
  assert.equal(el('compare-mode-latest').getAttribute('aria-pressed'), 'true');
  assert.equal(el('compare-mode-average').getAttribute('aria-pressed'), 'false');
  assert.equal(ui.state().compareMode, 'latest');
  assert.equal(grid.children[0].textContent, latestText, 'Toggle back to Latest restores original numbers');

  // 5. Key change triggers exactly one request and cached filter re-renders with zero requests
  const callsBeforeKey = calls.length;
  el('compare-key').value = 'unattributed';
  await el('compare-key').onchange();
  assert.equal(calls.length, callsBeforeKey + 1, 'Key change triggers exactly one request');
  assert.equal(calls.at(-1).body.key, 'unattributed');
  assert(grid.children[0].textContent.includes('this key:'), 'Key filtered view displays token share percent');

  // Re-select empty key: must re-render from cache with zero requests
  const callsBeforeCached = calls.length;
  el('compare-key').value = '';
  await el('compare-key').onchange();
  assert.equal(calls.length, callsBeforeCached, 'Cached filter must re-render with zero requests');
  assert(!grid.children[0].textContent.includes('this key:'), 'Unfiltered view does not display token share percent');

  // 5b. Date typing is debounced
  const callsBeforeDate = calls.length;
  el('compare-from').value = '2026-09-01';
  el('compare-from').oninput();
  assert.equal(calls.length, callsBeforeDate, 'Date typing must be debounced without immediate fetch');
  for (const fn of timers.values()) fn();
  timers.clear();
  for (let i = 0; i < 20 && calls.length === callsBeforeDate; i++) await Promise.resolve();
  assert.equal(calls.length, callsBeforeDate + 1, 'Debounced timer triggers fetch');
  assert.equal(calls.at(-1).body.from, '2026-09-01');

  // 6. Stale response is ignored
  hold = '/history/compare';
  held = false;
  el('compare-key').value = 'slow-filter';
  const slowPromise = el('compare-key').onchange();
  for (let i = 0; i < 20 && !held; i++) await Promise.resolve();
  assert(held, 'Slow request should be held');

  // Trigger another change while slow request is pending
  hold = '';
  el('compare-key').value = ''; // cached
  await el('compare-key').onchange();
  assert.equal(ui.state().currentCompareData.query.key || '', '', 'Current view is the empty filter');

  // Release the slow response
  release();
  await slowPromise;
  assert.equal(ui.state().currentCompareData.query.key || '', '', 'Stale response was ignored');

  // 6b. Concurrent loads of one filter (tab refresh + key change) share one request
  hold = '/history/compare';
  held = false;
  el('compare-key').value = 'shared-filter';
  const first = ui.loadCompare();
  for (let i = 0; i < 20 && !held; i++) await Promise.resolve();
  assert(held, 'Shared request should be held');
  const callsShared = calls.length;
  const second = ui.loadCompare();
  hold = '';
  release();
  await Promise.all([first, second]);
  assert.equal(calls.length, callsShared, 'A second load of an in-flight filter must not request again');
  assert.equal(ui.state().currentCompareData.query.key, 'shared-filter');
  el('compare-key').value = '';
  await el('compare-key').onchange();

  // 7. Older-windows list renders
  const olderBtns = grid.children[0].querySelectorAll('button').filter(b => b.textContent === 'Older windows');
  assert(olderBtns.length > 0, 'Card must have Older windows button');
  const callsBeforeWindows = calls.length;
  await olderBtns[0].onclick();
  assert.equal(calls.length, callsBeforeWindows + 1, 'Older windows button makes 1 compare-windows request');
  assert(calls.at(-1).path.endsWith('/history/compare-windows'));

  const olderItems = grid.children[0].querySelectorAll('.compare-older-item');
  assert(olderItems.length > 0, 'Older-windows list renders items');

  const cardTextBeforeItemClick = grid.children[0].textContent;
  olderItems[0].onclick();
  assert.notEqual(grid.children[0].textContent, cardTextBeforeItemClick, 'Clicking older window updates displayed window');

  // 8. Logout/disconnect clears state and cache
  el('disconnect').onclick();
  assert.equal(ui.state().currentCompareData, null, 'Disconnect clears compare data');
  assert.equal(ui.state().compareCache.size, 0, 'Disconnect clears compare cache');
  assert.equal(grid.children.length, 0, 'Disconnect clears compare grid');

  console.log('PASS: compare UI syntax and contracts verified.');
})().catch(err => {
  console.error(err);
  process.exitCode = 1;
});
`

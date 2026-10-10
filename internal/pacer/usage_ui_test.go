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

func TestUsageUISyntaxAndContracts(t *testing.T) {
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

	p := usageFixture(t)
	respProfiles := usageCall(p, "POST", "profiles", `{}`)
	if respProfiles.StatusCode != 200 {
		t.Fatal("usage profiles failed", respProfiles.StatusCode, string(respProfiles.Body))
	}
	var view historyreports.CompareView
	if err := json.Unmarshal(respProfiles.Body, &view); err != nil {
		t.Fatal(err)
	}

	var keyID string
	if len(view.APIKeys) > 0 {
		keyID = view.APIKeys[0].ID
	}
	respKey := usageCall(p, "POST", "profiles", `{"key":"`+keyID+`"}`)
	if respKey.StatusCode != 200 {
		t.Fatal("usage profiles key filtered failed", respKey.StatusCode, string(respKey.Body))
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

	bodyWindows, _ := json.Marshal(historyreports.CompareWindowsQuery{
		Profile: profileRef,
		Kind:    pool.Kind,
		Scope:   pool.Scope,
	})
	respWindows := usageCall(p, "POST", "windows", string(bodyWindows))
	if respWindows.StatusCode != 200 {
		t.Fatal("usage windows failed", respWindows.StatusCode, string(respWindows.Body))
	}

	windowID := ""
	if pool.Latest != nil {
		windowID = pool.Latest.ID
	}
	bodyInspect, _ := json.Marshal(historyreports.InspectQuery{
		Profile: profileRef,
		Kind:    pool.Kind,
		Scope:   pool.Scope,
		Window:  windowID,
	})
	respInspect := usageCall(p, "POST", "inspect", string(bodyInspect))
	if respInspect.StatusCode != 200 {
		t.Fatal("usage inspect failed", respInspect.StatusCode, string(respInspect.Body))
	}

	tmp := t.TempDir()
	profilesFile := filepath.Join(tmp, "profiles.json")
	keyFile := filepath.Join(tmp, "profiles_key.json")
	windowsFile := filepath.Join(tmp, "windows.json")
	inspectFile := filepath.Join(tmp, "inspect.json")

	if err := os.WriteFile(profilesFile, respProfiles.Body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, respKey.Body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(windowsFile, respWindows.Body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inspectFile, respInspect.Body, 0600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(ctx, node, "-e", usageUIHarness)
	cmd.Stdin = strings.NewReader(string(html))
	cmd.Env = append(os.Environ(),
		"USAGE_PROFILES_DATA="+profilesFile,
		"USAGE_KEY_DATA="+keyFile,
		"USAGE_WINDOWS_DATA="+windowsFile,
		"USAGE_INSPECT_DATA="+inspectFile,
		"KEY_ID="+keyID,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Node usage UI contracts: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "PASS: usage UI syntax and contracts verified.") {
		t.Fatalf("Node usage UI harness did not finish:\n%s", out)
	}
	t.Log(string(out))
}

const usageUIHarness = `
const fs = require('node:fs'), vm = require('node:vm'), assert = require('node:assert/strict');
const html = fs.readFileSync(0, 'utf8'), nodes = new Map(), calls = [], timers = new Map();
let clock = 0, held = false, hold = '', release = null;
let exclusionsPatched = false, triggerConflict = false;

class Element {
  constructor(tag = 'div', id = '') {
    this.tag = tag;
    this.id = id;
    this.children = [];
    this.value = '';
    this._text = '';
    this.dataset = {};
    this.style = {};
    this.disabled = false;
    this.hidden = false;
    this.checked = false;
    this.open = false;
    this.attributes = {};
  }
  set innerHTML(v) { throw Error('Unsafe HTML assignment'); }
  set textContent(v) { this._text = String(v ?? ''); this.children = []; }
  get textContent() { return this._text + this.children.map(c => c.textContent || '').join(' '); }
  append(...items) { for (const c of items) if (c && typeof c === 'object') c.parent = this; this.children.push(...items); }
  replaceChildren(...items) { this._text = ''; this.children = items.filter(Boolean); for (const c of this.children) if (typeof c === 'object') c.parent = this; }
  get options() { return this.children; }
  // remove() detaches this element; remove(i) removes a select option.
  remove(i) {
    if (i !== undefined) { this.children.splice(i, 1); return; }
    const k = this.parent ? this.parent.children.indexOf(this) : -1;
    if (k >= 0) this.parent.children.splice(k, 1);
  }
  after(node) { const k = this.parent.children.indexOf(this); node.parent = this.parent; this.parent.children.splice(k + 1, 0, node); }
  focus() {}
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
        else if (sel.includes('[') && sel.endsWith(']')) {
          const attr = sel.slice(sel.indexOf('[') + 1, -1);
          if (attr.includes('=')) {
            const [k, v] = attr.split('=');
            if (c[k] === v || c.attributes[k] === v) res.push(c);
          } else if (c[attr] !== undefined || c.attributes[attr] !== undefined) {
            res.push(c);
          }
        }
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

const keyId = process.env.KEY_ID;

function response(path, body, method) {
  if (method === 'PATCH' && path.endsWith('/usage/exclusions')) {
    exclusionsPatched = true;
    return { revision: 1, excluded: [{ profile: body.profile, window: body.window, excluded: body.excluded, note: body.note }] };
  }
  if (path.endsWith('/usage/profiles')) {
    if (body?.key === keyId) {
      const d = JSON.parse(fs.readFileSync(process.env.USAGE_KEY_DATA, 'utf8'));
      d.query = body;
      d.key_filtered = true;
      d.exclusions_editable = true;
      if (exclusionsPatched) d.exclusions_revision = 1;
      return d;
    }
    const d = JSON.parse(fs.readFileSync(process.env.USAGE_PROFILES_DATA, 'utf8'));
    d.query = body || {};
    d.exclusions_editable = true;
    if (exclusionsPatched) d.exclusions_revision = 1;
    return d;
  }
  if (path.endsWith('/usage/windows')) {
    const d = JSON.parse(fs.readFileSync(process.env.USAGE_WINDOWS_DATA, 'utf8'));
    if (exclusionsPatched && d.windows?.length) {
      d.windows[0].excluded = true;
      d.windows[0].note = 'test note';
    }
    if (d.windows?.length) {
      // A window the provider ended a day before its reset (Codex does).
      const early = { ...d.windows[0], id: 'ended-early', status: 'closed', excluded: false, note: '' };
      early.reset_ms = early.start_ms + 7 * 86400000;
      early.end_ms = early.reset_ms - 86400000;
      d.windows.push(early);
    }
    return d;
  }
  if (path.endsWith('/usage/inspect')) {
    return JSON.parse(fs.readFileSync(process.env.USAGE_INSPECT_DATA, 'utf8'));
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
  prompt: (msg) => 'test note',
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
    if (!opts.headers.Authorization && path.endsWith('/state')) return { ok: false, status: 401, json: async () => ({}) };
    assert.equal(opts.headers.Authorization, 'Bearer synthetic-memory-key');
    const body = opts.body ? JSON.parse(opts.body) : undefined;
    const method = opts.method || 'GET';
    calls.push({ path, body, method });
    if (hold && path.endsWith(hold)) {
      await new Promise(resolve => { release = resolve; held = true; });
    }
    if (triggerConflict && method === 'PATCH' && path.endsWith('/usage/exclusions')) {
      triggerConflict = false;
      return {
        ok: false,
        status: 409,
        json: async () => ({ error: 'Exclusions changed meanwhile. Reload and try again.' })
      };
    }
    return {
      ok: true,
      json: async () => response(path, body, method)
    };
  }
};
sandbox.window = {
  addEventListener() {},
  prompt: (msg) => 'test note'
};
sandbox.window.parent = sandbox.window;
Object.defineProperty(sandbox, 'localStorage', { get() { throw Error('Persistent credentials forbidden'); } });
Object.defineProperty(sandbox, 'sessionStorage', { get() { throw Error('Persistent credentials forbidden'); } });

const hook = 'globalThis.ui={loadUsage,renderUsage,clearUsage,openInspect,openWindows,connect(){connected=true;generation++;key="synthetic-memory-key"},state(){return {currentUsageData,usageMode,usageGeneration,usageCache,connected,key}},allowed};';
const script = html.split('<script>')[1].split('</script>')[0];
assert(script.includes('  if(window.parent!==window)'));
vm.createContext(sandbox);
vm.runInContext(script.replace('  if(window.parent!==window)', hook + '\n  if(window.parent!==window)'), sandbox, { timeout: 5000 });
const ui = sandbox.ui;

(async () => {
  // 1. Assert no innerHTML in script
  assert(!script.includes('.innerHTML'), 'script must never use innerHTML');

  // 2. Assert no /history/ in allowed set and all 4 usage routes present
  for (const r of ui.allowed) {
    assert(!r.includes('/history/'), 'no /history/ in allowed: ' + r);
  }
  assert(ui.allowed.has('POST /v0/management/plugins/pacer/usage/profiles'), 'allowed has POST usage/profiles');
  assert(ui.allowed.has('POST /v0/management/plugins/pacer/usage/windows'), 'allowed has POST usage/windows');
  assert(ui.allowed.has('POST /v0/management/plugins/pacer/usage/inspect'), 'allowed has POST usage/inspect');
  assert(ui.allowed.has('PATCH /v0/management/plugins/pacer/usage/exclusions'), 'allowed has PATCH usage/exclusions');

  // 3. Assert #usage-technical exists and is closed
  assert(nodes.has('usage-technical'), '#usage-technical must exist in DOM');
  assert.equal(el('usage-technical').open, false, '#usage-technical must be closed');
  // The forward browser suite's contract: these IDs stay inside the technical details.
  for (const id of ['forward', 'forward-title', 'forward-form', 'forward-load', 'forward-pin', 'forward-coverage', 'forward-status', 'forward-profile-editor', 'forward-json', 'forward-requests']) assert(nodes.has(id), '#' + id + ' must stay');

  // Connect and verify initial load
  ui.connect();
  assert.equal(calls.length, 0);

  // 4. Initial loadUsage: renders all profiles once
  await ui.loadUsage();
  assert.equal(calls.length, 1, 'Initial load should make exactly 1 request');
  assert(calls[0].path.endsWith('/usage/profiles'));
  assert.deepEqual(calls[0].body, {});

  const profilesData = JSON.parse(fs.readFileSync(process.env.USAGE_PROFILES_DATA, 'utf8'));
  const grid = el('usage-grid');
  const cards = grid.children.filter(c => c.className === 'usage-card');
  assert.equal(cards.length, profilesData.profiles.length, 'All profiles must be rendered once');

  // One short "as of" line on the page; where the numbers come from is in technical details.
  assert(el('usage-asof').textContent.startsWith('Data as of '), 'as-of line');
  assert(el('usage-record').textContent.startsWith('Imported history (CPAMP) up to '), 'record line');

  // Verify archived profiles ordered last with badge
  const nonArchivedCount = profilesData.profiles.filter(p => !p.archived).length;
  for (let i = 0; i < profilesData.profiles.length; i++) {
    const card = cards[i];
    if (i < nonArchivedCount) {
      assert(!card.textContent.includes('Archived'), 'Active profile card must not have Archived badge');
    } else {
      assert(card.textContent.includes('Archived'), 'Archived profile card must have Archived badge');
    }
  }

  // 5. Toggle sends no request
  const callsBeforeToggle = calls.length;
  el('usage-mode-average').onclick();
  assert.equal(calls.length, callsBeforeToggle, 'Toggle to Average must NOT send an API request');
  assert.equal(el('usage-mode-latest').getAttribute('aria-pressed'), 'false');
  assert.equal(el('usage-mode-average').getAttribute('aria-pressed'), 'true');
  assert.equal(ui.state().usageMode, 'average');

  el('usage-mode-latest').onclick();
  assert.equal(calls.length, callsBeforeToggle, 'Toggle to Latest must NOT send an API request');
  assert.equal(el('usage-mode-latest').getAttribute('aria-pressed'), 'true');
  assert.equal(el('usage-mode-average').getAttribute('aria-pressed'), 'false');
  assert.equal(ui.state().usageMode, 'latest');

  // 6. Key change sends exactly one request, cached filter sends zero
  const callsBeforeKey = calls.length;
  el('usage-key').value = keyId;
  await el('usage-key').onchange();
  assert.equal(calls.length, callsBeforeKey + 1, 'Key change triggers exactly one request');
  assert.equal(calls.at(-1).body.key, keyId);

  // Re-select empty key: cached filter sends zero requests
  const callsBeforeCached = calls.length;
  el('usage-key').value = '';
  await el('usage-key').onchange();
  assert.equal(calls.length, callsBeforeCached, 'Cached filter must re-render with zero requests');

  // 6b. Date typing is debounced
  const callsBeforeDate = calls.length;
  el('usage-from').value = '2026-10-01';
  el('usage-from').oninput();
  assert.equal(calls.length, callsBeforeDate, 'Date typing must be debounced without immediate fetch');
  for (const fn of timers.values()) fn();
  timers.clear();
  for (let i = 0; i < 20 && calls.length === callsBeforeDate; i++) await Promise.resolve();
  assert.equal(calls.length, callsBeforeDate + 1, 'Debounced timer triggers fetch');
  assert.equal(calls.at(-1).body.from, '2026-10-01');

  // 7. Stale response is ignored
  hold = '/usage/profiles';
  held = false;
  el('usage-key').value = 'slow-filter';
  const slowPromise = el('usage-key').onchange();
  for (let i = 0; i < 20 && !held; i++) await Promise.resolve();
  assert(held, 'Slow request should be held');

  // Switch to cached filter while slow request is pending
  hold = '';
  el('usage-key').value = '';
  await el('usage-key').onchange();
  assert.equal(ui.state().currentUsageData.query.key || '', '', 'Current view is empty filter');

  release();
  await slowPromise;
  assert.equal(ui.state().currentUsageData.query.key || '', '', 'Stale response was ignored');

  // 7b. Concurrent loads of one filter share one request
  hold = '/usage/profiles';
  held = false;
  el('usage-key').value = 'shared-filter';
  const first = ui.loadUsage();
  for (let i = 0; i < 20 && !held; i++) await Promise.resolve();
  assert(held, 'Shared request should be held');
  const callsShared = calls.length;
  const second = ui.loadUsage();
  hold = '';
  release();
  await Promise.all([first, second]);
  assert.equal(calls.length, callsShared, 'Concurrent load of in-flight filter does not send second request');
  assert.equal(ui.state().currentUsageData.query.key, 'shared-filter');
  el('usage-key').value = '';
  await el('usage-key').onchange();

  // 8. Inspect renders one models-table row per model and its totals
  const inspectBtns = cards[0].querySelectorAll('.usage-inspect-btn');
  assert(inspectBtns.length > 0, 'Card must have Inspect button');
  await inspectBtns[0].onclick();
  assert(el('usage-inspect-dialog').open, 'Inspect dialog must be open');

  const inspectData = JSON.parse(fs.readFileSync(process.env.USAGE_INSPECT_DATA, 'utf8'));
  const modelRows = el('usage-inspect-models').children;
  assert.equal(modelRows.length, inspectData.models.length + 1, 'Models table renders one row per model plus totals row');
  const totalsRow = modelRows[modelRows.length - 1];
  assert(totalsRow.textContent.includes('Total'), 'Last row of models table is totals row');

  el('usage-inspect-close').onclick();
  assert.equal(el('usage-inspect-dialog').open, false, 'Inspect dialog must be closed');

  // 9. Windows checkbox sends a PATCH with revision, clears cache and reloads
  const windowsBtns = cards[0].querySelectorAll('.usage-windows-btn');
  assert(windowsBtns.length > 0, 'Card must have Windows button');
  await windowsBtns[0].onclick();
  assert(el('usage-windows-dialog').open, 'Windows dialog must be open');

  const winRows = el('usage-windows-rows').children;
  assert(winRows.length > 0, 'Windows table must have rows');
  const checkbox = winRows[0].querySelector('input[type=checkbox]');
  assert(checkbox, 'Window row must have a checkbox');
  assert(!checkbox.disabled, 'Checkbox must be enabled when exclusions_editable');
  assert(checkbox.checked, 'Initial window should be checked (not excluded)');

  // Ended early: said under the dates, with the reset that was due.
  const windowsData = JSON.parse(fs.readFileSync(process.env.USAGE_WINDOWS_DATA, 'utf8'));
  assert.equal(winRows.length, windowsData.windows.length + 1, 'one row per window (plus the injected early one)');
  for (const [i, w] of windowsData.windows.entries()) {
    const early = w.status !== 'current' && w.end_ms && w.reset_ms && w.end_ms < w.reset_ms - 60000;
    assert.equal(winRows[i].textContent.includes('Ended early'), !!early, 'row ' + i + ' ended-early mark');
  }
  const earlyRow = winRows[winRows.length - 1];
  assert.match(earlyRow.textContent, /Ended early · reset was due \d{4}-\d{2}-\d{2} \d{2}:\d{2} UTC/, 'ended-early row');
  assert(earlyRow.querySelector('input[type=checkbox]').checked, 'an ended-early window counts in the average');

  // Uncheck window -> inline note editor (no browser prompt) -> PATCH -> reload
  const settle = async () => { for (let i = 0; i < 50; i++) await new Promise(r => setImmediate(r)); };
  const leaveOut = async (row, note) => {
    const box = row.querySelector('input[type=checkbox]');
    box.checked = false;
    await box.onchange();
    assert(box.checked, 'the box stays checked until the note is saved');
    const rows = el('usage-windows-rows').children;
    const editor = rows[rows.indexOf(row) + 1];
    assert(editor && editor.className === 'usage-exclude-editor', 'inline editor opens under the row');
    editor.querySelector('.usage-exclude-note').value = note;
    editor.querySelector('form').onsubmit({ preventDefault() {} });
    await settle();
  };
  sandbox.prompt = sandbox.window.prompt = () => { throw Error('browser prompt used'); };
  const callsBeforeWinChange = calls.length;
  await leaveOut(winRows[0], ' test note ');

  const patchCall = calls.find(c => c.method === 'PATCH' && c.path.endsWith('/usage/exclusions'));
  assert(patchCall, 'Unchecking sends a PATCH request');
  assert.equal(patchCall.body.revision, 0, 'PATCH sent with revision 0');
  assert.equal(patchCall.body.excluded, true, 'PATCH sent with excluded = true');
  assert.equal(patchCall.body.note, 'test note', 'PATCH sent with the typed note');

  // Verify reload occurred
  const profilesCallsAfter = calls.filter(c => c.path.endsWith('/usage/profiles'));
  assert(profilesCallsAfter.length > 1, 'Profiles reloaded after PATCH');
  const windowsCallsAfter = calls.filter(c => c.path.endsWith('/usage/windows'));
  assert(windowsCallsAfter.length > 1, 'Windows reloaded after PATCH');
  assert.equal(ui.state().currentUsageData.exclusions_revision, 1, 'Revision updated after reload');

  // 9b. 409 conflict reloads and shows "changed meanwhile"
  triggerConflict = true;
  await leaveOut(el('usage-windows-rows').children[0], 'again');
  assert.match(el('usage-windows-status').textContent, /changed meanwhile/i, '409 shows changed meanwhile');
  assert.equal(el('usage-windows-status').hidden, false);

  el('usage-windows-close').onclick();
  assert.equal(el('usage-windows-dialog').open, false, 'Windows dialog must be closed');

  // 10. Disconnect clears state and cache
  el('disconnect').onclick();
  assert.equal(ui.state().currentUsageData, null, 'Disconnect clears usage data');
  assert.equal(ui.state().usageCache.size, 0, 'Disconnect clears usage cache');
  assert.equal(el('usage-asof').textContent, '', 'Disconnect clears the as-of line');
  assert.equal(el('usage-record').textContent, '', 'Disconnect clears the record line');
  assert.equal(grid.children.length, 0, 'Disconnect clears usage grid');

  console.log('PASS: usage UI syntax and contracts verified.');
})().catch(err => {
  console.error(err);
  process.exitCode = 1;
});
`

"""Pacer Phase 1+2 on CT104: Pacer replaces quota-handoff 0.1.5-local.2 (routing dry-run).

CPA must run the pacer-boundary host (9f332564), so this swaps the CPA image as well as
the plugin. Server phases run on CT104 with this script on stdin; nothing secret is printed:

  ssh <pve> 'pct exec 104 -- python3 -B - recon'                          < phase1.py
  ssh <pve> 'pct exec 104 -- python3 -B - stage'                          < phase1.py
  ssh <pve> 'pct exec 104 -- python3 -B - cutover --i-have-user-approval' < phase1.py
  ssh <pve> 'pct exec 104 -- python3 -B - verify'                         < phase1.py
  ssh <pve> 'pct exec 104 -- python3 -B - rollback'                       < phase1.py

`bundle` runs on the Mac and assembles the pinned release directory that is copied to
RELEASE before `stage`.

recon     read-only facts and edit plan.
stage     verifies artifacts, builds the candidate image, takes private backups, initializes
          the Pacer state with pacer-state in a network-none container. No runtime change.
cutover   idle gate, then: stop CPA, edit config.yaml/compose by line, add the Pacer library,
          start the candidate, rewrite the legacy Caddy event path to Pacer (validate+reload),
          enable the daily report timer, verify. Any failure rolls back automatically.
verify    checks whichever side is running (candidate or incumbent).
rollback  idle gate, then byte-for-byte restore of every changed file and the old image.

Idle gate: three consecutive quiet samples (PHASE1_IDLE_INTERVAL s apart). A sample is quiet
when Caddy's admin API reports zero in-flight requests to the CPA upstream, CPA logged no
OpenCodePlus event poll (`/events?after=`) in the last 45 s, the running plugin has no
unfinished work (quota-handoff tickets, or Pacer unfinished cache-policy requests/attempts),
and no other house deploy script runs (known names, or any other `python3 -` stdin script).

Exit codes: 0 ok, 1 error, 2 refused (no change), 3 not idle (no change),
4 verification failed and rolled back, 5 rollback incomplete, 6 verify found failures.

Every path, name and URL has a PHASE1_* override (see settings below) so the same code runs
in the local rehearsal (scripts/production/rehearse.sh). Defaults are CT104 production.
PHASE1_INJECT_FAIL=<check> (fails that verification check) or step:<name> (raises after that
cutover step) exists only for the rehearsal and is refused unless PHASE1_REHEARSAL=1.
"""
import argparse
import copy
import datetime
import difflib
import fcntl
import hashlib
import json
import os
import posixpath
import re
import shlex
import shutil
import subprocess
import sys
import time
import traceback
import urllib.error
import urllib.request
from pathlib import Path

try:
    import yaml
except ImportError:  # recon reports it; edits refuse without it unless explicitly allowed
    yaml = None


def env(name, default):
    return os.environ.get('PHASE1_' + name, default)


TAG = 'pacer-phase1-0efd99c-9f332564-qh2'
ROOT = Path(env('ROOT', '/opt/cliproxyapi'))
RELEASE = Path(env('RELEASE', str(ROOT / 'releases' / TAG)))
BACKUP = Path(env('BACKUP', str(ROOT / 'backups' / TAG)))
CONFIG = ROOT / 'config.yaml'
COMPOSE = ROOT / 'docker-compose.yml'
PLUGINS = ROOT / 'plugins'
LIBDIR = PLUGINS / 'linux' / 'amd64'
QH_LIB = LIBDIR / 'quota-handoff-v0.1.5-local.2.so'
QH_STATE = PLUGINS / 'quota-handoff' / 'state.sqlite'
PACER_LIB = LIBDIR / 'pacer-v0.2.0-dev.so'
PACER_DIR = PLUGINS / 'pacer'
PACER_STATE = PACER_DIR / 'state.sqlite'
PACER_REPORTS = PACER_DIR / 'reports.sqlite'
PACER_FORWARD = PACER_DIR / 'forward-report.sqlite'
CONTAINER_PLUGINS = env('CONTAINER_PLUGINS', '/CLIProxyAPI/plugins')
CADDY_DIR = Path(env('CADDY_DIR', '/etc/caddy'))
SNIPPET = CADDY_DIR / 'cliproxyapi-quota-handoff.Caddyfile'
CADDYFILE = CADDY_DIR / 'Caddyfile'
MGMT_KEY_FILE = CADDY_DIR / 'cliproxyapi-management-auth.Caddyfile'
CADDY_CMD = shlex.split(env('CADDY_CMD', 'caddy'))
CADDY_CONFIG_ARG = env('CADDY_CONFIG_ARG', '/etc/caddy/Caddyfile')
CADDY_ADMIN = env('CADDY_ADMIN', 'http://localhost:2019')
CADDY_UPSTREAM = env('CADDY_UPSTREAM', '127.0.0.1:8317')
CONTAINER = env('CONTAINER', 'cliproxyapi')
SERVICE = env('SERVICE', 'cli-proxy-api')
CPAMP = env('CPAMP', 'cpa-manager-plus')
OTHERS_PREFIX = env('OTHERS_PREFIX', '')
CPA_URL = env('CPA_URL', 'http://127.0.0.1:8317')
PUBLIC_URL = env('PUBLIC_URL', 'https://cliproxy.boe.moe')
OLD_IMAGE = env('OLD_IMAGE', 'cliproxyapi-opus55:20261007-13')
NEW_IMAGE = env('NEW_IMAGE', 'cliproxyapi-opus55:pacer-9f332564')
PLATFORM = env('PLATFORM', '')
DOCKER_PREFIX = env('DOCKER_PREFIX', 'pacer-phase1')
SQLITE_IMAGE = env('SQLITE_IMAGE', '')
SYSTEMD = env('SYSTEMD', '1') == '1'
UNIT_DIR = Path(env('UNIT_DIR', '/etc/systemd/system'))
REPORT_DIR = Path(env('REPORT_DIR', str(ROOT / 'reports' / 'pacer-dry-run')))
SMOKE_MODEL = env('SMOKE_MODEL', 'claude-opus-5-5')
IDLE_INTERVAL = float(env('IDLE_INTERVAL', '5'))
POLL_WINDOW = int(env('POLL_WINDOW', '45'))
STALE_TICKET_SECONDS = 3600
REHEARSAL = env('REHEARSAL', '0') == '1'
INJECT = env('INJECT_FAIL', '')
ALLOW_NO_YAML = env('ALLOW_NO_YAML', '0') == '1'

QH_VERSION = '0.1.5-local.2'
PACER_VERSION = '0.2.0-dev'
LEGACY_EVENTS = '/v0/resource/plugins/quota-handoff/events'
PACER_EVENTS = '/v0/resource/plugins/pacer/events'
TIMER = 'pacer-dry-run-daily.timer'
UNIT_SERVICE = 'pacer-dry-run-daily.service'
SOURCES = {'pacer': '0efd99c26b1568fe9b82bf1bed34977ca11b1487',
           'cpa': '9f332564b5fddb12b9a524a7338952df18b72f1e'}
PINS = {'image/CLIProxyAPI': '7df161d07f36ce54c2efc6698f168fe33652007f15ebd9a4ab4e03e5b63ad029',
        'pacer-v0.2.0-dev.so': 'cf137cdaa6c8a1126df13c5f0a9fc858da204f7df2a5f918d80999ce1291b010',
        'pacer-state': '80c30903018a62791131afce3b86b04007694f9d1fbf0d6b164c1a7ecd4421dd',
        'quota-handoff-v0.1.5-local.2.so': '176f68a8ce34db0f35bc30372d15bddd4348978a3905b6cd916d8a6a40a3343f'}
DEPLOY_SCRIPTS = ('cutover_v8', 'deploy-discover', 'deploy-compact-on-switch', 'deploy-public-plugin',
                  'deploy-shadow', 'mute-notifications', 'deploy-quota-usage-ct104', 'deploy-profile-keys-ct104',
                  'deploy-model-settings-ct104')
# House scripts piped as `python3 -B - <phase>` show no file name; any other stdin script counts as busy.
STDIN_PYTHON = re.compile(r'(^|/)python3?(\.\d+)?\s+(-[A-Za-z]+\s+)*-(\s|$)', re.I)
EXIT_OK, EXIT_ERROR, EXIT_REFUSED, EXIT_NOT_IDLE, EXIT_ROLLED_BACK, EXIT_ROLLBACK_FAILED, EXIT_VERIFY = 0, 1, 2, 3, 4, 5, 6

DOCKERFILE = 'ARG BASE\nFROM ${BASE}\nCOPY CLIProxyAPI /CLIProxyAPI/CLIProxyAPI\n'
UNITS = {
    UNIT_SERVICE: '''[Unit]
Description=Pacer dry-run daily report (archives Pacer's report route, one file per UTC day)
After=docker.service

[Service]
Type=oneshot
UMask=0077
Nice=10
NoNewPrivileges=yes
ExecStart=/usr/bin/python3 -B {release}/dry-run-daily.py --out {reports}
''',
    TIMER: '''[Unit]
Description=Daily Pacer dry-run report

[Timer]
OnCalendar=*-*-* 00:15:00 UTC
Persistent=true

[Install]
WantedBy=timers.target
''',
}


class Refused(Exception):
    pass


class EditError(Refused):
    pass


# ---------------------------------------------------------------- small helpers

def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat(timespec='seconds')


def stamp():
    return datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')


def digest(path):
    h = hashlib.sha256()
    with open(path, 'rb') as f:
        for chunk in iter(lambda: f.read(1 << 20), b''):
            h.update(chunk)
    return h.hexdigest()


def sha_text(text):
    return hashlib.sha256(text.encode() if isinstance(text, str) else text).hexdigest()


def label(value):
    """Short one-way label for identifiers that may contain e-mail addresses."""
    return hashlib.sha256(str(value).encode()).hexdigest()[:12]


def meta(path):
    st = os.stat(path)
    return {'sha256': digest(path), 'size': st.st_size, 'mode': oct(st.st_mode & 0o7777),
            'uid': st.st_uid, 'gid': st.st_gid, 'inode': st.st_ino}


def write_in_place(path, data):
    # Single-file bind mounts and Caddy imports keep their inode, owner and mode.
    if isinstance(data, str):
        data = data.encode()
    with open(path, 'r+b') as f:
        f.seek(0)
        f.write(data)
        f.truncate()
        f.flush()
        os.fsync(f.fileno())


def save(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(path.name + '.tmp')
    tmp.write_text(json.dumps(value, indent=2, sort_keys=True, default=str) + '\n')
    tmp.chmod(0o600)
    os.replace(tmp, path)


def receipt_path(phase):
    return RELEASE / 'receipts' / ('%s-%s.json' % (phase, stamp()))


def run(cmd, check=True, cwd=None, timeout=600, input=None):
    out = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True, timeout=timeout, input=input)
    if check and out.returncode != 0:
        raise RuntimeError('%s failed (%d): %s' % (' '.join(cmd[:4]), out.returncode, (out.stderr or out.stdout).strip()[-400:]))
    return out


def platform_args():
    return ['--platform', PLATFORM] if PLATFORM else []


_counter = [0]


def unique(kind):
    _counter[0] += 1
    return '%s-%s-%d-%d' % (DOCKER_PREFIX, kind, os.getpid(), _counter[0])


def guard_rehearsal():
    if REHEARSAL and str(ROOT) == '/opt/cliproxyapi':
        raise Refused('rehearsal mode refuses the production root')
    if INJECT and not REHEARSAL:
        raise Refused('failure injection is rehearsal-only')


# ---------------------------------------------------------------- HTTP and CPA

OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}), type('NoRedirect', (urllib.request.HTTPRedirectHandler,), {'redirect_request': lambda *a, **k: None}))


def http(url, key=None, body=None, method=None, timeout=30, headers=None):
    h = {'Content-Type': 'application/json'}
    h.update(headers or {})
    if key:
        h['Authorization'] = 'Bearer ' + key
    req = urllib.request.Request(url, headers=h, method=method, data=None if body is None else json.dumps(body).encode())
    try:
        resp = OPENER.open(req, timeout=timeout)
    except urllib.error.HTTPError as e:
        resp = e
    except (urllib.error.URLError, OSError):
        return 0, b''
    with resp:
        return resp.status, resp.read()


def jbody(raw):
    try:
        return json.loads(raw)
    except ValueError:
        return None


def mgmt_key():
    m = re.search(r'(?i)Bearer\s+([^"\s]+)', MGMT_KEY_FILE.read_text())
    if not m:
        raise Refused('management key not found in ' + str(MGMT_KEY_FILE))
    return m.group(1)


def api(path, method='GET', body=None):
    code, raw = http(CPA_URL + '/v0/management/' + path, mgmt_key(), body, method)
    if code != 200:
        raise RuntimeError('management %s %s -> %s' % (method, path, code))
    return json.loads(raw)


def plugins():
    try:
        items = api('plugins')['plugins']
    except Exception:
        return None
    return {p['id']: {'registered': bool(p.get('registered')), 'enabled': bool(p.get('enabled')),
                      'effective': bool(p.get('effective_enabled')),
                      'version': (p.get('metadata') or {}).get('version')} for p in items}


def client_key():
    return api('api-keys')['api-keys'][0]


def health():
    return http(CPA_URL + '/healthz', timeout=5)[0] == 200


def wait_until(predicate, seconds, interval=1.0):
    deadline = time.monotonic() + seconds
    while True:
        value = predicate()
        if value or time.monotonic() > deadline:
            return value
        time.sleep(interval)


def models(key):
    code, raw = http(CPA_URL + '/v1/models', key)
    ids = sorted(m['id'] for m in (jbody(raw) or {}).get('data', [])) if code == 200 else []
    return {'status': code, 'count': len(ids), 'sha256': sha_text('\n'.join(ids)), 'ids': ids}


def wait_models(key, expected, seconds=90, interval=1):
    """Health/plugin registration precede the asynchronous per-auth model catalogue.

    Wait for the exact incumbent set, never accept a subset, superset or alias
    substitution. Persistent drift still fails the unchanged models_same gate.
    """
    latest = None
    observed = []

    def ready():
        nonlocal latest
        latest = models(key)
        identity = {k: latest[k] for k in ('status', 'count', 'sha256')}
        if not observed or observed[-1] != identity:
            observed.append(identity)
        return latest['status'] == 200 and latest['sha256'] == expected['sha256']

    wait_until(ready, seconds, interval)
    return dict(latest, startup_observations=observed)


def discovery(base, path):
    code, raw = http(base + path)
    body = jbody(raw) if code == 200 else None
    return {'status': code, 'protocol': (body or {}).get('protocol'), 'pacer': (body or {}).get('pacer') is True,
            'mode': (body or {}).get('mode')}


def usage(key):
    code, raw = http(PUBLIC_URL + LEGACY_EVENTS + '?view=usage&auth=api-key&all=true', key)
    creds = (jbody(raw) or {}).get('credentials') or [] if code == 200 else []
    return {'status': code, 'ids': sorted(label(c.get('id')) for c in creds)}


def inference(key, session=None):
    headers = {'X-Session-ID': session} if session else {}
    code, raw = http(PUBLIC_URL + '/v1/chat/completions', key,
                     {'model': SMOKE_MODEL, 'max_tokens': 16, 'messages': [{'role': 'user', 'content': 'Reply with exactly: OK'}]},
                     timeout=120, headers=headers)
    return {'status': code, 'model': SMOKE_MODEL}


def pacer_state_view():
    code, raw = http(CPA_URL + '/v0/management/plugins/pacer/state', mgmt_key(), timeout=30)
    body = jbody(raw) if code == 200 else {}
    return {'status': code, 'mode': ((body or {}).get('routing') or {}).get('mode'),
            'decisions': len((body or {}).get('decisions') or []),
            'decision_write_errors': (body or {}).get('decision_write_errors'),
            'accounts': len((body or {}).get('accounts') or [])}


# ---------------------------------------------------------------- Docker

def inspect(name):
    out = run(['docker', 'inspect', name], check=False)
    return json.loads(out.stdout)[0] if out.returncode == 0 else None


def process():
    item = inspect(CONTAINER)
    if not item:
        return None
    return {'id': item['Id'][:12], 'image': item['Config']['Image'], 'image_id': item['Image'][:19],
            'started': item['State']['StartedAt'], 'status': item['State']['Status'], 'user': item['Config'].get('User')}


def image_id(name):
    out = run(['docker', 'image', 'inspect', '--format', '{{.Id}}', name], check=False)
    return out.stdout.strip()[:19] if out.returncode == 0 else None


def compose(*args, timeout=300):
    return run(['docker', 'compose'] + list(args), cwd=str(ROOT), timeout=timeout)


def others():
    out = run(['docker', 'ps', '--format', '{{.Names}} {{.ID}} {{.Status}}'])
    rows = [l.split(' ', 2) for l in out.stdout.splitlines() if l.strip()]
    return sorted('%s %s' % (r[0], r[1]) for r in rows
                  if r[0] != CONTAINER and r[0].startswith(OTHERS_PREFIX) and not r[0].startswith(DOCKER_PREFIX + '-'))


def container_status(name):
    item = inspect(name) if name else None
    if not item:
        return None
    return '%s %s' % (item['State']['Status'], (item['State'].get('Health') or {}).get('Status', '-'))


def logs_since(since):
    out = run(['docker', 'logs', '--since', since, CONTAINER], check=False)
    return (out.stdout + out.stderr).splitlines()


def fatal_lines(since):
    return [re.sub(r'(?i)(bearer\s+)\S+', r'\1<redacted>', l)[:240] for l in logs_since(since)
            if re.search(r'(?i)\b(fatal|panic)\b', l)][:10]


def cpa_user():
    if yaml is not None:
        svc = ((yaml.safe_load(COMPOSE.read_text()) or {}).get('services') or {}).get(SERVICE) or {}
        if svc.get('user'):
            uid, _, gid = str(svc['user']).partition(':')
            return int(uid), int(gid or uid)
    st = PLUGINS.stat()
    return st.st_uid, st.st_gid


def chown_like(path, uid, gid):
    if os.geteuid() == 0:
        os.chown(path, uid, gid)


# ---------------------------------------------------------------- SQLite (read-only)

SQL_PROGRAM = r'''
import json, os, sqlite3, sys, time
op, path = sys.argv[1], sys.argv[2]
def ro(p):
    return sqlite3.connect('file:' + p + '?mode=ro', uri=True, timeout=5)
out = {}
if not os.path.exists(path):
    out['exists'] = False
elif op == 'qh':
    db = ro(path)
    out['exists'] = True
    try:
        row = db.execute('SELECT data FROM state WHERE id=1').fetchone()
        data = json.loads(row[0]) if row else {}
        if not row:
            out['state_row'] = False
    except Exception as e:
        data = None
        out['state_error'] = type(e).__name__
    tickets = ((data or {}).get('adaptive') or {}).get('tickets') or {}
    pending = [t for t in tickets.values() if not t.get('completed') and not t.get('orphan')]
    stale = [t for t in pending if time.time() - (t.get('started') or 0) > int(sys.argv[3])]
    out.update(open=len(pending) - len(stale), stale=len(stale))
elif op == 'pacer':
    db = ro(path)
    out['exists'] = True
    names = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'")}
    out['open'] = 0
    for table in ('cache_policy_requests', 'cache_policy_attempts'):
        if table in names:
            out['open'] += db.execute('SELECT count(*) FROM ' + table + " WHERE outcome=''").fetchone()[0]
    if 'observation_jobs' in names:
        out['observation_inflight'] = db.execute("SELECT count(*) FROM observation_jobs WHERE state='inflight'").fetchone()[0]
    if 'pacer_decisions' in names:
        out['decisions'] = db.execute('SELECT count(*) FROM pacer_decisions').fetchone()[0]
elif op == 'check':
    db = ro(path)
    out['exists'] = True
    out['quick_check'] = db.execute('PRAGMA quick_check').fetchone()[0]
    out['tables'] = sorted(r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'"))
elif op == 'backup':
    dest = sys.argv[3]
    fd = os.open(dest, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    os.close(fd)
    src, dst = ro(path), sqlite3.connect(dest)
    src.backup(dst)
    out['exists'] = True
    out['quick_check'] = dst.execute('PRAGMA quick_check').fetchone()[0]
    dst.close()
print(json.dumps(out))
'''


def sqlite(op, path, *extra, mounts=()):
    """CT104 runs this in-process next to CPA. The rehearsal sets PHASE1_SQLITE_IMAGE so the
    reader runs inside the Docker VM, where the WAL index is coherent with the CPA container."""
    args = [op, str(path)] + [str(e) for e in extra]
    if SQLITE_IMAGE:
        cmd = ['docker', 'run', '--rm', '--name', unique('sqlite'), '--network', 'none']
        for m in sorted({str(Path(path).parent)} | {str(m) for m in mounts}):
            cmd += ['-v', '%s:%s' % (m, m)]
        cmd += [SQLITE_IMAGE, 'python3', '-B', '-c', SQL_PROGRAM] + args
    else:
        cmd = [sys.executable, '-B', '-c', SQL_PROGRAM] + args
    return json.loads(run(cmd, timeout=120).stdout)


# ---------------------------------------------------------------- idle gate

def running_side():
    p = process()
    if not p or p['status'] != 'running':
        return 'stopped'
    return 'candidate' if p['image'] == NEW_IMAGE else 'incumbent' if p['image'] == OLD_IMAGE else 'unknown'


def caddy_inflight():
    code, raw = http(CADDY_ADMIN.rstrip('/') + '/reverse_proxy/upstreams', timeout=5)
    items = [u for u in (jbody(raw) or []) if isinstance(u, dict) and u.get('address') == CADDY_UPSTREAM] if code == 200 else []
    return sum(int(u.get('num_requests') or 0) for u in items) if items else None


def deploy_scripts():
    out = run(['ps', '-eo', 'pid=,args='], check=False).stdout
    hits = []
    for line in out.splitlines():
        pid, _, args = line.strip().partition(' ')
        if not pid.isdigit() or int(pid) == os.getpid():
            continue
        args = args.strip()
        if any(s in args for s in DEPLOY_SCRIPTS):
            hits.append(next(s for s in DEPLOY_SCRIPTS if s in args))
        elif STDIN_PYTHON.search(args):
            hits.append('stdin-python:' + ' '.join(args.split()[:4])[:60])
    return sorted(set(hits))


def sample():
    side = running_side()
    s = {'at': now(), 'side': side, 'caddy_inflight': caddy_inflight(), 'deploy_scripts': deploy_scripts()}
    s['event_polls'] = sum('/events?after=' in l for l in logs_since('%ds' % POLL_WINDOW)) if side != 'stopped' else 0
    if side == 'incumbent':
        st = sqlite('qh', QH_STATE, STALE_TICKET_SECONDS)
        s['plugin_open'], s['stale_tickets_ignored'] = st.get('open', 0), st.get('stale', 0)
    elif side == 'candidate':
        st = sqlite('pacer', PACER_STATE)
        s['plugin_open'], s['observation_inflight'] = st.get('open', 0), st.get('observation_inflight', 0)
    else:
        s['plugin_open'] = 0
    s['quiet'] = (s['caddy_inflight'] == 0 and s['event_polls'] == 0 and s['plugin_open'] == 0
                  and not s['deploy_scripts'] and side in ('incumbent', 'candidate', 'stopped'))
    return s


def idle_gate(wait, need=3):
    samples, quiet, deadline = [], 0, time.monotonic() + wait
    while True:
        s = sample()
        samples.append(s)
        quiet = quiet + 1 if s['quiet'] else 0
        if quiet >= need:
            return {'ok': True, 'samples': samples[-12:], 'sampled': len(samples)}
        if time.monotonic() + IDLE_INTERVAL > deadline:
            return {'ok': False, 'samples': samples[-12:], 'sampled': len(samples)}
        time.sleep(IDLE_INTERVAL)


# ---------------------------------------------------------------- line edits

def indent_of(line):
    return len(line) - len(line.lstrip(' '))


def content(line):
    s = line.strip()
    return bool(s) and not s.startswith('#')


def key_line(lines, start, end, indent, key):
    pat = re.compile('^' + ' ' * indent + re.escape(key) + r':\s*(#.*)?$')
    hits = [i for i in range(start, end) if pat.match(lines[i].rstrip('\n'))]
    if len(hits) != 1:
        raise EditError('expected one "%s:" at indent %d, found %d' % (key, indent, len(hits)))
    return hits[0]


def block_end(lines, i, indent):
    j = i + 1
    while j < len(lines) and not (content(lines[j]) and indent_of(lines[j]) <= indent):
        j += 1
    while j > i + 1 and not content(lines[j - 1]):
        j -= 1
    return j


def child_indent(lines, i, end):
    for j in range(i + 1, end):
        if content(lines[j]):
            return indent_of(lines[j])
    raise EditError('empty block at line %d' % (i + 1))


def forward_book_digest():
    value = (RELEASE / 'forward-book.digest').read_text().strip()
    if not re.fullmatch(r'[a-f0-9]{64}', value):
        raise Refused('bundle forward book digest is invalid')
    return value


def pacer_block(indent, step, mode):
    rows = [(0, 'pacer:'), (1, 'enabled: true'), (1, 'priority: 1'),
            (1, 'state_path: %s/pacer/state.sqlite' % CONTAINER_PLUGINS), (1, 'compact_on_switch: true'),
            (1, 'report_path: %s/pacer/reports.sqlite' % CONTAINER_PLUGINS),
            (1, 'forward_report_path: %s/pacer/forward-report.sqlite' % CONTAINER_PLUGINS),
            (1, 'forward_report_book: "%s"' % forward_book_digest()),
            (1, 'routing:'), (2, 'mode: %s' % mode), (2, 'providers: [claude]'),
            (1, 'cache_policy:'), (2, 'enabled: true'), (2, 'routes:'), (3, '- id: claude'),
            (3, '  models: [claude-opus-5-5]'), (2, 'trusted_empty_installations: []')]
    return [' ' * (indent + level * step) + text + '\n' for level, text in rows]


def pacer_config(mode):
    return {'enabled': True, 'priority': 1, 'state_path': '%s/pacer/state.sqlite' % CONTAINER_PLUGINS,
            'report_path': '%s/pacer/reports.sqlite' % CONTAINER_PLUGINS,
            'forward_report_path': '%s/pacer/forward-report.sqlite' % CONTAINER_PLUGINS,
            'forward_report_book': forward_book_digest(),
            'compact_on_switch': True, 'routing': {'mode': mode, 'providers': ['claude']},
            'cache_policy': {'enabled': True, 'routes': [{'id': 'claude', 'models': ['claude-opus-5-5']}],
                             'trusted_empty_installations': []}}


def require_yaml():
    if yaml is None and not ALLOW_NO_YAML:
        raise Refused('PyYAML is required for the semantic edit checks')
    return yaml is not None


def edit_config(text, mode):
    if not text.endswith('\n'):
        raise EditError('config.yaml must end with a newline')
    lines = text.splitlines(True)
    p = key_line(lines, 0, len(lines), 0, 'plugins')
    pend = block_end(lines, p, 0)
    ci = child_indent(lines, p, pend)
    c = key_line(lines, p + 1, pend, ci, 'configs')
    cend = block_end(lines, c, ci)
    pi = child_indent(lines, c, cend)
    if any(re.match(' ' * pi + r'pacer:\s*(#.*)?$', lines[i].rstrip('\n')) for i in range(c + 1, cend)):
        raise EditError('plugins.configs.pacer already exists')
    q = key_line(lines, c + 1, cend, pi, 'quota-handoff')
    qend = block_end(lines, q, pi)
    qi = child_indent(lines, q, qend)
    enabled = [i for i in range(q + 1, qend) if indent_of(lines[i]) == qi and re.match(r'\s*enabled:', lines[i])]
    new = list(lines)
    if len(enabled) == 1:
        m = re.match(r'^(\s*enabled:\s*)true(\s*(#.*)?)$', lines[enabled[0]].rstrip('\n'))
        if not m:
            raise EditError('quota-handoff enabled line is not "enabled: true"')
        new[enabled[0]] = m.group(1) + 'false' + m.group(2) + '\n'
        new[qend:qend] = pacer_block(pi, qi - pi, mode)
    elif not enabled:
        new[qend:qend] = pacer_block(pi, qi - pi, mode)
        new[q + 1:q + 1] = [' ' * qi + 'enabled: false\n']
    else:
        raise EditError('quota-handoff has %d enabled lines' % len(enabled))
    out = ''.join(new)
    changed = [l for l in difflib.ndiff(lines, new) if l[:2] in ('- ', '+ ')]
    if require_yaml():
        before, after = yaml.safe_load(text), yaml.safe_load(out)
        expect = copy.deepcopy(before)
        expect['plugins']['configs']['quota-handoff']['enabled'] = False
        expect['plugins']['configs']['pacer'] = pacer_config(mode)
        if after != expect:
            raise EditError('config edit is not exactly quota-handoff.enabled=false plus the pacer block')
        if before['plugins']['configs']['quota-handoff'].get('enabled', True) is not True:
            raise EditError('quota-handoff is not enabled in config.yaml')
    return out, {'changed_lines': len(changed), 'pacer_block_lines': len(pacer_block(pi, qi - pi, mode)),
                 'indent': {'configs': ci, 'plugin': pi, 'step': qi - pi}}


def edit_compose(text):
    pat = re.compile(r'^([ \t]+)image:[ \t]*' + re.escape(OLD_IMAGE) + r'[ \t]*$', re.M)
    hits = list(pat.finditer(text))
    if len(hits) != 1:
        raise EditError('expected one "image: %s" line in compose, found %d' % (OLD_IMAGE, len(hits)))
    m, ind = hits[0], hits[0].group(1)
    repl = '%s# Pacer phase 1 candidate (CPA %s); previous: %s\n%simage: %s' % (ind, SOURCES['cpa'][:8], OLD_IMAGE, ind, NEW_IMAGE)
    out = text[:m.start()] + repl + text[m.end():]
    if require_yaml():
        before, after = yaml.safe_load(text), yaml.safe_load(out)
        expect = copy.deepcopy(before)
        expect['services'][SERVICE]['image'] = NEW_IMAGE
        if after != expect:
            raise EditError('compose edit is not exactly services.%s.image' % SERVICE)
    return out


def edit_snippet(text):
    if PACER_EVENTS in text:
        raise EditError('Caddy snippet already mentions the Pacer event path')
    lines = text.splitlines(True)
    paths = [i for i, l in enumerate(lines) if re.match(r'^\s*path\s+' + re.escape(LEGACY_EVENTS) + r'\s*$', l.rstrip('\n'))]
    uses = [i for i, l in enumerate(lines) if LEGACY_EVENTS in l and content(l)]
    if len(paths) != 1 or uses != paths:
        raise EditError('expected exactly one "path %s" matcher line' % LEGACY_EVENTS)
    i = paths[0]
    name = None
    for j in range(i - 1, -1, -1):
        m = re.match(r'^\s*(@[\w-]+)\s*\{\s*$', lines[j].rstrip('\n'))
        if m:
            name = m.group(1)
            break
        if content(lines[j]) and '}' in lines[j]:
            break
    if not name:
        raise EditError('path line is not inside a named matcher block')
    handlers = [k for k, l in enumerate(lines) if re.match(r'^\s*(route|handle)\s+' + re.escape(name) + r'\s*\{\s*$', l.rstrip('\n'))]
    if len(handlers) != 1:
        raise EditError('expected one route/handle for ' + name)
    h = handlers[0]
    inner = next((indent_of(lines[k]) for k in range(h + 1, len(lines)) if content(lines[k])), None)
    if inner is None or inner <= indent_of(lines[h]):
        raise EditError('handler block for %s is empty' % name)
    new = list(lines)
    new[i] = lines[i][:indent_of(lines[i])] + 'path %s %s\n' % (LEGACY_EVENTS, PACER_EVENTS)
    new.insert(h + 1, ' ' * inner + 'rewrite %s %s\n' % (LEGACY_EVENTS, PACER_EVENTS))
    return ''.join(new), {'matcher': name, 'changed_lines': 2}


def layout():
    """The idle gate and the Pacer state path assume plugins/ is mounted at CONTAINER_PLUGINS."""
    cfg = yaml.safe_load(CONFIG.read_text()) or {}
    pl = cfg.get('plugins') or {}
    qh = (pl.get('configs') or {}).get('quota-handoff') or {}
    svc = ((yaml.safe_load(COMPOSE.read_text()) or {}).get('services') or {}).get(SERVICE) or {}
    hosts = []
    for v in svc.get('volumes') or []:
        host, _, rest = v.partition(':') if isinstance(v, str) else (v.get('source', ''), '', v.get('target', ''))
        if rest.split(':')[0].rstrip('/') == CONTAINER_PLUGINS:
            hosts.append(host)
    pdir = pl.get('dir')
    checks = {'plugins_mount': any((ROOT / h).resolve() == PLUGINS.resolve() for h in hosts),
              'container_name': svc.get('container_name') == CONTAINER,
              'plugins_dir': pdir is None or posixpath.normpath(posixpath.join('/CLIProxyAPI', str(pdir))) == CONTAINER_PLUGINS,
              'qh_state_path': qh.get('state_path') in (None, CONTAINER_PLUGINS + '/quota-handoff/state.sqlite')}
    return {'ok': all(checks.values()), 'checks': checks, 'plugins_dir_set': pdir is not None,
            'qh_state_path_set': 'state_path' in qh}


def snippet_imported():
    return bool(re.search(r'(?m)^\s*import\s+' + re.escape(str(Path(CADDY_CONFIG_ARG).parent / SNIPPET.name)) + r'\s*$',
                          CADDYFILE.read_text()))


def caddy(action):
    out = run(CADDY_CMD + [action, '--config', CADDY_CONFIG_ARG, '--adapter', 'caddyfile'], check=False, timeout=120)
    return out.returncode == 0, (out.stderr or out.stdout).strip().splitlines()[-1:] if out.returncode else []


def units():
    fields = {'release': RELEASE, 'reports': REPORT_DIR}
    return {name: text.format(**fields) for name, text in UNITS.items()}


def daily_check():
    """Run the bundled archiver's --check: it GETs Pacer's report route for today."""
    cmd = [sys.executable, '-B', str(RELEASE / 'dry-run-daily.py'), '--check']
    out = run(cmd, check=False, timeout=120)
    return out.returncode == 0, jbody(out.stdout) or {'stderr': out.stderr.strip()[-200:]}


# ---------------------------------------------------------------- artifacts and stage state

def manifest():
    m = json.loads((RELEASE / 'manifest.json').read_text())
    if m.get('tag') != TAG or m.get('sources') != SOURCES:
        raise Refused('manifest tag/sources differ from this script')
    if not {'reports.sqlite', 'forward-book.json', 'forward-book.digest'} <= set(m['files']):
        raise Refused('bundle lacks prepared usage reports or forward book')
    bad = [name for name, sha in m['files'].items() if digest(RELEASE / name) != sha]
    bad += [name for name, sha in PINS.items() if m['files'].get(name) != sha]
    if bad:
        raise Refused('release artifacts do not match: ' + ', '.join(sorted(bad)))
    return m


def staged():
    path = BACKUP / 'stage.json'
    if not path.exists():
        raise Refused('run stage first')
    return json.loads(path.read_text())


TRACKED = {'config': CONFIG, 'compose': COMPOSE, 'snippet': SNIPPET, 'qh_lib': QH_LIB}


def backup_copy(name):
    return BACKUP / ('files/' + name)


def live_matches_backup(st):
    return {name: digest(path) == st['files'][name]['sha256'] if path.exists() else False for name, path in TRACKED.items()}


def baseline(include_inference=True):
    key = client_key()
    pl = plugins() or {}
    b = {'plugins': pl, 'registered': sorted(i for i, p in pl.items() if p['registered']), 'models': models(key)}
    if include_inference:  # first, so the usage view below has seen the current credential set
        b['inference'] = inference(key)
    b.update({'usage': usage(key), 'others': others(), 'discovery_public': discovery(PUBLIC_URL, LEGACY_EVENTS),
              'cpamp': container_status(CPAMP)})
    return b, key


# ---------------------------------------------------------------- phases

def recon(args):
    r = {'phase': 'recon', 'at': now(), 'python': sys.version.split()[0], 'pyyaml': yaml is not None,
         'settings': {'root': str(ROOT), 'release': str(RELEASE), 'container': CONTAINER, 'service': SERVICE,
                      'old_image': OLD_IMAGE, 'new_image': NEW_IMAGE, 'caddy_upstream': CADDY_UPSTREAM}}
    r['process'] = process()
    r['side'] = running_side()
    r['cpamp'] = container_status(CPAMP)
    r['new_image_present'] = image_id(NEW_IMAGE)
    files = {}
    for name, path in list(TRACKED.items()) + [('caddyfile', CADDYFILE), ('qh_state', QH_STATE)]:
        files[name] = {k: v for k, v in meta(path).items() if k != 'inode'} if path.exists() else None
    r['files'] = files
    r['plugin_libraries'] = sorted('%s %s' % (p.name, digest(p)[:16]) for p in LIBDIR.glob('*.so')) if LIBDIR.exists() else None
    r['pacer_dir_exists'] = PACER_DIR.exists()
    plan = {}
    for name, fn in (('config', lambda: edit_config(CONFIG.read_text(), 'dry-run')[1]),
                     ('compose', lambda: bool(edit_compose(COMPOSE.read_text()))),
                     ('snippet', lambda: edit_snippet(SNIPPET.read_text())[1])):
        try:
            plan[name] = {'applies': True, 'detail': fn()}
        except Exception as e:
            plan[name] = {'applies': False, 'reason': '%s: %s' % (type(e).__name__, e)}
    r['edit_plan'] = plan
    if yaml is not None and CONFIG.exists():
        cfg = yaml.safe_load(CONFIG.read_text()) or {}
        pl = cfg.get('plugins') or {}
        qh = (pl.get('configs') or {}).get('quota-handoff') or {}
        r['config_facts'] = {'plugins_enabled': pl.get('enabled'), 'plugins_dir': pl.get('dir'),
                             'plugin_configs': sorted((pl.get('configs') or {}).keys()),
                             'quota_handoff': {k: qh.get(k) for k in ('enabled', 'priority', 'compact_on_switch', 'discover_credentials')},
                             'quota_handoff_adaptive_mode': (qh.get('adaptive') or {}).get('mode'),
                             'routing': {k: v for k, v in (cfg.get('routing') or {}).items() if k in ('strategy', 'session-affinity', 'session-affinity-ttl')}}
        svc = ((yaml.safe_load(COMPOSE.read_text()) or {}).get('services') or {}).get(SERVICE) or {}
        r['compose_facts'] = {'image': svc.get('image'), 'user': svc.get('user'), 'container_name': svc.get('container_name'),
                              'plugins_mount': [v for v in svc.get('volumes') or [] if 'plugins' in str(v)]}
        r['layout'] = layout()
    r['caddy'] = {'snippet_imported': snippet_imported() if CADDYFILE.exists() else None,
                  'validate_current': caddy('validate')[0] if CADDYFILE.exists() else None,
                  'admin_upstreams': None}
    code, raw = http(CADDY_ADMIN.rstrip('/') + '/reverse_proxy/upstreams', timeout=5)
    if code == 200:
        r['caddy']['admin_upstreams'] = [{'address': u.get('address'), 'num_requests': u.get('num_requests')} for u in jbody(raw) or []]
    try:
        r['plugins'] = plugins()
        r['pacer_state_view'] = pacer_state_view() if (r['plugins'] or {}).get('pacer', {}).get('registered') else None
    except Exception as e:
        r['plugins_error'] = type(e).__name__
    if QH_STATE.exists():
        r['qh_state'] = sqlite('qh', QH_STATE, STALE_TICKET_SECONDS)
    r['release'] = {'present': (RELEASE / 'manifest.json').exists()}
    if r['release']['present']:
        try:
            r['release']['manifest_ok'] = bool(manifest())
        except Exception as e:
            r['release']['manifest_ok'] = False
            r['release']['reason'] = str(e)
    r['backup_exists'] = (BACKUP / 'stage.json').exists()
    r['systemd'] = {'available': shutil.which('systemctl') is not None,
                    'timer_installed': (UNIT_DIR / TIMER).exists()}
    r['idle_sample'] = sample()
    r['disk_free_gib'] = round(shutil.disk_usage(str(ROOT)).free / 2 ** 30, 1)
    print(json.dumps(r, indent=2, sort_keys=True, default=str))
    return EXIT_OK


def stage_reserve():
    # Free space already excludes retained reports/images. Reserve their bytes
    # only if they will be created, plus the same full report-sized growth margin.
    report_bytes = (RELEASE / 'reports.sqlite').stat().st_size
    return (report_bytes + (0 if PACER_REPORTS.exists() else report_bytes)
            + (0 if image_id(NEW_IMAGE) else (RELEASE / 'image/CLIProxyAPI').stat().st_size))


def stage(args):
    guard_rehearsal()
    require_yaml()
    if (BACKUP / 'stage.json').exists():
        raise Refused('stage already done; inspect %s instead of overwriting backups' % BACKUP)
    m = manifest()
    r = {'phase': 'stage', 'started': now(), 'routing_mode': args.routing_mode, 'manifest_files': m['files']}
    # Keep room for the staged report copy, usage-record growth and the CPA image.
    reserve = stage_reserve()
    if shutil.disk_usage(ROOT).free - reserve < 1.5 * 2**30:
        raise Refused('less than 1.5 GiB would remain after staging and usage-record reserve')
    p = process()
    if not p or p['image'] != OLD_IMAGE or p['status'] != 'running':
        raise Refused('incumbent CPA is not running %s' % OLD_IMAGE)
    pl = plugins() or {}
    qh = pl.get('quota-handoff') or {}
    if not (qh.get('registered') and qh.get('effective') and qh.get('version') == QH_VERSION):
        raise Refused('quota-handoff %s is not the registered incumbent' % QH_VERSION)
    if 'pacer' in pl or PACER_LIB.exists():
        raise Refused('a Pacer plugin is already present')
    if digest(QH_LIB) != PINS[QH_LIB.name]:
        raise Refused('live quota-handoff library differs from the pinned rollback artifact')
    if not snippet_imported():
        raise Refused('Caddyfile does not import the quota-handoff snippet')
    r['layout'] = layout()
    if not r['layout']['ok']:
        raise Refused('unexpected plugins layout: %s' % r['layout']['checks'])
    plans = {'config': edit_config(CONFIG.read_text(), args.routing_mode), 'compose': edit_compose(COMPOSE.read_text()),
             'snippet': edit_snippet(SNIPPET.read_text())}
    ok, err = caddy('validate')
    if not ok:
        raise Refused('current Caddy configuration does not validate: %s' % err)
    # Health first: a refusal here leaves no image, backup or state behind.
    r['baseline'], key = baseline()
    if r['baseline']['inference']['status'] != 200 or r['baseline']['usage']['status'] != 200 or not r['baseline']['usage']['ids']:
        raise Refused('incumbent baseline is unhealthy (inference %s, usage %s, %d credentials); not staging' % (
            r['baseline']['inference']['status'], r['baseline']['usage']['status'], len(r['baseline']['usage']['ids'])))
    # Candidate image: the incumbent image with only the CPA binary replaced.
    if image_id(NEW_IMAGE) is None:
        run(['docker', 'build', '--network', 'none', '--pull=false'] + platform_args() +
            ['--build-arg', 'BASE=' + OLD_IMAGE, '-t', NEW_IMAGE, str(RELEASE / 'image')], timeout=900)
    sums = run(['docker', 'run', '--rm', '--name', unique('imagecheck'), '--network', 'none'] + platform_args() +
               ['--entrypoint', 'sha256sum', NEW_IMAGE, '/CLIProxyAPI/CLIProxyAPI']).stdout.split()[0]
    if sums != PINS['image/CLIProxyAPI']:
        raise Refused('candidate image binary differs from the pinned CPA build')
    cfg_old, cfg_new = [json.loads(run(['docker', 'image', 'inspect', '--format', '{{json .Config}}', i]).stdout) for i in (OLD_IMAGE, NEW_IMAGE)]
    same = {k: cfg_old.get(k) == cfg_new.get(k) for k in ('Cmd', 'Entrypoint', 'WorkingDir', 'User', 'Env')}
    if not all(same.values()):
        raise Refused('candidate image runtime config differs: %s' % same)
    r['image'] = {'name': NEW_IMAGE, 'id': image_id(NEW_IMAGE), 'binary_sha256': sums, 'runtime_config_same': True}
    # Private backups: exact bytes plus ownership/mode/inode.
    BACKUP.mkdir(mode=0o700, parents=True, exist_ok=True)
    (BACKUP / 'files').mkdir(mode=0o700, exist_ok=True)
    files = {}
    for name, path in TRACKED.items():
        shutil.copyfile(path, backup_copy(name))
        backup_copy(name).chmod(0o600)
        files[name] = meta(path)
        if digest(backup_copy(name)) != files[name]['sha256']:
            raise RuntimeError('backup copy mismatch for ' + name)
    files['caddyfile'] = meta(CADDYFILE)
    qh_copy = BACKUP / 'files' / ('qh-state-%s.sqlite' % stamp())  # forensic copy; a retried stage never collides
    r['qh_state_backup'] = dict(sqlite('backup', QH_STATE, qh_copy, mounts=[BACKUP / 'files']), path=qh_copy.name) if QH_STATE.exists() else None
    # Pacer state: initialized once, owner-only, never recreated (docs/pacer-installation.md).
    uid, gid = cpa_user()
    if PACER_STATE.exists():
        r['pacer_state'] = {'initialized': 'kept existing'}
    else:
        PACER_DIR.mkdir(mode=0o700, exist_ok=True)
        chown_like(PACER_DIR, uid, gid)
        run(['docker', 'run', '--rm', '--name', unique('state-init'), '--network', 'none', '--cap-drop', 'ALL',
             '--security-opt', 'no-new-privileges', '--user', '%d:%d' % (uid, gid)] + platform_args() +
            ['-v', '%s:/state' % PACER_DIR, '-v', '%s:/pacer-state:ro' % (RELEASE / 'pacer-state'),
             '--entrypoint', '/pacer-state', NEW_IMAGE, 'init', '--state', '/state/state.sqlite', '--routes', 'claude'], timeout=300)
        r['pacer_state'] = {'initialized': now()}
    check = sqlite('check', PACER_STATE)
    if check.get('quick_check') != 'ok' or 'cache_policy_meta' not in check.get('tables', []):
        raise Refused('Pacer state did not initialize correctly: %s' % check.get('quick_check'))
    r['pacer_state'].update({'quick_check': 'ok', 'tables': len(check['tables']), 'mode': oct(PACER_STATE.stat().st_mode & 0o777),
                             'dir_mode': oct(PACER_DIR.stat().st_mode & 0o777)})
    # Only the prepared artifact is needed by usage_live; the multi-GB history
    # archive stays offline. Never replace an existing artifact with other bytes.
    if PACER_REPORTS.exists():
        if digest(PACER_REPORTS) != m['files']['reports.sqlite']:
            raise Refused('existing reports artifact differs; refusing overwrite')
    else:
        with PACER_REPORTS.open('xb') as dst, (RELEASE / 'reports.sqlite').open('rb') as src:
            shutil.copyfileobj(src, dst)
        # OpenPrepared reports are immutable: historyreports.Open refuses owner
        # write permission too. The separate usage record is the writable store.
        PACER_REPORTS.chmod(0o400)
        chown_like(PACER_REPORTS, uid, gid)
    if digest(PACER_REPORTS) != m['files']['reports.sqlite'] or sqlite('check', PACER_REPORTS).get('quick_check') != 'ok':
        raise Refused('prepared reports artifact failed integrity check')
    if PACER_REPORTS.stat().st_mode & 0o277:
        raise Refused('prepared reports artifact must be private and read-only')
    initialized = PACER_DIR / 'forward-init.json'
    if PACER_FORWARD.exists():
        if not initialized.exists():
            raise Refused('existing forward store has no successful initialization receipt')
        result = json.loads(initialized.read_text())
    else:
        result = json.loads(run(['docker', 'run', '--rm', '--name', unique('forward-init'), '--network', 'none',
            '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges', '--user', '%d:%d' % (uid, gid)] + platform_args() +
            ['-v', '%s:/state' % PACER_DIR, '-v', '%s:/pacer-state:ro' % (RELEASE / 'pacer-state'),
             '-v', '%s:/forward-book.json:ro' % (RELEASE / 'forward-book.json'), '--entrypoint', '/pacer-state', NEW_IMAGE,
             'forward-init', '--output', '/state/forward-report.sqlite', '--book-json', '/forward-book.json'], timeout=120).stdout)
        save(initialized, result)
    if result.get('forward_report_book') != forward_book_digest() or sqlite('check', PACER_FORWARD).get('quick_check') != 'ok':
        raise Refused('forward store digest or integrity differs from the bundle')
    r['usage_record'] = {'reports_sha256': digest(PACER_REPORTS), 'forward_book': forward_book_digest(), 'history_archive': False}
    r['expected'] = {'config': sha_text(plans['config'][0]), 'compose': sha_text(plans['compose']),
                     'snippet': sha_text(plans['snippet'][0]), 'config_plan': plans['config'][1],
                     'snippet_plan': plans['snippet'][1]}
    r['files'] = files
    r['cpa_user'] = '%d:%d' % (uid, gid)
    r['finished'] = now()
    save(BACKUP / 'stage.json', r)
    save(receipt_path('stage'), r)
    print(json.dumps({k: r[k] for k in ('phase', 'image', 'pacer_state', 'expected', 'routing_mode')}, indent=2, sort_keys=True))
    return EXIT_OK


def lock():
    RELEASE.mkdir(parents=True, exist_ok=True)
    f = open(RELEASE / '.phase1.lock', 'w')
    try:
        fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError:
        raise Refused('another phase1 run holds the lock')
    return f


def verify_candidate(st, pre, since, inference_session=None):
    v, c = {'side': 'candidate'}, {}
    p = process()
    v['process'] = p
    c['image'] = bool(p) and p['image'] == NEW_IMAGE and p['image_id'] == st['image']['id']
    c['health'] = bool(wait_until(health, 90))
    pl = wait_until(lambda: (lambda x: x if x and (x.get('pacer') or {}).get('effective') else None)(plugins()), 60) or plugins() or {}
    v['plugins'] = pl
    pc = pl.get('pacer') or {}
    c['pacer_registered'] = pc.get('registered') and pc.get('effective') and pc.get('version') == PACER_VERSION
    c['quota_handoff_inactive'] = not (pl.get('quota-handoff') or {}).get('effective')
    c['plugins_preserved'] = set(pre['registered']) - {'quota-handoff', 'pacer'} <= {i for i, x in pl.items() if x['registered']}
    key = client_key()
    v['models'] = wait_models(key, pre['models'])
    c['models_same'] = v['models']['status'] == 200 and v['models']['sha256'] == pre['models']['sha256']
    v['auth'] = {'models_anonymous': http(CPA_URL + '/v1/models')[0], 'management_anonymous': http(CPA_URL + '/v0/management/plugins')[0]}
    c['auth_enforced'] = v['auth']['models_anonymous'] in (401, 403) and v['auth']['management_anonymous'] == 401
    v['discovery_direct'] = discovery(CPA_URL, PACER_EVENTS)
    c['discovery_direct'] = v['discovery_direct']['status'] == 200 and v['discovery_direct']['pacer']
    v['discovery_public_legacy'] = discovery(PUBLIC_URL, LEGACY_EVENTS)
    c['discovery_public_legacy'] = v['discovery_public_legacy']['status'] == 200 and v['discovery_public_legacy']['pacer']
    want = set(pre['usage']['ids'])
    v['usage'] = wait_until(lambda: (lambda u: u if u['status'] == 200 and want <= set(u['ids']) else None)(usage(key)), 60, 2) or usage(key)
    c['usage_view_public'] = v['usage']['status'] == 200 and want <= set(v['usage']['ids'])
    def usage_record():
        code, raw = http(CPA_URL + '/v0/management/plugins/pacer/usage/profiles', mgmt_key(), {}, 'POST', timeout=60)
        body = jbody(raw) or {}
        return {'status': code, 'live': body.get('live'), 'profiles': len(body.get('profiles') or [])}
    v['usage_record'] = wait_until(lambda: (lambda u: u if u['status'] == 200 and (u['live'] or {}).get('state') == 'live' else None)(usage_record()), 120, 2) or usage_record()
    c['usage_record_live'] = v['usage_record']['status'] == 200 and (v['usage_record']['live'] or {}).get('state') == 'live' and v['usage_record']['profiles'] > 0
    before = pacer_state_view()
    v['inference'] = inference(key, inference_session)
    c['inference_public'] = v['inference']['status'] == 200
    mode = st['routing_mode']
    if mode == 'dry-run' and inference_session:
        # Under a plugin scheduler CPA reports host affinity as unsupported, so Pacer traces a
        # proposal only once the chat has a binding: the second request of the same session.
        v['inference_followup'] = inference(key, inference_session)
    v['pacer_state'] = after = pacer_state_view()
    c['pacer_state'] = after['status'] == 200 and after['mode'] == mode and after['decision_write_errors'] == 0
    if mode == 'dry-run' and inference_session:
        c['dry_run_trace'] = v['inference_followup']['status'] == 200 and (
            after['decisions'] > before['decisions'] or after['decisions'] == 200)
    c['config_on_disk'] = digest(CONFIG) == st['expected']['config']
    c['files'] = (digest(COMPOSE) == st['expected']['compose'] and digest(SNIPPET) == st['expected']['snippet']
                  and digest(PACER_LIB) == PINS['pacer-v0.2.0-dev.so'])
    v['fatal_lines'] = fatal_lines(since)
    c['logs_clean'] = not v['fatal_lines']
    v['others'] = others()
    c['others_unchanged'] = v['others'] == pre['others']
    c['cpamp_unchanged'] = container_status(CPAMP) == pre['cpamp']
    ok, v['daily_check'] = daily_check()
    c['daily_report_check'] = (ok and v['daily_check'].get('status') == 200
                               and 'day_in_progress' in (v['daily_check'].get('reasons') or []))
    if SYSTEMD:
        c['timer_enabled'] = run(['systemctl', 'is-enabled', TIMER], check=False).stdout.strip() == 'enabled'
    return finish_checks(v, c)


def verify_incumbent(st, pre, since):
    v, c = {'side': 'incumbent'}, {}
    p = process()
    v['process'] = p
    c['image'] = bool(p) and p['image'] == OLD_IMAGE
    c['health'] = bool(wait_until(health, 90))
    pl = wait_until(lambda: (lambda x: x if x and (x.get('quota-handoff') or {}).get('effective') else None)(plugins()), 60) or plugins() or {}
    v['plugins'] = pl
    qh = pl.get('quota-handoff') or {}
    c['quota_handoff_registered'] = qh.get('registered') and qh.get('effective') and qh.get('version') == QH_VERSION
    c['pacer_absent'] = not (pl.get('pacer') or {}).get('registered') and not PACER_LIB.exists()
    c['plugins_preserved'] = set(pre['registered']) <= {i for i, x in pl.items() if x['registered']}
    key = client_key()
    v['models'] = models(key)
    c['models_same'] = v['models']['status'] == 200 and v['models']['sha256'] == pre['models']['sha256']
    v['auth'] = {'models_anonymous': http(CPA_URL + '/v1/models')[0], 'management_anonymous': http(CPA_URL + '/v0/management/plugins')[0]}
    c['auth_enforced'] = v['auth']['models_anonymous'] in (401, 403) and v['auth']['management_anonymous'] == 401
    v['discovery_public_legacy'] = discovery(PUBLIC_URL, LEGACY_EVENTS)
    c['discovery_public_legacy'] = (v['discovery_public_legacy']['status'] == 200 and not v['discovery_public_legacy']['pacer']
                                    and v['discovery_public_legacy']['protocol'] == pre['discovery_public']['protocol'])
    want = set(pre['usage']['ids'])
    v['usage'] = wait_until(lambda: (lambda u: u if u['status'] == 200 and want <= set(u['ids']) else None)(usage(key)), 60, 2) or usage(key)
    c['usage_view_public'] = v['usage']['status'] == 200 and want <= set(v['usage']['ids'])
    v['inference'] = inference(key)
    c['inference_public'] = v['inference']['status'] == 200
    v['files'] = {}
    for name, path in TRACKED.items():
        m, b = meta(path), st['files'][name]
        v['files'][name] = {k: m[k] == b[k] for k in ('sha256', 'mode', 'uid', 'gid', 'inode')}
    c['files_restored_byte_for_byte'] = all(all(x.values()) for x in v['files'].values())
    v['fatal_lines'] = fatal_lines(since)
    c['logs_clean'] = not v['fatal_lines']
    if 'others' in pre:
        v['others'] = others()
        c['others_unchanged'] = v['others'] == pre['others']
    c['timer_absent'] = not (UNIT_DIR / TIMER).exists() and not (UNIT_DIR / UNIT_SERVICE).exists() if SYSTEMD else True
    return finish_checks(v, c)


def finish_checks(v, c):
    c = {k: bool(x) for k, x in c.items()}
    if INJECT and INJECT in c and v.get('side') == 'candidate':  # never fake a failed rollback
        c[INJECT] = False
        v['injected_failure'] = INJECT
    v['checks'] = c
    v['failed'] = sorted(k for k, x in c.items() if not x)
    v['ok'] = not v['failed']
    return v


def apply_candidate(st, r):
    steps = r.setdefault('steps', [])

    def step(name, **extra):
        steps.append(dict(extra, step=name, at=now()))
        save(r['_path'], r)
        if INJECT == 'step:' + name:
            raise RuntimeError('injected failure after step ' + name)

    m = manifest()
    texts = {'config': edit_config(backup_copy('config').read_text(), st['routing_mode'])[0],
             'compose': edit_compose(backup_copy('compose').read_text()),
             'snippet': edit_snippet(backup_copy('snippet').read_text())[0]}
    for name, text in texts.items():
        if sha_text(text) != st['expected'][name]:
            raise Refused('%s edit no longer matches the staged plan' % name)
    r['stopped_at'] = now()
    compose('stop', '-t', '30', SERVICE)
    step('cpa_stopped')
    write_in_place(CONFIG, texts['config'])
    write_in_place(COMPOSE, texts['compose'])
    compose('config', '-q')
    step('config_and_compose_written', config=digest(CONFIG), compose=digest(COMPOSE))
    uid, gid = cpa_user()
    tmp = LIBDIR / ('.' + PACER_LIB.name + '.tmp')
    shutil.copyfile(RELEASE / 'pacer-v0.2.0-dev.so', tmp)
    chown_like(tmp, LIBDIR.stat().st_uid, LIBDIR.stat().st_gid)
    tmp.chmod(0o755)
    os.replace(tmp, PACER_LIB)
    if digest(PACER_LIB) != m['files']['pacer-v0.2.0-dev.so']:
        raise RuntimeError('installed Pacer library digest mismatch')
    step('pacer_library_installed')
    r['recreated_at'] = now()
    compose('up', '-d', '--no-deps', '--pull', 'never', SERVICE)
    if not wait_until(health, 90):
        raise RuntimeError('candidate CPA is not healthy')
    if not wait_until(lambda: ((plugins() or {}).get('pacer') or {}).get('effective'), 60):
        raise RuntimeError('Pacer did not register')
    step('candidate_running', process=process())
    write_in_place(SNIPPET, texts['snippet'])
    ok, err = caddy('validate')
    if not ok:
        write_in_place(SNIPPET, backup_copy('snippet').read_bytes())
        raise RuntimeError('candidate Caddy snippet does not validate: %s' % err)
    ok, err = caddy('reload')
    if not ok:
        raise RuntimeError('Caddy reload failed: %s' % err)
    step('caddy_reloaded')
    REPORT_DIR.mkdir(mode=0o700, parents=True, exist_ok=True)
    if SYSTEMD:
        for name, text in units().items():
            if text != (RELEASE / 'systemd' / name).read_text():
                raise RuntimeError('rendered unit %s differs from the reviewed bundle copy' % name)
            (UNIT_DIR / name).write_text(text)
            (UNIT_DIR / name).chmod(0o644)
        run(['systemctl', 'daemon-reload'])
        run(['systemctl', 'enable', '--now', TIMER])
        step('timer_enabled', next=run(['systemctl', 'list-timers', TIMER, '--no-pager'], check=False).stdout.strip().splitlines()[1:2])
    else:
        step('timer_skipped', reason='PHASE1_SYSTEMD=0')


def restore_incumbent(st, r):
    """Idempotent: brings every tracked file back to its staged bytes from any partial state."""
    steps = r.setdefault('steps', [])

    def step(name, **extra):
        steps.append(dict(extra, step=name, at=now()))
        save(r['_path'], r)

    if SYSTEMD and ((UNIT_DIR / TIMER).exists() or (UNIT_DIR / UNIT_SERVICE).exists()):
        run(['systemctl', 'disable', '--now', TIMER], check=False)
        for name in (TIMER, UNIT_SERVICE):
            if (UNIT_DIR / name).exists():
                (UNIT_DIR / name).unlink()
        run(['systemctl', 'daemon-reload'])
        step('timer_removed')
    p = process()
    needs_restart = (not p or p['image'] != OLD_IMAGE or p['status'] != 'running' or PACER_LIB.exists()
                     or digest(CONFIG) != st['files']['config']['sha256'] or digest(COMPOSE) != st['files']['compose']['sha256'])
    if needs_restart:
        r['stopped_at'] = now()
        if p:
            compose('stop', '-t', '30', SERVICE)
        step('cpa_stopped')
    for name in ('config', 'compose', 'qh_lib'):
        path = TRACKED[name]
        if not path.exists() or digest(path) != st['files'][name]['sha256']:
            if path.exists():
                write_in_place(path, backup_copy(name).read_bytes())
            else:
                shutil.copyfile(backup_copy(name), path)
                path.chmod(int(st['files'][name]['mode'], 8))
                chown_like(path, st['files'][name]['uid'], st['files'][name]['gid'])
            step('restored', file=name)
    if PACER_LIB.exists():
        if digest(PACER_LIB) == PINS['pacer-v0.2.0-dev.so']:
            PACER_LIB.unlink()
        else:
            (BACKUP / 'unexpected').mkdir(mode=0o700, exist_ok=True)
            shutil.move(str(PACER_LIB), str(BACKUP / 'unexpected' / (PACER_LIB.name + '.' + stamp())))
        step('pacer_library_removed')
    if needs_restart:
        r['recreated_at'] = now()
        compose('up', '-d', '--no-deps', '--pull', 'never', SERVICE)
        if not wait_until(health, 90):
            raise RuntimeError('incumbent CPA is not healthy after rollback')
        if not wait_until(lambda: ((plugins() or {}).get('quota-handoff') or {}).get('effective'), 60):
            raise RuntimeError('quota-handoff did not register after rollback')
        step('incumbent_running', process=process())
    if digest(SNIPPET) != st['files']['snippet']['sha256']:
        write_in_place(SNIPPET, backup_copy('snippet').read_bytes())
        ok, err = caddy('validate')
        if not ok:
            raise RuntimeError('restored Caddy snippet does not validate: %s' % err)
        ok, err = caddy('reload')
        if not ok:
            raise RuntimeError('Caddy reload failed during rollback: %s' % err)
        step('caddy_restored')
    r.setdefault('recreated_at', r.get('started'))


def rollback_flow(st, r, reason, wait, pre):
    rb = {'reason': reason, 'started': now()}
    r['rollback'] = rb
    gate = idle_gate(wait)
    rb['idle_gate'] = {'ok': gate['ok'], 'sampled': gate['sampled'], 'last': gate['samples'][-1]}
    if not gate['ok']:
        rb['result'] = 'deferred: not idle'
        return False
    sub = {'_path': r['_path'], 'started': rb['started']}
    rb['steps'] = sub.setdefault('steps', [])
    try:
        restore_incumbent(st, sub)
        rb['verify'] = verify_incumbent(st, pre, sub.get('recreated_at') or rb['started'])
        rb['result'] = 'rolled back' if rb['verify']['ok'] else 'rolled back with failed checks'
        return rb['verify']['ok']
    except Exception:
        rb['error'] = traceback.format_exc()[-1200:]
        rb['result'] = 'rollback error'
        return False


def cutover(args):
    guard_rehearsal()
    if not args.i_have_user_approval:
        raise Refused('cutover restarts production CPA; pass --i-have-user-approval only after the user approved this window')
    held = lock()
    st = staged()
    manifest()
    r = {'phase': 'cutover', 'started': now(), 'routing_mode': st['routing_mode'], 'injected': INJECT or None}
    r['_path'] = receipt_path('cutover')
    save(r['_path'], r)
    try:
        drift = {k: v for k, v in live_matches_backup(st).items() if not v}
        if drift:
            raise Refused('live files changed since stage: %s' % sorted(drift))
        if running_side() != 'incumbent' or PACER_LIB.exists():
            raise Refused('incumbent is not the expected running quota-handoff CPA')
        if image_id(NEW_IMAGE) != st['image']['id']:
            raise Refused('candidate image changed since stage')
        r['idle_gate'] = gate = idle_gate(args.idle_wait)
        if not gate['ok']:
            r['result'] = 'not idle; nothing changed'
            return EXIT_NOT_IDLE
        r['pre'], _ = baseline()
        if r['pre']['inference']['status'] != 200 or r['pre']['usage']['status'] != 200:
            raise Refused('incumbent pre-check failed; nothing changed')
        gate2 = idle_gate(args.idle_wait)
        r['idle_gate_final'] = {'ok': gate2['ok'], 'sampled': gate2['sampled'], 'last': gate2['samples'][-1]}
        if not gate2['ok']:
            r['result'] = 'not idle after pre-check; nothing changed'
            return EXIT_NOT_IDLE
        try:
            apply_candidate(st, r)
            r['verify'] = verify_candidate(st, r['pre'], r['recreated_at'], 'pacer-phase1-smoke-' + stamp())
            if not r['verify']['ok']:
                raise RuntimeError('verification failed: %s' % r['verify']['failed'])
        except Exception as error:
            r['error'] = '%s: %s' % (type(error).__name__, str(error)[:400])
            r['traceback'] = traceback.format_exc()[-1200:]
            # Failure is not permission to interrupt new in-flight requests.
            # A busy rollback returns exit 5 for the operator's idle-gated retry.
            ok = rollback_flow(st, r, 'automatic: ' + r['error'], args.rollback_idle_wait, r['pre'])
            r['result'] = 'rolled back' if ok else 'ROLLBACK INCOMPLETE'
            return EXIT_ROLLED_BACK if ok else EXIT_ROLLBACK_FAILED
        r['result'] = 'deployed'
        r['cutover_day'] = r['started'][:10]
        return EXIT_OK
    except Refused as e:
        r['result'] = 'refused: %s' % e
        return EXIT_REFUSED
    finally:
        r['finished'] = now()
        path = r.pop('_path')
        save(path, r)
        summary(r, path)
        held.close()


def rollback(args):
    guard_rehearsal()
    if args.force:
        raise Refused('forced rollback is prohibited; wait for confirmed idleness')
    held = lock()
    st = staged()
    r = {'phase': 'rollback', 'started': now()}
    r['_path'] = receipt_path('rollback')
    try:
        pre = st['baseline']
        r['side_before'] = running_side()
        ok = rollback_flow(st, r, 'operator', args.idle_wait, pre)
        if r['rollback'].get('result', '').startswith('deferred'):
            r['result'] = 'not idle; nothing changed'
            return EXIT_NOT_IDLE
        r['result'] = 'rolled back' if ok else 'ROLLBACK INCOMPLETE'
        return EXIT_OK if ok else EXIT_ROLLBACK_FAILED
    finally:
        r['finished'] = now()
        path = r.pop('_path')
        save(path, r)
        summary(r, path)
        held.close()


def verify(args):
    guard_rehearsal()
    st = staged()
    side = running_side()
    pre = dict(st['baseline'])
    pre.pop('others', None)  # other services may legitimately change between windows
    since = (process() or {}).get('started') or '10m'
    if side == 'candidate':
        pre['others'] = others()
        v = verify_candidate(st, pre, since, 'pacer-phase1-verify-' + stamp())
    elif side == 'incumbent':
        v = verify_incumbent(st, pre, since)
    else:
        v = {'side': side, 'ok': False, 'failed': ['running_side'], 'checks': {}}
    r = {'phase': 'verify', 'at': now(), 'verify': v, 'result': 'ok' if v['ok'] else 'failed'}
    path = receipt_path('verify')
    save(path, r)
    summary(r, path)
    return EXIT_OK if v['ok'] else EXIT_VERIFY


def summary(r, path):
    out = {'phase': r['phase'], 'result': r.get('result'), 'receipt': str(path)}
    for key in ('verify',):
        if key in r:
            out['side'] = r[key].get('side')
            out['failed'] = r[key].get('failed')
    if 'rollback' in r:
        rb = r['rollback']
        out['rollback'] = {'result': rb.get('result'), 'failed': (rb.get('verify') or {}).get('failed'),
                           'idle_gate_ok': rb.get('idle_gate', {}).get('ok')}
    for key in ('idle_gate', 'idle_gate_final'):
        if key in r:
            out[key] = {'ok': r[key]['ok'], 'sampled': r[key]['sampled']}
    if 'error' in r:
        out['error'] = r['error']
    print(json.dumps(out, indent=2, sort_keys=True), flush=True)


def bundle(args):
    here = Path(__file__).resolve().parent
    out = Path(args.out)
    if out.exists() and any(out.iterdir()):
        raise Refused('bundle output directory is not empty')
    (out / 'image').mkdir(parents=True, exist_ok=True)
    (out / 'systemd').mkdir(exist_ok=True)
    sources = {'image/CLIProxyAPI': Path(args.cpa_binary), 'pacer-v0.2.0-dev.so': Path(args.pacer_dist) / 'pacer-v0.2.0-dev.so',
                'pacer-state': Path(args.pacer_dist) / 'pacer-state', QH_LIB.name: Path(args.qh_lib),
                'reports.sqlite': Path(args.reports), 'forward-book.json': Path(args.book_json),
                'dry-run-daily.py': here / 'dry-run-daily.py', 'phase1.py': here / 'phase1.py'}
    if not re.fullmatch(r'[a-f0-9]{64}', args.book_digest):
        raise Refused('invalid forward book digest')
    for name, src in sources.items():
        shutil.copyfile(src, out / name)
        (out / name).chmod(0o755 if name in ('image/CLIProxyAPI', 'pacer-state') else 0o600)
    (out / 'forward-book.digest').write_text(args.book_digest + '\n')
    (out / 'image' / 'Dockerfile').write_text(DOCKERFILE)
    global RELEASE
    RELEASE = Path(args.release_path)
    for name, text in units().items():
        (out / 'systemd' / name).write_text(text)
    files = {}
    for path in sorted(p for p in out.rglob('*') if p.is_file()):
        files[str(path.relative_to(out))] = digest(path)
    bad = [n for n, sha in PINS.items() if files.get(n) != sha]
    if bad:
        raise Refused('local artifacts differ from pins: %s' % bad)
    m = {'tag': TAG, 'sources': SOURCES, 'files': files, 'base_image': OLD_IMAGE, 'candidate_image': NEW_IMAGE}
    (out / 'manifest.json').write_text(json.dumps(m, indent=2, sort_keys=True) + '\n')
    print(json.dumps({'bundle': str(out), 'files': len(files), 'manifest_sha256': digest(out / 'manifest.json')}, indent=2))
    return EXIT_OK


def main():
    ap = argparse.ArgumentParser(prog='phase1.py', description=__doc__.split('\n')[0])
    sub = ap.add_subparsers(dest='phase', required=True)
    sub.add_parser('recon')
    s = sub.add_parser('stage')
    s.add_argument('--routing-mode', choices=('off', 'dry-run'), default='dry-run')
    c = sub.add_parser('cutover')
    c.add_argument('--i-have-user-approval', action='store_true')
    c.add_argument('--idle-wait', type=int, default=600)
    c.add_argument('--rollback-idle-wait', type=int, default=120)
    sub.add_parser('verify')
    r = sub.add_parser('rollback')
    r.add_argument('--idle-wait', type=int, default=600)
    r.add_argument('--force', action='store_true', help='legacy flag; always refused (rollback requires idleness)')
    b = sub.add_parser('bundle')
    b.add_argument('--out', required=True)
    b.add_argument('--release-path', default=str(RELEASE))
    b.add_argument('--cpa-binary', required=True)
    b.add_argument('--pacer-dist', required=True)
    b.add_argument('--qh-lib', required=True)
    b.add_argument('--reports', required=True)
    b.add_argument('--book-json', required=True)
    b.add_argument('--book-digest', required=True)
    args = ap.parse_args()
    os.umask(0o077)
    try:
        return {'recon': recon, 'stage': stage, 'cutover': cutover, 'verify': verify, 'rollback': rollback,
                'bundle': bundle}[args.phase](args)
    except Refused as e:
        print(json.dumps({'phase': args.phase, 'result': 'refused', 'reason': str(e)}), flush=True)
        return EXIT_REFUSED


if __name__ == '__main__':
    sys.exit(main())

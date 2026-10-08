"""Pacer Phase 2 dry-run daily report archiver. Stdlib only; fetches Pacer's
authoritative daily report route over the management API and stores it verbatim.

  dry-run-daily.py --out DIR            archive every complete UTC day that has no
                                        report yet, from the newest existing archive
                                        (else the cutover day, else 31 days back) to
                                        yesterday (timer)
  dry-run-daily.py --out DIR --day D    (re)write the report for UTC day D
  dry-run-daily.py --out DIR --latest   today's report as pacer-dry-run-latest.{json,md}
  dry-run-daily.py --check              GET today's report; print status/reasons; write nothing

The report itself is computed by Pacer and is the authority (mode-mark proof,
counterfactual simulation, projection): POST nothing, read the stored evidence and
write the response body verbatim. This script never reads Pacer's state database.

Auth uses the same management key file and PHASE1_* overrides as phase1.py:
PHASE1_CADDY_DIR (key file directory) and PHASE1_CPA_URL. The key is never printed,
logged or written.

Exit codes: 0 ok, 1 error (nothing written), 2 refused / bad request (nothing written).
"""

import argparse
import datetime
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

ROUTE = '/v0/management/plugins/pacer/dry-run-report'
UTC = datetime.timezone.utc
MAX_ATTEMPTS = 5

SCRIPT_DIR = Path(__file__).resolve().parent
CADDY_DIR = Path(os.environ.get('PHASE1_CADDY_DIR', '/etc/caddy'))
MGMT_KEY_FILE = CADDY_DIR / 'cliproxyapi-management-auth.Caddyfile'
CPA_URL = os.environ.get('PHASE1_CPA_URL', 'http://127.0.0.1:8317')

OPENER = urllib.request.build_opener(
    urllib.request.ProxyHandler({}),
    type('NoRedirect', (urllib.request.HTTPRedirectHandler,), {'redirect_request': lambda *a, **k: None}))


def today():
    return datetime.datetime.now(UTC).strftime('%Y-%m-%d')


def shift(day, days):
    d = datetime.datetime.strptime(day, '%Y-%m-%d').replace(tzinfo=UTC)
    return (d + datetime.timedelta(days=days)).strftime('%Y-%m-%d')


def valid_day(text):
    try:
        return datetime.datetime.strptime(text, '%Y-%m-%d').strftime('%Y-%m-%d') == text
    except (TypeError, ValueError):
        return False


def mgmt_key():
    try:
        text = MGMT_KEY_FILE.read_text()
    except OSError as e:
        raise SystemExit(json.dumps({'ok': False, 'error': 'management key file unreadable', 'file': str(MGMT_KEY_FILE)}, sort_keys=True))
    m = re.search(r'(?i)Bearer\s+([^"\s]+)', text)
    if not m:
        raise SystemExit(json.dumps({'ok': False, 'error': 'management key not found in ' + str(MGMT_KEY_FILE)}, sort_keys=True))
    return m.group(1)


def get(day, key):
    """GET one day's report. Returns (status, body, reasons, error)."""
    url = '%s%s?day=%s' % (CPA_URL.rstrip('/'), ROUTE, day)
    req = urllib.request.Request(url, headers={'Authorization': 'Bearer ' + key, 'Accept': 'application/json'})
    for attempt in range(MAX_ATTEMPTS):
        try:
            resp = OPENER.open(req, timeout=60)
        except urllib.error.HTTPError as e:
            with e:
                status, body, headers = e.status, e.read(), dict(e.headers)
        except (urllib.error.URLError, OSError):
            return 0, b'', None, 'connection_failed'
        else:
            with resp:
                status, body, headers = resp.status, resp.read(), dict(resp.headers)
        if status != 429 or attempt == MAX_ATTEMPTS - 1:
            return status, body, error_code(status, body), None
        try:
            wait = float(headers.get('Retry-After', '1'))
        except (TypeError, ValueError):
            wait = 1.0
        time.sleep(max(0.0, min(wait, 30.0)))
    return 429, b'', 'too_many_requests', None


def jbody(raw):
    try:
        return json.loads(raw)
    except (TypeError, ValueError):
        return None


def error_code(status, body):
    body = jbody(body) or {}
    code = body.get('error')
    if isinstance(code, str) and code:
        return code
    if status == 200:
        return None
    return 'connection_failed' if status == 0 else 'http_%d' % status


def write_atomic(out, stem, suffix, data):
    out.mkdir(mode=0o700, parents=True, exist_ok=True)
    path = out / (stem + suffix)
    tmp = out / ('.' + stem + suffix + '.tmp')
    fd = os.open(str(tmp), os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o640)
    try:
        os.write(fd, data)
        os.fsync(fd)
    finally:
        os.close(fd)
    os.chmod(str(tmp), 0o640)
    os.replace(str(tmp), str(path))
    return str(path)


def summary(report):
    """A short human summary of one route response. Echoes only what the JSON holds."""
    meta = report.get('meta') or {}
    reasons = meta.get('reasons') or []
    lines = ['# Pacer dry-run report %s' % (meta.get('day') or '?'), '',
             '- Complete: %s%s.' % (bool(meta.get('complete')),
                                    ' (reasons: %s)' % ', '.join(reasons) if reasons else ''),
             '- Decisions: %s over %s chats; unattributed: %s.' % (
                 meta.get('decisions', 0), meta.get('chats', 0), meta.get('unattributed_decisions', 0)),
             '- Would-be moves: %s.' % ((report.get('would_be_moves') or {}).get('total', 0)),
             '- Flip-flops in %sh: %s.' % ((report.get('flip_flops') or {}).get('window_hours', 24),
                                           (report.get('flip_flops') or {}).get('total', 0)), '']
    placements = report.get('placements') or []
    if placements:
        lines += ['## Placements', '',
                  '| provider | model | decisions | chats | agree | differ | unhandled |',
                  '|---|---|---|---|---|---|---|']
        lines += ['| %s | %s | %s | %s | %s | %s | %s |' % (
            p.get('provider'), p.get('model'), p.get('decisions'), p.get('chats'),
            p.get('agree'), p.get('differ'), p.get('unhandled')) for p in placements]
        lines += ['']
    resets = report.get('resets') or []
    known = [x for x in resets if (x.get('projection') or {}).get('known')]
    unknown = [x for x in resets if not (x.get('projection') or {}).get('known')]
    lines += ['## Resets (%d)' % len(resets), '',
              '- projection known: %d; unknown: %d.' % (len(known), len(unknown))]
    reasons_by_count = {}
    for x in unknown:
        for reason in ((x.get('projection') or {}).get('reasons') or []):
            reasons_by_count[reason] = reasons_by_count.get(reason, 0) + 1
    if reasons_by_count:
        lines += ['- unknown reasons: ' + ', '.join('%s=%d' % (k, reasons_by_count[k]) for k in sorted(reasons_by_count))]
    if resets:
        lines += ['', '| provider | account | reset_at | projection |', '|---|---|---|---|']
        for x in resets:
            p = x.get('projection') or {}
            note = 'known' if p.get('known') else 'unknown' + (':' + ','.join(p.get('reasons') or []) if p.get('reasons') else '')
            lines += ['| %s | %s | %s | %s |' % (x.get('provider'), x.get('account'), x.get('reset_at'), note)]
    lines += ['']
    return '\n'.join(lines)


def fetch(out, stem, day, key):
    status, body, code, err = get(day, key)
    if status == 0:
        print(json.dumps({'ok': False, 'day': day, 'status': status, 'error': err or 'connection_failed'}, sort_keys=True))
        return 1
    if status == 400:
        print(json.dumps({'ok': False, 'day': day, 'status': status, 'error': code}, sort_keys=True))
        return 2
    if status != 200:
        print(json.dumps({'ok': False, 'day': day, 'status': status, 'error': code}, sort_keys=True))
        return 1
    report = jbody(body)
    if report is None:
        print(json.dumps({'ok': False, 'day': day, 'status': status, 'error': 'invalid_json'}, sort_keys=True))
        return 1
    written = [write_atomic(out, stem, '.json', body),
               write_atomic(out, stem, '.md', summary(report).encode())]
    print(json.dumps({'ok': True, 'day': day, 'written': written}, sort_keys=True))
    return 0


def existing_days(out):
    days = set()
    if out.is_dir():
        for path in out.glob('pacer-dry-run-*.json'):
            name = path.name[len('pacer-dry-run-'):-len('.json')]
            if valid_day(name):
                days.add(name)
    return days


def cutover_day():
    """The day routing changed, as recorded in phase1's newest deployed cutover receipt."""
    receipts = SCRIPT_DIR / 'receipts'
    if not receipts.is_dir():
        return None
    days = []
    for path in sorted(receipts.glob('cutover-*.json')):
        try:
            r = json.loads(path.read_text())
        except (OSError, ValueError):
            continue
        d = r.get('cutover_day')
        if isinstance(d, str) and valid_day(d):
            days.append(d)
    return max(days) if days else None


def first_day(out):
    have = existing_days(out)
    start = max(have) if have else (cutover_day() or shift(today(), -31))
    oldest = shift(today(), -31)
    return max(start, oldest)


def main():
    ap = argparse.ArgumentParser(description=__doc__.split('\n')[0])
    ap.add_argument('--out', help='report directory')
    mode = ap.add_mutually_exclusive_group()
    mode.add_argument('--day', help='UTC day YYYY-MM-DD (rewrites an existing report)')
    mode.add_argument('--latest', action='store_true', help='today\'s report as pacer-dry-run-latest.*')
    mode.add_argument('--check', action='store_true', help='GET today only; print status/reasons; write nothing')
    args = ap.parse_args()
    os.umask(0o027)
    key = mgmt_key()
    if args.check:
        status, body, _, _ = get(today(), key)
        result = {'status': status, 'error': error_code(status, body),
                  'reasons': (jbody(body).get('meta') or {}).get('reasons') if status == 200 else None}
        print(json.dumps(result, sort_keys=True))
        return 0 if status == 200 else 1
    if not args.out:
        ap.error('--out is required unless --check')
    out = Path(args.out)
    if args.latest:
        return fetch(out, 'pacer-dry-run-latest', today(), key)
    if args.day:
        if not valid_day(args.day):
            print(json.dumps({'ok': False, 'day': args.day, 'status': 400, 'error': 'invalid_day'}, sort_keys=True))
            return 2
        return fetch(out, 'pacer-dry-run-' + args.day, args.day, key)
    day, last = first_day(out), shift(today(), -1)
    while day <= last:
        if not (out / ('pacer-dry-run-' + day + '.json')).exists():
            rc = fetch(out, 'pacer-dry-run-' + day, day, key)
            if rc != 0:
                return rc
        day = shift(day, 1)
    return 0


if __name__ == '__main__':
    sys.exit(main())

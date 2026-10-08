"""Pacer Phase 2 dry-run daily report. Reads Pacer state read-only; stdlib only.

  dry-run-daily.py --state STATE --out DIR            one report per complete UTC day that has
                                                      dry-run decisions and no report yet (timer)
  dry-run-daily.py --state STATE --out DIR --day D    (re)write the report for UTC day D
  dry-run-daily.py --state STATE --out DIR --latest   rolling last-24h snapshot (pacer-dry-run-latest.*)
  dry-run-daily.py --state STATE --check              schema/mode probe as JSON; writes nothing

Each report covers placements (proposed account and reason per routed request), agreement with
the account CPA actually used, would-be moves, flip-flops (a chat proposed back to an account it
left less than 24 h earlier), and capacity: the remaining % observed just before each reset during
the day plus, for open windows, the burn rate and projected unused % at reset. Dry-run does not
change placement, so capacity figures describe the serving (incumbent fill-first) routing; Pacer's
counterfactual is not simulated here.

Account and chat identifiers are shown only as sha256 labels (first 12 hex characters).
"""
import argparse
import datetime
import hashlib
import json
import os
import sqlite3
import statistics
import sys
from collections import Counter, defaultdict
from pathlib import Path

VERSION = 1
DAY = 86400
FLIP_WINDOW = DAY
NS = 10 ** 9
UTC = datetime.timezone.utc


def label(value):
    return hashlib.sha256(str(value).encode()).hexdigest()[:12] if value else None


def iso(seconds):
    return datetime.datetime.fromtimestamp(seconds, UTC).isoformat(timespec='seconds').replace('+00:00', 'Z')


def parse_time(text):
    if not text or text.startswith('0001-01-01'):
        return None
    text = text.replace('Z', '+00:00')
    if '.' in text:  # Go writes up to nanoseconds; Python parses up to microseconds
        head, _, tail = text.partition('.')
        n = len(tail) - len(tail.lstrip('0123456789'))
        text = head + '.' + tail[:n][:6].ljust(6, '0') + tail[n:]
    return datetime.datetime.fromisoformat(text).timestamp()


def connect(path):
    if not Path(path).exists():
        raise SystemExit(json.dumps({'ok': False, 'error': 'state not found'}))
    db = sqlite3.connect('file:%s?mode=ro' % path, uri=True, timeout=10)
    db.execute('PRAGMA busy_timeout=10000')
    return db


def tables(db):
    return {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'")}


def routing_mode(db, names):
    if 'pacer_routing_settings' not in names:
        return None
    row = db.execute('SELECT effective FROM pacer_routing_settings WHERE id=1').fetchone()
    return row[0] if row else None


def decisions(db, start, end):
    out, bad = [], 0
    for at, body in db.execute('SELECT at, body FROM pacer_decisions WHERE at >= ? AND at < ? ORDER BY at, seq',
                               (int(start * NS), int(end * NS))):
        try:
            d = json.loads(body)
            p = d.get('proposed') or {}
            out.append({'at': at / NS, 'chat': d.get('chat') or '', 'provider': d.get('provider') or '',
                        'model': d.get('model') or '', 'previous': d.get('previous_account') or '',
                        'auth': d.get('actual_auth') or '', 'outcome': d.get('outcome') or '',
                        'handled': bool(p.get('handled')), 'p_auth': p.get('auth') or '', 'p_account': p.get('account') or '',
                        'reason': p.get('reason') or '', 'forced': bool(p.get('forced')), 'pace': p.get('pace')})
        except (TypeError, ValueError):
            bad += 1
    return out, bad


def flip_flops(seq, start):
    """seq: per chat [(at, account)]. Counts returns to an account left < 24 h earlier, inside the day."""
    count, chats, examples = 0, set(), []
    for chat, items in seq.items():
        left = {}  # account -> time the chat moved away from it
        current = None
        for at, account in items:
            if not account or account == current:
                continue
            if current is not None:
                left[current] = at
            if account in left and at - left[account] < FLIP_WINDOW and at >= start:
                count += 1
                chats.add(chat)
                if len(examples) < 10:
                    examples.append({'chat': label(chat), 'account': label(account), 'at': iso(at),
                                     'hours_since_left': round((at - left[account]) / 3600, 2)})
            current = account
    return {'count': count, 'chats': len(chats), 'examples': examples}


def placements(rows, history, start):
    auth_account = {}
    for r in history:
        if r['p_auth'] and r['p_account']:
            auth_account[r['p_auth']] = r['p_account']
    by_reason, by_outcome, by_account, unhandled = Counter(), Counter(), Counter(), Counter()
    agree = Counter()
    moves, move_reasons, move_chats, actual_moves = 0, Counter(), set(), 0
    pace = defaultdict(list)
    for r in rows:
        by_reason[r['reason']] += 1
        by_outcome[r['outcome']] += 1
        actual_account = auth_account.get(r['auth'])
        if not r['handled']:
            unhandled[r['reason']] += 1
            agree['no_proposal'] += 1
        else:
            by_account[label(r['p_account'])] += 1
            if isinstance(r['pace'], (int, float)):
                pace[label(r['p_account'])].append(r['pace'])
            if r['p_auth'] == r['auth']:
                agree['same_auth'] += 1
            elif actual_account and actual_account == r['p_account']:
                agree['same_account'] += 1
            elif actual_account:
                agree['different_account'] += 1
            else:
                agree['actual_account_unknown'] += 1
            if r['previous'] and r['p_account'] != r['previous']:
                moves += 1
                move_reasons[r['reason']] += 1
                move_chats.add(r['chat'])
        if r['previous'] and actual_account and actual_account != r['previous']:
            actual_moves += 1
    handled = sum(1 for r in rows if r['handled'])
    proposed_seq, actual_seq = defaultdict(list), defaultdict(list)
    for r in history:
        if r['handled']:
            proposed_seq[r['chat']].append((r['at'], r['p_account']))
        actual_seq[r['chat']].append((r['at'], auth_account.get(r['auth'])))
    return {
        'placements': {'decisions': len(rows), 'handled': handled, 'chats': len({r['chat'] for r in rows}),
                       'by_reason': dict(sorted(by_reason.items())), 'by_outcome': dict(sorted(by_outcome.items())),
                       'by_proposed_account': dict(sorted(by_account.items())), 'unhandled': dict(sorted(unhandled.items())),
                       'pace': {k: {'n': len(v), 'median': round(statistics.median(v), 4), 'min': round(min(v), 4),
                                    'max': round(max(v), 4)} for k, v in sorted(pace.items())}},
        'agreement': dict(sorted(agree.items()), rate=round((agree['same_auth'] + agree['same_account']) / handled, 4) if handled else None),
        'would_be_moves': {'total': moves, 'chats': len(move_chats), 'by_reason': dict(sorted(move_reasons.items())),
                           'actual_moves': actual_moves},
        'flip_flops': {'window_hours': FLIP_WINDOW // 3600, 'proposed': flip_flops(proposed_seq, start),
                       'actual': flip_flops(actual_seq, start)},
    }


def capacity(db, names, start, end):
    if 'observation_history' not in names:
        return {'resets': [], 'open_windows': [], 'samples': 0}
    cycles, bad, samples = defaultdict(list), 0, 0
    for account, key, received, raw, disposition in db.execute(
            'SELECT account, key, received_at, sample, disposition FROM observation_history '
            'WHERE received_at >= ? AND received_at < ? ORDER BY id', (int((start - 8 * DAY) * NS), int(end * NS))):
        if not disposition.startswith('accepted'):
            continue
        try:
            s = json.loads(raw)
            remaining, reset = s.get('remaining'), parse_time(s.get('reset'))
            seen = parse_time((s.get('usage') or {}).get('at')) or received / NS
        except (TypeError, ValueError):
            bad += 1
            continue
        if remaining is None or reset is None:
            continue
        samples += 1
        cycles[(account, key, round(reset / 60))].append((seen, float(remaining), reset))
    resets, open_windows = [], []
    for (account, key, _), points in sorted(cycles.items()):
        points.sort()
        reset = points[-1][2]
        before = [p for p in points if p[0] <= reset]
        if not before:
            continue
        if start <= reset < end:
            seen, remaining, _ = before[-1]
            resets.append({'account': label(account), 'window': key, 'reset': iso(reset), 'unused_pct': round(remaining, 2),
                           'last_seen_before_reset_s': int(reset - seen)})
        elif reset >= end:
            recent = [p for p in before if p[0] >= end - DAY] or before[-1:]
            first, last = recent[0], recent[-1]
            hours = (last[0] - first[0]) / 3600
            burn = max(0.0, (first[1] - last[1]) / hours) if hours > 0 else None
            to_reset = (reset - last[0]) / 3600
            projected = None if burn is None else round(min(100.0, max(0.0, last[1] - burn * to_reset)), 2)
            open_windows.append({'account': label(account), 'window': key, 'reset': iso(reset),
                                 'remaining_pct': round(last[1], 2), 'as_of': iso(last[0]), 'samples': len(recent),
                                 'burn_pct_per_hour': None if burn is None else round(burn, 3),
                                 'projected_unused_pct': projected})
    return {'resets': resets, 'open_windows': open_windows, 'samples': samples, 'parse_errors': bad}


def build(db, start, end, kind):
    names = tables(db)
    if 'pacer_decisions' not in names:
        raise SystemExit(json.dumps({'ok': False, 'error': 'pacer_decisions table missing'}))
    rows, bad = decisions(db, start, end)
    history, _ = decisions(db, start - FLIP_WINDOW, end)
    first = db.execute('SELECT min(at) FROM pacer_decisions').fetchone()[0]
    r = {'report': 'pacer-dry-run', 'version': VERSION, 'kind': kind, 'generated_at': iso(datetime.datetime.now(UTC).timestamp()),
         'window': {'from': iso(start), 'to': iso(end)}, 'routing_mode': routing_mode(db, names),
         'coverage': {'first_decision_ever': iso(first / NS) if first else None,
                      'first_decision': iso(rows[0]['at']) if rows else None, 'last_decision': iso(rows[-1]['at']) if rows else None,
                      'partial': bool(first is None or first / NS > start)},
         'integrity': {'decision_parse_errors': bad}}
    r.update(placements(rows, history, start))
    r['capacity'] = capacity(db, names, start, end)
    r['notes'] = ['Dry-run does not change placement: capacity figures describe the serving routing.',
                  'The first request of a new chat is not traced (CPA reports host affinity as unsupported under a '
                  'plugin scheduler, so Pacer proposes only once a chat has a binding); new_chat counts are a lower bound.',
                  'Labels are sha256(identifier)[:12].']
    return r


def markdown(r):
    p, a, m, f, c = r['placements'], r['agreement'], r['would_be_moves'], r['flip_flops'], r['capacity']
    lines = ['# Pacer dry-run report %s (%s)' % (r.get('day') or r['window']['to'], r['kind']), '',
             '- Window: %s to %s; routing mode `%s`%s.' % (r['window']['from'], r['window']['to'], r['routing_mode'],
                                                          '; partial coverage' if r['coverage']['partial'] else ''),
             '- Placements: %d decisions (%d with a proposal) over %d chats; reasons %s.' % (
                 p['decisions'], p['handled'], p['chats'], json.dumps(p['by_reason'], sort_keys=True)),
             '- Agreement with the served account: %s (%s).' % (a.get('rate'), json.dumps({k: v for k, v in a.items() if k != 'rate'}, sort_keys=True)),
             '- Would-be moves: %d over %d chats %s; actual moves by serving routing: %d.' % (
                 m['total'], m['chats'], json.dumps(m['by_reason'], sort_keys=True), m['actual_moves']),
             '- Flip-flops (< %d h): proposed %d (%d chats); actual %d.' % (
                 f['window_hours'], f['proposed']['count'], f['proposed']['chats'], f['actual']['count']), '']
    if c['resets']:
        lines += ['| account | window | reset | unused % at reset | last seen before reset |', '|---|---|---|---|---|']
        lines += ['| %s | %s | %s | %s | %ss |' % (x['account'], x['window'], x['reset'], x['unused_pct'], x['last_seen_before_reset_s'])
                  for x in c['resets']] + ['']
    if c['open_windows']:
        lines += ['| account | window | reset | remaining % | burn %/h | projected unused % |', '|---|---|---|---|---|---|']
        lines += ['| %s | %s | %s | %s | %s | %s |' % (x['account'], x['window'], x['reset'], x['remaining_pct'],
                                                       x['burn_pct_per_hour'], x['projected_unused_pct']) for x in c['open_windows']] + ['']
    return '\n'.join(lines)


def write(out, stem, report):
    out.mkdir(mode=0o700, parents=True, exist_ok=True)
    paths = []
    for suffix, text in (('.json', json.dumps(report, indent=2, sort_keys=True) + '\n'), ('.md', markdown(report) + '\n')):
        path = out / (stem + suffix)
        tmp = out / ('.' + stem + suffix + '.tmp')
        tmp.write_text(text)
        tmp.chmod(0o600)
        os.replace(tmp, path)
        paths.append(str(path))
    return paths


def day_start(text):
    return datetime.datetime.strptime(text, '%Y-%m-%d').replace(tzinfo=UTC).timestamp()


def check(db):
    names = tables(db)
    now = datetime.datetime.now(UTC).timestamp()
    out = {'ok': 'pacer_decisions' in names, 'routing_mode': routing_mode(db, names),
           'quick_check': db.execute('PRAGMA quick_check').fetchone()[0],
           'tables': {t: t in names for t in ('pacer_decisions', 'pacer_routing_settings', 'observation_history')}}
    if out['ok']:
        r = build(db, now - DAY, now, 'check')
        out.update(decisions_24h=r['placements']['decisions'], observation_samples=r['capacity']['samples'],
                   decision_parse_errors=r['integrity']['decision_parse_errors'])
    out['ok'] = out['ok'] and out['quick_check'] == 'ok'
    return out


def main():
    ap = argparse.ArgumentParser(description=__doc__.split('\n')[0])
    ap.add_argument('--state', required=True)
    ap.add_argument('--out')
    mode = ap.add_mutually_exclusive_group()
    mode.add_argument('--day', help='UTC day YYYY-MM-DD (rewrites an existing report)')
    mode.add_argument('--latest', action='store_true')
    mode.add_argument('--check', action='store_true')
    args = ap.parse_args()
    os.umask(0o077)
    db = connect(args.state)
    if args.check:
        out = check(db)
        print(json.dumps(out, sort_keys=True))
        return 0 if out['ok'] else 1
    if not args.out:
        ap.error('--out is required unless --check')
    out = Path(args.out)
    now = datetime.datetime.now(UTC).timestamp()
    written = []
    if args.latest:
        written += write(out, 'pacer-dry-run-latest', build(db, now - DAY, now, 'rolling-24h'))
    else:
        if args.day:
            days = [day_start(args.day)]
        else:
            first = db.execute('SELECT min(at) FROM pacer_decisions').fetchone()[0] if 'pacer_decisions' in tables(db) else None
            today = day_start(iso(now)[:10])
            days = []
            if first:
                d = day_start(iso(first / NS)[:10])
                while d < today:
                    if not (out / ('pacer-dry-run-%s.json' % iso(d)[:10])).exists():
                        days.append(d)
                    d += DAY
        for d in days:
            if d + DAY > now and not args.day:
                continue
            r = build(db, d, d + DAY, 'daily')
            r['day'] = iso(d)[:10]
            written += write(out, 'pacer-dry-run-%s' % r['day'], r)
    print(json.dumps({'ok': True, 'written': written}, sort_keys=True))
    return 0


if __name__ == '__main__':
    sys.exit(main())

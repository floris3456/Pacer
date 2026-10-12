#!/usr/bin/env python3
"""Native CPA/Pacer quota safety acceptance against a nonforwarding local fixture.

Usage: native-quota-safety.py CPA_BINARY PACER_SO PACER_STATE_BINARY
Only dummy credentials are used. Evidence remains in a printed /tmp directory.
Managed cache policy and real client compaction are outside this fixture's scope.
"""
import base64
import hashlib
import importlib.util
import json
import os
import pathlib
import shutil
import socket
import sqlite3
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request

SOURCE = pathlib.Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('native', SOURCE / 'tests/lab/native-managed-client.py')
native = importlib.util.module_from_spec(spec)
spec.loader.exec_module(native)
TOKENS = native.PROVIDERS
CALLERS = ['quota-main-dummy', 'quota-a-dummy', 'quota-b-dummy', 'quota-c-dummy']
IDS = ['quota-account-a.json', 'quota-account-b.json', 'quota-account-c.json']
MODEL = native.MODEL
WEEK_RESET = '2099-01-08T00:00:00Z'
SHORT_RESET = '2099-01-01T00:00:00Z'


def check(value, message):
    if not value:
        raise AssertionError(message)


class Handler(native.FixtureHandler):
    def do_CONNECT(self):
        with self.server.lock:
            self.server.tunnels.append(self.path)
        return super().do_CONNECT()

    def do_GET(self):
        if self.path == '/api/oauth/usage':
            key = self.headers.get('Authorization', '').removeprefix('Bearer ')
            check(key in TOKENS, 'non-dummy provider key')
            index = TOKENS.index(key)
            with self.server.lock:
                left = self.server.left[index]
                overage = self.server.overage[index]
                self.server.meters.append(index)
            return self.reply(200, {'five_hour': {'utilization': 20, 'resets_at': SHORT_RESET},
                                    'seven_day': {'utilization': 100-left, 'resets_at': WEEK_RESET},
                                    'extra_usage': {'is_enabled': overage}})
        return self.reply(403, {'error': 'external_network_disabled'})

    def do_POST(self):
        check(self.path.split('?', 1)[0] == '/v1/messages', 'unexpected provider POST')
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        key = self.headers.get('X-Api-Key') or self.headers.get('Authorization', '').removeprefix('Bearer ')
        check(key in TOKENS, 'non-dummy provider credential')
        index = TOKENS.index(key)
        marker = body['messages'][-1]['content']
        if isinstance(marker, list):
            marker = marker[0]['text']
        with self.server.lock:
            if marker == 'seed-a-empty':
                self.server.left[0] = 0
            elif marker == 'seed-b-empty':
                self.server.left[1] = 0
            elif marker == 'deplete-c':
                self.server.left[2] = 0
                self.server.overage[2] = False
            elif marker == 'enable-c-overage':
                self.server.left[2] = 0
                self.server.overage[2] = True
            left = self.server.left[index]
            overage = self.server.overage[index]
            number = len(self.server.attempts)
            self.server.attempts.append({'account': index, 'marker': marker, 'body': body,
                'weekly_left': left, 'overage_enabled': overage,
                'coordination_leaked': any(k.lower().startswith(('x-pacer-', 'x-quota-')) for k in self.headers)})
        answer = {'id': f'msg_quota_{number}', 'type': 'message', 'role': 'assistant', 'model': MODEL,
                  'content': [{'type': 'text', 'text': f'Local answer from account {index}'}],
                  'stop_reason': 'end_turn', 'stop_sequence': None,
                  'usage': {'input_tokens': 10, 'output_tokens': 5,
                            'cache_read_input_tokens': 0, 'cache_creation_input_tokens': 0}}
        data = json.dumps(answer).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(data)))
        self.send_header('anthropic-ratelimit-unified-overage-status', 'allowed' if overage else 'rejected')
        if not overage:
            self.send_header('anthropic-ratelimit-unified-overage-disabled-reason', 'member_zero_credit_limit')
        self.send_header('anthropic-ratelimit-unified-5h-utilization', '0.2')
        self.send_header('anthropic-ratelimit-unified-5h-reset', SHORT_RESET)
        self.send_header('anthropic-ratelimit-unified-7d-utilization', str((100-left)/100))
        self.send_header('anthropic-ratelimit-unified-7d-reset', WEEK_RESET)
        self.end_headers()
        self.wfile.write(data)


def main():
    if len(sys.argv) != 4:
        raise SystemExit(__doc__)
    binary, plugin, initializer = (pathlib.Path(value).resolve() for value in sys.argv[1:])
    check(all(path.is_file() for path in (binary, plugin, initializer)), 'missing native artifact')
    ROOT = pathlib.Path(tempfile.mkdtemp(prefix='pacer-native-quota-safety-'))
    print(f'Native quota safety evidence: {ROOT}', flush=True)
    work = ROOT / 'fixture'
    work.mkdir()
    (work / 'plugins').mkdir()
    (work / 'auths').mkdir()
    shutil.copyfile(plugin, work / 'plugins/pacer.so')
    env = {'PATH': os.environ.get('PATH', os.defpath), 'TZ': 'UTC',
           'NO_PROXY': '127.0.0.1,localhost', 'XDG_CONFIG_HOME': str(work / 'config'),
           'XDG_CACHE_HOME': str(work / 'cache')}
    subprocess.run([str(initializer), 'init', '--state', str(work / 'pacer.sqlite'), '--routes', native.ROUTE],
                   check=True, timeout=20, cwd=work, env=env, stdout=subprocess.DEVNULL)
    with (work / 'cert.log').open('w') as log:
        subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1',
            '-subj', '/CN=api.anthropic.com', '-addext', 'subjectAltName=DNS:api.anthropic.com',
            '-keyout', str(work / 'fixture.key'), '-out', str(work / 'fixture.crt')],
            check=True, timeout=15, stdout=log, stderr=subprocess.STDOUT, env=env)
    tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    tls.load_cert_chain(work / 'fixture.crt', work / 'fixture.key')
    native.FixtureHandler = Handler
    fixture = native.Fixture(tls)
    fixture.left = [80, 80, 50]
    fixture.overage = [False, False, False]
    fixture.tunnels, fixture.meters = [], []
    threading.Thread(target=fixture.serve_forever, daemon=True).start()
    env['SSL_CERT_FILE'] = str(work / 'fixture.crt')
    for name in ('HTTP_PROXY', 'HTTPS_PROXY', 'ALL_PROXY', 'http_proxy', 'https_proxy', 'all_proxy'):
        env[name] = fixture.base
    with socket.socket() as reservation:
        reservation.bind(('127.0.0.1', 0))
        port = reservation.getsockname()[1]
    base = f'http://127.0.0.1:{port}'
    for i, token in enumerate(TOKENS):
        (work / 'auths' / IDS[i]).write_text(json.dumps({'type': 'claude', 'access_token': token,
            'expired': '2099-01-01T00:00:00Z', 'account_uuid': f'00000000-0000-0000-0000-{i+1:012d}',
            'email': f'quota-{i}@example.invalid', 'proxy_url': fixture.base,
            'claude_device_ids': [hashlib.sha256(token.encode()).hexdigest()], 'cloak_mode': 'never'})+'\n')
    grants = [{'caller_scope': native.caller_scope(CALLERS[0]), 'auth_ids': IDS}]
    grants += [{'caller_scope': native.caller_scope(caller), 'auth_ids': [IDS[i]]} for i, caller in enumerate(CALLERS[1:])]
    config = {'config-version': 8, 'server': {'host': '127.0.0.1', 'port': port},
        'management': {'disable-control-panel': True}, 'oauth': {'auth-dir': str(work / 'auths')},
        'access': {'api-keys': CALLERS}, 'routing': {'strategy': 'fill-first', 'session-affinity': True,
            'retry': {'request-retry': 0}, 'cooldown': {'disable-cooling': True}},
        'plugins': {'enabled': True, 'dir': str(work / 'plugins'), 'required-interceptors': ['pacer'],
            'configs': {'pacer': {'enabled': True, 'state_path': str(work / 'pacer.sqlite'),
                'compact_on_switch': True, 'routing': {'mode': 'dry-run'},
                'usage_auth_url': base+'/v1/models', 'client_access': {'grants': grants},
                'cache_policy': {'enabled': False}}}}}
    (work / 'config.yaml').write_text(json.dumps(config, indent=2)+'\n')
    artifacts = [{'path': str(path), 'sha256': native.file_sha256(path)} for path in (binary, plugin, initializer)]
    source = {name: native.file_sha256(SOURCE/name) for name in (
        'internal/pacer/quota_safety.go', 'internal/pacer/quota_safety_test.go',
        'internal/pacer/plugin.go', 'internal/pacer/observation.go', 'internal/pacer/measurement.go',
        'internal/observation/credits.go', 'internal/observation/claude_extra.go',
        'tests/lab/native-quota-safety.py', 'tests/lab/native-managed-client.py')}
    (ROOT/'artifacts.json').write_text(json.dumps({'native': artifacts, 'sources': source}, indent=2)+'\n')
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    events, checks = [], []
    process = None

    def request(marker, caller=CALLERS[0], checkpoint='checkpoint-old', claim=True):
        headers = {'Content-Type': 'application/json', 'Authorization': 'Bearer '+caller,
                   'X-Claude-Code-Session-Id': 'quota-main' if caller == CALLERS[0] else caller}
        if claim:
            value = {'protocol': 1, 'installation': 'native-quota-safety', 'session': 'quota-main',
                'route': native.ROUTE, 'request': marker, 'kind': 'primary',
                'capability': hashlib.sha256(b'dummy-private-capability').hexdigest(),
                'checkpoint': checkpoint, 'auto': True, 'fresh': False, 'compact_on_switch': True}
            headers['X-Quota-Handoff'] = base64.urlsafe_b64encode(json.dumps(value).encode()).rstrip(b'=').decode()
        body = {'model': MODEL, 'max_tokens': 32, 'stream': False,
                'messages': [{'role': 'user', 'content': marker}]}
        req = urllib.request.Request(base+'/v1/messages', data=json.dumps(body).encode(), headers=headers)
        try:
            response = opener.open(req, timeout=15)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            raw = response.read()
            answer = json.loads(raw)
            status = response.status
            h = dict(response.headers)
        events.append({'marker': marker, 'status': status, 'headers': h, 'body': answer, 'provider_count': len(fixture.attempts)})
        (ROOT/'http-events.json').write_text(json.dumps(events, indent=2)+'\n')
        return status, h, answer

    def snapshot():
        with sqlite3.connect((work/'pacer.sqlite').as_uri()+'?mode=ro', uri=True) as db:
            windows = [{'account': row[0], **json.loads(row[1])} for row in db.execute('SELECT account,body FROM observation_windows')]
            chats = [json.loads(row[0]) for row in db.execute('SELECT body FROM pacer_chats')]
        with sqlite3.connect((work/'pacer.sqlite.credits.sqlite').as_uri()+'?mode=ro', uri=True) as db:
            extra = [dict(zip(('account', 'state', 'observed', 'source'), row))
                     for row in db.execute('SELECT account,state,observed,source FROM observation_credits')]
        return {'windows': windows, 'chats': chats, 'extra_usage': extra}

    def check_quota(left):
        until = time.monotonic()+8
        while True:
            state = snapshot()
            actual = sorted(w['remaining'] for w in state['windows'] if w['seconds'] == 604800)
            if actual == sorted(left):
                return state
            check(time.monotonic() < until, f'quota observation missing: {actual}; expected {left}')
            threading.Event().wait(0.2)

    def passed(name):
        checks.append(name)
        print('PASS '+name, flush=True)

    log = (ROOT/'server.log').open('w')
    try:
        process = subprocess.Popen([str(binary), '--config', str(work/'config.yaml'), '--local-model'],
                                   cwd=work, env=env, stdout=log, stderr=subprocess.STDOUT)
        deadline = time.monotonic()+20
        while True:
            check(process.poll() is None, 'CPA exited at startup')
            try:
                req = urllib.request.Request(base+'/v1/models', headers={'Authorization': 'Bearer '+CALLERS[0]})
                with opener.open(req, timeout=1) as response:
                    if any(m['id'] == MODEL for m in json.load(response)['data']):
                        break
            except (OSError, ValueError, KeyError):
                pass
            check(time.monotonic() < deadline, 'CPA not ready')
            threading.Event().wait(0.2)
        status, _, body = request('initial-a')
        check(status == 200 and fixture.attempts[-1]['account'] == 0, f'initial binding failed: {status} {body}')
        passed('initial-primary-binds-account-a-through-native-host')
        for i, marker in enumerate(('seed-a-empty', 'seed-b-empty', 'seed-c-usable')):
            status, _, body = request(marker, caller=CALLERS[i+1], claim=False)
            check(status == 200 and fixture.attempts[-1]['account'] == i, f'quota seed failed: {status} {body}')
        state = check_quota([0, 0, 50])
        (ROOT/'quota-before-switch.json').write_text(json.dumps(state, indent=2)+'\n')
        check(len(state['extra_usage']) == 3 and all(e['state'] == 'empty' and e['source'] == 'header' for e in state['extra_usage']), 'missing explicit extra-usage evidence')
        passed('native-response-observation-has-weekly-a0-b0-c50-and-disabled-overage')
        before = len(fixture.attempts)
        status, _, body = request('switch-required')
        check(status == 409 and body['error']['code'] == 'quota_compact_required', f'expected forced compaction: {status} {body}')
        check(len(fixture.attempts) == before, 'switch refusal reached provider')
        passed('empty-current-and-next-skip-to-usable-third-before-dispatch')
        status, _, body = request('resume-c', checkpoint='checkpoint-portable-new')
        check(status == 200 and fixture.attempts[-1]['account'] == 2, f'portable resume failed: {status} {body}')
        passed('committed-portable-checkpoint-resumes-on-account-c')
        for marker in ('healthy-affinity-one', 'healthy-affinity-two'):
            status, _, body = request(marker, checkpoint='checkpoint-portable-new')
            check(status == 200 and fixture.attempts[-1]['account'] == 2, f'healthy affinity changed: {status} {body}')
        passed('healthy-same-account-affinity-remains-on-c')
        for marker in ('enable-c-overage', 'overage-continues'):
            status, _, body = request(marker, checkpoint='checkpoint-portable-new')
            check(status == 200 and fixture.attempts[-1]['account'] == 2, f'extra capacity blocked: {status} {body}')
        extra_state = check_quota([0, 0, 0])
        check(sum(e['state'] == 'available' for e in extra_state['extra_usage']) == 1, 'allowed overage was not persisted')
        (ROOT/'quota-overage-available.json').write_text(json.dumps(extra_state, indent=2)+'\n')
        passed('weekly-empty-account-remains-usable-with-explicit-overage-allowed')
        status, _, body = request('deplete-c', checkpoint='checkpoint-portable-new')
        check(status == 200 and fixture.attempts[-1]['account'] == 2, f'depletion capture failed: {status} {body}')
        state = check_quota([0, 0, 0])
        (ROOT/'quota-all-empty.json').write_text(json.dumps(state, indent=2)+'\n')
        binding = next(c for c in state['chats'] if c['caller_scope'] == native.caller_scope(CALLERS[0]))
        before = len(fixture.attempts)
        for marker in ('all-empty', 'all-empty-repeat'):
            status, headers, body = request(marker, checkpoint='checkpoint-portable-new')
            check(status == 429 and body['error']['code'] == 'quota_exhausted', f'wrong all-empty result: {status} {body}')
            check({k.lower(): v for k, v in headers.items()}.get('x-should-retry') == 'false', 'retry guard lost')
            check(len(fixture.attempts) == before, 'empty destination reached provider')
        current = next(c for c in snapshot()['chats'] if c['caller_scope'] == native.caller_scope(CALLERS[0]))
        check(current == binding, 'all-empty refusal mutated binding/checkpoint')
        passed('all-empty429-is-nonretryable-before-provider-or-compaction-and-preserves-binding')
        check(not any(a['coordination_leaked'] for a in fixture.attempts), 'coordination headers leaked')
        check(all(a['account'] in (0, 1, 2) for a in fixture.attempts), 'foreign account dispatched')
        check(native.file_sha256(binary) == artifacts[0]['sha256'], 'host binary changed during fixture')
        result = {'status': 'passed', 'checks': checks, 'real_provider_calls': 0,
                  'fake_provider_calls': len(fixture.attempts), 'artifacts': artifacts,
                  'limitations': ['Managed cache policy disabled; portable checkpoint committed synthetically by harness; no real OpenCode process.'],
                  'host_unchanged': True, 'routing_mode': 'dry-run', 'private_optimizer_enabled': False}
        (ROOT/'result.json').write_text(json.dumps(result, indent=2)+'\n')
        print(json.dumps(result), flush=True)
    finally:
        if process is not None:
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        log.close()
        (ROOT/'provider-attempts.json').write_text(json.dumps(fixture.attempts, indent=2)+'\n')
        (ROOT/'network-evidence.json').write_text(json.dumps({'tunnels': fixture.tunnels, 'meter_reads': fixture.meters,
            'fixture': fixture.base, 'forwards_external_sockets': False}, indent=2)+'\n')
        fixture.shutdown()
        fixture.server_close()


if __name__ == '__main__':
    main()

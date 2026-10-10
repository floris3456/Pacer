"""Focused bundle/config regressions; no live services or provider requests."""
import importlib.util
import json
import pathlib
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from types import SimpleNamespace
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('phase1', pathlib.Path(__file__).with_name('phase1.py'))
p = importlib.util.module_from_spec(spec)
spec.loader.exec_module(p)


class UsageBundle(unittest.TestCase):
    def test_catalogue_readiness_requires_exact_incumbent_set(self):
        expected = {'sha256': p.sha_text('alias')}
        cases = [
            ('startup settles', [(503, []), (200, ['alias', 'original']), (200, ['alias'])], True),
            ('persistent extra alias', [(200, ['alias', 'original'])], False),
            ('missing model', [(200, [])], False),
            ('alias substitution', [(200, ['original'])], False),
        ]
        for name, replies, accepted in cases:
            with self.subTest(name=name):
                calls = []

                class Handler(BaseHTTPRequestHandler):
                    def do_GET(self):
                        calls.append(self.path)
                        status, ids = replies[min(len(calls) - 1, len(replies) - 1)]
                        self.send_response(status)
                        self.end_headers()
                        self.wfile.write(json.dumps({'data': [{'id': x} for x in ids]}).encode())

                    def log_message(self, *args):
                        pass

                server = HTTPServer(('127.0.0.1', 0), Handler)
                worker = threading.Thread(target=server.serve_forever, daemon=True)
                worker.start()
                try:
                    with patch.object(p, 'CPA_URL', 'http://127.0.0.1:%d' % server.server_port):
                        got = p.wait_models('synthetic', expected, seconds=.1, interval=.001)
                    self.assertEqual(got['status'] == 200 and got['sha256'] == expected['sha256'], accepted)
                    self.assertTrue(all(path == '/v1/models' for path in calls))
                    if accepted:
                        self.assertEqual(got['ids'], ['alias'])
                        self.assertEqual(len(got['startup_observations']), 3)
                finally:
                    server.shutdown()
                    worker.join(5)
                    server.server_close()

    def test_rollback_never_overrides_busy_gate(self):
        r = {}
        with patch.object(p, 'idle_gate', return_value={'ok': False, 'sampled': 1, 'samples': [{'quiet': False}]}), patch.object(p, 'restore_incumbent') as restore:
            self.assertFalse(p.rollback_flow({}, r, 'automatic failure', 1, {}))
            restore.assert_not_called()
            self.assertEqual(r['rollback']['result'], 'deferred: not idle')
        with patch.object(p, 'lock') as lock:
            with self.assertRaises(p.Refused):
                p.rollback(SimpleNamespace(force=True))
            lock.assert_not_called()

    def test_config_paths_and_book_are_exact(self):
        with tempfile.TemporaryDirectory() as tmp, patch.object(p, 'RELEASE', pathlib.Path(tmp)):
            (p.RELEASE / 'forward-book.digest').write_text('a' * 64 + '\n')
            for step in (2, 4):
                text = 'plugins:\n' + ' ' * step + 'configs:\n' + ' ' * (2*step) + 'quota-handoff:\n' + ' ' * (3*step) + 'enabled: true\n'
                edited, _ = p.edit_config(text, 'dry-run')
                cfg = p.yaml.safe_load(edited)['plugins']['configs']
                self.assertEqual(cfg['pacer'], p.pacer_config('dry-run'))
                self.assertFalse(cfg['quota-handoff']['enabled'])
                self.assertNotIn('history_path', cfg['pacer'])
                self.assertEqual(cfg['pacer']['forward_report_book'], 'a'*64)
                for key in ('state_path', 'report_path', 'forward_report_path'):
                    self.assertTrue(cfg['pacer'][key].startswith('/CLIProxyAPI/plugins/pacer/'))
            (p.RELEASE / 'forward-book.digest').write_text('invalid')
            with self.assertRaises(p.Refused):
                p.pacer_config('dry-run')

    def test_manifest_requires_usage_artifacts_and_checks_tampering(self):
        with tempfile.TemporaryDirectory() as tmp, patch.object(p, 'RELEASE', pathlib.Path(tmp)), patch.object(p, 'PINS', {}):
            m = dict(tag=p.TAG, sources=p.SOURCES, files={})
            path = p.RELEASE / 'manifest.json'
            path.write_text(json.dumps(m))
            with self.assertRaises(p.Refused):
                p.manifest()
            for name in ('reports.sqlite', 'forward-book.json', 'forward-book.digest'):
                (p.RELEASE / name).write_bytes(b'fixture')
                m['files'][name] = p.digest(p.RELEASE / name)
            path.write_text(json.dumps(m))
            self.assertEqual(p.manifest(), m)
            (p.RELEASE / 'reports.sqlite').write_bytes(b'changed')
            with self.assertRaises(p.Refused):
                p.manifest()


if __name__ == '__main__':
    unittest.main()

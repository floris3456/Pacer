"""Offline tests for the native acceptance checker; no lab/provider traffic."""
import importlib.util
import copy
import pathlib
import threading
import unittest
import urllib.request
from http.server import ThreadingHTTPServer

import upstream

spec = importlib.util.spec_from_file_location("history_checker", pathlib.Path(__file__).with_name("check-history.py"))
checker = importlib.util.module_from_spec(spec)
spec.loader.exec_module(checker)
coverage_spec = importlib.util.spec_from_file_location('coverage_checker', pathlib.Path(__file__).with_name('check-coverage.py'))
coverage_checker = importlib.util.module_from_spec(coverage_spec)
coverage_spec.loader.exec_module(coverage_checker)


class HistoryCheckerTests(unittest.TestCase):
    def test_acceptance_runners_preserve_accumulated_fixture_evidence(self):
        for name in ('check-host.py', 'check-inference.py', 'check-ttl.py', 'check-history.py', 'check-coverage.py'):
            self.assertNotIn('/_test/reset', pathlib.Path(__file__).with_name(name).read_text())

    def test_coverage_checker_requires_real_unknown_reports_not_absence(self):
        row = {'measurement': {'coverage': {'observed': False}, 'five_hour': {'known': False},
                               'seven_day': {'known': False}}, 'pace_unit': '%/h · equal sizes'}
        state = {'accounts': [copy.deepcopy(row), copy.deepcopy(row)], 'routing': {'can_activate': False},
                 'measurement': {'delivery': 'native-acknowledged-scoped', 'source_checkpoint': {
                     'domain': 'request_start_only', 'account_membership_complete': False,
                     'physical_carry_in_proven': False, 'calls': 2, 'errors': 0}}}
        self.assertFalse(coverage_checker.validate_state(state)['capacity_known'])
        for field in ('observed', 'known', 'missing'):
            bad = copy.deepcopy(state)
            if field == 'missing':
                del bad['accounts'][0]['measurement']
            elif field == 'known':
                bad['accounts'][0]['measurement']['seven_day']['known'] = True
            else:
                bad['accounts'][0]['measurement']['coverage']['observed'] = True
            with self.assertRaises((AssertionError, KeyError)):
                coverage_checker.validate_state(bad)

    def test_counter_uses_the_real_fixture_endpoint(self):
        server = ThreadingHTTPServer(('127.0.0.1', 0), upstream.Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            http = urllib.request.build_opener(urllib.request.ProxyHandler({}))
            before = upstream.analytics_calls
            self.assertEqual({'calls': before}, checker.analytics_calls(http, 'http://127.0.0.1:' + str(server.server_port)))
            self.assertEqual(before, upstream.analytics_calls)
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)
            self.assertFalse(thread.is_alive())

    def test_csv_accepts_row_order_only(self):
        a = 'header,value\n"quoted\nrow",2\n"doubled""quote",3\n'
        b = 'header,value\n"doubled""quote",3\n"quoted\nrow",2\n'
        self.assertTrue(checker.same_csv_rows(a, b))
        self.assertFalse(checker.same_csv_rows(a, b.replace(',3', ',4')))
        self.assertFalse(checker.same_csv_rows(a, b.replace('header,value', 'value,header')))
        self.assertFalse(checker.same_csv_rows(a, a + '"quoted\nrow",2\n'))
        self.assertFalse(checker.same_csv_rows('h\nx\n', 'h\n"x"\n'))
        self.assertFalse(checker.same_csv_rows('h\nx\n', 'h\r\nx\r\n'))

    def test_rejects_incomplete_csv(self):
        for value in ('h\n"unclosed\n', 'h\nmissing-terminator'):
            with self.assertRaises(ValueError):
                checker.csv_records(value)

    def test_original_oracle_inventory(self):
        cases, exports, _ = checker.load_oracles()
        self.assertEqual(53, len(cases))
        self.assertEqual(104, len(exports))
        for case in cases:
            if case['View'] != 'forecasts':
                for kind in ('json', 'csv'):
                    key = case['View'] + '/' + case['Name'] + '/' + kind
                    self.assertIn(key, exports)
                    if kind == 'csv':
                        self.assertTrue(checker.same_csv_rows(exports[key], exports[key]))


if __name__ == '__main__':
    unittest.main()

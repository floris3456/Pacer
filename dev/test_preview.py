import importlib.util
import json
from pathlib import Path
import sqlite3
import tempfile
import unittest


def module(name):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(name + ".py"))
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


snapshot, serve = module("snapshot"), module("serve")


class PreviewTests(unittest.TestCase):
    def test_backup_preserves_guarded_generation_and_refuses_invalid_source(self):
        with tempfile.TemporaryDirectory() as directory:
            source, copy = Path(directory)/"source.sqlite", Path(directory)/"copy.sqlite"
            db = sqlite3.connect(source)
            db.executescript("CREATE TABLE measurement_report_meta(id INTEGER PRIMARY KEY,schema_cookie INTEGER); CREATE TABLE events(id INTEGER); INSERT INTO events VALUES(42);")
            cookie = db.execute("pragma schema_version").fetchone()[0]
            db.execute("insert into measurement_report_meta values(1,?)", (cookie,)); db.commit()
            snapshot.backup(source, copy)
            copied = sqlite3.connect(copy)
            self.assertEqual(copied.execute("pragma schema_version").fetchone()[0], cookie)
            self.assertEqual(copied.execute("select * from events").fetchall(), [(42,)])
            copied.close()
            db.execute("create table unrecognized(id integer)"); db.commit()
            with self.assertRaisesRegex(AssertionError, "Source feed"):
                snapshot.backup(source, Path(directory)/"refused.sqlite")
            db.close()

    def test_dummy_provider_balances_are_remaining_not_used(self):
        windows = [{"scope":"gemini", "seconds":604800,"remaining":25,"reset":"2026-10-18T12:00:00Z"}, {"scope":"gemini", "seconds":18000,"remaining":100,"reset":"2026-10-11T17:00:00Z"}]
        groups = serve.quota("antigravity", windows)["groups"]
        self.assertEqual([b["remainingFraction"] for b in groups[0]["buckets"]], [.25, 1])
        windows[0]["scope"]="all"; windows[1]["scope"]="all"
        self.assertEqual(serve.quota("codex",windows)["rate_limit"]["primary_window"]["used_percent"],75)
        self.assertEqual(serve.quota("opencode-go",windows)["usage"]["rolling"]["percent"],0)

    def test_secret_inventory_includes_nested_credentials_not_identities(self):
        value={"access":{"api-keys":["secret-client-key-12345"]},"auth":{"access_token":"secret-access-token-12345","account_id":"account-identity-12345"}}
        secrets=snapshot.secret_values(value)
        self.assertEqual(secrets,{b"secret-client-key-12345",b"secret-access-token-12345"})


if __name__ == "__main__": unittest.main()

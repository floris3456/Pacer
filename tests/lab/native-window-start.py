#!/usr/bin/env python3
"""Exercise automatic quota starts through actual CPA + Pacer binaries.

Usage: native-window-start.py CPA_BINARY PACER_SO PACER_STATE_BINARY [--retry]
Only synthetic credentials and a nonforwarding local provider are used.
Quota evidence is seeded offline; each boot exercises the real 30-second worker.
"""

import datetime as dt
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
import urllib.request


SOURCE = pathlib.Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("native", SOURCE / "tests/lab/native-managed-client.py")
native = importlib.util.module_from_spec(spec)
spec.loader.exec_module(native)
MODEL = "claude-haiku-5-5"
KEYS = ["window-main-fixture", "window-restricted-fixture", "window-third-fixture"]
IDS = ["a-window.json", "b-window-alias.json", "c-paid-window.json"]
UUIDS = ["11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"]


def check(ok, message):
    if not ok:
        raise AssertionError(message)


def timestamp(value):
    return value.isoformat(timespec="milliseconds").replace("+00:00", "Z")


def account_key(uuid):
    return "claude:" + hashlib.sha256(("claude:account:" + uuid).encode()).hexdigest()


class Handler(native.FixtureHandler):
    def do_GET(self):
        # The initial seed is the only quota snapshot needed in this test.
        self.reply(429, {"error": "fixture_quota_backoff"})

    def do_POST(self):
        check(self.path.split("?", 1)[0] == "/v1/messages", "unexpected inference route")
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        token = self.headers.get("Authorization", "").removeprefix("Bearer ")
        with self.server.lock:
            self.server.attempts.append({"token": token, "body": body,
                "private_headers": [k for k in self.headers if k.lower().startswith("x-pacer-")]})
            attempt = len(self.server.attempts)
        if getattr(self.server, "retry_case", False) and attempt == 1:
            # Authoritative 429 headers survive an interrupted error body.
            data = b'{"type":"error"'
            self.send_response(429)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data) + 100))
            self.send_header("Retry-After", "001800")
            self.end_headers()
            self.wfile.write(data)
            self.wfile.flush()
            self.close_connection = True
            return
        data = json.dumps({"id": "msg_window_fixture", "type": "message", "role": "assistant",
            "model": MODEL, "content": [{"type": "text", "text": "."}], "stop_reason": "max_tokens",
            "usage": {"input_tokens": 9, "output_tokens": 1,
                      "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.send_header("anthropic-ratelimit-unified-5h-utilization", "0.001")
        self.send_header("anthropic-ratelimit-unified-5h-reset", str(int(time.time()) + 18000))
        self.end_headers()
        self.wfile.write(data)


def seed(work):
    # This is fixture-owned offline SQLite, never the user's OpenCode database.
    now = dt.datetime.now(dt.timezone.utc)
    at = timestamp(now)
    with sqlite3.connect(work / "pacer.sqlite") as db:
        db.execute("CREATE TABLE IF NOT EXISTS observation_windows (account TEXT NOT NULL, key TEXT NOT NULL, provider TEXT NOT NULL, body TEXT NOT NULL, PRIMARY KEY(account,key))")
        for uuid in UUIDS:
            for seconds in (18000, 604800):
                window = {"provider": "claude", "scope": "all", "seconds": seconds,
                    "remaining": 100 if seconds == 18000 else 80,
                    "usage": {"at": at, "source": "api", "quality": "measured"},
                    "cycle_at": at}
                if seconds == 18000:
                    window.update(dormant=True, status_evidence={"at": at, "source": "api", "quality": "measured", "raw": "null"})
                else:
                    reset = timestamp(now + dt.timedelta(days=7))
                    window.update(reset=reset, cycle=reset,
                        reset_evidence={"at": at, "source": "api", "quality": "absolute"})
                db.execute("INSERT OR REPLACE INTO observation_windows VALUES (?,?,?,?)",
                    (account_key(uuid), "claude:all:" + str(seconds), "claude", json.dumps(window)))
    with sqlite3.connect(work / "pacer.sqlite.credits.sqlite") as db:
        db.execute("CREATE TABLE IF NOT EXISTS observation_credits (account TEXT PRIMARY KEY, state TEXT NOT NULL, observed INTEGER NOT NULL, balance TEXT NOT NULL DEFAULT '', source TEXT NOT NULL DEFAULT 'api')")
        for i, uuid in enumerate(UUIDS):
            db.execute("INSERT OR REPLACE INTO observation_credits VALUES (?,?,?,?,?)",
                (account_key(uuid), "empty" if i == 0 else "available", int(now.timestamp()*1000), "", "api"))


def main():
    if len(sys.argv) not in (4, 5) or (len(sys.argv) == 5 and sys.argv[4] != "--retry"):
        raise SystemExit(__doc__)
    retry_case = len(sys.argv) == 5
    binary, plugin, initializer = [pathlib.Path(p).resolve() for p in sys.argv[1:4]]
    check(all(p.is_file() for p in (binary, plugin, initializer)), "missing test artifact")
    root = pathlib.Path(tempfile.mkdtemp(prefix="pacer-native-window-start-"))
    print("Evidence: " + str(root), flush=True)
    work = root / "fixture"
    work.mkdir()
    (work / "plugins").mkdir()
    (work / "auths").mkdir()
    shutil.copyfile(plugin, work / "plugins/pacer.so")
    env = {"PATH": os.environ.get("PATH", os.defpath), "TZ": "UTC", "NO_PROXY": "127.0.0.1,localhost",
           "XDG_CONFIG_HOME": str(work / "config"), "XDG_CACHE_HOME": str(work / "cache")}
    subprocess.run([str(initializer), "init", "--state", str(work / "pacer.sqlite"), "--routes", "claude-main"],
                   check=True, timeout=20, cwd=work, env=env, stdout=subprocess.DEVNULL)
    with (root / "cert.log").open("w") as log:
        subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
            "-subj", "/CN=api.anthropic.com", "-addext", "subjectAltName=DNS:api.anthropic.com",
            "-keyout", str(work / "fixture.key"), "-out", str(work / "fixture.crt")],
            check=True, timeout=15, stdout=log, stderr=subprocess.STDOUT, env=env)
    tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    tls.load_cert_chain(work / "fixture.crt", work / "fixture.key")
    native.FixtureHandler = Handler
    fixture = native.Fixture(tls)
    fixture.retry_case = retry_case
    threading.Thread(target=fixture.serve_forever, daemon=True).start()
    for name in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"):
        env[name] = fixture.base
    env["SSL_CERT_FILE"] = str(work / "fixture.crt")
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    base = f"http://127.0.0.1:{port}"
    for i, name in enumerate(IDS):
        n = 0 if i < 2 else 1
        (work / "auths" / name).write_text(json.dumps({"type": "claude", "access_token": native.PROVIDERS[n],
            "expired": "2099-01-01T00:00:00Z", "account_uuid": UUIDS[n],
            "email": f"window-{n}@example.invalid", "proxy_url": fixture.base,
            "claude_device_ids": [hashlib.sha256(native.PROVIDERS[n].encode()).hexdigest()]}))
    config = {"config-version": 8, "server": {"host": "127.0.0.1", "port": port},
        "management": {"disable-control-panel": True}, "oauth": {"auth-dir": str(work / "auths")},
        "access": {"api-keys": KEYS}, "routing": {"strategy": "round-robin", "retry": {"request-retry": 3}},
        "plugins": {"enabled": True, "dir": str(work / "plugins"), "required-interceptors": ["pacer"],
            "configs": {"pacer": {"enabled": True, "state_path": str(work / "pacer.sqlite"),
                "routing": {"mode": "off"}, "usage_auth_url": base + "/v1/models",
                "client_access": {"grants": [{"caller_scope": native.caller_scope(key),
                    "auth_ids": [IDS[-1]] if i == 1 else IDS} for i, key in enumerate(KEYS)]},
                "quota_window_start": {"enabled": True, "model": MODEL}}}}}
    (work / "config.yaml").write_text(json.dumps(config))
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def usage(key=KEYS[0]):
        req = urllib.request.Request(base + "/v0/resource/plugins/pacer/events?auth=api-key&view=usage&all=true&v=1",
            headers={"Authorization": "Bearer " + key})
        with opener.open(req, timeout=3) as response:
            return json.load(response)

    process = None
    try:
        boots = 4 if retry_case else 2
        for boot in range(boots):
            if retry_case and boot == 2:
                # Advance only this stopped, disposable fixture's retry clock.
                # Production ledgers are never edited to expedite a retry.
                with sqlite3.connect(work / "pacer.sqlite.window-start.sqlite") as db:
                    shift = 31 * 60 * 1_000_000_000
                    db.execute("UPDATE quota_window_starts SET started=started-?,finished=finished-?,cycle_started=cycle_started-?,retry_at=retry_at-? WHERE state='retry_wait'", (shift, shift, shift, shift))
            seed(work)
            with (root / f"server-{boot}.log").open("w") as log:
                process = subprocess.Popen([str(binary), "--config", str(work / "config.yaml"), "--local-model"],
                    cwd=work, env=env, stdout=log, stderr=subprocess.STDOUT)
            # CPA loads OAuth files after registering plugins. The first tick
            # discovers them; the following tick can start the dormant account.
            expect_dispatch = boot == 0 or (retry_case and boot == 2)
            deadline = time.monotonic() + (75 if expect_dispatch else 40)
            ready = False
            while time.monotonic() < deadline:
                check(process.poll() is None, "fixture CPA exited")
                try:
                    view = usage()
                    (root / f"usage-{boot}.json").write_text(json.dumps(view, indent=2))
                    state = next(r for r in view["credentials"] if r["id"] == IDS[0])["quota_window_start"]["state"]
                    ready = state == ("retry_wait" if retry_case and boot < 2 else "confirmed")
                    if ready and (expect_dispatch or time.monotonic() > deadline - 10):
                        break
                except (OSError, KeyError, StopIteration):
                    pass
                threading.Event().wait(2)
            check(ready, "worker did not reach the expected retry/confirmation state")
            with fixture.lock:
                attempts = list(fixture.attempts)
            expected = 2 if retry_case and boot >= 2 else 1
            check(len(attempts) == expected, "duplicate start, alias failover or paid-account dispatch")
            starter = next(r for r in view["credentials"] if r["id"] == IDS[0])["quota_window_start"]
            check(starter["attempt"]["attempts"] == expected, "attempt budget lost across restart")
            if retry_case and boot < 2:
                attempt = starter["attempt"]
                check(attempt["http_status"] == 429 and attempt["reason"] == "http_rate_limited", "HTTP refusal diagnostic lost")
                started = dt.datetime.fromisoformat(attempt["started"].replace("Z", "+00:00"))
                retry = dt.datetime.fromisoformat(attempt["retry_at"].replace("Z", "+00:00"))
                check((retry - started).total_seconds() >= 1800, "Retry-After was shortened")
            one = attempts[-1]
            check(one["token"] == native.PROVIDERS[0], "starter used a different billing account")
            check(not one["private_headers"], "internal coordination leaked upstream")
            body = one["body"]
            check(body["model"] == MODEL and body["max_tokens"] == 1, "unbounded/incorrect model request")
            check(body.get("thinking") == {"type": "disabled"}, "thinking not explicitly disabled")
            check(len(body["messages"]) == 1 and not body.get("tools"), "history or tools included")
            check("cache_control" not in json.dumps(body) and "prompt_cache" not in json.dumps(body), "cache writes requested")
            limited = usage(KEYS[1])["credentials"]
            check(len(limited) == 1 and limited[0]["id"] == IDS[-1], "starter status escaped caller grants")
            check(limited[0]["quota_window_start"]["reason"] == "paid_usage_not_disabled", "paid usage status missing")
            with sqlite3.connect((work / "pacer.sqlite").as_uri() + "?mode=ro", uri=True) as db:
                records = [json.loads(body) for body, in db.execute("SELECT body FROM measurement_records")]
                captures = list(db.execute("SELECT request_id,auth FROM pacer_usage_credentials WHERE request_id LIKE 'pacer-window-start:%'"))
            starts = [body for body in records if body.get("request_id", "").startswith("pacer-window-start:")]
            check(len(starts) == expected, "starter usage.handle was not accepted exactly once per attempt")
            record = max(starts, key=lambda record: record["usage"].get("output", 0))
            check((record["request_id"], IDS[0]) in captures, "starter lacked immutable credential capture")
            check(record["attribution"]["account_id"] == account_key(UUIDS[0]) and not record["attribution"].get("session_id"), "starter usage has wrong account or user session")
            check(not record["policy"].get("kind") and not record["activity"]["primary_prefix"], "starter polluted managed cache accounting")
            check(record["usage"]["output"] == (0 if retry_case and boot < 2 else 1) and record["usage"]["cache_create"] == 0, "starter usage counters missing")
            (root / f"measurement-{boot}.json").write_text(json.dumps(starts, indent=2))
            (root / f"usage-{boot}.json").write_text(json.dumps(view, indent=2))
            print(f"PASS boot {boot}: {state}, {expected} attempts; exact-account, scoped status and isolated usage", flush=True)
            process.terminate()
            process.wait(timeout=15)
            process = None
        (root / "result.json").write_text(json.dumps({"passed": True, "fakeProviderCalls": 2 if retry_case else 1,
            "realProviderCalls": 0, "boots": boots, "retryCase": retry_case, "artifacts": {str(p): native.file_sha256(p) for p in (binary, plugin, initializer)}}, indent=2))
    finally:
        with fixture.lock:
            (root / "attempts.json").write_text(json.dumps(fixture.attempts, indent=2))
        if process is not None:
            process.terminate()
            try:
                process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
        fixture.shutdown()
        fixture.server_close()


if __name__ == "__main__":
    main()

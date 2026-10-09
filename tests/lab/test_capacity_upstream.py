"""Capacity fixture and its dummy logins, on owned ephemeral loopback servers only."""
import json
import pathlib
import re
import shutil
import socket
import ssl
import subprocess
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.request

import capacity_upstream as fixture

HERE = pathlib.Path(__file__).resolve().parent
HTTP = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def serve(state, control=True, tls=None):
    server = fixture.FixtureServer(("127.0.0.1", 0), state, control=control, tls=tls)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server, thread


class Base(unittest.TestCase):
    hold_max = 600.0

    def setUp(self):
        self.ping = fixture.PING_SECONDS
        self.state = fixture.State(now=1_900_000_000, hold_max=self.hold_max)
        self.server, self.thread = serve(self.state)
        self.base = "http://127.0.0.1:%d" % self.server.server_port

    def tearDown(self):
        fixture.PING_SECONDS = self.ping
        for account in fixture.ACCOUNTS:
            self.state.accounts[account]["release"].set()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)
        self.assertFalse(self.thread.is_alive())

    def call(self, path, body=None, token=None, raw=None, headers=None, base=None, context=None):
        sent = dict(headers or {})
        if token:
            sent["Authorization"] = "Bearer " + token
        data = raw if raw is not None else None if body is None else json.dumps(body).encode()
        if data is not None:
            sent.setdefault("Content-Type", "application/json")
        request = urllib.request.Request((base or self.base) + path, data=data, headers=sent)
        try:
            with HTTP.open(request, timeout=10) as response:
                return response.status, response.read(), dict(response.headers)
        except urllib.error.HTTPError as error:
            return error.code, error.read(), dict(error.headers)

    def json(self, path, body=None, token=None, status=200):
        code, raw, _ = self.call(path, body, token)
        self.assertEqual(status, code, raw[:300])
        return json.loads(raw)

    def attempts(self):
        return self.json("/_test/attempts")

    def message(self, account, stream=False, token=None):
        return self.call("/v1/messages", {"model": "claude-opus-5-5", "max_tokens": 64, "stream": stream,
                                          "messages": [{"role": "user", "content": "dummy"}]},
                         token or fixture.access_token(account))


class Providers(Base):
    def test_health_reports_accounts_and_fixture_clock(self):
        before = time.time()
        body = self.json("/health")
        self.assertEqual({"ready": True, "accounts": list(fixture.ACCOUNTS)}, {k: body[k] for k in ("ready", "accounts")})
        self.assertLessEqual(before - 1, body["now"])
        self.assertLessEqual(body["now"], time.time() + 1)

    def test_usage_is_per_account_with_a_partial_scoped_window(self):
        for index, account in enumerate(fixture.ACCOUNTS):
            if index:
                self.json("/_test/usage", {"account": account, "five_hour": 10 + index, "seven_day": 20 + index,
                                           "five_hour_reset": 1_900_018_000 + index, "seven_day_reset": 1_900_604_800 + index})
        for index, account in enumerate(fixture.ACCOUNTS):
            body = self.json("/api/oauth/usage", token=fixture.access_token(account))
            self.assertEqual({"five_hour": {"utilization": 10 + index, "resets_at": 1_900_018_000 + index},
                              "seven_day": {"utilization": 20 + index, "resets_at": 1_900_604_800 + index},
                              "seven_day_opus": {"utilization": 1, "resets_at": None}}, body)
        counts = self.attempts()["counts"]
        self.assertEqual({a: {"/api/oauth/usage": 1} for a in fixture.ACCOUNTS}, counts)

    def test_original_fixture_tokens_get_upstream_constants_on_usage_only(self):
        state = fixture.State(now=1_900_000_000, original_resets=(1_800_000_111, 1_800_000_222))
        self.state.original = state.original
        for token in ("sk-ant-oat01-pacer-a", "sk-ant-oat01-pacer-b"):
            self.assertEqual({"five_hour": {"utilization": 20, "resets_at": 1_800_000_111},
                              "seven_day": {"utilization": 30, "resets_at": 1_800_000_222}},
                             self.json("/api/oauth/usage", token=token))
            self.assertEqual(401, self.call("/api/oauth/profile", token=token)[0])
            self.assertEqual(401, self.message("cap-a", token=token)[0])
        counts = self.attempts()["counts"]
        self.assertEqual(1, counts["fixture-a"]["/api/oauth/usage"])
        self.assertEqual(1, counts["fixture-b"]["/api/oauth/usage"])

    def test_unknown_tokens_are_refused_and_counted_without_the_token(self):
        for path in ("/api/oauth/usage", "/api/oauth/profile"):
            self.assertEqual(401, self.call(path, token="sk-ant-oat01-not-a-fixture")[0])
            self.assertEqual(401, self.call(path)[0])
        self.assertEqual(401, self.message("cap-a", token="sk-ant-oat01-not-a-fixture")[0])
        snapshot = self.attempts()
        self.assertEqual({"unknown": {"/api/oauth/usage": 2, "/api/oauth/profile": 2, "/v1/messages": 1}}, snapshot["counts"])
        self.assertNotIn("sk-ant-", json.dumps(snapshot))

    def test_x_api_key_is_accepted_like_a_bearer_token(self):
        code, raw, _ = self.call("/api/oauth/usage", headers={"x-api-key": fixture.access_token("cap-b")})
        self.assertEqual(200, code, raw)

    def test_profile_matches_cpa_oauth_profile_shape(self):
        body = self.json("/api/oauth/profile", token=fixture.access_token("cap-c"))
        self.assertEqual({"account": {"uuid": fixture.ACCOUNTS["cap-c"], "email": "cap-c@example.invalid"},
                          "organization": {"uuid": fixture.ORG_UUID, "name": fixture.ORG_NAME}}, body)
        self.assertEqual({"cap-c": {"/api/oauth/profile": 1}}, self.attempts()["counts"])

    def test_refresh_rotates_once_and_supersedes_the_old_pair(self):
        old_access, old_refresh = fixture.access_token("cap-d"), fixture.refresh_token("cap-d")
        body = self.json("/v1/oauth/token", {"grant_type": "refresh_token", "refresh_token": old_refresh,
                                            "client_id": "dummy"})
        self.assertEqual(fixture.access_token("cap-d", 1), body["access_token"])
        self.assertEqual(fixture.refresh_token("cap-d", 1), body["refresh_token"])
        self.assertEqual((fixture.TOKEN_LIFETIME, "Bearer"), (body["expires_in"], body["token_type"]))
        self.assertEqual({"uuid": fixture.ACCOUNTS["cap-d"], "email_address": "cap-d@example.invalid"}, body["account"])
        self.assertEqual({"uuid": fixture.ORG_UUID, "name": fixture.ORG_NAME}, body["organization"])
        code, raw, _ = self.call("/v1/oauth/token", {"grant_type": "refresh_token", "refresh_token": old_refresh})
        self.assertEqual((400, "invalid_grant"), (code, json.loads(raw)["error"]))
        # The new pair works; the superseded access token still works but is counted.
        self.assertEqual(fixture.ACCOUNTS["cap-d"], self.json("/api/oauth/profile", token=body["access_token"])["account"]["uuid"])
        self.json("/api/oauth/usage", token=old_access)
        snapshot = self.attempts()
        self.assertEqual({"cap-d": 1}, snapshot["generations"] and {k: v for k, v in snapshot["generations"].items() if v})
        self.assertEqual({"cap-d": 1}, snapshot["refresh_rejected"])
        self.assertEqual({"cap-d": 1}, snapshot["stale_tokens"])
        self.assertEqual({"/v1/oauth/token": 2, "/api/oauth/profile": 1, "/api/oauth/usage": 1}, snapshot["counts"]["cap-d"])
        self.assertNotIn("sk-ant-", json.dumps(snapshot))

    def test_refresh_accepts_form_encoding_and_refuses_other_grants(self):
        raw = ("grant_type=refresh_token&refresh_token=" + fixture.refresh_token("cap-e")).encode()
        code, body, _ = self.call("/v1/oauth/token", raw=raw, headers={"Content-Type": "application/x-www-form-urlencoded"})
        self.assertEqual(200, code, body)
        self.assertEqual(fixture.access_token("cap-e", 1), json.loads(body)["access_token"])
        code, body, _ = self.call("/v1/oauth/token", {"grant_type": "authorization_code", "code": "x"})
        self.assertEqual((400, "unsupported_grant_type"), (code, json.loads(body)["error"]))
        code, body, _ = self.call("/v1/oauth/token", {"grant_type": "refresh_token", "refresh_token": 7})
        self.assertEqual((400, "invalid_grant"), (code, json.loads(body)["error"]))

    def test_messages_report_fixed_usage_and_unified_quota_headers(self):
        self.json("/_test/usage", {"account": "cap-a", "five_hour": 12, "seven_day": 22,
                                   "five_hour_reset": 1_900_018_000, "seven_day_reset": 1_900_604_800})
        code, raw, headers = self.message("cap-a")
        self.assertEqual(200, code, raw)
        body = json.loads(raw)
        self.assertEqual({"input_tokens": 1000, "output_tokens": 100, "cache_creation_input_tokens": 0,
                          "cache_read_input_tokens": 0, "service_tier": "standard"}, body["usage"])
        self.assertEqual(("claude-opus-5-5", "end_turn"), (body["model"], body["stop_reason"]))
        self.assertEqual("0.12", headers["anthropic-ratelimit-unified-5h-utilization"])
        self.assertEqual("0.22", headers["anthropic-ratelimit-unified-7d-utilization"])
        self.assertEqual("1900018000", headers["anthropic-ratelimit-unified-5h-reset"])
        self.assertEqual("1900604800", headers["anthropic-ratelimit-unified-7d-reset"])
        self.assertRegex(headers["request-id"], r"^req_capacity_a_\d{6}$")
        log = [e for e in self.attempts()["log"] if e["path"] == "/v1/messages"]
        self.assertEqual(1, len(log))
        self.assertEqual(("cap-a", 200, False, "complete", False), (log[0]["account"], log[0]["status"], log[0]["held"], log[0]["outcome"], log[0]["stream"]))
        self.assertLessEqual(log[0]["started"], log[0]["at"])

    def test_stream_is_a_complete_anthropic_event_sequence(self):
        code, raw, headers = self.message("cap-b", stream=True)
        self.assertEqual(200, code)
        self.assertEqual("text/event-stream", headers["Content-Type"])
        events = re.findall(r"^event: (\w+)$", raw.decode(), re.M)
        self.assertEqual(["message_start", "content_block_start", "content_block_delta", "content_block_stop",
                          "message_delta", "message_stop"], events)
        start = json.loads(re.search(r"^data: (.*)$", raw.decode(), re.M).group(1))
        self.assertEqual(1000, start["message"]["usage"]["input_tokens"])
        self.assertIn('"output_tokens": 100', raw.decode())

    def test_held_stream_pings_until_release_and_records_its_span(self):
        fixture.PING_SECONDS = 0.05
        self.assertEqual({"account": "cap-d", "armed": True, "holding": 0}, self.json("/_test/hold", {"account": "cap-d"}))
        result = {}
        worker = threading.Thread(target=lambda: result.update(zip(("code", "raw", "headers"), self.message("cap-d", stream=True))))
        worker.start()
        deadline = time.monotonic() + 5
        while self.attempts()["holding"]["cap-d"] != 1:
            self.assertLess(time.monotonic(), deadline)
            time.sleep(0.02)
        self.assertFalse(self.attempts()["armed"]["cap-d"])
        # Another account is never held by cap-d's hold.
        self.assertEqual(200, self.message("cap-e")[0])
        time.sleep(0.3)
        self.assertTrue(worker.is_alive())
        released = time.time()
        self.assertEqual({"account": "cap-d", "armed": False, "holding": 1}, self.json("/_test/release", {"account": "cap-d"}))
        worker.join(5)
        self.assertFalse(worker.is_alive())
        text = result["raw"].decode()
        self.assertEqual(200, result["code"])
        self.assertGreaterEqual(text.count("event: ping"), 2)
        self.assertTrue(text.rstrip().endswith('data: {"type": "message_stop"}'))
        entry = [e for e in self.attempts()["log"] if e["path"] == "/v1/messages" and e["account"] == "cap-d"]
        self.assertEqual(1, len(entry))
        self.assertEqual((True, "released", True), (entry[0]["held"], entry[0]["outcome"], entry[0]["stream"]))
        self.assertLess(entry[0]["started"], released)
        self.assertGreaterEqual(entry[0]["at"], released)
        snapshot = self.attempts()
        self.assertEqual((0, {}), (snapshot["holding"]["cap-d"], snapshot["hold_expired"]))

    def test_hold_arms_only_the_next_request(self):
        fixture.PING_SECONDS = 0.05
        self.json("/_test/hold", {"account": "cap-a"})
        worker = threading.Thread(target=lambda: self.message("cap-a"))
        worker.start()
        deadline = time.monotonic() + 5
        while self.attempts()["holding"]["cap-a"] != 1:
            self.assertLess(time.monotonic(), deadline)
            time.sleep(0.02)
        self.assertEqual(200, self.message("cap-a")[0])
        self.json("/_test/release", {"account": "cap-a"})
        worker.join(5)
        held = [e["held"] for e in self.attempts()["log"] if e["path"] == "/v1/messages"]
        self.assertEqual([False, True], held)

    def test_control_inputs_are_validated(self):
        good = {"account": "cap-a", "five_hour": 1, "seven_day": 2, "five_hour_reset": 1_900_000_001, "seven_day_reset": 1_900_000_002}
        for bad in ({**good, "account": "cap-z"}, {**good, "five_hour": 101}, {**good, "seven_day": -1},
                    {**good, "five_hour": True}, {**good, "five_hour": 1.5}, {**good, "seven_day_reset": 0},
                    {**good, "five_hour_reset": fixture.MAX_RESET + 1}, {**good, "extra": 1},
                    {k: v for k, v in good.items() if k != "seven_day"}, {**good, "partial_window": "no"}):
            self.assertEqual(400, self.call("/_test/usage", bad)[0], bad)
        for bad in ({"account": "cap-z"}, {}, {"account": "cap-a", "extra": 1}):
            self.assertEqual(400, self.call("/_test/hold", bad)[0])
            self.assertEqual(400, self.call("/_test/release", bad)[0])
        body = self.json("/_test/usage", {**good, "partial_window": False})
        self.assertEqual({"five_hour": {"utilization": 1, "resets_at": 1_900_000_001},
                          "seven_day": {"utilization": 2, "resets_at": 1_900_000_002}}, body["usage"])

    def test_bodies_are_bounded_and_must_be_json(self):
        self.assertEqual(400, self.call("/v1/messages", raw=b"not json", token=fixture.access_token("cap-a"))[0])
        self.assertEqual(400, self.call("/v1/messages", raw=b"[1]", token=fixture.access_token("cap-a"))[0])
        for length in (str(fixture.MAX_BODY + 1), "-1", "x"):
            with socket.create_connection(("127.0.0.1", self.server.server_port), timeout=10) as conn:
                conn.sendall(("POST /v1/messages HTTP/1.1\r\nHost: x\r\nContent-Length: %s\r\n\r\n" % length).encode())
                self.assertTrue(conn.recv(4096).startswith(b"HTTP/1.0 413 "), length)
        self.assertEqual(404, self.call("/v1/unknown", {})[0])
        self.assertEqual(404, self.call("/v1/unknown", raw=b"not json")[0])
        self.assertEqual(404, self.call("/elsewhere")[0])
        self.assertEqual({}, self.attempts()["counts"])


class HoldExpiry(Base):
    hold_max = 0.2

    def test_an_unreleased_hold_completes_normally_and_is_counted(self):
        fixture.PING_SECONDS = 0.05
        self.json("/_test/hold", {"account": "cap-b"})
        code, raw, _ = self.message("cap-b")
        self.assertEqual(200, code)
        self.assertEqual(100, json.loads(raw)["usage"]["output_tokens"])
        snapshot = self.attempts()
        self.assertEqual({"cap-b": 1}, snapshot["hold_expired"])
        self.assertEqual("hold_expired", [e for e in snapshot["log"] if e["path"] == "/v1/messages"][0]["outcome"])


class ProviderListener(unittest.TestCase):
    """The TLS listener (control=False) never serves the /_test control paths."""

    def check(self, base, context=None):
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPSHandler(context=context))

        def call(path, body=None):
            request = urllib.request.Request(base + path, data=None if body is None else json.dumps(body).encode(),
                                             headers={"Content-Type": "application/json"})
            try:
                with opener.open(request, timeout=10) as response:
                    return response.status, response.read()
            except urllib.error.HTTPError as error:
                return error.code, error.read()

        self.assertEqual(200, call("/health")[0])
        self.assertEqual(404, call("/_test/attempts")[0])
        for path in ("/_test/usage", "/_test/hold", "/_test/release"):
            self.assertEqual(404, call(path, {"account": "cap-a"})[0])
        request = urllib.request.Request(base + "/api/oauth/usage", headers={"Authorization": "Bearer " + fixture.access_token("cap-a")})
        with opener.open(request, timeout=10) as response:
            self.assertEqual(10, json.load(response)["five_hour"]["utilization"])

    def test_plain_provider_listener_refuses_control_paths(self):
        state = fixture.State()
        server, thread = serve(state, control=False)
        try:
            self.check("http://127.0.0.1:%d" % server.server_port)
            self.assertFalse(state.accounts["cap-a"]["armed"])
        finally:
            server.shutdown()
            server.server_close()
            thread.join(5)

    def test_tls_listener_serves_provider_paths_with_the_lab_cert(self):
        if not shutil.which("openssl"):
            self.skipTest("openssl unavailable")
        with tempfile.TemporaryDirectory() as directory:
            cert, key = pathlib.Path(directory, "cert.pem"), pathlib.Path(directory, "key.pem")
            made = subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                                   "-subj", "/CN=127.0.0.1", "-keyout", str(key), "-out", str(cert)],
                                  capture_output=True, timeout=60)
            if made.returncode:
                self.skipTest("openssl could not make a test cert")
            tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
            tls.load_cert_chain(str(cert), str(key))
            state = fixture.State()
            server, thread = serve(state, control=False, tls=tls)
            try:
                client = ssl.create_default_context(cafile=str(cert))
                client.check_hostname = False
                self.check("https://127.0.0.1:%d" % server.server_port, client)
            finally:
                server.shutdown()
                server.server_close()
                thread.join(5)


class Logins(unittest.TestCase):
    def test_dummy_logins_match_the_fixture_and_carry_no_account_uuid(self):
        files = sorted(p.name for p in (HERE / "capacity-auths").iterdir())
        self.assertEqual([a + ".json" for a in fixture.ACCOUNTS], files)
        devices = set()
        for account in fixture.ACCOUNTS:
            login = json.loads((HERE / "capacity-auths" / (account + ".json")).read_text())
            self.assertEqual({"type", "access_token", "refresh_token", "expired", "email", "claude_device_ids",
                              "disabled", "prefix"}, set(login))
            self.assertEqual(("claude", fixture.access_token(account), fixture.refresh_token(account)),
                             (login["type"], login["access_token"], login["refresh_token"]))
            self.assertEqual((account + "@example.invalid", False, account), (login["email"], login["disabled"], login["prefix"]))
            self.assertRegex(login["expired"], r"^20\d\d-\d\d-\d\dT\d\d:\d\d:\d\dZ$")
            self.assertGreater(login["expired"], time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() + 30 * 86400)))
            self.assertEqual(1, len(login["claude_device_ids"]))
            self.assertRegex(login["claude_device_ids"][0], r"^[0-9a-f]{64}$")
            devices.update(login["claude_device_ids"])
            self.assertNotIn("account_uuid", json.dumps(login))
        self.assertEqual(len(fixture.ACCOUNTS), len(devices))


if __name__ == "__main__":
    unittest.main()

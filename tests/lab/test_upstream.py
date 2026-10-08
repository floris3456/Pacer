"""Offline fixture controls, using owned ephemeral loopback servers only."""
import json
import threading
import unittest
import urllib.error
import urllib.request

import upstream


class FixtureTests(unittest.TestCase):
    def setUp(self):
        self.server = upstream.ThreadingHTTPServer(("127.0.0.1", 0), upstream.Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.base = "http://127.0.0.1:" + str(self.server.server_port)
        self.http = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        self.assertEqual(200, self.call("/_test/reset", {})[0])
        self.assertEqual(200, self.call("/_test/service-tier", {})[0])

    def tearDown(self):
        self.assertEqual(200, self.call("/_test/service-tier", {})[0])
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)
        self.assertFalse(self.thread.is_alive())

    def call(self, path, body=None):
        request = urllib.request.Request(self.base + path, data=None if body is None else json.dumps(body).encode(),
                                         headers={"Content-Type": "application/json", "Authorization": "Bearer sk-ant-oat01-pacer-a"})
        try:
            with self.http.open(request, timeout=5) as response:
                return response.status, response.read()
        except urllib.error.HTTPError as error:
            return error.code, error.read()

    def test_explicit_summary_text_reaches_both_native_protocol_shapes(self):
        summary = "## Objective\n- Offline fixture only.\n\n## Progress\n- Preserve the dummy task."
        self.assertEqual(200, self.call("/_test/response", {"text": summary})[0])
        status, raw = self.call("/v1/messages", {"model": "claude-opus-5-5", "messages": []})
        self.assertEqual(200, status)
        self.assertEqual(summary, json.loads(raw)["content"][0]["text"])
        status, raw = self.call("/v1/messages", {"model": "claude-opus-5-5", "messages": [], "stream": True})
        self.assertEqual(200, status)
        events = [json.loads(line[6:]) for line in raw.decode().splitlines() if line.startswith("data: ")]
        self.assertEqual(summary, next(e["delta"]["text"] for e in events if e["type"] == "content_block_delta"))
        self.assertEqual("message_stop", events[-1]["type"])
        self.assertEqual(0, json.loads(self.call("/_test/analytics")[1])["calls"])
        self.assertEqual(0, json.loads(self.call("/_test/activity")[1])["active_messages"])
        self.assertEqual(200, self.call("/_test/reset", {})[0])
        self.assertEqual("Pacer fixture OK", json.loads(self.call("/v1/messages", {"messages": []})[1])["content"][0]["text"])

    def test_bounded_controls_refuse_busy_or_invalid_input(self):
        for body in [{"text": ""}, {"text": "x" * 16385}, {"text": []}, {"text": "\ud800"}, {"text": "ok", "unknown": True}, []]:
            self.assertEqual(400, self.call("/_test/response", body)[0])
        with upstream.lock:
            upstream.active_messages = 1
        try:
            self.assertEqual(409, self.call("/_test/response", {"text": "new"})[0])
            self.assertEqual(409, self.call("/_test/reset", {})[0])
            self.assertEqual(409, self.call("/_test/recover", {"accounts": ["a"]})[0])
            self.assertEqual(409, self.call("/_test/exhaust", {"accounts": ["a"]})[0])
        finally:
            with upstream.lock:
                upstream.active_messages = 0

    def test_targeted_recovery_retains_attempt_evidence(self):
        for control in ("/_test/exhaust", "/_test/recover"):
            for body in ({}, {"accounts": []}, {"accounts": "a"}, {"accounts": ["unknown"]}, {"accounts": ["a"], "extra": True}):
                self.assertEqual(400, self.call(control, body)[0])
        self.assertEqual(200, self.call("/_test/exhaust", {"accounts": ["a", "b"]})[0])
        self.assertEqual(429, self.call("/v1/messages", {"messages": []})[0])
        with upstream.lock:
            before = list(upstream.attempts)
        self.assertEqual(200, self.call("/_test/recover", {"accounts": ["a"]})[0])
        with upstream.lock:
            self.assertEqual(before, upstream.attempts)
            self.assertEqual({"b"}, upstream.exhausted)
        self.assertEqual(200, self.call("/v1/messages", {"messages": []})[0])
        self.assertEqual(0, json.loads(self.call("/_test/activity")[1])["active_messages"])

    def test_assigned_service_tier_is_opt_in_and_independent_of_request(self):
        self.assertEqual({"service_tier": None}, json.loads(self.call("/_test/service-tier")[1]))
        for tier in (None, "standard", "priority", None):
            status, raw = self.call("/_test/service-tier", {"service_tier": tier})
            self.assertEqual(200, status)
            self.assertEqual({"service_tier": tier}, json.loads(raw))
            for requested in ("auto", "standard_only", "priority"):
                for stream in (False, True):
                    status, raw = self.call("/v1/messages", {"messages": [], "service_tier": requested, "stream": stream})
                    self.assertEqual(200, status)
                    if stream:
                        events = [json.loads(line[6:]) for line in raw.decode().splitlines() if line.startswith("data: ")]
                        usage = events[0]["message"]["usage"]
                        self.assertEqual({"output_tokens": 4}, next(e["usage"] for e in events if e["type"] == "message_delta"))
                    else:
                        usage = json.loads(raw)["usage"]
                    if tier is None:
                        self.assertNotIn("service_tier", usage)
                    else:
                        self.assertEqual(tier, usage["service_tier"])
                    self.assertEqual(10, usage["input_tokens"])
                    self.assertEqual(500, usage["cache_read_input_tokens"])
                    self.assertEqual({"ephemeral_5m_input_tokens": 25, "ephemeral_1h_input_tokens": 75}, usage["cache_creation"])

    def test_service_tier_control_preserves_history_and_other_controls(self):
        self.assertEqual(200, self.call("/_test/response", {"text": "retained reply"})[0])
        self.assertEqual(200, self.call("/v1/messages", {"messages": []})[0])
        self.assertEqual(200, self.call("/_test/exhaust", {"accounts": ["b"]})[0])
        self.assertEqual(200, self.call("/_test/hold", {})[0])
        before = self.call("/_test/attempts")[1]
        try:
            for body in ({"service_tier": "standard"}, {"service_tier": None}, {"service_tier": "priority"}, {}):
                self.assertEqual(200, self.call("/_test/service-tier", body)[0])
                self.assertEqual(before, self.call("/_test/attempts")[1])
                self.assertEqual("retained reply", upstream.reply_text)
                self.assertEqual({"b"}, upstream.exhausted)
                self.assertFalse(upstream.release_stream.is_set())
            for value in ("auto", "standard_only", "batch", "STANDARD", " standard ", "", 1, True, [], {}):
                self.assertEqual(400, self.call("/_test/service-tier", {"service_tier": value})[0])
            self.assertEqual(400, self.call("/_test/service-tier", {"service_tier": "standard", "extra": 1})[0])
            with upstream.lock:
                upstream.active_messages = 1
            try:
                self.assertEqual(409, self.call("/_test/service-tier", {"service_tier": "standard"})[0])
                self.assertEqual({"service_tier": None}, json.loads(self.call("/_test/service-tier")[1]))
            finally:
                with upstream.lock:
                    upstream.active_messages = 0
        finally:
            self.call("/_test/release", {})


if __name__ == "__main__":
    unittest.main()

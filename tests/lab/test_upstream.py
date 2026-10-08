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

    def tearDown(self):
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
        finally:
            with upstream.lock:
                upstream.active_messages = 0


if __name__ == "__main__":
    unittest.main()

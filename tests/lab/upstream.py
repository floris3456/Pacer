"""Reusable offline upstream fixture. Run only on an isolated Docker network.

All credentials and content are dummy test inputs. This fixture never makes
outbound requests and does not write request bodies or authorization to logs.
"""
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

lock = threading.Lock()
attempts = []
exhausted = set()


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        if self.path == "/health":
            return self.reply(200, {"ready": True})
        if self.path == "/_test/attempts":
            with lock:
                return self.reply(200, attempts)
        return self.reply(404, {"error": "not_found"})

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        if length > 1024 * 1024:
            return self.reply(413, {"error": "too_large"})
        try:
            body = json.loads(self.rfile.read(length))
        except (ValueError, UnicodeDecodeError):
            return self.reply(400, {"error": "invalid_json"})
        if self.path == "/_test/exhaust":
            with lock:
                exhausted.update(body.get("accounts", []))
            return self.reply(200, {"ok": True})
        if self.path != "/v1/messages":
            return self.reply(404, {"error": "not_found"})
        # Expose only the known dummy account label, never arbitrary bearer text.
        token = self.headers.get("Authorization", "").removeprefix("Bearer ")
        account = {"sk-ant-oat01-pacer-a": "a", "sk-ant-oat01-pacer-b": "b"}.get(token)
        if account is None:
            return self.reply(401, {"error": {"type": "authentication_error"}})
        with lock:
            attempts.append({"account": account, "body": body,
                             "beta": self.headers.get("anthropic-beta", ""),
                             "coordination_leaked": any(k.lower().startswith(("x-quota-", "x-pacer-")) for k in self.headers)})
            limited = account in exhausted
        if limited:
            return self.reply(429, {"error": {"type": "rate_limit_error", "message": "fixture exhausted"}})
        message = {"id": "msg_pacer_fixture", "type": "message", "role": "assistant",
                   "model": body.get("model", "claude-opus-5-5"),
                   "content": [{"type": "text", "text": "Pacer fixture OK"}],
                   "stop_reason": "end_turn", "stop_sequence": None,
                   "usage": {"input_tokens": 10, "output_tokens": 4,
                             "cache_creation_input_tokens": 100,
                             "cache_read_input_tokens": 500,
                             "cache_creation": {"ephemeral_5m_input_tokens": 25,
                                                "ephemeral_1h_input_tokens": 75}}}
        if body.get("stream"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            start = {**message, "content": [], "stop_reason": None}
            events = [("message_start", {"type": "message_start", "message": start}),
                      ("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}),
                      ("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "Pacer fixture OK"}}),
                      ("content_block_stop", {"type": "content_block_stop", "index": 0}),
                      ("message_delta", {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": None}, "usage": {"output_tokens": 4}}),
                      ("message_stop", {"type": "message_stop"})]
            for kind, data in events:
                self.wfile.write(("event: " + kind + "\ndata: " + json.dumps(data) + "\n\n").encode())
            return
        return self.reply(200, message)

    def reply(self, status, body):
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()

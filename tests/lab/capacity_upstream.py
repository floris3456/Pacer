"""Capacity-lab upstream fixture. Run only on an isolated Docker network.

Separate from upstream.py, which must never be edited or restarted. All
credentials, UUIDs and content are dummy test inputs. This fixture never makes
outbound requests and does not write request bodies or authorization to logs.

HTTPS :443 (FIXTURE_CERT/FIXTURE_KEY) answers the provider paths CPA and Pacer
call on api.anthropic.com and platform.claude.com. Plain HTTP :8080 serves the
same paths plus the /_test control paths, which the TLS listener refuses.
"""
import collections
import json
import os
import ssl
import threading
import time
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# Dummy identities. Nothing here authorizes any live provider.
ACCOUNTS = {
    "cap-a": "6f1d1c2a-0000-4000-8000-00000000000a",
    "cap-b": "6f1d1c2a-0000-4000-8000-00000000000b",
    "cap-c": "6f1d1c2a-0000-4000-8000-00000000000c",
    "cap-d": "6f1d1c2a-0000-4000-8000-00000000000d",
    "cap-e": "6f1d1c2a-0000-4000-8000-00000000000e",
}
ORG_UUID = "6f1d1c2a-0000-4000-8000-0000000000f0"
ORG_NAME = "Pacer capacity lab (dummy)"
# The original fixture's two literal tokens (tests/lab/upstream.py). While
# api.anthropic.com is redirected here, their scheduled usage reads still get
# upstream.py's constants; every other path refuses them.
ORIGINAL_TOKENS = {"sk-ant-oat01-pacer-a": "fixture-a", "sk-ant-oat01-pacer-b": "fixture-b"}
INPUT_TOKENS, OUTPUT_TOKENS = 1000, 100
# Longer than CPA's 4 h Claude refresh lead, so one rotation never cascades.
TOKEN_LIFETIME = 28800
MAX_BODY = 1024 * 1024
PING_SECONDS = 5.0
MAX_RESET = 253402300799
PROVIDER_PATHS = ("/v1/messages", "/api/oauth/usage", "/api/oauth/profile", "/v1/oauth/token")


def access_token(account, generation=0):
    return "sk-ant-oat01-pacer-capacity-" + account[4:] + ("-g%d" % generation if generation else "")


def refresh_token(account, generation=0):
    return "sk-ant-ort01-pacer-capacity-" + account[4:] + ("-g%d" % generation if generation else "")


def is_int(value):
    return isinstance(value, int) and not isinstance(value, bool)


class State:
    """All mutable fixture state, guarded by one lock."""

    def __init__(self, now=None, original_resets=None, hold_max=600.0):
        now = int(time.time() if now is None else now)
        self.lock = threading.Lock()
        self.hold_max = hold_max
        self.access = {}  # access token -> (account, generation)
        self.refresh = {}  # refresh token -> (account, generation)
        self.accounts = {}
        for name in ACCOUNTS:
            self.accounts[name] = {"generation": 0, "five_hour": 10, "seven_day": 20,
                                   "five_hour_reset": now + 18000, "seven_day_reset": now + 604800,
                                   "partial_window": True, "armed": False, "holding": 0,
                                   "release": threading.Event(), "fail": 0}
            self.access[access_token(name)] = (name, 0)
            self.refresh[refresh_token(name)] = (name, 0)
        short, long = original_resets or (now + 18000, now + 604800)
        self.original = {"five_hour": {"utilization": 20, "resets_at": short},
                         "seven_day": {"utilization": 30, "resets_at": long}}
        self.counts = collections.defaultdict(collections.Counter)
        self.stale_tokens = collections.Counter()
        self.refresh_rejected = collections.Counter()
        self.hold_expired = collections.Counter()
        self.log = collections.deque(maxlen=5000)
        self.sequence = 0

    def record(self, path, account, status, **extra):
        """Caller holds the lock. Never records tokens or bodies."""
        self.counts[account][path] += 1
        self.log.append({"at": time.time(), "path": path, "account": account, "status": status, **extra})

    def usage(self, name):
        a = self.accounts[name]
        # A 5-hour reset of 0 is a window that has not started (Anthropic: null).
        out = {"five_hour": {"utilization": a["five_hour"], "resets_at": a["five_hour_reset"] or None},
               "seven_day": {"utilization": a["seven_day"], "resets_at": a["seven_day_reset"]}}
        if a["partial_window"]:
            # A scoped window with no reset keeps Pacer's scheduler on its
            # 15-minute partial cadence instead of the 1 h idle wait. Pacer
            # never samples scoped windows for capacity.
            out["seven_day_opus"] = {"utilization": 1, "resets_at": None}
        return out

    def snapshot(self):
        with self.lock:
            return {"counts": {k: dict(v) for k, v in sorted(self.counts.items())},
                    "stale_tokens": dict(self.stale_tokens), "refresh_rejected": dict(self.refresh_rejected),
                    "hold_expired": dict(self.hold_expired),
                    "holding": {n: a["holding"] for n, a in self.accounts.items()},
                    "armed": {n: a["armed"] for n, a in self.accounts.items()},
                    "generations": {n: a["generation"] for n, a in self.accounts.items()},
                    "usage": {n: self.usage(n) for n in self.accounts},
                    "log": list(self.log)}


class Handler(BaseHTTPRequestHandler):
    timeout = 120

    def log_message(self, *_):
        pass

    @property
    def state(self):
        return self.server.state

    def route(self):
        return urllib.parse.urlsplit(self.path).path

    def do_GET(self):
        path = self.route()
        if path == "/health":
            return self.reply(200, {"ready": True, "accounts": sorted(ACCOUNTS), "now": time.time()})
        if path == "/api/oauth/usage":
            return self.oauth_usage()
        if path == "/api/oauth/profile":
            return self.profile()
        if path == "/_test/attempts" and self.server.control:
            return self.reply(200, self.state.snapshot())
        return self.reply(404, {"error": "not_found"})

    def do_POST(self):
        path = self.route()
        raw = self.read_body()
        if raw is None:
            return None
        # The body is drained first: refusing unread bytes would reset the connection.
        if path not in PROVIDER_PATHS and not (self.server.control and path in ("/_test/usage", "/_test/hold", "/_test/release", "/_test/fail")):
            return self.reply(404, {"error": "not_found"})
        body = self.parse_body(raw)
        if body is None:
            return None
        if path == "/v1/messages":
            return self.messages(body)
        if path == "/v1/oauth/token":
            return self.token(body)
        if path == "/_test/usage":
            return self.set_usage(body)
        if path in ("/_test/hold", "/_test/release"):
            return self.hold(path, body)
        if path == "/_test/fail":
            return self.set_fail(body)
        return self.reply(405, {"error": "method_not_allowed"})

    def set_fail(self, body):
        """The account's next `count` messages fail like a Cloudflare 502."""
        account, count = body.get("account"), body.get("count")
        if set(body) != {"account", "count"} or account not in ACCOUNTS or not is_int(count) or not 0 <= count <= 100:
            return self.reply(400, {"error": "invalid_fixture_fail"})
        with self.state.lock:
            self.state.accounts[account]["fail"] = count
            self.state.log.append({"at": time.time(), "path": "/_test/fail", "account": account, "status": 200})
        return self.reply(200, {"account": account, "fail": count})

    def read_body(self):
        try:
            length = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            length = -1
        if length < 0 or length > MAX_BODY:
            self.close_connection = True
            self.reply(413, {"error": "too_large"})
            return None
        return self.rfile.read(length)

    def parse_body(self, raw):
        try:
            body = json.loads(raw)
        except (ValueError, UnicodeDecodeError):
            # A refresh grant may arrive form-encoded; nothing else may.
            if self.route() != "/v1/oauth/token":
                body = None
            else:
                try:
                    body = dict(urllib.parse.parse_qsl(raw.decode(), strict_parsing=True))
                except (ValueError, UnicodeDecodeError):
                    body = None
        if not isinstance(body, dict):
            self.reply(400, {"error": "invalid_json"})
            return None
        return body

    def bearer(self):
        """(account, generation) for a known dummy token, else (None, None)."""
        token = self.headers.get("Authorization", "")
        if not token.startswith("Bearer "):
            token = "Bearer " + self.headers.get("x-api-key", "")
        token = token[len("Bearer "):].strip()
        if token in ORIGINAL_TOKENS:
            return ORIGINAL_TOKENS[token], None
        return self.state.access.get(token, (None, None))

    def refuse(self, path, account):
        with self.state.lock:
            self.state.record(path, account or "unknown", 401)
        return self.reply(401, {"type": "error", "error": {"type": "authentication_error", "message": "invalid fixture token"}})

    def note_token(self, account, generation):
        """Caller holds the lock. Superseded tokens still work but are counted."""
        if generation is not None and generation < self.state.accounts[account]["generation"]:
            self.state.stale_tokens[account] += 1

    def oauth_usage(self):
        account, generation = self.bearer()
        if account in ("fixture-a", "fixture-b"):
            with self.state.lock:
                self.state.record("/api/oauth/usage", account, 200)
                body = json.loads(json.dumps(self.state.original))
            return self.reply(200, body)
        if account is None:
            return self.refuse("/api/oauth/usage", None)
        with self.state.lock:
            self.note_token(account, generation)
            self.state.record("/api/oauth/usage", account, 200)
            body = self.state.usage(account)
        return self.reply(200, body)

    def profile(self):
        account, generation = self.bearer()
        if account is None or account not in ACCOUNTS:
            return self.refuse("/api/oauth/profile", account)
        with self.state.lock:
            self.note_token(account, generation)
            self.state.record("/api/oauth/profile", account, 200)
        return self.reply(200, {"account": {"uuid": ACCOUNTS[account], "email": account + "@example.invalid"},
                                "organization": {"uuid": ORG_UUID, "name": ORG_NAME}})

    def token(self, body):
        state = self.state
        if body.get("grant_type") != "refresh_token":
            with state.lock:
                state.record("/v1/oauth/token", "unknown", 400)
            return self.reply(400, {"error": "unsupported_grant_type"})
        presented = body.get("refresh_token")
        with state.lock:
            account, generation = state.refresh.get(presented, (None, None)) if isinstance(presented, str) else (None, None)
            if account is None or generation != state.accounts[account]["generation"]:
                if account is not None:
                    state.refresh_rejected[account] += 1
                state.record("/v1/oauth/token", account or "unknown", 400)
                return self.reply(400, {"error": "invalid_grant", "error_description": "refresh token is not current"})
            a = state.accounts[account]
            a["generation"] += 1
            access, refresh = access_token(account, a["generation"]), refresh_token(account, a["generation"])
            state.access[access] = (account, a["generation"])
            state.refresh[refresh] = (account, a["generation"])
            state.record("/v1/oauth/token", account, 200, generation=a["generation"])
        return self.reply(200, {"access_token": access, "refresh_token": refresh, "token_type": "Bearer",
                                "expires_in": TOKEN_LIFETIME,
                                "organization": {"uuid": ORG_UUID, "name": ORG_NAME},
                                "account": {"uuid": ACCOUNTS[account], "email_address": account + "@example.invalid"}})

    def messages(self, body):
        state = self.state
        account, generation = self.bearer()
        if account is None or account not in ACCOUNTS:
            return self.refuse("/v1/messages", account)
        started = time.time()
        with state.lock:
            self.note_token(account, generation)
            a = state.accounts[account]
            if a["fail"] > 0:
                a["fail"] -= 1
                state.record("/v1/messages", account, 502)
                page = b"<html><head><title>502 Bad Gateway</title></head><body><center><h1>502 Bad Gateway</h1></center><hr><center>cloudflare</center></body></html>"
                self.send_response(502)
                self.send_header("Content-Type", "text/html")
                self.send_header("Content-Length", str(len(page)))
                self.end_headers()
                self.wfile.write(page)
                return None
            state.sequence += 1
            request_id = "req_capacity_%s_%06d" % (account[4:], state.sequence)
            if not a["five_hour_reset"]:
                a["five_hour_reset"] = int(time.time()) + 18000  # the first request starts the window
            held = a["armed"]
            if held:
                a["armed"] = False
                a["holding"] += 1
            release = a["release"]
            quota = dict(a)
        message = {"id": "msg_capacity_" + account[4:], "type": "message", "role": "assistant",
                   "model": body.get("model", "claude-opus-5-5"),
                   "content": [{"type": "text", "text": "Pacer capacity fixture OK"}],
                   "stop_reason": "end_turn", "stop_sequence": None,
                   "usage": {"input_tokens": INPUT_TOKENS, "output_tokens": OUTPUT_TOKENS,
                             "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
                             "service_tier": "standard"}}
        outcome = "complete"
        try:
            if body.get("stream"):
                outcome = self.stream(message, request_id, quota, release if held else None)
            else:
                if held:
                    outcome = self.wait(release, None)
                self.reply(200, message, extra=self.quota_headers(quota, request_id))
        except (BrokenPipeError, ConnectionResetError, ssl.SSLError, TimeoutError):
            outcome = "client_gone"
        finally:
            with state.lock:
                if held:
                    a["holding"] -= 1
                    if outcome == "hold_expired":
                        state.hold_expired[account] += 1
                state.record("/v1/messages", account, 200, started=started, held=held, outcome=outcome,
                             stream=bool(body.get("stream")))
        return None

    def wait(self, release, ping):
        """Hold until released. Expiry completes normally; it never truncates."""
        deadline = time.monotonic() + self.state.hold_max
        while not release.wait(PING_SECONDS):
            if time.monotonic() >= deadline:
                return "hold_expired"
            if ping:
                ping()
        return "released"

    def stream(self, message, request_id, quota, release):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        for name, value in self.quota_headers(quota, request_id).items():
            self.send_header(name, value)
        self.end_headers()
        text = message["content"][0]["text"]
        start = {**message, "content": [], "stop_reason": None}
        self.event("message_start", {"type": "message_start", "message": start})
        outcome = "complete"
        if release is not None:
            outcome = self.wait(release, lambda: self.event("ping", {"type": "ping"}))
        for kind, data in (("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}),
                           ("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": text}}),
                           ("content_block_stop", {"type": "content_block_stop", "index": 0}),
                           ("message_delta", {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": None}, "usage": {"output_tokens": OUTPUT_TOKENS}}),
                           ("message_stop", {"type": "message_stop"})):
            self.event(kind, data)
        return outcome

    def event(self, kind, data):
        self.wfile.write(("event: " + kind + "\ndata: " + json.dumps(data) + "\n\n").encode())
        self.wfile.flush()

    @staticmethod
    def quota_headers(a, request_id):
        return {"request-id": request_id,
                "anthropic-ratelimit-unified-5h-utilization": "%g" % (a["five_hour"] / 100),
                "anthropic-ratelimit-unified-5h-reset": str(a["five_hour_reset"]),
                "anthropic-ratelimit-unified-7d-utilization": "%g" % (a["seven_day"] / 100),
                "anthropic-ratelimit-unified-7d-reset": str(a["seven_day_reset"])}

    def set_usage(self, body):
        required = {"account", "five_hour", "seven_day", "five_hour_reset", "seven_day_reset"}
        account = body.get("account")
        valid = (required <= set(body) <= required | {"partial_window"} and account in ACCOUNTS
                 and all(is_int(body[k]) and 0 <= body[k] <= 100 for k in ("five_hour", "seven_day"))
                 and all(is_int(body[k]) and 0 < body[k] <= MAX_RESET for k in ("five_hour_reset", "seven_day_reset") if not (k == "five_hour_reset" and body[k] == 0 and body["five_hour"] == 0))
                 and isinstance(body.get("partial_window", True), bool))
        if not valid:
            return self.reply(400, {"error": "invalid_fixture_usage"})
        with self.state.lock:
            a = self.state.accounts[account]
            for key in required - {"account"}:
                a[key] = body[key]
            if "partial_window" in body:
                a["partial_window"] = body["partial_window"]
            self.state.log.append({"at": time.time(), "path": "/_test/usage", "account": account, "status": 200})
            usage = self.state.usage(account)
        return self.reply(200, {"account": account, "usage": usage})

    def hold(self, path, body):
        account = body.get("account")
        if set(body) != {"account"} or account not in ACCOUNTS:
            return self.reply(400, {"error": "invalid_fixture_account"})
        with self.state.lock:
            a = self.state.accounts[account]
            if path == "/_test/hold":
                if not a["armed"] and not a["holding"]:
                    a["release"] = threading.Event()
                a["armed"] = True
            else:
                a["armed"] = False
                a["release"].set()
            self.state.log.append({"at": time.time(), "path": path, "account": account, "status": 200})
            result = {"account": account, "armed": a["armed"], "holding": a["holding"]}
        return self.reply(200, result)

    def reply(self, status, body, extra=None):
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        for name, value in (extra or {}).items():
            self.send_header(name, value)
        self.end_headers()
        self.wfile.write(data)
        return None


class FixtureServer(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, address, state, control, tls=None):
        self.state, self.control, self.tls = state, control, tls
        super().__init__(address, Handler)

    def finish_request(self, request, client_address):
        # Handshake in the per-connection thread, never in the accept loop.
        if self.tls is None:
            return super().finish_request(request, client_address)
        request.settimeout(30)
        try:
            conn = self.tls.wrap_socket(request, server_side=True)
        except (ssl.SSLError, OSError):
            return None
        try:
            return super().finish_request(conn, client_address)
        finally:
            conn.close()


def original_resets():
    raw = (os.environ.get("FIXTURE_ORIGINAL_FIVE_HOUR_RESET"), os.environ.get("FIXTURE_ORIGINAL_SEVEN_DAY_RESET"))
    if all(raw):
        return int(raw[0]), int(raw[1])
    return None


def main():
    state = State(original_resets=original_resets(), hold_max=float(os.environ.get("FIXTURE_HOLD_MAX", "600")))
    if os.environ.get("FIXTURE_CERT"):
        tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        tls.load_cert_chain(os.environ["FIXTURE_CERT"], os.environ["FIXTURE_KEY"])
        provider = FixtureServer(("0.0.0.0", 443), state, control=False, tls=tls)
        threading.Thread(target=provider.serve_forever, daemon=True).start()
    FixtureServer(("0.0.0.0", 8080), state, control=True).serve_forever()


if __name__ == "__main__":
    main()

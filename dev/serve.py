"""Preview transport and offline provider fixtures; no production credentials."""
import datetime
import http.client
import json
import ssl
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlsplit

PREFIX = "/pacer-dev"
ADMIN = "pacer-preview-admin-not-production"


class QuietHandler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def reply(self, status, body, content_type="application/json"):
        if not isinstance(body, bytes):
            body = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Content-Type-Options", "nosniff")
        self.end_headers()
        self.wfile.write(body)


class Web(QuietHandler):
    def do_GET(self):
        if self.path == PREFIX:
            self.send_response(302)
            self.send_header("Location", PREFIX + "/")
            self.end_headers()
            return
        if self.path == PREFIX + "/":
            html = Path("/source/internal/management/index.html").read_text()
            # Only the transport URL changes; API allowlisting/semantics stay in
            # the real page. Every source edit is served on the next reload.
            needle = "fetch(path,{"
            if html.count(needle) != 1:
                return self.reply(503, {"error": "Preview transport hook changed"})
            html = html.replace(needle, "fetch('/pacer-dev'+path,{")
            manifest = json.loads(Path("/manifest.json").read_text())
            banner = '<div style="padding:8px 20px;background:#26392c;color:#ddf0e2;font:13px system-ui">DEVELOPMENT · isolated copy · dummy providers · snapshot ' + manifest["snapshot_at"] + '</div>'
            html = html.replace("<body>", "<body>" + banner, 1)
            return self.reply(200, html.encode(), "text/html; charset=utf-8")
        return self.proxy()

    def proxy(self):
        path = urlsplit(self.path).path
        if not path.startswith(PREFIX + "/v0/management/") and not path.startswith(PREFIX + "/v8/management/"):
            return self.reply(404, {"error": "not_found"})
        length = int(self.headers.get("Content-Length", "0"))
        if length > 1024 * 1024:
            return self.reply(413, {"error": "too_large"})
        body = self.rfile.read(length) if length else None
        conn = http.client.HTTPConnection("cpa", 8317, timeout=120)
        try:
            conn.request(self.command, self.path[len(PREFIX):], body,
                         {"Authorization": "Bearer " + ADMIN, "Content-Type": "application/json"})
            result = conn.getresponse()
            self.reply(result.status, result.read(), result.getheader("Content-Type", "application/json"))
        except (OSError, http.client.HTTPException):
            self.reply(502, {"error": "Preview CPA is unavailable"})
        finally:
            conn.close()

    do_POST = proxy
    do_PATCH = proxy
    do_DELETE = proxy
    do_PUT = proxy


def reset_value(value):
    if not value or value.startswith("0001-"):
        return None
    return value


def quota(provider, windows):
    if provider == "claude":
        out = {}
        for w in windows:
            name = "five_hour" if w["seconds"] == 18000 else "seven_day"
            if w["scope"] not in ("all", ""): name = "seven_day_opus"
            out[name] = {"utilization": 100-w["remaining"], "resets_at": reset_value(w.get("reset"))} if w.get("remaining") is not None else None
        return out
    if provider == "codex":
        buckets = []
        for w in windows:
            if w.get("remaining") is None or w["scope"] not in ("all", "codex-main", "limit:premium"): continue
            reset = reset_value(w.get("reset"))
            buckets.append({"used_percent": 100-w["remaining"], "limit_window_seconds": w["seconds"], "reset_at": int(datetime.datetime.fromisoformat(reset.replace("Z", "+00:00")).timestamp()) if reset else 0})
        return {"rate_limit": dict(zip(("primary_window", "secondary_window"), buckets))}
    if provider == "antigravity":
        groups = []
        for scope, name in (("gemini", "Gemini models"), ("claude_gpt", "Claude & GPT models")):
            groups.append({"displayName": name, "buckets": [{"window": "5h" if w["seconds"] == 18000 else "weekly", "remainingFraction": w["remaining"]/100, "resetTime": reset_value(w.get("reset"))} for w in windows if w["scope"] == scope and w.get("remaining") is not None]})
        return {"groups": groups}
    return {"usage": {({18000:"rolling", 604800:"weekly", 2592000:"monthly"}[w["seconds"]]): {"percent":100-w["remaining"],"resetsAt":reset_value(w.get("reset"))} for w in windows if w["seconds"] in (18000,604800,2592000) and w.get("remaining") is not None}}


class Providers(QuietHandler):
    def do_GET(self):
        fixtures = json.loads(Path("/fixtures.json").read_text())
        token = self.headers.get("Authorization", "").removeprefix("Bearer ")
        if token not in fixtures:
            return self.reply(401, {"error": "Dummy preview credential required"})
        account = fixtures[token]
        paths = {"claude":"/api/oauth/usage", "codex":"/backend-api/wham/usage", "antigravity":"/v1internal:retrieveUserQuotaSummary", "opencode-go":"/zen/go/v1/usage"}
        if urlsplit(self.path).path != paths.get(account["provider"]):
            return self.reply(501, {"error": "Preview provider: inference is not implemented"})
        return self.reply(200, quota(account["provider"], account["windows"]))

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        if length > 1024*1024: return self.reply(413, {"error":"too_large"})
        self.rfile.read(length)
        return self.do_GET()


if __name__ == "__main__":
    provider = sys.argv[1] == "providers"
    server = ThreadingHTTPServer(("0.0.0.0", 443 if provider else 8080), Providers if provider else Web)
    if provider:
        tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        tls.load_cert_chain("/tls/cert.pem", "/tls/key.pem")
        server.socket = tls.wrap_socket(server.socket, server_side=True)
    server.serve_forever()

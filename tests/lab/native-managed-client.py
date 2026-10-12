#!/usr/bin/env python3
"""Native CPA + Pacer + standalone SDK acceptance, with a loopback fake provider.

Usage: native-managed-client.py CPA_BINARY PACER_SO SDK_JS [PACER_STATE_BINARY]
Every credential is dummy. This starts no containers and contacts no provider.
Logs, durable client state, and final-wire evidence are retained under /tmp.
"""

import hashlib
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
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


MODEL = "claude-opus-5-5"
ROUTE = "claude-main"
INSTALLATION = "native-managed-lab"
CALLERS = ["local-caller-one", "local-caller-two", "local-caller-three"]
PROVIDERS = ["sk-ant-oat01-local-provider-one", "sk-ant-oat01-local-provider-two", "sk-ant-oat01-local-provider-three"]


def caller_scope(key):
    return hashlib.sha256(("cli-proxy-api:caller-scope:v1\0" + key).encode()).hexdigest()


def file_sha256(path):
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


class Fixture(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, tls):
        super().__init__(("127.0.0.1", 0), FixtureHandler)
        self.tls = tls
        self.lock = threading.Lock()
        self.attempts = []
        self.holds = {}
        self.active = 0
        self.max_active = 0
        self.parallel = threading.Barrier(3)

    @property
    def base(self):
        return f"http://127.0.0.1:{self.server_port}"

    def release(self):
        with self.lock:
            for hold in self.holds.values():
                hold.set()


class FixtureHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_):
        pass

    def handle(self):
        try:
            super().handle()
        except (ConnectionResetError, BrokenPipeError):
            pass  # Expected when a fixture client or the native process exits.

    def do_CONNECT(self):
        # Terminate only the default Claude hostname locally; never forward a socket.
        if self.path != "api.anthropic.com:443":
            return self.reply(403, {"error": "external_network_disabled"})
        self.send_response(200)
        self.end_headers()
        self.wfile.flush()
        self.close_connection = True
        try:
            with self.server.tls.wrap_socket(self.connection, server_side=True) as connection:
                FixtureHandler(connection, self.client_address, self.server)
        except (ssl.SSLError, ConnectionError, OSError):
            pass  # Client cancellation may close a tunnel before TLS completes.

    def reply(self, status, body):
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        try:
            self.wfile.write(data)
        except (BrokenPipeError, ConnectionResetError):
            pass  # Expected after the client deliberately stops a held request.

    def do_GET(self):
        if self.path == "/api/oauth/usage":
            return self.reply(200, {"five_hour": {"utilization": 20, "resets_at": "2099-01-01T00:00:00Z"},
                                    "seven_day": {"utilization": 30, "resets_at": "2099-01-08T00:00:00Z"}})
        if self.path == "/_test/attempts":
            with self.server.lock:
                snapshot = list(self.server.attempts)
            return self.reply(200, snapshot)
        if self.path == "/_test/activity":
            with self.server.lock:
                snapshot = {"active": self.server.active, "max_active": self.server.max_active}
            return self.reply(200, snapshot)
        return self.reply(404, {"error": "not_found"})

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        if not 0 < length <= 1024 * 1024:
            return self.reply(413, {"error": "invalid_size"})
        try:
            body = json.loads(self.rfile.read(length))
        except (ValueError, UnicodeDecodeError):
            return self.reply(400, {"error": "invalid_json"})
        if self.path == "/_test/release":
            self.server.release()
            return self.reply(200, {"ok": True})
        if self.path.split("?", 1)[0] != "/v1/messages":
            return self.reply(404, {"error": "not_found"})
        key = self.headers.get("X-Api-Key") or self.headers.get("Authorization", "").removeprefix("Bearer ")
        if key not in PROVIDERS:
            return self.reply(401, {"error": {"type": "authentication_error", "message": "fixture key required"}})
        account = PROVIDERS.index(key)
        encoded = json.dumps(body)
        mode = next((marker for marker in ("STOP", "FAIL", "CONCURRENT")
                     if "PACER_LAB_" + marker in encoded), "OK")
        hold = threading.Event()
        with self.server.lock:
            number = len(self.server.attempts)
            self.server.attempts.append({
                "number": number, "account": account, "mode": mode, "body": body,
                "headers": dict(self.headers),  # Only this fixture's literal dummy credentials exist.
                "coordination_leaked": any(name.lower().startswith(("x-pacer-", "x-quota-"))
                                           for name in self.headers),
            })
            self.server.active += 1
            self.server.max_active = max(self.server.max_active, self.server.active)
            if mode == "STOP":
                self.server.holds[number] = hold
        try:
            if mode == "CONCURRENT":
                try:
                    self.server.parallel.wait(timeout=15)
                except threading.BrokenBarrierError:
                    return self.reply(503, {"error": {"type": "api_error", "message": "concurrency barrier failed"}})
            if mode == "FAIL":
                return self.reply(400, {"error": {"type": "invalid_request_error", "message": "intentional local helper failure"}})
            if mode == "STOP" and not hold.wait(timeout=30):
                return self.reply(408, {"error": {"type": "timeout_error", "message": "fixture hold expired"}})
            return self.reply(200, {
                "id": f"msg_native_lab_{number}", "type": "message", "role": "assistant",
                "model": body.get("model", MODEL),
                "content": [{"type": "text", "text": f"Local dummy answer from account {account}"}],
                "stop_reason": "end_turn", "stop_sequence": None,
                "usage": {"input_tokens": 10, "output_tokens": 5,
                          "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0},
            })
        finally:
            with self.server.lock:
                self.server.active -= 1
                self.server.holds.pop(number, None)


def main():
    if len(sys.argv) not in (4, 5):
        raise SystemExit(__doc__)
    binary, plugin, sdk = (pathlib.Path(p).resolve() for p in sys.argv[1:4])
    initializer = pathlib.Path(sys.argv[4]).resolve() if len(sys.argv) == 5 else plugin.parent / "pacer-state"
    for path in (binary, plugin, sdk, initializer):
        if not path.is_file():
            raise SystemExit(f"Missing artifact: {path}")
    root = pathlib.Path(tempfile.mkdtemp(prefix="pacer-native-managed-"))
    print(f"Native managed-client evidence: {root}", flush=True)
    artifacts = {"native": [{"path": str(path), "sha256": file_sha256(path)} for path in (binary, plugin, initializer)],
                 "sdk_entrypoint": str(sdk),
                 "sdk_modules": {str(path.relative_to(sdk.parent)): file_sha256(path)
                                 for path in sorted(sdk.parent.rglob("*.js"))}}
    (root / "artifacts.json").write_text(json.dumps(artifacts, indent=2) + "\n")
    (root / "plugins").mkdir()
    (root / "auths").mkdir()
    shutil.copyfile(plugin, root / "plugins" / plugin.name)
    env = {"PATH": os.environ.get("PATH", os.defpath), "HOME": str(root),
           "XDG_CONFIG_HOME": str(root / "config"), "XDG_CACHE_HOME": str(root / "cache"),
           "NO_PROXY": "127.0.0.1,localhost", "TZ": "UTC"}
    subprocess.run([str(initializer), "init", "--state", str(root / "pacer.sqlite"), "--routes", ROUTE],
                   check=True, timeout=20, cwd=root, env=env)
    cert, private_key = root / "fixture.crt", root / "fixture.key"
    with (root / "fixture-cert.log").open("w") as cert_log:
        subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                        "-subj", "/CN=api.anthropic.com", "-addext", "subjectAltName=DNS:api.anthropic.com",
                        "-keyout", str(private_key), "-out", str(cert)],
                       check=True, timeout=15, stdout=cert_log, stderr=subprocess.STDOUT, env=env)
    tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    tls.load_cert_chain(cert, private_key)
    env["SSL_CERT_FILE"] = str(cert)
    fixture = Fixture(tls)
    fixture_thread = threading.Thread(target=fixture.serve_forever, daemon=True)
    fixture_thread.start()
    # CPA has a background version lookup even with --local-model. Route any
    # environment-proxy-aware non-loopback HTTP through this nonforwarding fixture.
    # Native provider credentials independently use this explicit local TLS proxy.
    for name in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"):
        env[name] = fixture.base
    with socket.socket() as reservation:
        reservation.bind(("127.0.0.1", 0))
        port = reservation.getsockname()[1]
    base = f"http://127.0.0.1:{port}"
    auth_ids = [f"local-account-{i}.json" for i in range(len(PROVIDERS))]
    for i, key in enumerate(PROVIDERS):
        (root / "auths" / auth_ids[i]).write_text(json.dumps({
            "type": "claude", "access_token": key, "expired": "2099-01-01T00:00:00Z",
            "account_uuid": f"00000000-0000-0000-0000-{i + 1:012d}",
            "email": f"local-account-{i}@example.invalid", "proxy_url": fixture.base,
            "claude_device_ids": [hashlib.sha256(key.encode()).hexdigest()], "cloak_mode": "never",
        }) + "\n")
    config = {
        "config-version": 8, "server": {"host": "127.0.0.1", "port": port},
        "management": {"disable-control-panel": True}, "oauth": {"auth-dir": str(root / "auths")},
        "access": {"api-keys": CALLERS + ["local-caller-no-grant"]},
        "routing": {"strategy": "fill-first", "retry": {"request-retry": 0},
                    "cooldown": {"disable-cooling": True}},
        "plugins": {
            "enabled": True, "dir": str(root / "plugins"), "required-interceptors": ["pacer"],
            "configs": {"pacer": {
                "enabled": True, "state_path": str(root / "pacer.sqlite"), "usage_auth_url": base + "/v1/models",
                "client_access": {"grants": [{"caller_scope": caller_scope(key), "auth_ids": [auth_ids[i]]}
                                              for i, key in enumerate(CALLERS)]},
                "cache_policy": {"enabled": True, "require_managed": True,
                                 "routes": [{"id": ROUTE, "models": [MODEL]}],
                                 "trusted_empty_installations": [{"caller_digest": caller_scope(key),
                                                                   "installation_id": INSTALLATION} for key in CALLERS]},
            }},
        },
    }
    (root / "config.yaml").write_text(json.dumps(config, indent=2) + "\n")
    manifest = {"base": base, "upstream": fixture.base, "sdk": str(sdk), "model": MODEL,
                "route": ROUTE, "installation": INSTALLATION, "callers": CALLERS, "authIDs": auth_ids}
    (root / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    process = None
    log = None

    def stop():
        nonlocal process, log
        if process is not None:
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
            process = None
        if log is not None:
            log.close()
            log = None

    def start(phase):
        nonlocal process, log
        log_path = root / f"server-{phase}.log"
        log = log_path.open("w")
        process = subprocess.Popen([str(binary), "--config", str(root / "config.yaml"), "--local-model"],
                                   cwd=root, env=env, stdout=log, stderr=subprocess.STDOUT)
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            if process.poll() is not None:
                break
            try:
                with opener.open(base + "/v0/resource/plugins/pacer/events", timeout=1) as response:
                    discovery = json.load(response)
                    if response.status == 200 and discovery.get("api"):
                        request = urllib.request.Request(base + "/v1/models", headers={"Authorization": "Bearer " + CALLERS[0]})
                        with opener.open(request, timeout=1) as models:
                            if any(model.get("id") == MODEL for model in json.load(models).get("data", [])):
                                (root / f"discovery-{phase}.json").write_text(json.dumps(discovery, indent=2) + "\n")
                                return
            except (OSError, ValueError):
                pass
            time.sleep(0.1)
        raise RuntimeError(f"Isolated CPA did not become ready; see {log_path}\n" + log_path.read_text()[-5000:])

    def check_native_usage():
        expected = {"caller-one": 0, "caller-two": 1, "caller-three": 2, "helper-stopped": 2}
        deadline = time.monotonic() + 10
        while True:
            with sqlite3.connect((root / "pacer.sqlite").as_uri() + "?mode=ro", uri=True) as database:
                records = [json.loads(row[0]) for row in database.execute(
                    "SELECT body FROM measurement_records WHERE json_extract(body,'$.policy.kind')='auxiliary'")]
            observed = {record["attribution"].get("session_id") for record in records if record["success"]}
            if observed.issuperset(expected):
                break
            if time.monotonic() >= deadline:
                raise AssertionError(f"Missing native auxiliary usage: expected {sorted(expected)}, observed {sorted(observed)}")
            time.sleep(0.1)  # Native usage delivery is asynchronous, not a delivery watermark.
        for record in records:
            policy, native, activity, attribution = (record[name] for name in ("policy", "native", "activity", "attribution"))
            assert not policy.get("selected_ttl") and not policy.get("effective_ttl"), record
            assert native["breakpoint_count"] == 0 and native["dispatched"] is True, record
            assert activity["primary_prefix"] is False and not activity.get("protection_ttl"), record
            assert record["request_id"] and record["attempt_id"], record
            assert attribution["installation_id"] == INSTALLATION and attribution["provider_route"] == ROUTE, record
            caller = {**expected, "helper-failure": 1}[attribution["session_id"]]
            assert attribution["caller_id"] == caller_scope(CALLERS[caller]), record
        (root / "native-auxiliary-usage.json").write_text(json.dumps(records, indent=2) + "\n")
        result = json.loads((root / "result.json").read_text())
        result["checks"].append("native-auxiliary-usage-retains-attribution-with-no-ttl-breakpoints-or-primary-warmth")
        result["passed"] = len(result["checks"])
        (root / "result.json").write_text(json.dumps(result, indent=2) + "\n")

    try:
        script = pathlib.Path(__file__).with_suffix(".mjs")
        for phase in ("initial", "restart"):
            start(phase)
            with (root / f"client-{phase}.log").open("w") as client_log:
                result = subprocess.run(["node", str(script), str(root), phase], cwd=root, env=env,
                                        stdout=client_log, stderr=subprocess.STDOUT, timeout=100)
            if result.returncode:
                raise RuntimeError(f"SDK {phase} phase failed ({result.returncode}):\n"
                                   + (root / f"client-{phase}.log").read_text()[-7000:])
            if phase == "restart":
                check_native_usage()
            stop()
        print((root / "result.json").read_text(), end="")
    finally:
        fixture.release()
        stop()
        fixture.shutdown()
        fixture.server_close()
        (root / "upstream-attempts.json").write_text(json.dumps(fixture.attempts, indent=2) + "\n")


if __name__ == "__main__":
    main()

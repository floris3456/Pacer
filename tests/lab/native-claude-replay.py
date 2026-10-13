#!/usr/bin/env python3
"""Exercise CPA + native Pacer replay against a nonforwarding loopback Claude.

Usage: native-claude-replay.py CPA_BINARY PACER_SO PACER_STATE_BINARY
All credentials and prompts are synthetic. The real conservative lease schedule
makes this integration take about five minutes. Evidence remains under /tmp.
"""

import base64
import concurrent.futures
import copy
import hashlib
import importlib.util
import json
import os
import pathlib
import shutil
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request


spec = importlib.util.spec_from_file_location("native_lab", pathlib.Path(__file__).with_name("native-managed-client.py"))
native = importlib.util.module_from_spec(spec)
spec.loader.exec_module(native)
POLICY = "/v1/plugins/pacer/cache-policy"
REPLAY = "/v0/resource/plugins/pacer/events?view=claude-replay&auth=api-key&v=1"
INSTALLATION = "native-replay-lab"


class ReplayHandler(native.FixtureHandler):
    def do_POST(self):
        if self.path.split("?", 1)[0] != "/v1/messages":
            return self.reply(403, {"error": "external_network_disabled"})
        size = int(self.headers.get("Content-Length", "0"))
        if not 0 < size <= 32 << 20:
            return self.reply(413, {"error": "invalid_size"})
        raw = self.rfile.read(size)
        body = json.loads(raw)
        credential = self.headers.get("X-Api-Key") or self.headers.get("Authorization", "").removeprefix("Bearer ")
        if credential not in native.PROVIDERS:
            return self.reply(401, {"error": "synthetic_credential_required"})
        kind = "native_compaction" if body.get("compaction") else "keepalive" if body.get("max_tokens") == 0 else "primary"
        with self.server.lock:
            number = len(self.server.attempts)
            self.server.attempts.append({
                "number": number, "kind": kind, "account": native.PROVIDERS.index(credential),
                "body": body, "sha256": hashlib.sha256(raw).hexdigest(), "headers": dict(self.headers),
                "coordination_leaked": any(name.lower().startswith(("x-pacer-", "x-quota-")) for name in self.headers),
            })
        if kind == "native_compaction":
            self.server.native_started.set()
            self.server.native_release.wait()
        content = [{"type": "compaction", "content": "Synthetic portable checkpoint.", "signature": "synthetic-signed-checkpoint"}] if kind == "native_compaction" else [{"type": "text", "text": "Synthetic Claude response."}]
        return self.reply(200, {
            "id": f"msg_replay_lab_{number}", "type": "message", "role": "assistant", "model": body["model"],
            "content": content, "stop_reason": "compaction" if kind == "native_compaction" else "end_turn",
            "usage": {"input_tokens": 4, "output_tokens": 2, "cache_read_input_tokens": 2048, "cache_creation_input_tokens": 0},
        })


def check(value, message):
    if not value:
        raise AssertionError(message)


def cache_markers(value):
    found = []
    if isinstance(value, dict):
        if "cache_control" in value:
            found.append(value["cache_control"])
        for item in value.values():
            found.extend(cache_markers(item))
    elif isinstance(value, list):
        for item in value:
            found.extend(cache_markers(item))
    return found


def normalize_cch(value):
    # Exactly the signer's five variable hex bytes are excluded from equality.
    import re
    result = copy.deepcopy(value)
    blocks = result.get("system") if isinstance(result, dict) else result
    if isinstance(blocks, list) and blocks and isinstance(blocks[0], dict):
        block = blocks[0]
        text = block.get("text", "")
        if block.get("type") == "text" and not block.get("cache_control") and text.startswith("x-anthropic-billing-header:"):
            block["text"] = re.sub(r"(?<=cch=)[0-9a-f]{5}(?=;)", "00000", text, count=1)
    return result


def durable_checkpoint(path, value):
    temporary = path.with_suffix(".pending")
    with temporary.open("w") as output:
        json.dump(value, output, indent=2)
        output.write("\n")
        output.flush()
        os.fsync(output.fileno())
    temporary.replace(path)
    descriptor = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def main():
    if len(sys.argv) != 4:
        raise SystemExit(__doc__)
    binary, plugin, initializer = (pathlib.Path(arg).resolve() for arg in sys.argv[1:])
    check(all(path.is_file() for path in (binary, plugin, initializer)), "missing native artifact")
    root = pathlib.Path(tempfile.mkdtemp(prefix="pacer-native-replay-"))
    print(f"Replay native ABI evidence: {root}", flush=True)
    (root / "plugins").mkdir()
    (root / "auths").mkdir()
    shutil.copyfile(plugin, root / "plugins" / "pacer.so")
    result = {"checks": [], "real_provider_calls": 0, "artifacts": [
        {"path": str(path), "sha256": native.file_sha256(path)} for path in (binary, plugin, initializer)]}
    (root / "artifacts.json").write_text(json.dumps(result["artifacts"], indent=2) + "\n")
    env = {"PATH": os.environ.get("PATH", os.defpath), "TZ": "UTC", "NO_PROXY": "127.0.0.1,localhost",
           "XDG_CONFIG_HOME": str(root / "config"), "XDG_CACHE_HOME": str(root / "cache")}
    subprocess.run([str(initializer), "init", "--state", str(root / "pacer.sqlite"), "--routes", native.ROUTE],
                   check=True, timeout=20, cwd=root, env=env, stdout=subprocess.DEVNULL)
    cert, key = root / "fixture.crt", root / "fixture.key"
    with (root / "fixture-cert.log").open("w") as log:
        subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                        "-subj", "/CN=api.anthropic.com", "-addext", "subjectAltName=DNS:api.anthropic.com",
                        "-keyout", str(key), "-out", str(cert)],
                       check=True, timeout=15, stdout=log, stderr=subprocess.STDOUT, env=env)
    tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    tls.load_cert_chain(cert, key)
    native.FixtureHandler = ReplayHandler
    fixture = native.Fixture(tls)
    fixture.native_started, fixture.native_release = threading.Event(), threading.Event()
    threading.Thread(target=fixture.serve_forever, daemon=True).start()
    env["SSL_CERT_FILE"] = str(cert)
    for name in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"):
        env[name] = fixture.base
    with socket.socket() as reservation:
        reservation.bind(("127.0.0.1", 0))
        port = reservation.getsockname()[1]
    base = f"http://127.0.0.1:{port}"
    auth_ids = [f"replay-account-{i}.json" for i in range(2)]
    for i, credential in enumerate(native.PROVIDERS[:2]):
        (root / "auths" / auth_ids[i]).write_text(json.dumps({
            "type": "claude", "access_token": credential, "expired": "2099-01-01T00:00:00Z",
            "account_uuid": f"00000000-0000-0000-0000-{i + 1:012d}", "email": f"replay-{i}@example.invalid",
            "proxy_url": fixture.base, "claude_device_ids": [hashlib.sha256(credential.encode()).hexdigest()],
            "cloak_mode": "never",
        }) + "\n")
    config = {
        "config-version": 8, "server": {"host": "127.0.0.1", "port": port},
        "management": {"disable-control-panel": True}, "oauth": {"auth-dir": str(root / "auths")},
        "access": {"api-keys": native.CALLERS},
        "routing": {"strategy": "round-robin", "retry": {"request-retry": 0}, "cooldown": {"disable-cooling": True}},
        "plugins": {"enabled": True, "dir": str(root / "plugins"), "required-interceptors": ["pacer"], "configs": {"pacer": {
            "enabled": True, "state_path": str(root / "pacer.sqlite"), "usage_auth_url": base + "/v1/models",
            "client_access": {"grants": [{"caller_scope": native.caller_scope(caller), "auth_ids": auth_ids} for caller in native.CALLERS[:2]]},
            "cache_policy": {"enabled": True, "require_managed": True, "routes": [{"id": native.ROUTE, "models": [native.MODEL]}],
                             "trusted_empty_installations": [{"caller_digest": native.caller_scope(native.CALLERS[0]), "installation_id": INSTALLATION}]},
        }}},
    }
    (root / "config.yaml").write_text(json.dumps(config, indent=2) + "\n")
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    events = []

    def request(method, path, body=None, headers=None, caller=native.CALLERS[0]):
        outgoing = {"Content-Type": "application/json"}
        if caller is not None:
            outgoing["Authorization"] = "Bearer " + caller
        outgoing.update(headers or {})
        req = urllib.request.Request(base + path, data=None if body is None else json.dumps(body).encode(), headers=outgoing, method=method)
        try:
            response = opener.open(req)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            raw = response.read()
            try:
                answer = json.loads(raw)
            except ValueError:
                answer = {"raw": raw.decode(errors="replace")}
            status = response.status
        events.append({"method": method, "path": path, "status": status, "response": answer})
        (root / "http-events.json").write_text(json.dumps(events, indent=2) + "\n")
        return status, answer

    def checked(name):
        result["checks"].append(name)
        print("PASS " + name, flush=True)

    def scope(session):
        return {"installation_id": INSTALLATION, "session_id": session["id"], "provider_route": native.ROUTE}

    def command(session, operation, ttl="5m", **extra):
        state = session.get("state")
        body = {"protocol": 1, "operation": operation, **scope(session), "context_epoch": state["active"]["context_epoch"] if state else "empty-" + session["id"],
                "expected_revision": state["revision"] if state else 0, "ttl": ttl, "operation_id": f"op-{len(events)}", **extra}
        return request("POST", POLICY, body, {"X-Pacer-Cache-Capability": session["capability"]})

    def read_policy(session):
        status, state = request("GET", POLICY + "?" + urllib.parse.urlencode({"protocol": 1, **scope(session)}), headers={"X-Pacer-Cache-Capability": session["capability"]})
        check(status == 200, f"policy read failed: {status} {state}")
        session["state"] = state
        return state

    def reference(session, kind="primary"):
        state = session["state"]
        return {"X-Pacer-Cache-" + key: str(value) for key, value in {
            "Protocol": 1, "Installation": INSTALLATION, "Session": session["id"], "Route": native.ROUTE,
            "Epoch": state["active"]["context_epoch"], "Revision": state["revision"], "Capability": session["capability"], "Kind": kind,
        }.items()}

    def replay(session, operation, kind, **extra):
        return request("POST", REPLAY, {"operation": operation, "kind": kind, **extra}, reference(session))

    def start_session(name, role, screenshots=False):
        session = {"id": name, "capability": base64.urlsafe_b64encode(hashlib.sha256(name.encode()).digest()).decode().rstrip("=")}
        status, state = command(session, "ensure", enrollment="new_empty")
        check(status == 200, f"enrollment failed: {status} {state}")
        session["state"] = state
        session["body"] = {"model": native.MODEL, "max_tokens": 2048, "stream": False,
                           "system": [{"type": "text", "text": "Synthetic stable replay system."}],
                           "messages": [{"role": "user", "content": [{"type": "text", "text": "Synthetic prefix " + name,
                                                                         "cache_control": {"type": "ephemeral", "ttl": "5m"}}]}]}
        if screenshots:
            session["body"]["messages"][0]["content"].extend([
                {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "abcd" * (1 << 18)}}
                for _ in range(5)
            ])
        headers = reference(session)
        headers.update({"X-Pacer-Replay": "v=1; role=" + role, "X-Pacer-Request": name + "-primary"})
        status, answer = request("POST", "/v1/messages", session["body"], headers)
        check(status == 200, f"primary failed: {status} {answer}")
        session["wire"] = copy.deepcopy(fixture.attempts[-1])
        return session

    process = None
    log = (root / "server.log").open("w")
    try:
        process = subprocess.Popen([str(binary), "--config", str(root / "config.yaml"), "--local-model"], cwd=root, env=env, stdout=log, stderr=subprocess.STDOUT)
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            check(process.poll() is None, "CPA exited during startup")
            try:
                with opener.open(base + "/v0/resource/plugins/pacer/events", timeout=1) as response:
                    discovery = json.load(response)
                    if response.status == 200 and discovery.get("api"):
                        break
            except (OSError, ValueError):
                threading.Event().wait(0.2)
        else:
            raise AssertionError("CPA did not become ready")
        root_session = start_session("root-keepalive", "root", screenshots=True)
        check(len(json.dumps(root_session["body"]).encode()) > 4 << 20, "large ingress scenario below old snapshot limit")
        check(len(json.dumps(root_session["wire"]["body"]).encode()) > 4 << 20, "large final wire scenario below old snapshot limit")
        status, acquired = replay(root_session, "acquire", "keepalive")
        check(status == 200 and acquired.get("state") == "ready", f"root acquire failed: {status} {acquired}")
        root_lease = acquired["lease"]
        check(root_lease["request_id"] and acquired["cache_read_tokens"] > 0, "capture identity or cache proof missing")
        result["request_id_correlated"] = root_lease["request_id"] == "root-keepalive-primary"
        checked("authenticated-native-ABI-primary-capture-and-root-acquire")
        status, oversized_native = replay(root_session, "acquire", "native_compaction")
        check(status == 200 and oversized_native.get("reason") == "snapshot_too_large" and not oversized_native.get("lease"),
              "large native command must be declined before client upload")
        checked("large-image-history-keeps-ping-authority-and-reports-native-command-limit")

        count = len(fixture.attempts)
        for name, headers, caller in [
            ("unauthenticated", reference(root_session), None),
            ("unknown-key", reference(root_session), "forged"),
            ("foreign-caller", reference(root_session), native.CALLERS[1]),
            ("ungranted-caller", reference(root_session), native.CALLERS[2]),
            ("missing-capability", {key: value for key, value in reference(root_session).items() if not key.endswith("Capability")}, native.CALLERS[0]),
            ("forged-capability", {**reference(root_session), "X-Pacer-Cache-Capability": "A" * 43}, native.CALLERS[0]),
        ]:
            status, answer = request("POST", REPLAY, {"operation": "acquire", "kind": "keepalive"}, headers, caller)
            check(status >= 400 or answer.get("state") == "unavailable", f"{name} accepted: {status} {answer}")
            check(len(fixture.attempts) == count, name + " reached upstream")
            checked(name + "-denied-before-upstream")

        subagent = start_session("subagent-expiry", "subagent")
        count = len(fixture.attempts)
        status, answer = replay(subagent, "acquire", "keepalive")
        check(status == 200 and answer.get("state") == "unavailable" and len(fixture.attempts) == count, f"subagent ping admitted: {status} {answer}")
        checked("subagent-cannot-acquire-or-send-keepalive")
        status, expiring = replay(subagent, "acquire", "native_compaction")
        check(status == 200 and expiring.get("state") == "ready", f"subagent native lease unavailable: {status} {expiring}")
        status, pending = command(subagent, "prepare", ttl="1h")
        check(status == 200 and pending.get("pending"), f"expiry transition prepare failed: {status} {pending}")
        subagent["state"] = pending

        session = start_session("native-transition", "root")
        status, acquired = replay(session, "acquire", "native_compaction")
        check(status == 200 and acquired.get("state") == "ready", f"native acquire failed: {status} {acquired}")
        status, pending = command(session, "prepare", ttl="1h")
        check(status == 200 and pending.get("pending") and pending["active"]["ttl"] == "5m", f"prepare failed: {status} {pending}")
        session["state"] = pending
        candidate = {**copy.deepcopy(session["body"]), "compaction": {"type": "summarize"}}
        count = len(fixture.attempts)
        for name, headers, caller, token in [
            ("unauthenticated-execute", reference(session), None, acquired["lease"]["token"]),
            ("foreign-caller-execute", reference(session), native.CALLERS[1], acquired["lease"]["token"]),
            ("missing-capability-execute", {key: value for key, value in reference(session).items() if not key.endswith("Capability")}, native.CALLERS[0], acquired["lease"]["token"]),
            ("forged-capability-execute", {**reference(session), "X-Pacer-Cache-Capability": "A" * 43}, native.CALLERS[0], acquired["lease"]["token"]),
            ("forged-token-execute", reference(session), native.CALLERS[0], "A" * 43),
        ]:
            status, answer = request("POST", REPLAY, {"operation": "execute", "kind": "native_compaction", "token": token, "request": candidate}, headers, caller)
            check(status >= 400 or answer.get("state") == "unavailable", f"{name} accepted: {status} {answer}")
            check(len(fixture.attempts) == count, name + " reached upstream")
            checked(name + "-denied-before-upstream")
        checkpoint = "portable-native-checkpoint"
        receipt = {"version": 1, "transition_id": pending["pending"]["transition_id"], "previous_checkpoint": pending["pending"]["from_checkpoint"], "checkpoint": checkpoint, "committed": True}
        pool = concurrent.futures.ThreadPoolExecutor(max_workers=1)
        try:
            execution = pool.submit(replay, session, "execute", "native_compaction", token=acquired["lease"]["token"], request=candidate)
            try:
                check(fixture.native_started.wait(20), f"native replay did not reach upstream; HTTP evidence: {root / 'http-events.json'}")
                inflight = read_policy(session)
                check(inflight.get("active_native") == 1 and inflight["active_attempts"] == 1 and inflight["active"]["ttl"] == "5m", f"native lifecycle not guarded: {inflight}")
                status, answer = command(session, "commit", ttl="1h", context_epoch=checkpoint, checkpoint=checkpoint, transition_id=receipt["transition_id"], checkpoint_receipt=receipt)
                check(status == 409, f"in-flight native commit was accepted: {status} {answer}")
                checked("native-transition-holds-old-TTL-and-blocks-inflight-commit")
            finally:
                fixture.native_release.set()
            status, answer = execution.result(timeout=20)
        finally:
            fixture.native_release.set()
            pool.shutdown(wait=False, cancel_futures=True)
        check(status == 200 and answer.get("state") == "completed" and answer.get("settled") is True, f"native completion not settled: {status} {answer}")
        settled = read_policy(session)
        check(settled.get("active_native", 0) == 0 and settled["active_attempts"] == 0 and settled["pending"] and settled["active"]["ttl"] == "5m", f"native settlement invalid: {settled}")
        native_wire = fixture.attempts[-1]
        markers = cache_markers(native_wire["body"])
        check(markers and all(marker.get("ttl", "5m") == "5m" for marker in markers), "native replay lost original cache TTL")
        check(native_wire["account"] == session["wire"]["account"], "native replay changed credentials")
        for key in ("system", "messages", "tools", "metadata"):
            check(normalize_cch(native_wire["body"].get(key)) == normalize_cch(session["wire"]["body"].get(key)), "native replay changed final prefix: " + key)
        checked("native-replay-preserves-final-prefix-exact-credential-and-settles-active-native")
        durable_checkpoint(root / "portable-checkpoint.json", {"checkpoint": checkpoint, "receipt": receipt, "message": answer["response"]})
        status, committed = command(session, "commit", ttl="1h", context_epoch=checkpoint, checkpoint=checkpoint, transition_id=receipt["transition_id"], checkpoint_receipt=receipt)
        check(status == 200 and committed["active"]["ttl"] == "1h" and committed["active"]["checkpoint"] == checkpoint and not committed.get("pending"), f"checkpoint commit failed: {status} {committed}")
        checked("portable-checkpoint-commit-selects-target-TTL")

        check(result["request_id_correlated"], "supplied primary request ID was lost before replay capture")
        target_ms = root_lease["not_before_ms"] + 250
        print(f"Waiting for advertised keepalive not_before_ms={target_ms}; no inference while waiting.", flush=True)
        while time.time() * 1000 < target_ms:
            threading.Event().wait(min(30, (target_ms - time.time() * 1000) / 1000))
        status, refreshed = replay(root_session, "execute", "keepalive", token=root_lease["token"])
        check(status == 200 and refreshed.get("state") == "refreshed" and refreshed.get("settled") is True, f"root keepalive failed: {status} {refreshed}")
        ping_wire = fixture.attempts[-1]
        check(ping_wire["kind"] == "keepalive" and ping_wire["account"] == root_session["wire"]["account"], "keepalive changed credential")
        expected = copy.deepcopy(root_session["wire"]["body"])
        expected.update({"max_tokens": 0, "stream": False})
        check(normalize_cch(ping_wire["body"]) == normalize_cch(expected), "keepalive changed captured final wire")
        checked("large-image-root-keepalive-replays-final-wire-with-the-same-credential")

        target_ms = expiring["lease"]["expires_ms"] + 250
        print(f"Waiting for unused native lease expiry_ms={target_ms}.", flush=True)
        while time.time() * 1000 < target_ms:
            threading.Event().wait(min(30, (target_ms - time.time() * 1000) / 1000))
        count = len(fixture.attempts)
        status, answer = replay(subagent, "execute", "native_compaction", token=expiring["lease"]["token"], request={**subagent["body"], "compaction": {"type": "summarize"}})
        check(status == 200 and answer.get("state") == "unavailable" and len(fixture.attempts) == count, f"expired lease dispatched: {status} {answer}")
        checked("expired-replay-token-denied-before-upstream")
        check(len(fixture.attempts) == 5, f"unexpected physical request count: {len(fixture.attempts)}")
        check(not any(attempt["coordination_leaked"] for attempt in fixture.attempts), "coordination headers leaked upstream")
        checked("five-fake-provider-calls-no-coordination-leaks-zero-real-calls")
        result["status"] = "passed"
    except Exception as error:
        result.update({"status": "failed", "error": repr(error)})
        raise
    finally:
        fixture.native_release.set()
        if process is not None:
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        log.close()
        fixture.shutdown()
        fixture.server_close()
        result["fake_provider_calls"] = len(fixture.attempts)
        (root / "final-wire.json").write_text(json.dumps(fixture.attempts, indent=2) + "\n")
        (root / "result.json").write_text(json.dumps(result, indent=2) + "\n")
        print(f"Replay result: {root / 'result.json'}", flush=True)


if __name__ == "__main__":
    main()

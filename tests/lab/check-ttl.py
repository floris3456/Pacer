"""Native CPA/plugin/SQLite/Claude final-boundary acceptance; offline dummy lab only."""
import base64
import concurrent.futures
import json
import os
import pathlib
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

BASE = "http://pacer-lab-cpa.orb.local:8317"
UPSTREAM = "http://pacer-upstream.orb.local:8080"
http = urllib.request.build_opener(urllib.request.ProxyHandler({}))
checks = []


def call(url, body=None, headers=None, method=None, timeout=40):
    request = urllib.request.Request(url, data=None if body is None else json.dumps(body).encode(),
                                    headers={"Content-Type": "application/json", **(headers or {})}, method=method)
    try:
        with http.open(request, timeout=timeout) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as error:
        return error.code, error.read()


def checked(name, condition):
    assert condition, name
    checks.append({"name": name, "pass": True})


def cache_ttls(value):
    if isinstance(value, list):
        return [ttl for item in value for ttl in cache_ttls(item)]
    if isinstance(value, dict):
        own = [value["cache_control"].get("ttl", "5m")] if isinstance(value.get("cache_control"), dict) else []
        return own + [ttl for key, item in value.items() if key != "cache_control" for ttl in cache_ttls(item)]
    return []


cap = base64.urlsafe_b64encode(os.urandom(32)).decode().rstrip("=")
session = "native-ttl-" + uuid.uuid4().hex
scope = dict(installation_id="pacer-offline-lab", session_id=session, provider_route="claude")
headers = {"Authorization": "Bearer pacer-test-inference", "X-Pacer-Cache-Capability": cap}


def command(operation, state=None, ttl="1h", **extra):
    body = dict(protocol=1, operation=operation, **scope,
                context_epoch=state["active"]["context_epoch"] if state else "empty",
                expected_revision=state["revision"] if state else 0, ttl=ttl,
                operation_id=uuid.uuid4().hex)
    body.update(extra)
    status, raw = call(BASE + "/v1/plugins/pacer/cache-policy", body, headers)
    return status, json.loads(raw)


def infer(state, stream=False, hold=False):
    body = {"model": "claude-opus-5-5", "max_tokens": 64, "stream": stream,
            "system": [{"type": "text", "text": "Offline fixture", "cache_control": {"type": "ephemeral", "ttl": "5m"}}],
            "messages": [{"role": "user", "content": "PACER_HOLD" if hold else "Dummy context only, no reasoning"}]}
    return call(BASE + "/v1/messages", body, {**headers, "X-Pacer-Cache-Protocol": "1",
                "X-Pacer-Cache-Installation": scope["installation_id"], "X-Pacer-Cache-Session": scope["session_id"],
                "X-Pacer-Cache-Route": scope["provider_route"], "X-Pacer-Cache-Epoch": state["active"]["context_epoch"],
                "X-Pacer-Cache-Revision": str(state["revision"]), "X-Pacer-Cache-Kind": "primary"})


def commit(pending):
    checkpoint = "checkpoint-" + uuid.uuid4().hex
    return command("commit", pending, ttl=pending["pending"]["ttl"],
                   transition_id=pending["pending"]["transition_id"], checkpoint=checkpoint, context_epoch=checkpoint,
                   checkpoint_receipt={"version": 1, "transition_id": pending["pending"]["transition_id"],
                                       "previous_checkpoint": pending["pending"]["from_checkpoint"],
                                        "checkpoint": checkpoint, "committed": True})


def read_effective(ttl):
    # Bounded native asynchronous-delivery wait, never a delivery watermark.
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        status, raw = call(BASE + "/v1/plugins/pacer/cache-policy?" + query, headers=headers,
                           timeout=max(.1, min(2, deadline - time.monotonic())))
        assert status == 200, status
        result = json.loads(raw)
        if result.get("last_effective_ttl") == ttl:
            checked("authorized-native-marker-readback-" + ttl, True)
            checked("marker-readback-does-not-invent-refresh-" + ttl,
                    not any(result.get(key) is not None for key in ("cache_refresh_at", "protected_until")))
            return result
        time.sleep(.05)
    raise AssertionError("Native marker-TTL readback did not arrive within its deadline")


assert json.loads(call(UPSTREAM + "/_test/activity")[1])["active_messages"] == 0
status, discovery = call(BASE + "/v0/resource/plugins/pacer/events")
checked("schema7-discovery-advertises-policy", status == 200 and json.loads(discovery)["cache_policy"]["contract_revision"] == 2)
status, state = command("ensure", enrollment="new_empty")
assert status == 200, (status, state)
checked("explicit-trusted-installation-enrollment", state["active"]["ttl"] == "1h")
status, body = infer(state)
assert status == 200, (status, body.decode()[:700])
last = json.loads(call(UPSTREAM + "/_test/attempts")[1])[-1]
checked("final-Claude-body-rewrites-5m-to-selected-1h", bool(cache_ttls(last["body"])) and set(cache_ttls(last["body"])) == {"1h"} and not last["coordination_leaked"])

# Refresh acknowledged state after logical completion marks this history used.
query = urllib.parse.urlencode(dict(protocol=1, **scope))
state = read_effective("1h")
assert call(UPSTREAM + "/_test/hold", {})[0] == 200
with concurrent.futures.ThreadPoolExecutor(max_workers=1) as pool:
    response = pool.submit(infer, state, True, True)
    deadline = time.monotonic() + 10
    while True:
        activity = json.loads(call(UPSTREAM + "/_test/activity")[1])
        if activity["active_messages"]:
            break
        assert time.monotonic() < deadline, "held stream did not reach fake upstream"
        time.sleep(.05)
    status, pending = command("prepare", state, ttl="5m")
    assert status == 200, (status, pending)
    try:
        before = len(json.loads(call(UPSTREAM + "/_test/attempts")[1]))
        status, refused = infer(state)
        checked("pending-transition-refuses-new-primary-before-upstream", status == 409 and len(json.loads(call(UPSTREAM + "/_test/attempts")[1])) == before)
        status, refused = commit(pending)
        checked("inflight-stream-prevents-commit", status == 409 and refused["error"]["code"] == "pacer_cache_inflight")
    finally:
        assert call(UPSTREAM + "/_test/release", {})[0] == 200
    status, body = response.result(timeout=10)
    checked("old-snapshot-stream-finishes-under-old-policy", status == 200 and b"message_stop" in body)

status, active = commit(pending)
assert status == 200, (status, active)
checked("transition-bound-checkpoint-commits-after-shutdown", active["active"]["ttl"] == "5m" and active.get("pending") is None)
status, raw = call(BASE + "/v1/plugins/pacer/cache-policy?" + query, headers=headers)
checked("new-epoch-does-not-borrow-old-marker-evidence", status == 200 and json.loads(raw).get("last_effective_ttl") is None)
status, body = infer(active)
assert status == 200, (status, body.decode()[:700])
last = json.loads(call(UPSTREAM + "/_test/attempts")[1])[-1]
checked("new-context-emits-selected-5m", bool(cache_ttls(last["body"])) and set(cache_ttls(last["body"])) == {"5m"})
read_effective("5m")
status, raw = call(BASE + "/v1/plugins/pacer/cache-policy?" + query,
                   headers={**headers, "X-Pacer-Cache-Capability": base64.urlsafe_b64encode(os.urandom(32)).decode().rstrip("=")})
checked("foreign-capability-cannot-read-policy", status == 403 and b"last_effective_ttl" not in raw)
out = pathlib.Path(os.environ.get("PACER_EVIDENCE", "../../evidence/implementation"))
(out / "native-ttl.json").write_text(json.dumps(checks, indent=2) + "\n")
print(json.dumps(checks, indent=2))

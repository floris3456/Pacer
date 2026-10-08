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


def call(url, body=None, headers=None, method=None):
    request = urllib.request.Request(url, data=None if body is None else json.dumps(body).encode(),
                                    headers={"Content-Type": "application/json", **(headers or {})}, method=method)
    try:
        with http.open(request, timeout=40) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as error:
        return error.code, error.read()


def checked(name, condition):
    assert condition, name
    checks.append({"name": name, "pass": True})


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


assert call(UPSTREAM + "/_test/reset", {})[0] == 200
status, discovery = call(BASE + "/v0/resource/plugins/pacer/events")
checked("schema7-discovery-advertises-policy", status == 200 and json.loads(discovery)["cache_policy"]["contract_revision"] == 2)
status, state = command("ensure", enrollment="new_empty")
assert status == 200, (status, state)
checked("explicit-trusted-installation-enrollment", state["active"]["ttl"] == "1h")
status, body = infer(state)
assert status == 200, (status, body.decode()[:700])
last = json.loads(call(UPSTREAM + "/_test/attempts")[1])[-1]
checked("final-Claude-body-rewrites-5m-to-selected-1h", bool(last["cache_ttls"]) and set(last["cache_ttls"]) == {"1h"} and not last["coordination_leaked"])

# Refresh acknowledged state after logical completion marks this history used.
query = urllib.parse.urlencode(dict(protocol=1, **scope))
status, raw = call(BASE + "/v1/plugins/pacer/cache-policy?" + query, headers=headers)
assert status == 200, (status, raw)
state = json.loads(raw)
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
status, body = infer(active)
assert status == 200, (status, body.decode()[:700])
last = json.loads(call(UPSTREAM + "/_test/attempts")[1])[-1]
checked("new-context-emits-selected-5m", bool(last["cache_ttls"]) and set(last["cache_ttls"]) == {"5m"})
status, raw = call(BASE + "/v1/plugins/pacer/cache-policy?" + query,
                   headers={**headers, "X-Pacer-Cache-Capability": base64.urlsafe_b64encode(os.urandom(32)).decode().rstrip("=")})
checked("foreign-capability-cannot-read-policy", status == 403)
out = pathlib.Path(os.environ.get("PACER_EVIDENCE", "../../evidence/implementation"))
(out / "native-ttl.json").write_text(json.dumps(checks, indent=2) + "\n")
print(json.dumps(checks, indent=2))

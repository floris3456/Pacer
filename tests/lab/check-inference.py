"""Real native CPA + fake TLS Claude: no live provider or cross-account reasoning."""
import base64
import hashlib
import json
import os
import pathlib
import urllib.error
import urllib.request
import uuid

base = "http://pacer-lab-cpa.orb.local:8317"
upstream = "http://pacer-upstream.orb.local:8080"
http = urllib.request.build_opener(urllib.request.ProxyHandler({}))
checks = []


def call(url, body=None, headers=None):
    request = urllib.request.Request(url, data=None if body is None else json.dumps(body).encode(),
                                     headers={"Content-Type": "application/json", **(headers or {})})
    try:
        with http.open(request, timeout=20) as response:
            return response.status, response.read(), dict(response.headers)
    except urllib.error.HTTPError as error:
        return error.code, error.read(), dict(error.headers)


capability = uuid.uuid4().hex + uuid.uuid4().hex
session = "pacer-fixture-" + uuid.uuid4().hex
claim = dict(protocol=1, installation="pacer-offline-lab", session=session, route="claude/opus",
             request="logical", kind="primary", capability=hashlib.sha256(capability.encode()).hexdigest(),
             auto=True, compact_on_switch=True, checkpoint="checkpoint-before")


def infer(stream=False, text="Dummy test context, no reasoning"):
    headers = {"Authorization": "Bearer pacer-test-inference", "X-Session-Affinity": session,
               "X-Quota-Handoff": base64.urlsafe_b64encode(json.dumps(claim).encode()).decode().rstrip("=")}
    return call(base + "/v1/messages", {"model": "claude-opus-5-5", "max_tokens": 64, "stream": stream,
                                        "messages": [{"role": "user", "content": text}]}, headers)


status, body, headers = infer()
assert status == 200, (status, body.decode()[:500])
attempts = json.loads(call(upstream + "/_test/attempts")[1])
account = attempts[-1]["account"]
assert not attempts[-1]["coordination_leaked"]
assert headers.get("X-Quota-Protocol") == "1", headers
checks.append({"name": "native-coordination-ack-and-header-stripping", "pass": True})

status, body, _ = infer(stream=True)
assert status == 200 and b"message_stop" in body, (status, body.decode()[:500])
checks.append({"name": "stream-fake-upstream", "pass": True})

status, body, _ = call(base + "/v0/resource/plugins/pacer/events?model=claude%2Fopus", headers={"Authorization": "Bearer " + capability})
assert status == 200, (status, body)
snapshot = json.loads(body)
assert snapshot["mode"] == "off" and snapshot["primary_used"] and len(snapshot["windows"]) == 2, snapshot
checks.append({"name": "capability-snapshot-passive-quota", "pass": True})

before = json.loads(call(upstream + "/_test/analytics")[1])["calls"]
for _ in range(20):
    status, _, _ = call(base + "/v0/management/plugins/pacer/state", headers={"Authorization": "Bearer pacer-test-admin"})
    assert status == 200
after = json.loads(call(upstream + "/_test/analytics")[1])["calls"]
assert before == after
checks.append({"name": "management-reads-zero-quota-calls", "pass": True, "calls": after-before})

attempts_before = len(json.loads(call(upstream + "/_test/attempts")[1]))
assert call(upstream + "/_test/exhaust", {"accounts": [account]})[0] == 200
status, body, _ = infer()
assert status == 409 and b"quota_compact_required" in body, (status, body.decode()[:500])
new_attempts = json.loads(call(upstream + "/_test/attempts")[1])[attempts_before:]
assert all(item["account"] == account for item in new_attempts), "uncompacted context reached another account"
checks.append({"name": "upstream-429-forced-switch-409-before-next-account", "pass": True})

claim["kind"] = "compaction"
status, body, _ = infer(text="Dummy summary call")
assert status == 200, (status, body.decode()[:500])
claim["kind"] = "primary"
status, body, _ = infer()
assert status == 409 and b"quota_compact_required" in body, (status, body.decode()[:500])
claim["checkpoint"] = "checkpoint-committed"
status, body, _ = infer(text="Dummy committed summary only")
assert status == 200, (status, body.decode()[:500])
last = json.loads(call(upstream + "/_test/attempts")[1])[-1]
assert last["account"] != account and not last["coordination_leaked"]
checks.append({"name": "summary-insufficient-committed-checkpoint-admitted", "pass": True})

out = pathlib.Path(os.environ.get("PACER_EVIDENCE", "../../evidence/implementation"))
out.mkdir(parents=True, exist_ok=True)
(out / "native-inference.json").write_text(json.dumps(checks, indent=2) + "\n")
print(json.dumps(checks, indent=2))

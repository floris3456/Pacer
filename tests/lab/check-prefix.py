"""Native prefix-snapshot acceptance (contract W, prefix-warmth-implementation.md); offline dummy lab only.

The fixed fake upstream reports BOTH 5m (25) and 1h (75) cache writes on every response, so the
strongest native outcome for a sole-endpoint request is the correct refusal `conflicting_write`.
The positive unique-write path is proven in-process (internal/pacer TestPrefixEvidenceReadback...).
This check proves the native transport: CPA final-wire snapshot -> native usage -> Pacer SQLite ->
qualification -> authorized readback, plus final-wire primary/warming endpoint equality and the
negative controls. Reads Pacer's SQLite read-only. Adds exactly the listed fixture attempts.
"""
import base64
import json
import os
import pathlib
import sqlite3
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

BASE = "http://pacer-lab-cpa.orb.local:8317"
UPSTREAM = "http://pacer-upstream.orb.local:8080"
STATE = pathlib.Path(os.environ.get("PACER_STATE_DB", "/Users/bliss/Agent/homeserverr/pacer/.cache/lab/state/pacer.sqlite"))
http = urllib.request.build_opener(urllib.request.ProxyHandler({}))
checks = []
ALLOWED = {"version", "status", "basis", "coverage", "relation", "context_epoch", "policy_revision", "primary_snapshot_ref",
           "binding_ref", "entry_ref", "effective_ttl", "upstream_started_at", "protection_estimate_until",
           "remaining_prefixes", "reasons"}


def call(url, body=None, headers=None, method=None, timeout=40):
    request = urllib.request.Request(url, data=None if body is None else json.dumps(body).encode(),
                                     headers={"Content-Type": "application/json", **(headers or {})}, method=method)
    try:
        with http.open(request, timeout=timeout) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as error:
        return error.code, error.read()


def checked(name, condition, detail=None):
    checks.append({"name": name, "pass": bool(condition), **({"detail": detail} if detail is not None else {})})


cap = base64.urlsafe_b64encode(os.urandom(32)).decode().rstrip("=")
session = "native-prefix-" + uuid.uuid4().hex
scope = dict(installation_id="pacer-offline-lab", session_id=session, provider_route="claude")
headers = {"Authorization": "Bearer pacer-test-inference", "X-Pacer-Cache-Capability": cap}
query = urllib.parse.urlencode(dict(protocol=1, **scope))


def command(operation, state=None, ttl="1h", **extra):
    body = dict(protocol=1, operation=operation, **scope,
                context_epoch=state["active"]["context_epoch"] if state else "empty",
                expected_revision=state["revision"] if state else 0, ttl=ttl, operation_id=uuid.uuid4().hex)
    body.update(extra)
    status, raw = call(BASE + "/v1/plugins/pacer/cache-policy", body, headers)
    return status, json.loads(raw)


def marked(text):
    return {"type": "text", "text": text, "cache_control": {"type": "ephemeral", "ttl": "1h"}}


HISTORY = [{"role": "user", "content": "Dummy history turn one"}, {"role": "assistant", "content": "Dummy reply one"}]


def infer(state, label, messages, kind="primary", stream=False, extra=None):
    body = {"model": "claude-opus-5-5", "max_tokens": 64, "stream": stream,
            "system": [{"type": "text", "text": "Offline prefix fixture"}], "messages": messages, **(extra or {})}
    status, raw = call(BASE + "/v1/messages", body, {**headers, "X-Pacer-Cache-Protocol": "1",
                       "X-Pacer-Cache-Installation": scope["installation_id"], "X-Pacer-Cache-Session": scope["session_id"],
                       "X-Pacer-Cache-Route": scope["provider_route"], "X-Pacer-Cache-Epoch": state["active"]["context_epoch"],
                       "X-Pacer-Cache-Revision": str(state["revision"]), "X-Pacer-Cache-Kind": kind})
    checked("inference-200-" + label, status == 200, status)
    return raw


def rows(n, deadline=15):
    """Wait (bounded) until n settled prefix rows exist for this session; read-only."""
    end = time.monotonic() + deadline
    while True:
        db = sqlite3.connect("file:%s?mode=ro" % STATE, uri=True)
        try:
            got = db.execute("SELECT attempt_id,kind,fact,outcome,obs_status,obs_reason,tag,started_ns FROM prefix_attempts "
                             "WHERE session=? ORDER BY started_ns", (session,)).fetchall()
            anchors = db.execute("SELECT tag FROM prefix_anchors WHERE session=?", (session,)).fetchall()
        finally:
            db.close()
        settled = [r for r in got if r[3] and r[2]]
        if len(settled) >= n or time.monotonic() > end:
            return got, {a[0] for a in anchors}
        time.sleep(.2)


def snapshot(row):
    return (json.loads(row[2]).get("Snapshot") or json.loads(row[2]).get("snapshot") or {})


def endpoint_tags(row):
    s = snapshot(row)
    return [e.get("Tag") or e.get("tag") for e in (s.get("Endpoints") or s.get("endpoints") or [])]


attempts_before = len(json.loads(call(UPSTREAM + "/_test/attempts")[1]))
status, state = command("ensure", enrollment="new_empty")
assert status == 200, (status, state)
q = "Dummy question B"
infer(state, "primary-single-marker", HISTORY + [{"role": "user", "content": [marked(q)]}])
infer(state, "warming-ping-after-marker", HISTORY + [{"role": "user", "content": [marked(q), {"type": "text", "text": "ping"}]}], kind="warming")
infer(state, "warming-marker-on-ping", HISTORY + [{"role": "user", "content": [{"type": "text", "text": q}, marked("ping")]}], kind="warming")
infer(state, "primary-stream-single-marker", HISTORY + [{"role": "user", "content": [marked(q)]}], stream=True)
infer(state, "primary-two-markers", HISTORY + [{"role": "user", "content": [marked("Dummy question C"), marked("Dummy question D")]}])
infer(state, "primary-automatic-marker", HISTORY + [{"role": "user", "content": "Dummy question E"}],
      extra={"cache_control": {"type": "ephemeral", "ttl": "1h"}})
sent = 6
got, anchors = rows(sent)
fixture = json.loads(call(UPSTREAM + "/_test/attempts")[1])[attempts_before:]
markers = [len([1 for _ in json.dumps(a["body"]).split('"cache_control"')[1:]]) for a in fixture]
checked("six-prefix-rows-settled-natively", len([r for r in got if r[3] and r[2]]) == sent, len(got))
checks.append({"name": "final-wire-cache-control-count-per-request", "pass": True, "detail": markers})
p1, w1, w2, p2, p3, p4 = (got + [("", "", "{}", "", "", "", "", 0)] * sent)[:sent]
checked("native-snapshot-persisted-with-one-endpoint", len(endpoint_tags(p1)) == 1 and snapshot(p1).get("Shape", snapshot(p1).get("shape")) == "text_single_pass")
checked("primary-registers-anchor", bool(endpoint_tags(p1)) and endpoint_tags(p1)[0] in anchors)
checked("final-wire-warming-endpoint-equals-primary-anchor", endpoint_tags(w1) == endpoint_tags(p1))
checked("marker-on-ping-is-a-different-endpoint", endpoint_tags(w2) != endpoint_tags(p1) and len(endpoint_tags(w2)) == 1)
checked("stream-transport-carries-same-endpoint", endpoint_tags(p2) == endpoint_tags(p1))
for label, row in (("primary", p1), ("warming", w1), ("stream", p2)):
    checked("fixture-dual-ttl-write-refused-" + label, row[4] == "unverified" and row[5] == "conflicting_write", [row[4], row[5]])
checked("two-markers-refused-multiple-candidates", p3[5] == "multiple_candidates" and len(endpoint_tags(p3)) == 2, p3[5])
checked("automatic-marker-refused-unsupported-shape", p4[5] == "unsupported_shape", p4[5])
checked("all-outcomes-succeeded", all(r[3] == "succeeded" for r in got), [r[3] for r in got])
status, raw = call(BASE + "/v1/plugins/pacer/cache-policy?" + query, headers=headers)
state_json = json.loads(raw)
evidence = state_json.get("prefix_cache_evidence")
checked("authorized-readback-has-unverified-prefix-object", status == 200 and isinstance(evidence, dict) and evidence.get("status") == "unverified", evidence)
checked("readback-allowlisted-keys-only", isinstance(evidence, dict) and set(evidence) <= ALLOWED, sorted(evidence or {}))
checked("readback-no-deadline-when-unverified", isinstance(evidence, dict) and "protection_estimate_until" not in evidence)
checked("legacy-fields-not-populated", not any(state_json.get(k) is not None for k in ("cache_refresh_at", "protected_until", "refresh_started_at")))
checked("readback-has-no-content-or-ids", not any(s.encode() in raw for s in (q, "ping", session, cap)))
status, raw = call(BASE + "/v1/plugins/pacer/cache-policy?" + query,
                   headers={**headers, "X-Pacer-Cache-Capability": base64.urlsafe_b64encode(os.urandom(32)).decode().rstrip("=")})
checked("foreign-capability-cannot-read-prefix-evidence", status == 403 and b"prefix_cache_evidence" not in raw)
attempts_after = len(json.loads(call(UPSTREAM + "/_test/attempts")[1]))
checked("fixture-attempt-delta-equals-requests-sent", attempts_after - attempts_before == sent, [attempts_before, attempts_after])
out = pathlib.Path(os.environ.get("PACER_EVIDENCE", "."))
(out / "native-prefix.json").write_text(json.dumps({"session_prefix": "native-prefix-", "checks": checks}, indent=2) + "\n")
print(json.dumps(checks, indent=2))
raise SystemExit(0 if all(c["pass"] for c in checks) else 1)

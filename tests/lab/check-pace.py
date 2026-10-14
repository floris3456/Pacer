"""Pace routing through a real CPA host and the native Pacer library.

Offline lab only (see README, "Pace routing lab"): CPA, Pacer and
capacity_upstream.py on an internal Docker network, with three dummy Claude
logins. The meters are the docs/pace-routing.md example. No live provider use.
"""
import base64
import hashlib
import json
import os
import time
import urllib.error
import urllib.request

CPA = os.environ.get("PACE_LAB_CPA", "http://pace-lab-cpa:8317")
FIXTURE = os.environ.get("PACE_LAB_FIXTURE", "http://pace-lab-upstream:8080")
if CPA != "http://pace-lab-cpa:8317" or FIXTURE != "http://pace-lab-upstream:8080":
    raise SystemExit("Only the isolated pace-routing lab is allowed")
ADMIN, INFERENCE = "pacer-test-admin", "pacer-test-inference"
http = urllib.request.build_opener(urllib.request.ProxyHandler({}))
checks = []


def call(url, method="GET", body=None, key=None, headers=None):
    data = None if body is None else json.dumps(body).encode()
    request = urllib.request.Request(url, data=data, method=method, headers={"Content-Type": "application/json", **({"Authorization": "Bearer " + key} if key else {}), **(headers or {})})
    try:
        with http.open(request, timeout=30) as response:
            return response.status, json.loads(response.read() or b"null")
    except urllib.error.HTTPError as error:
        raw = error.read()
        try:
            return error.code, json.loads(raw)
        except ValueError:
            return error.code, raw.decode(errors="replace")


def check(name, ok, detail=None):
    checks.append({"check": name, "pass": bool(ok), **({"detail": detail} if detail is not None else {})})
    print(("PASS " if ok else "FAIL ") + name + ("" if ok or detail is None else " " + json.dumps(detail)[:600]), flush=True)


def state():
    status, body = call(CPA + "/v0/management/plugins/pacer/state", key=ADMIN)
    assert status == 200, body
    return body


def accounts():
    return {a["id"].removesuffix(".json"): a for a in state()["accounts"]}


def meters(account, five, five_reset, weekly, weekly_reset):
    status, body = call(FIXTURE + "/_test/usage", "POST", {"account": account, "five_hour": five, "seven_day": weekly, "five_hour_reset": five_reset, "seven_day_reset": weekly_reset, "partial_window": False})
    assert status == 200, body


def fail(account, count):
    status, body = call(FIXTURE + "/_test/fail", "POST", {"account": account, "count": count})
    assert status == 200, body


def read_meters(expect, changed):
    """Queue a meter read of the changed accounts and wait until every account shows
    the expected 7-day % left and 5-hour state."""
    for name, a in accounts().items():
        if name in changed:
            status, body = call(CPA + "/v0/management/plugins/pacer/observations/reconcile", "POST", {"auth_index": a["auth_index"]}, key=ADMIN)
            assert status == 200, body
    deadline = time.time() + 420  # first reads after startup: 1-5 min jitter, 30 s apart
    while time.time() < deadline:
        current = accounts()
        seen = {n: {w["seconds"]: w for w in a["windows"] if w["scope"] == "all"} for n, a in current.items()}
        if all(seen[n].get(604800, {}).get("remaining") == left and (dormant is None or (seen[n].get(18000, {}).get("state") == "dormant") == dormant) for n, (left, dormant) in expect.items()):
            return current
        time.sleep(2)
    raise SystemExit("meters not read: " + json.dumps({n: a["windows"] for n, a in accounts().items()})[:2000])


def chat(session, model="claude-opus-5-5"):
    before = {k: v.get("/v1/messages", 0) for k, v in call(FIXTURE + "/_test/attempts")[1]["counts"].items()}
    status, body = call(CPA + "/v1/messages", "POST", {"model": model, "max_tokens": 16, "messages": [{"role": "user", "content": "Reply with ok"}]}, key=INFERENCE, headers={"X-Session-ID": session})
    after = {k: v.get("/v1/messages", 0) for k, v in call(FIXTURE + "/_test/attempts")[1]["counts"].items()}
    served = [k for k in after if after[k] > before.get(k, 0)]
    decisions = state()["decisions"]
    return status, served, decisions[0] if decisions else None


def claim_chat(session, checkpoint, request):
    """A request with OpenCode's quota claim (docs/client-protocol.md)."""
    claim = {"protocol": 1, "installation": "pace-lab", "session": session, "route": "claude/lab", "request": request, "kind": "primary",
             "capability": hashlib.sha256(b"pace-lab-capability-" + session.encode()).hexdigest(), "checkpoint": checkpoint, "auto": True, "fresh": False, "compact_on_switch": True}
    header = base64.urlsafe_b64encode(json.dumps(claim).encode()).decode().rstrip("=")
    before = {k: v.get("/v1/messages", 0) for k, v in call(FIXTURE + "/_test/attempts")[1]["counts"].items()}
    status, body = call(CPA + "/v1/messages", "POST", {"model": "claude-opus-5-5", "max_tokens": 16, "messages": [{"role": "user", "content": "Reply with ok"}]}, key=INFERENCE, headers={"X-Quota-Handoff": header})
    after = {k: v.get("/v1/messages", 0) for k, v in call(FIXTURE + "/_test/attempts")[1]["counts"].items()}
    return status, body, [k for k in after if after[k] > before.get(k, 0)], state()["decisions"][0]


def main():
    now = int(time.time())
    run = "lab-%d-" % now  # fresh chats on every run
    s = state()
    check("legacy dry-run YAML loads as off", s["routing"]["mode"] == "off" and s["routing"]["modes"] == ["off", "on"], s["routing"])
    check("dry-run report route removed", call(CPA + "/v0/management/plugins/pacer/dry-run-report", key=ADMIN)[0] == 404)
    # docs/pace-routing.md example: cap-a ~ ae0a, cap-b ~ 1655, cap-c ~ 70f0 (5-hour window not started).
    meters("cap-a", 40, now + 2 * 3600, 87, now + 7 * 3600)
    meters("cap-b", 10, now + 4 * 3600, 52, now + 98 * 3600)
    meters("cap-c", 0, 0, 14, now + 159 * 3600)
    current = read_meters({"cap-a": (13, False), "cap-b": (48, False), "cap-c": (86, True)}, ["cap-a", "cap-b", "cap-c"])
    paces = {n: a.get("pace") for n, a in current.items()}
    check("pace per account from live meters, dormant window usable", paces["cap-a"] and abs(paces["cap-a"] - 13 / 7) < 0.05 and paces["cap-b"] and abs(paces["cap-b"] - 48 / 98) < 0.01 and paces["cap-c"] and abs(paces["cap-c"] - 86 / 159) < 0.01, paces)
    status, body = call(CPA + "/v0/management/plugins/pacer/routing", "PATCH", {"mode": "on"}, key=ADMIN)
    check("routing switched on through management", status == 200 and state()["routing"]["mode"] == "on", body)
    status, served, d = chat(run + "1")
    check("new chat goes to the fastest-expiring 7-day meter", status == 200 and served == ["cap-a"] and d["proposed"]["reason"] == "new_chat" and d["actual_auth"] == "cap-a.json", {"status": status, "served": served, "decision": d})
    check("decision trace records every candidate's pace and meters", d and len(d["proposed"]["candidates"]) == 3 and all(c["known"] and c["budgets"] for c in d["proposed"]["candidates"]), d and d["proposed"]["candidates"])
    status, served, d = chat(run + "1")
    check("warm chat stays (unknown warmth, within 1 h)", status == 200 and served == ["cap-a"] and d["proposed"]["reason"] == "warm", {"served": served, "decision": d and d["proposed"]})
    # One upstream 502 on the warm chat's account: CPA retries the request without
    # it; Pacer refuses that retry instead of moving the chat, so the client gets
    # the (retryable) upstream error and its own retry lands on the same account.
    fail("cap-a", 1)
    status, served, d = chat(run + "1")
    check("upstream 502 on a warm chat: retryable error, no move", status in (502, 503) and served == ["cap-a"] and d["outcome"] == "transient_wait" and d["proposed"]["unoffered"], {"status": status, "served": served, "decision": d})
    status, served, d = chat(run + "1")
    check("the client's retry stays on the warm account", status == 200 and served == ["cap-a"] and d["proposed"]["reason"] == "warm", {"status": status, "served": served, "decision": d and d["proposed"]})
    # Past the retry setting the chat moves.
    status, body = call(CPA + "/v0/management/plugins/pacer/settings", "PATCH", {"transient_retries": 1}, key=ADMIN)
    check("settings saved through management", status == 200 and body["settings"]["transient_retries"] == 1 and "transient_retries" in body["changed"], body)
    status, served, d = chat(run + "outage")  # a second warm chat on cap-a
    placed = status == 200 and served == ["cap-a"]
    fail("cap-a", 5)
    status, served, d = chat(run + "outage")
    held = status in (502, 503) and d["outcome"] == "transient_wait"
    status, served, d = chat(run + "outage")
    check("a lasting failure moves the warm chat after the retries", placed and held and status == 200 and "cap-a" in served and len(served) == 2 and d["proposed"]["reason"] == "forced_move" and d["actual_auth"] != "cap-a.json", {"placed": placed, "held": held, "status": status, "served": served, "decision": d and d["proposed"]})
    fail("cap-a", 0)
    call(CPA + "/v0/management/plugins/pacer/settings", "PATCH", {"transient_retries": 3, "compaction_models": ["claude-sonnet-4-6", "claude-haiku-lab"]}, key=ADMIN)
    # cap-a's 5-hour window is used up. Its next reply carries the meters in its
    # headers (Pacer reads an account's meter at most every 15 minutes).
    meters("cap-a", 99, now + 2 * 3600, 88, now + 7 * 3600)
    status, served, d = chat(run + "1")
    five = {w["seconds"]: w for w in accounts()["cap-a"]["windows"]}[18000]
    check("reply headers update the meter", status == 200 and served == ["cap-a"] and five["remaining"] == 1 and five["source"] == "header", five)
    # New chats fill the band, a running 5-hour window first.
    status, served, d = chat(run + "2")
    check("new chat: running 5-hour window before the dormant one", status == 200 and served == ["cap-b"] and d["proposed"]["reason"] == "new_chat", {"served": served, "decision": d and d["proposed"]})
    status, served, d = chat(run + "1")
    check("warm chat on an account at its floor is moved (forced)", status == 200 and served == ["cap-b"] and d["proposed"]["reason"] == "forced_move" and d["proposed"]["forced"], {"served": served, "decision": d and d["proposed"]})
    # A client that declares compact_on_switch: a forced move asks it to compact
    # first (409), then admits the compacted chat on the new account.
    status, body, served, d = claim_chat(run + "claim", "c1", "r1")
    check("claim chat placed by pace", status == 200 and served == ["cap-b"] and d["proposed"]["reason"] == "new_chat", {"status": status, "served": served, "decision": d["proposed"]})
    meters("cap-b", 99, now + 4 * 3600, 53, now + 98 * 3600)
    status, body, served, d = claim_chat(run + "claim", "c1", "r2")
    check("claim chat stays while warm; reply headers bring cap-b to its floor", status == 200 and served == ["cap-b"] and d["proposed"]["reason"] == "warm", {"served": served, "decision": d["proposed"]})
    status, body, served, d = claim_chat(run + "claim", "c1", "r3")
    check("forced move asks the client to compact first, naming the summary models", status == 409 and isinstance(body, dict) and body["error"]["code"] == "quota_compact_required" and body["error"].get("compaction_models") == ["claude-sonnet-4-6", "claude-haiku-lab"] and served == [] and d["proposed"]["reason"] == "forced_move", {"status": status, "body": body, "served": served})
    status, body, served, d = claim_chat(run + "claim", "c2", "r4")
    check("compacted chat continues on the new account", status == 200 and served == ["cap-c"] and d["actual_auth"] == "cap-c.json", {"status": status, "served": served, "decision": d["proposed"]})
    status, body = call(CPA + "/v0/management/plugins/pacer/routing", "PATCH", {"mode": "off"}, key=ADMIN)
    count = len(state()["decisions"])
    status, served, d = chat(run + "3")
    check("off: CPA routes and Pacer records no decision", status == 200 and len(served) == 1 and len(state()["decisions"]) == count, {"served": served})
    check("invalid mode refused", call(CPA + "/v0/management/plugins/pacer/routing", "PATCH", {"mode": "dry-run"}, key=ADMIN)[0] == 400)
    print(json.dumps({"passed": sum(c["pass"] for c in checks), "failed": sum(not c["pass"] for c in checks)}))
    raise SystemExit(0 if all(c["pass"] for c in checks) else 1)


if __name__ == "__main__":
    main()

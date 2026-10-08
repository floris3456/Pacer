"""Native capacity lower-bound acceptance on the capacity fixture. Parent-run only.

  PACER_CAPACITY_READY=1 PACER_EVIDENCE=<dir> python3 tests/lab/check-capacity.py phase1|phase2|phase3

Needs the window from capacity-window.sh (fixture up, hosts armed, only cap-a..cap-e
Claude logins, CPA started after parking). Samples are only Pacer's own scheduled
usage reads (one per account every >= 15 min), so each interval takes 15-30 min.
Every Pacer usage read reaches the fixture, so its log dates the samples s1 and s2.

phase1  Verify every cap login in this CPA process (one request each: request-time
        profile, or a forced refresh if the login already carries account_uuid).
        Then five cases in parallel, one account each, over its next reads s1 -> s2:
          a  3 requests, +2 %                    -> lower_bound 0.006 $/% (0.018 / (2 + 1))
          b  a request every 45 s up to s2       -> unknown: provider_settle_unproven
          c  1 request, both resets move 1 h     -> unknown: reset_between_samples
          d  1 stream held open across s2        -> unknown: cut_not_quiescent
          e  1 request, -2 %                     -> unknown: nonpositive_change
        On success writes capacity-restart-required.json. The parent then runs
        docker restart pacer-lab-cpa and, within 20 s, capacity-window.sh up.
phase2  The interval across the restart is unknown (inventory_incarnation_changed,
        account_ref_changed). A clean interval after it stays unknown, because the
        restart dropped every verification (unknown_account_ref_tuple). Start it
        within ~45 min of the restart: readback keeps only ~1 h of intervals.
phase3  A forced token refresh per account verifies it again (refresh profile); a
        clean interval after that is a lower bound again. cap-c is refreshed a second
        time inside that interval: a same-account rotation is no binding change.

Reads Pacer readback and fixture counters, sends dummy requests through CPA and
forces refreshes through CPA's management API. Never prints or stores tokens.
"""
import datetime
import json
import os
import pathlib
import re
import sys
import threading
import time
import urllib.error
import urllib.request

CPA = "http://pacer-lab-cpa.orb.local:8317"
CONTROL = "http://pacer-upstream-capacity.orb.local:8080"
STATE = "/v0/management/plugins/pacer/state"
REFRESH = "/v0/management/auth-files/refresh"
ACCOUNTS = ("cap-a", "cap-b", "cap-c", "cap-d", "cap-e")
MODEL = "claude-opus-5-5"
SEVEN_DAY, FIVE_HOUR = "all:604800", "all:18000"
WINDOWS = (SEVEN_DAY, FIVE_HOUR)
BASIS = "sandwiched_cuts_provider_settle_assumed"
REQUEST_USD = 0.006  # DefaultPriceBook opus 5.5 standard: 1000 input x $4/M + 100 output x $20/M
SETTLE = 120  # capacity.SettleWindow
ACCOUNT_MIN_GAP = 900  # observation.AccountMinGap: no read starts sooner after the last one
SAMPLE_TIMEOUT = 1800
SANDWICH_MARGIN = 5  # cut b and inventory c2 follow the provider read at once
READY_SETTLE = 45  # CPA persists a verified login; its watcher update must land before any c1
READBACK_TIMEOUT = 150  # the background worker judges every 30 s
MATCH = 3.0  # seconds between the fixture's read log and Pacer's sample time
BUSY_FROM, BUSY_PERIOD = 780, 45  # case b: from 2 min before the earliest s2, always < SETTLE apart
HOLD_FROM = 780  # case d: start the held stream 2 min before the earliest s2
HOLD_CONFIRM = 30
STREAM_TIMEOUT = 1800  # longer than the fixture's 600 s hold limit
FIVE_HOUR_LEFT, SEVEN_DAY_LEFT = 7200, 86400  # resets nearer than this are moved before s1
POLL = 1.0
PROFILE, TOKEN, MESSAGES, USAGE = "/api/oauth/profile", "/v1/oauth/token", "/v1/messages", "/api/oauth/usage"
RESTART_MARKER = "capacity-restart-required.json"
MEMBERSHIP_REASONS = ("account_ref_changed", "account_ref_unknown", "binding_changed_inside", "inventory_incarnation_changed",
                      "inventory_sequence_regressed", "inventory_unknown", "no_account_tuples", "unknown_account_ref_tuple")


# ---------------------------------------------------------------- pure helpers

def is_int(value):
    return isinstance(value, int) and not isinstance(value, bool)


def parse_time(value):
    """Go RFC 3339 (up to nanoseconds) -> epoch seconds."""
    match = re.fullmatch(r"(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d{1,9}))?(Z|[+-]\d\d:\d\d)", value or "")
    assert match, "not an RFC 3339 time: %r" % (value,)
    zone = "+00:00" if match.group(3) == "Z" else match.group(3)
    whole = datetime.datetime.fromisoformat(match.group(1) + zone).timestamp()
    fraction = match.group(2) or ""
    return whole + (int(fraction) / 10 ** len(fraction) if fraction else 0)


def iso(epoch):
    return datetime.datetime.fromtimestamp(epoch, datetime.timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def entries(snapshot, account, path, after=float("-inf"), before=float("inf"), status=200):
    """Fixture log entries, oldest first. The log holds no token or body."""
    return sorted((e for e in snapshot["log"] if e.get("path") == path and e.get("account") == account
                   and (status is None or e.get("status") == status) and after < e["at"] < before), key=lambda e: e["at"])


def reads(snapshot, account, after=float("-inf"), before=float("inf")):
    return [e["at"] for e in entries(snapshot, account, USAGE, after, before)]


def account_name(auth_id):
    match = re.search(r"(?:^|/)(cap-[a-e])(?:\.json)?$", str(auth_id or ""))
    return match.group(1) if match else None


def cap_rows(state):
    """Claude readback rows by cap account, plus every other Claude row id."""
    rows, others = {}, []
    for row in state["accounts"]:
        if row.get("provider") != "claude":
            continue
        name = account_name(row.get("id"))
        if name is None:
            others.append(str(row.get("id")))
        else:
            assert name not in rows, "two readback rows for " + name
            rows[name] = row
    return rows, others


def check_flags(state):
    """No path may claim a known capacity."""
    assert state["measurement"]["capacity_known"] is False, "measurement.capacity_known is not false"
    for row in state["accounts"]:
        capacity = row.get("capacity")
        if row.get("provider") == "claude":
            assert isinstance(capacity, dict), "claude row %s has no capacity object" % row.get("id")
        if capacity is not None:
            assert capacity["capacity_known"] is False, "capacity_known is not false for " + str(row.get("id"))
            assert capacity["exclusivity"] == "unproven", "exclusivity is not unproven for " + str(row.get("id"))


def intervals(capacity):
    """The newest judged interval (top level) and the earlier ones beside it."""
    out = [capacity] if capacity.get("interval_to") else []
    return out + list(capacity.get("history") or [])


def find_interval(capacity, window, start, end):
    """The judged interval of one window from sample start to sample end, or None."""
    found = [i for i in intervals(capacity) if i.get("window") == window
             and (start is None or abs(parse_time(i["interval_from"]) - start) <= MATCH)
             and abs(parse_time(i["interval_to"]) - end) <= MATCH]
    assert len(found) <= 1, "%d %s intervals end at %s" % (len(found), window, iso(end))
    return found[0] if found else None


def brief(interval):
    if not interval:
        return None
    return {k: interval.get(k) for k in ("window", "interval_from", "interval_to", "status", "reasons", "value")}


def close(actual, expected):
    return isinstance(actual, (int, float)) and not isinstance(actual, bool) and abs(actual - expected) <= 1e-9


def check_common(interval, window):
    assert interval["window"] == window, (interval["window"], window)
    assert interval["exclusivity"] == "unproven", "exclusivity %r" % interval["exclusivity"]
    assert interval["basis"] == BASIS, "basis %r" % interval["basis"]
    assert interval.get("capacity_known", False) is False


def expect_lower_bound(interval, window, records, delta):
    check_common(interval, window)
    assert interval["status"] == "lower_bound" and interval["reasons"] == [], \
        "%s: expected lower_bound, got %s %s" % (window, interval["status"], interval["reasons"])
    value = interval["value"]
    usd = REQUEST_USD * records
    expected = {"lower_bound_usd_per_percent": usd / (delta + 1), "cpa_value_usd": usd,
                "delta_percent": delta, "quantum_percent": 1, "records": records}
    assert isinstance(value, dict) and set(value) == set(expected), "%s: value fields %r" % (window, value)
    for key, want in expected.items():
        assert close(value[key], want), "%s: %s is %r, expected %r" % (window, key, value[key], want)
    assert is_int(value["records"])


def expect_unknown(interval, window, reasons):
    check_common(interval, window)
    assert interval["status"] == "unknown" and interval["value"] is None, \
        "%s: expected unknown, got %s value %r" % (window, interval["status"], interval["value"])
    assert interval["reasons"] == sorted(reasons), "%s: reasons %r, expected exactly %r" % (window, interval["reasons"], sorted(reasons))


def expect_unknown_including(interval, window, required, forbidden=()):
    check_common(interval, window)
    reasons = interval["reasons"]
    assert interval["status"] == "unknown" and interval["value"] is None, \
        "%s: expected unknown, got %s" % (window, interval["status"])
    assert reasons == sorted(set(reasons)), "%s: reasons not sorted and unique: %r" % (window, reasons)
    missing = sorted(set(required) - set(reasons))
    present = sorted(set(forbidden) & set(reasons))
    assert not missing and not present, "%s: reasons %r lack %r or carry %r" % (window, reasons, missing, present)


def restart_window(window, marker):
    """(cpa_started_at, hosts_armed_at) of a CPA restart after phase 1, re-armed by up."""
    started, armed = window.get("cpa_started_at"), window.get("hosts_armed_at")
    assert is_int(started) and is_int(armed), "window.json lacks cpa_started_at/hosts_armed_at; run capacity-window.sh up"
    assert started > marker["host_finished_at"], "CPA has not restarted since phase 1 finished"
    assert armed >= started, "capacity-window.sh up has not re-armed the hosts since the CPA restart"
    return started, armed


def summarize(phase, results, expected):
    """A phase passes only if every expected case ran, judged both windows and passed."""
    ran = sorted(results)
    complete = ran == sorted(expected)
    judged = all(set((r.get("intervals") or {})) >= set(WINDOWS) for r in results.values())
    return {"phase": phase, "cases_expected": sorted(expected), "cases_ran": ran,
            "pass": bool(results) and complete and judged and all(r.get("pass") is True for r in results.values())}


SECRET = re.compile(r"sk-ant-|Bearer\s+\S")


def redact(text):
    return re.sub(r"(sk-ant-|Bearer\s+)\S*", "[redacted]", text)


def public(document):
    text = json.dumps(document, indent=2, sort_keys=True)
    assert not SECRET.search(text), "refusing to write a receipt that carries a credential"
    return text + "\n"


# ---------------------------------------------------------------- live lab I/O

class Held:
    """A streamed request that the fixture holds open until released."""

    def __init__(self, lab, account):
        self.outcome = None
        self.thread = threading.Thread(target=self.run, args=(lab, account), daemon=True)
        self.thread.start()

    def run(self, lab, account):
        try:
            self.outcome = lab.infer(account, stream=True)
        except Exception as error:  # reported by result()
            self.outcome = (0, {"error": type(error).__name__})

    def result(self, timeout):
        self.thread.join(timeout)
        assert not self.thread.is_alive(), "held stream did not finish within %d s of its release" % timeout
        return self.outcome


class Lab:
    """All live I/O. Tests substitute a fake with the same methods."""

    def __init__(self, environ):
        self.http = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        self.management_key = environ.get("PACER_MANAGEMENT_KEY", "pacer-test-admin")
        self.inference_key = environ.get("PACER_INFERENCE_KEY", "pacer-test-inference")
        here = pathlib.Path(__file__).resolve().parent
        self.window_path = pathlib.Path(environ.get("PACER_LAB_DIR") or here.parents[3] / ".cache" / "lab") / "capacity-window" / "window.json"
        self.skew = 0.0
        self.lock = threading.Lock()
        self.cached = (0.0, None)

    def call(self, url, body=None, headers=None, timeout=30):
        request = urllib.request.Request(url, data=None if body is None else json.dumps(body).encode(),
                                         headers={"Content-Type": "application/json", **(headers or {})})
        try:
            with self.http.open(request, timeout=timeout) as response:
                return response.status, response.read()
        except urllib.error.HTTPError as error:
            return error.code, error.read()

    def json(self, url, body=None, headers=None):
        status, raw = self.call(url, body, headers)
        assert status == 200, "%s: HTTP %d" % (url.split("?")[0], status)
        return json.loads(raw)

    def now(self):
        """The fixture's clock, which dates the read log."""
        return time.time() + self.skew

    def sleep(self, seconds):
        time.sleep(seconds)

    def health(self):
        before = time.time()
        body = self.json(CONTROL + "/health")
        self.skew = body["now"] - (before + time.time()) / 2
        return body

    def attempts(self, fresh=False):
        with self.lock:
            at, cached = self.cached
            if fresh or cached is None or time.monotonic() - at >= POLL:
                cached = self.json(CONTROL + "/_test/attempts")
                self.cached = (time.monotonic(), cached)
            return cached

    def set_usage(self, account, **values):
        return self.json(CONTROL + "/_test/usage", dict(values, account=account))

    def hold(self, account):
        return self.json(CONTROL + "/_test/hold", {"account": account})

    def release(self, account):
        return self.json(CONTROL + "/_test/release", {"account": account})

    def state(self):
        return self.json(CPA + STATE, headers={"Authorization": "Bearer " + self.management_key})

    def refresh(self, account):
        # The response carries the refreshed login: read it, never keep it.
        status, _ = self.call(CPA + REFRESH, {"name": account + ".json"}, {"Authorization": "Bearer " + self.management_key}, timeout=60)
        return status

    def infer(self, account, stream=False):
        body = {"model": account + "/" + MODEL, "max_tokens": 64, "stream": stream,
                "messages": [{"role": "user", "content": "Dummy capacity lab request, no reasoning"}]}
        # A client that gives up on a held stream would make CPA record a
        # physical gap, which is manager-wide and would taint every case.
        status, raw = self.call(CPA + "/v1/messages", body, {"Authorization": "Bearer " + self.inference_key},
                                timeout=STREAM_TIMEOUT if stream else 60)
        detail = {"stream": stream, "bytes": len(raw)}
        if status != 200:
            detail["error"] = redact(raw[:200].decode("utf-8", "replace"))
        elif stream:
            detail["complete"] = b"message_stop" in raw
        else:
            usage = json.loads(raw).get("usage", {})
            detail["complete"] = usage.get("input_tokens") == 1000 and usage.get("output_tokens") == 100
        return status, detail

    def infer_held(self, account):
        return Held(self, account)

    def window(self):
        return json.loads(self.window_path.read_text())


# ---------------------------------------------------------------- shared steps

def wait_until(lab, target):
    while lab.now() < target:
        lab.sleep(min(POLL, max(target - lab.now(), 0.01)))


def wait_read(lab, account, after, tick=None, timeout=SAMPLE_TIMEOUT):
    """The first scheduled Pacer usage read of account after a fixture time."""
    deadline = max(after, lab.now()) + timeout
    while True:
        found = reads(lab.attempts(), account, after)
        if found:
            return found[0]
        assert lab.now() < deadline, "no Pacer usage read of %s within %d s after %s" % (account, timeout, iso(after))
        if tick:
            tick()
        lab.sleep(POLL)


def wait_intervals(lab, account, start, end, timeout=READBACK_TIMEOUT):
    """Both judged window intervals start -> end (start None: any start)."""
    deadline = lab.now() + timeout
    while True:
        rows, _ = cap_rows(lab.state())
        capacity = rows[account]["capacity"]
        found = {w: find_interval(capacity, w, start, end) for w in WINDOWS}
        if all(found.values()):
            return found
        assert lab.now() < deadline, "%s: readback has no judged interval for %s -> %s; newest: %r" % (
            account, iso(start) if start else "*", iso(end), brief(capacity))
        lab.sleep(5)


def infer_ok(lab, account, stream=False):
    status, detail = lab.infer(account, stream)
    assert status == 200 and detail.get("complete"), "%s request through CPA: HTTP %d %r" % (account, status, detail)
    return detail


def usage_of(snapshot, account):
    u = snapshot["usage"][account]
    return {"five_hour": u["five_hour"]["utilization"], "seven_day": u["seven_day"]["utilization"],
            "five_hour_reset": u["five_hour"]["resets_at"], "seven_day_reset": u["seven_day"]["resets_at"]}


def change_usage(lab, account, delta=0, shift=False):
    values = usage_of(lab.attempts(fresh=True), account)
    values["five_hour"] += delta
    values["seven_day"] += delta
    if shift:  # any move beyond the 2 s tolerance is a different cycle
        values["five_hour_reset"] += 3600
        values["seven_day_reset"] += 3600
    lab.set_usage(account, **values)
    return values


def normalize(lab):
    """Before any s1: resets far enough out and room for +-2 %. Returns what moved."""
    now, moved = int(lab.now()), {}
    snapshot = lab.attempts(fresh=True)
    for account in ACCOUNTS:
        values = usage_of(snapshot, account)
        before = dict(values)
        if values["five_hour_reset"] < now + FIVE_HOUR_LEFT or values["seven_day_reset"] < now + SEVEN_DAY_LEFT:
            values["five_hour_reset"], values["seven_day_reset"] = now + 18000, now + 604800
        if not (5 <= values["five_hour"] <= 90 and 5 <= values["seven_day"] <= 90):
            values["five_hour"], values["seven_day"] = 10, 20
        if values != before:
            lab.set_usage(account, **values)
            moved[account] = {"from": before, "to": values}
    return moved


def preflight(lab):
    health = lab.health()
    assert health.get("ready") is True and health.get("accounts") == list(ACCOUNTS), "capacity fixture is not the expected one"
    state = lab.state()
    rows, others = cap_rows(state)
    assert not others, ("non-cap Claude logins %r stay unverified, so every interval would be "
                        "unknown_account_ref_tuple; use PACER_CAPACITY_PARK_ORIGINALS=1 and restart CPA" % others)
    assert sorted(rows) == list(ACCOUNTS), "readback lacks cap logins: %r" % sorted(set(ACCOUNTS) - set(rows))
    check_flags(state)
    return {"fixture_skew_seconds": round(lab.now() - time.time(), 3), "claude_rows": sorted(rows)}


def refresh_all(lab, accounts, since):
    """Force a refresh per account; CPA must rotate (token) and verify (profile) each."""
    statuses = {account: lab.refresh(account) for account in accounts}
    assert all(s == 200 for s in statuses.values()), "forced refresh: %r" % statuses
    snapshot = lab.attempts(fresh=True)
    out = {}
    for account in accounts:
        token = entries(snapshot, account, TOKEN, since)
        profile = entries(snapshot, account, PROFILE, since)
        assert token and profile, "%s: refresh made no fixture token grant and profile read" % account
        assert token[-1]["at"] <= profile[-1]["at"], "%s: profile read did not follow the token grant" % account
        out[account] = {"token_at": iso(token[-1]["at"]), "profile_at": iso(profile[-1]["at"]),
                        "generation": snapshot["generations"][account]}
    return out, max(entries(snapshot, a, PROFILE, since)[-1]["at"] for a in accounts)


def run_parallel(lab, ready_at, cases):
    """One thread per case, all started at once; each case owns one account."""
    results = {}

    def guarded(name, case):
        receipt = {"case": name, "account": case[0], "expect": case[1], "pass": False}
        try:
            case[2](lab, case[0], ready_at, receipt)
            receipt["pass"] = True
        except Exception as error:  # every failure lands in the receipt
            receipt["error"] = "%s: %s" % (type(error).__name__, redact(str(error)))
        results[name] = receipt

    threads = [threading.Thread(target=guarded, args=item, name="capacity-case-" + item[0], daemon=True) for item in cases.items()]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()
    return results


def interval_flow(lab, account, ready_at, receipt, act=None, tick=None, after=None):
    """s1 = first read after ready_at; act; s2 = next read; after; judged intervals."""
    t1 = wait_read(lab, account, ready_at)
    receipt["s1"] = iso(t1)
    wait_until(lab, t1 + SANDWICH_MARGIN)
    if act:
        act(t1)
    t2 = wait_read(lab, account, t1, tick=(lambda: tick(t1)) if tick else None)
    receipt["s2"] = iso(t2)
    wait_until(lab, t2 + SANDWICH_MARGIN)
    if after:
        after(t2)
    found = wait_intervals(lab, account, t1, t2)
    receipt["intervals"] = {w: brief(found[w]) for w in WINDOWS}
    sent = entries(lab.attempts(fresh=True), account, MESSAGES, t1, t2)
    receipt["requests_between_samples"] = len(sent)
    return t1, t2, found, sent


# ---------------------------------------------------------------- phase 1 cases

def case_a(lab, account, ready_at, r):
    def act(t1):
        for _ in range(3):
            infer_ok(lab, account)
        r["usage"] = change_usage(lab, account, 2)

    _, _, found, sent = interval_flow(lab, account, ready_at, r, act)
    assert len(sent) == 3, "%d of 3 requests reached %s" % (len(sent), account)
    for w in WINDOWS:
        expect_lower_bound(found[w], w, records=3, delta=2)


def case_b(lab, account, ready_at, r):
    plan = {}

    def act(t1):
        r["usage"] = change_usage(lab, account, 1)
        plan["next"] = t1 + BUSY_FROM

    def tick(t1):
        if lab.now() >= plan["next"]:
            infer_ok(lab, account)
            plan["next"] = lab.now() + BUSY_PERIOD

    _, t2, found, sent = interval_flow(lab, account, ready_at, r, act, tick)
    assert sent, "no request reached %s between the samples" % account
    gap = t2 - sent[-1]["at"]
    r["last_request_before_s2_seconds"] = round(gap, 3)
    assert gap < SETTLE - SANDWICH_MARGIN, "premise: last request %.0f s before s2 is not inside the settle window" % gap
    for w in WINDOWS:
        expect_unknown(found[w], w, ["carry_in_unproven", "provider_settle_unproven"])


def case_c(lab, account, ready_at, r):
    def act(t1):
        infer_ok(lab, account)
        r["usage"] = change_usage(lab, account, 1, shift=True)

    _, _, found, sent = interval_flow(lab, account, ready_at, r, act)
    assert len(sent) == 1, "%d of 1 request reached %s" % (len(sent), account)
    for w in WINDOWS:
        expect_unknown(found[w], w, ["reset_between_samples"])


def case_d(lab, account, ready_at, r):
    plan = {}

    def act(t1):
        r["usage"] = change_usage(lab, account, 1)

    def tick(t1):
        if "held" in plan or lab.now() < t1 + HOLD_FROM:
            return
        armed = lab.hold(account)
        assert armed["armed"] is True and armed["holding"] == 0, "hold not armed: %r" % armed
        plan["held"] = lab.infer_held(account)
        deadline = lab.now() + HOLD_CONFIRM
        while lab.attempts(fresh=True)["holding"][account] != 1:
            assert lab.now() < deadline, "the fixture never held the stream"
            lab.sleep(POLL)

    def after(t2):
        assert "held" in plan, "s2 came before the hold started"
        lab.release(account)
        status, detail = plan["held"].result(60)
        assert status == 200 and detail.get("complete"), "held stream: HTTP %d %r" % (status, detail)

    _, t2, found, sent = interval_flow(lab, account, ready_at, r, act, tick, after)
    assert not sent, "%d unexpected requests completed on %s before s2" % (len(sent), account)
    done = entries(lab.attempts(fresh=True), account, MESSAGES, t2)
    held = [e for e in done if e.get("held")]
    assert len(held) == 1 and held[0]["outcome"] == "released", "held stream outcome: %r" % held
    assert held[0]["started"] < t2 < held[0]["at"], "premise: the stream was not in flight at s2"
    r["held_stream"] = {"started": iso(held[0]["started"]), "completed": iso(held[0]["at"]), "outcome": held[0]["outcome"]}
    for w in WINDOWS:
        expect_unknown(found[w], w, ["carry_in_unproven", "cut_not_quiescent", "physical_proof_unavailable"])


def case_e(lab, account, ready_at, r):
    def act(t1):
        infer_ok(lab, account)
        r["usage"] = change_usage(lab, account, -2)

    _, _, found, sent = interval_flow(lab, account, ready_at, r, act)
    assert len(sent) == 1, "%d of 1 request reached %s" % (len(sent), account)
    for w in WINDOWS:
        expect_unknown(found[w], w, ["nonpositive_change"])


PHASE1 = {
    "a": ("cap-a", "3 requests, +2 %: lower_bound 0.006 $/% in both windows", case_a),
    "b": ("cap-b", "requests every 45 s up to s2: unknown [carry_in_unproven, provider_settle_unproven]", case_b),
    "c": ("cap-c", "1 request, both resets move 1 h: unknown [reset_between_samples]", case_c),
    "d": ("cap-d", "stream held across s2: unknown [carry_in_unproven, cut_not_quiescent, physical_proof_unavailable]", case_d),
    "e": ("cap-e", "1 request, -2 %: unknown [nonpositive_change]", case_e),
}


def verify_logins(lab):
    """Every cap login verified by this CPA process: a request-time profile read,
    or a forced refresh where the login already carries account_uuid."""
    start = lab.now()
    for account in ACCOUNTS:
        infer_ok(lab, account)
    snapshot = lab.attempts(fresh=True)
    steering = {a: len(entries(snapshot, a, MESSAGES, start)) for a in ACCOUNTS}
    assert all(n == 1 for n in steering.values()), "requests did not reach their prefixed logins: %r" % steering
    profiled = [a for a in ACCOUNTS if entries(snapshot, a, PROFILE, start)]
    refreshed, verified_at = {}, max([e["at"] for a in profiled for e in entries(snapshot, a, PROFILE, start)] or [start])
    missing = [a for a in ACCOUNTS if a not in profiled]
    if missing:
        refreshed, at = refresh_all(lab, missing, start)
        verified_at = max(verified_at, at)
    return {"request_profile": profiled, "forced_refresh": refreshed}, verified_at


def phase1(lab, out, receipt):
    receipt["started_at"] = iso(lab.now())
    marker = out / RESTART_MARKER
    if marker.exists():
        marker.unlink()
    receipt["preflight"] = preflight(lab)
    receipt["normalized"] = normalize(lab)
    receipt["verification"], verified_at = verify_logins(lab)
    ready_at = max(verified_at, lab.now()) + READY_SETTLE
    receipt["ready_at"] = iso(ready_at)
    receipt["cases"] = run_parallel(lab, ready_at, PHASE1)
    check_flags(lab.state())
    receipt.update(summarize("phase1", receipt["cases"], PHASE1))
    receipt["finished_at"] = iso(lab.now())
    if receipt["pass"]:
        marker = {"phase1_finished_at": receipt["finished_at"], "host_finished_at": int(time.time()),
                  "last_reads": {c["account"]: c["s2"] for c in receipt["cases"].values()}}
        (out / RESTART_MARKER).write_text(public(marker))
        receipt["next"] = ("docker restart pacer-lab-cpa, then within 20 s PACER_CAPACITY_WINDOW=1 "
                           "tests/lab/capacity-window.sh up, then check-capacity.py phase2")


# ---------------------------------------------------------------- phase 2

def phase2(lab, out, receipt):
    receipt["started_at"] = iso(lab.now())
    marker_path, previous = out / RESTART_MARKER, out / "capacity-phase1.json"
    assert marker_path.exists() and previous.exists(), "phase1 has not passed in " + str(out)
    assert json.loads(previous.read_text())["pass"] is True, "phase1 did not pass"
    marker = json.loads(marker_path.read_text())
    started, armed = restart_window(lab.window(), marker)
    receipt["restart"] = {"cpa_started_at": iso(started), "hosts_armed_at": iso(armed)}
    receipt["preflight"] = preflight(lab)
    moved = normalize(lab)
    receipt["normalized"] = moved
    ready_at = lab.now() + (READY_SETTLE if moved else 0)
    receipt["ready_at"] = iso(ready_at)

    def case(lab, account, ready_at, r):
        snapshot = lab.attempts(fresh=True)
        before = reads(snapshot, account, before=started)
        assert before, "no read of %s before the restart" % account
        pre, post = before[-1], wait_read(lab, account, started)
        r["across_restart"] = {"s1": iso(pre), "s2": iso(post)}
        found = wait_intervals(lab, account, pre, post)
        for w in WINDOWS:
            expect_unknown_including(found[w], w, ["membership_unproven", "inventory_incarnation_changed", "account_ref_changed", "carry_in_unproven"],
                                     ["binding_changed_inside", "unknown_account_ref_tuple", "no_account_tuples", "account_ref_unknown", "inventory_unknown"])
        r["across_restart"]["intervals"] = {w: brief(found[w]) for w in WINDOWS}

        def act(t1):
            infer_ok(lab, account)
            r["usage"] = change_usage(lab, account, 1)

        _, t2, found, sent = interval_flow(lab, account, ready_at, r, act)
        assert len(sent) == 1, "%d of 1 request reached %s" % (len(sent), account)
        relogin = entries(lab.attempts(fresh=True), account, PROFILE, started, status=None) + entries(lab.attempts(), account, TOKEN, started, status=None)
        assert not relogin, "premise: %s was profiled or refreshed after the restart" % account
        for w in WINDOWS:
            expect_unknown(found[w], w, ["account_ref_unknown", "membership_unproven", "no_account_tuples", "unknown_account_ref_tuple"])

    cases = {a[-1]: (a, "across the restart: incarnation changed; after it: unverified, unknown_account_ref_tuple", case) for a in ACCOUNTS}
    receipt["cases"] = run_parallel(lab, ready_at, cases)
    check_flags(lab.state())
    receipt.update(summarize("phase2", receipt["cases"], cases))
    receipt["finished_at"] = iso(lab.now())
    if receipt["pass"]:
        receipt["next"] = "check-capacity.py phase3"


# ---------------------------------------------------------------- phase 3

ROTATE_INSIDE = "cap-c"


def phase3(lab, out, receipt):
    receipt["started_at"] = iso(lab.now())
    previous = out / "capacity-phase2.json"
    assert previous.exists() and json.loads(previous.read_text())["pass"] is True, "phase2 has not passed in " + str(out)
    receipt["preflight"] = preflight(lab)
    receipt["normalized"] = normalize(lab)
    start = lab.now()
    receipt["refresh"], verified_at = refresh_all(lab, ACCOUNTS, start)
    ready_at = max(verified_at, lab.now()) + READY_SETTLE
    receipt["ready_at"] = iso(ready_at)

    def case(lab, account, ready_at, r):
        def act(t1):
            for _ in range(3):
                infer_ok(lab, account)
            r["usage"] = change_usage(lab, account, 2)
            if account == ROTATE_INSIDE:
                inside, _ = refresh_all(lab, [account], t1)
                r["refresh_inside"] = inside[account]

        t1, _, found, sent = interval_flow(lab, account, ready_at, r, act)
        assert len(sent) == 3, "%d of 3 requests reached %s" % (len(sent), account)
        for w in WINDOWS:
            expect_lower_bound(found[w], w, records=3, delta=2)
        # The interval into s1 starts before the refreshes only if no read fell
        # between them and ready_at; only then is its c1 all-unverified.
        earlier = reads(lab.attempts(), account, before=t1)
        if earlier and earlier[-1] < start:
            into = wait_intervals(lab, account, earlier[-1], t1)
            r["into_s1"] = {w: brief(into[w]) for w in WINDOWS}
            for w in WINDOWS:
                expect_unknown_including(into[w], w, ["membership_unproven", "unknown_account_ref_tuple"])
        else:
            r["into_s1_note"] = "a read fell between the refreshes and s1; not asserted"

    cases = {a[-1]: (a, "refreshed and verified again: lower_bound 0.006 $/%" + (", second refresh inside" if a == ROTATE_INSIDE else ""), case)
             for a in ACCOUNTS}
    receipt["cases"] = run_parallel(lab, ready_at, cases)
    check_flags(lab.state())
    receipt.update(summarize("phase3", receipt["cases"], cases))
    receipt["finished_at"] = iso(lab.now())


PHASES = {"phase1": phase1, "phase2": phase2, "phase3": phase3}


def main(argv=None, environ=None, lab=None):
    argv = sys.argv[1:] if argv is None else argv
    environ = os.environ if environ is None else environ
    if environ.get("PACER_CAPACITY_READY") != "1":
        raise SystemExit("check-capacity: refusing: the parent must arm the capacity window and set PACER_CAPACITY_READY=1")
    if not environ.get("PACER_EVIDENCE"):
        raise SystemExit("check-capacity: refusing: PACER_EVIDENCE must name the receipt directory")
    if len(argv) != 1 or argv[0] not in PHASES:
        raise SystemExit("usage: PACER_CAPACITY_READY=1 PACER_EVIDENCE=<dir> check-capacity.py phase1|phase2|phase3")
    out = pathlib.Path(environ["PACER_EVIDENCE"])
    out.mkdir(parents=True, exist_ok=True)
    lab = Lab(environ) if lab is None else lab
    receipt = {"phase": argv[0], "pass": False}
    try:
        PHASES[argv[0]](lab, out, receipt)
    except Exception as error:  # the receipt records why; the exit code fails
        receipt["error"] = "%s: %s" % (type(error).__name__, redact(str(error)))
        receipt["pass"] = False
    finally:
        try:
            text = public(receipt)
        except AssertionError:
            receipt = {"phase": argv[0], "pass": False, "error": "receipt withheld: it carried a credential"}
            text = public(receipt)
        (out / ("capacity-%s.json" % argv[0])).write_text(text)
    print(json.dumps({k: receipt.get(k) for k in ("phase", "pass", "cases_ran", "error", "next") if k in receipt}, indent=2))
    for name, case in sorted((receipt.get("cases") or {}).items()):
        print("case %s (%s): %s%s" % (name, case["account"], "PASS" if case["pass"] else "FAIL", "" if case["pass"] else " - " + case.get("error", "")))
    return 0 if receipt["pass"] else 1


if __name__ == "__main__":
    sys.exit(main())

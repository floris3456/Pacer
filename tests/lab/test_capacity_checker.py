"""check-capacity.py offline: assertion units, refusals, and every phase driven
against a fake lab whose virtual clock runs each 15-minute sample cycle at once."""
import contextlib
import datetime
import importlib.util
import io
import json
import os
import pathlib
import subprocess
import sys
import tempfile
import threading
import unittest

HERE = pathlib.Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("check_capacity", HERE / "check-capacity.py")
check = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(check)

ACCOUNTS = check.ACCOUNTS
T0 = 4_000_000_000.0  # far beyond any real host clock, so a fake restart is always "after" phase 1
LOWER_BOUND = {"lower_bound_usd_per_percent": 0.006, "cpa_value_usd": 0.018, "delta_percent": 2, "quantum_percent": 1, "records": 3}
SPANNING = ["account_ref_changed", "carry_in_unproven", "incarnation_changed_between_samples", "inventory_incarnation_changed", "membership_unproven"]
UNVERIFIED = ["account_ref_unknown", "membership_unproven", "no_account_tuples", "unknown_account_ref_tuple"]
REVERIFYING = ["account_ref_changed", "account_ref_unknown", "membership_unproven", "no_account_tuples", "unknown_account_ref_tuple"]
PHASE1 = {
    "cap-a": ("lower_bound", [], LOWER_BOUND),
    "cap-b": ("unknown", ["carry_in_unproven", "provider_settle_unproven"], None),
    "cap-c": ("unknown", ["reset_between_samples"], None),
    "cap-d": ("unknown", ["carry_in_unproven", "cut_not_quiescent", "physical_proof_unavailable"], None),
    "cap-e": ("unknown", ["nonpositive_change"], None),
}


def go_time(epoch):
    whole = int(epoch)
    stamp = datetime.datetime.fromtimestamp(whole, datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S")
    return stamp + ".%09dZ" % int(round((epoch - whole) * 1e9))


def interval(window, status, reasons, value, start=T0, end=T0 + 900):
    return {"status": status, "reasons": list(reasons), "exclusivity": "unproven", "window": window,
            "interval_from": go_time(start), "interval_to": go_time(end), "value": value, "basis": check.BASIS}


def model(fake, account, s1, s2):
    """Pacer's judgment as the cases expect it, from the fake's own record.
    Like capacity.Project, membership and carry-in reasons are reported together."""
    restart = fake.restart_at
    if restart and s1 < restart < s2:
        return "unknown", SPANNING, None
    membership = []
    if restart and s1 > restart:
        firsts = [min([at for at, a in fake.refreshes if a == x and at > restart] or [None]) for x in ACCOUNTS]
        if None in firsts or s2 < max(firsts):
            membership = UNVERIFIED
        elif s1 < max(firsts):
            membership = REVERIFYING
    active = [e for e in fake.log if e["account"] == account and s1 < e["at"] < s2 and e["path"] in (check.MESSAGES, "/_test/usage")]
    carry = [] if active else ["carry_in_unproven", "physical_proof_mismatch"]
    if membership or carry:
        return "unknown", sorted(set(membership + carry)), None
    return ("lower_bound", [], LOWER_BOUND) if restart else fake.phase1[account]


class FakeHeld:
    def result(self, timeout):
        return 200, {"stream": True, "complete": True}


class FakeLab:
    """Same methods as check.Lab. Each thread has its own virtual clock; the
    main thread's clock is the furthest any thread has reached."""

    def __init__(self, judge=model, uuid_on_disk=False, misroute=None, misroute_after=0, extra_rows=(), phase1=None,
                 capacity_known=False):
        self.judge, self.misroute, self.extra_rows = judge, misroute or {}, list(extra_rows)
        self.misroute_after, self.sent = misroute_after, dict.fromkeys(ACCOUNTS, 0)
        self.capacity_known = capacity_known
        self.phase1 = dict(PHASE1, **(phase1 or {}))
        self.lock = threading.RLock()
        self.local = threading.local()
        self.main_clock = self.highest = T0
        self.log, self.refreshes = [], []
        self.usage = {a: {"five_hour": 10, "seven_day": 20, "five_hour_reset": int(T0) + 18000, "seven_day_reset": int(T0) + 604800}
                      for a in ACCOUNTS}
        self.offsets = {a: 70 + 31 * i for i, a in enumerate(ACCOUNTS)}
        self.verified, self.on_disk = set(), set(ACCOUNTS) if uuid_on_disk else set()
        self.generations = dict.fromkeys(ACCOUNTS, 0)
        self.armed, self.held = dict.fromkeys(ACCOUNTS, False), {}
        self.restart_at, self.window_doc = None, {}
        self.state_reads = 0

    def now(self):
        with self.lock:
            if threading.current_thread() is threading.main_thread():
                self.main_clock = max(self.main_clock, self.highest)
                return self.main_clock
            if not hasattr(self.local, "now"):
                self.local.now = self.main_clock
            return self.local.now

    def sleep(self, seconds):
        with self.lock:
            now = self.now() + seconds
            if threading.current_thread() is threading.main_thread():
                self.main_clock = now
            else:
                self.local.now = now
            self.highest = max(self.highest, now)

    def advance(self, seconds):
        self.sleep(seconds)

    def reads(self, account, until):
        out, at = [], T0 + self.offsets[account]
        while at <= until:
            out.append(at)
            at += check.ACCOUNT_MIN_GAP
        return out

    def note(self, path, account, **extra):
        self.sleep(0.02)
        with self.lock:
            entry = dict({"at": self.now(), "path": path, "account": account, "status": 200}, **extra)
            entry.setdefault("started", entry["at"])
            self.log.append(entry)
            return entry

    def health(self):
        return {"ready": True, "accounts": list(ACCOUNTS), "now": self.now()}

    def attempts(self, fresh=False):
        now = self.now()
        with self.lock:
            log = [dict(e) for e in self.log if e["at"] <= now]
            log += [{"at": at, "path": check.USAGE, "account": a, "status": 200} for a in ACCOUNTS for at in self.reads(a, now)]
            usage = {a: {"five_hour": {"utilization": u["five_hour"], "resets_at": u["five_hour_reset"]},
                         "seven_day": {"utilization": u["seven_day"], "resets_at": u["seven_day_reset"]}} for a, u in self.usage.items()}
            return {"log": log, "usage": usage, "holding": {a: int(a in self.held) for a in ACCOUNTS},
                    "generations": dict(self.generations)}

    def set_usage(self, account, **values):
        assert set(values) == {"five_hour", "seven_day", "five_hour_reset", "seven_day_reset"}, values
        self.note("/_test/usage", account)
        with self.lock:
            self.usage[account] = dict(values)
        return {"account": account}

    def hold(self, account):
        with self.lock:
            self.armed[account] = True
        return {"account": account, "armed": True, "holding": 0}

    def infer_held(self, account):
        self.sleep(0.02)
        with self.lock:
            assert self.armed[account]
            self.armed[account], self.held[account] = False, self.now()
        return FakeHeld()

    def release(self, account):
        with self.lock:
            started = self.held.pop(account)
        self.note(check.MESSAGES, account, started=started, held=True, outcome="released", stream=True)
        return {"account": account, "armed": False, "holding": 1}

    def infer(self, account, stream=False):
        with self.lock:
            self.sent[account] += 1
            target = self.misroute.get(account, account) if self.sent[account] > self.misroute_after else account
            fetch = target not in self.verified and target not in self.on_disk
            if fetch:
                self.verified.add(target)
            self.on_disk.add(target)
        if fetch:
            self.note(check.PROFILE, target)
        self.note(check.MESSAGES, target, held=False, outcome="complete", stream=stream)
        return 200, {"stream": stream, "complete": True, "bytes": 10}

    def refresh(self, account):
        token = self.note(check.TOKEN, account)
        self.note(check.PROFILE, account)
        with self.lock:
            self.generations[account] += 1
            self.verified.add(account)
            self.refreshes.append((token["at"], account))
        return 200

    def restart(self):
        with self.lock:
            self.restart_at = self.now()
            self.verified.clear()
            self.window_doc = {"cpa_started_at": int(self.restart_at), "hosts_armed_at": int(self.restart_at) + 10}

    def window(self):
        return dict(self.window_doc)

    def capacity(self, account, now):
        reads = self.reads(account, now - 20)
        judged = []
        for s1, s2 in list(zip(reads, reads[1:]))[-9:]:
            result = self.judge(self, account, s1, s2)
            if result:
                judged += [interval(w, *result, start=s1 + 0.2, end=s2 + 0.2) for w in check.WINDOWS]
        if not judged:
            return {"status": "unknown", "reasons": ["no_judged_interval"], "exclusivity": "unproven", "capacity_known": False,
                    "interval_from": None, "interval_to": None, "age_seconds": None, "value": None, "history": []}
        judged.sort(key=lambda i: (check.parse_time(i["interval_to"]), i["window"] == check.SEVEN_DAY), reverse=True)
        return dict(judged[0], capacity_known=False, age_seconds=0, history=judged[1:9])

    def state(self):
        now = self.now()
        with self.lock:
            self.state_reads += 1
            rows = [{"id": a + ".json", "provider": "claude", "capacity": self.capacity(a, now)} for a in ACCOUNTS]
            return {"measurement": {"capacity_known": self.capacity_known}, "accounts": rows + self.extra_rows}


def run(phase, lab, out):
    stdout = io.StringIO()
    with contextlib.redirect_stdout(stdout):
        code = check.main([phase], {"PACER_CAPACITY_READY": "1", "PACER_EVIDENCE": str(out)}, lab=lab)
    receipt = json.loads((pathlib.Path(out) / ("capacity-%s.json" % phase)).read_text())
    return code, receipt, stdout.getvalue()


class Units(unittest.TestCase):
    def test_go_times_parse_to_the_nanosecond_and_zones(self):
        self.assertEqual(4_000_000_000.5, check.parse_time("2096-10-02T07:06:40.500000000Z"))
        self.assertEqual(4_000_000_000, check.parse_time("2096-10-02T07:06:40Z"))
        self.assertEqual(4_000_000_000.25, check.parse_time("2096-10-02T09:06:40.25+02:00"))
        self.assertAlmostEqual(4_000_000_000.123456789, check.parse_time("2096-10-02T07:06:40.123456789Z"), places=6)
        for bad in (None, "", "2096-10-02 07:06:40Z", "2096-10-02T07:06:40", "2096-10-02T07:06:40.1234567890Z"):
            with self.assertRaises(AssertionError):
                check.parse_time(bad)

    def test_intervals_are_found_by_window_and_both_sample_times(self):
        capacity = dict(interval(check.SEVEN_DAY, "unknown", ["x"], None, T0 + 0.4, T0 + 900.4), capacity_known=False,
                        history=[interval(check.FIVE_HOUR, "unknown", ["x"], None, T0 + 0.4, T0 + 900.4),
                                 interval(check.SEVEN_DAY, "unknown", ["y"], None, T0 - 900, T0 + 0.4)])
        self.assertEqual(["x"], check.find_interval(capacity, check.SEVEN_DAY, T0, T0 + 900)["reasons"])
        self.assertEqual(check.FIVE_HOUR, check.find_interval(capacity, check.FIVE_HOUR, T0, T0 + 900)["window"])
        self.assertEqual(["y"], check.find_interval(capacity, check.SEVEN_DAY, None, T0)["reasons"])
        self.assertIsNone(check.find_interval(capacity, check.SEVEN_DAY, T0 + 10, T0 + 900))
        self.assertIsNone(check.find_interval(capacity, check.FIVE_HOUR, None, T0))
        self.assertIsNone(check.find_interval(capacity, check.SEVEN_DAY, T0, T0 + 900 + check.MATCH + 1))
        twice = dict(capacity, history=[capacity["history"][0], dict(capacity, history=[])])
        with self.assertRaises(AssertionError):
            check.find_interval(twice, check.SEVEN_DAY, T0, T0 + 900)
        empty = {"status": "unknown", "interval_to": None, "history": []}
        self.assertEqual([], check.intervals(empty))

    def test_lower_bound_needs_the_exact_value_and_no_reason(self):
        good = interval(check.SEVEN_DAY, "lower_bound", [], dict(LOWER_BOUND))
        check.expect_lower_bound(good, check.SEVEN_DAY, records=3, delta=2)
        check.expect_lower_bound(dict(good, value=dict(LOWER_BOUND, lower_bound_usd_per_percent=0.018 / 3)), check.SEVEN_DAY, 3, 2)
        wrong = [dict(good, status="unknown"), dict(good, reasons=["x"]), dict(good, value=None),
                 dict(good, value=dict(LOWER_BOUND, records=2)), dict(good, value=dict(LOWER_BOUND, cpa_value_usd=0.012)),
                 dict(good, value=dict(LOWER_BOUND, lower_bound_usd_per_percent=0.009)),
                 dict(good, value=dict(LOWER_BOUND, quantum_percent=0.1)), dict(good, value=dict(LOWER_BOUND, records=True)),
                 dict(good, value=dict(LOWER_BOUND, known_ratio=1)), dict(good, exclusivity="proven"),
                 dict(good, basis="assumed"), dict(good, capacity_known=True), dict(good, window=check.FIVE_HOUR)]
        for bad in wrong:
            with self.assertRaises(AssertionError, msg=bad):
                check.expect_lower_bound(bad, check.SEVEN_DAY, records=3, delta=2)
        with self.assertRaises(AssertionError):
            check.expect_lower_bound(good, check.SEVEN_DAY, records=3, delta=1)

    def test_unknown_needs_exactly_the_expected_reasons(self):
        good = interval(check.FIVE_HOUR, "unknown", ["carry_in_unproven", "provider_settle_unproven"], None)
        check.expect_unknown(good, check.FIVE_HOUR, ["provider_settle_unproven", "carry_in_unproven"])
        for bad in (dict(good, reasons=["carry_in_unproven"]), dict(good, reasons=good["reasons"] + ["record_missing"]),
                    dict(good, status="lower_bound"), dict(good, value=LOWER_BOUND), dict(good, reasons=list(reversed(good["reasons"])))):
            with self.assertRaises(AssertionError, msg=bad):
                check.expect_unknown(bad, check.FIVE_HOUR, ["carry_in_unproven", "provider_settle_unproven"])

    def test_unknown_including_checks_required_and_forbidden_reasons(self):
        good = interval(check.SEVEN_DAY, "unknown", SPANNING, None)
        check.expect_unknown_including(good, check.SEVEN_DAY, ["membership_unproven", "inventory_incarnation_changed"], ["binding_changed_inside"])
        with self.assertRaises(AssertionError):
            check.expect_unknown_including(good, check.SEVEN_DAY, ["unknown_account_ref_tuple"])
        with self.assertRaises(AssertionError):
            check.expect_unknown_including(good, check.SEVEN_DAY, [], ["account_ref_changed"])
        with self.assertRaises(AssertionError):
            check.expect_unknown_including(dict(good, status="lower_bound"), check.SEVEN_DAY, [])

    def test_flags_refuse_any_known_capacity(self):
        state = {"measurement": {"capacity_known": False},
                 "accounts": [{"id": "cap-a.json", "provider": "claude", "capacity": {"capacity_known": False, "exclusivity": "unproven"}},
                              {"id": "codex.json", "provider": "codex", "capacity": {"capacity_known": False, "exclusivity": "unproven"}}]}
        check.check_flags(state)
        for mutate in (lambda s: s["measurement"].update(capacity_known=True),
                       lambda s: s["accounts"][0]["capacity"].update(capacity_known=True),
                       lambda s: s["accounts"][1]["capacity"].update(exclusivity="proven"),
                       lambda s: s["accounts"][0].pop("capacity")):
            broken = json.loads(json.dumps(state))
            mutate(broken)
            with self.assertRaises((AssertionError, KeyError)):
                check.check_flags(broken)

    def test_rows_are_mapped_by_login_name_and_others_reported(self):
        state = {"accounts": [{"id": "cap-a.json", "provider": "claude"}, {"id": "auths/cap-b", "provider": "claude"},
                              {"id": "fixture-a.json", "provider": "claude"}, {"id": "cap-c.json", "provider": "codex"},
                              {"id": "cap-ab.json", "provider": "claude"}]}
        rows, others = check.cap_rows(state)
        self.assertEqual(["cap-a", "cap-b"], sorted(rows))
        self.assertEqual(["fixture-a.json", "cap-ab.json"], others)
        with self.assertRaises(AssertionError):
            check.cap_rows({"accounts": [{"id": "cap-a.json", "provider": "claude"}, {"id": "cap-a", "provider": "claude"}]})

    def test_a_vacuous_or_partial_run_never_passes(self):
        both = {w: {} for w in check.WINDOWS}
        self.assertFalse(check.summarize("phase1", {}, check.PHASE1)["pass"])
        self.assertFalse(check.summarize("phase1", {"a": {"pass": True, "intervals": both}}, check.PHASE1)["pass"])
        every = {k: {"pass": True, "intervals": both} for k in check.PHASE1}
        self.assertTrue(check.summarize("phase1", every, check.PHASE1)["pass"])
        self.assertFalse(check.summarize("phase1", dict(every, c={"pass": True, "intervals": {check.SEVEN_DAY: {}}}), check.PHASE1)["pass"])
        self.assertFalse(check.summarize("phase1", dict(every, c={"pass": True}), check.PHASE1)["pass"])
        self.assertFalse(check.summarize("phase1", dict(every, e={"pass": False, "intervals": both}), check.PHASE1)["pass"])
        self.assertFalse(check.summarize("phase1", dict(every, z={"pass": True, "intervals": both}), check.PHASE1)["pass"])

    def test_receipts_refuse_credentials(self):
        self.assertIn('"ok": 1', check.public({"ok": 1}))
        for secret in ({"token": "sk-ant-oat01-pacer-capacity-a"}, {"e": "Authorization: Bearer pacer-test-admin"}):
            with self.assertRaises(AssertionError):
                check.public(secret)
        text = check.redact('{"error": "bad Bearer pacer-test-inference for sk-ant-oat01-pacer-capacity-a"}')
        self.assertEqual('{"error": "bad [redacted] for [redacted]', text)
        check.public({"e": text})

    def test_live_requests_give_held_streams_a_long_client_timeout(self):
        lab, seen = check.Lab({}), []

        def call(url, body=None, headers=None, timeout=30):
            seen.append((url, body["model"], body["stream"], timeout, headers["Authorization"] == "Bearer pacer-test-inference"))
            if body["stream"]:
                return 200, b"event: message_stop\ndata: {}\n\n"
            return 200, json.dumps({"usage": {"input_tokens": 1000, "output_tokens": 100}}).encode()

        lab.call = call
        size = len(json.dumps({"usage": {"input_tokens": 1000, "output_tokens": 100}}))
        self.assertEqual((200, {"stream": False, "bytes": size, "complete": True}), lab.infer("cap-a"))
        self.assertTrue(lab.infer("cap-d", stream=True)[1]["complete"])
        self.assertEqual([(check.CPA + "/v1/messages", "cap-a/claude-opus-5-5", False, 60, True),
                          (check.CPA + "/v1/messages", "cap-d/claude-opus-5-5", True, check.STREAM_TIMEOUT, True)], seen)
        self.assertGreater(check.STREAM_TIMEOUT, 600)
        lab.call = lambda *a, **k: (401, b'{"error": "rejected Bearer pacer-test-inference"}')
        status, detail = lab.infer("cap-a")
        self.assertEqual(401, status)
        self.assertIn("[redacted]", detail["error"])
        self.assertNotIn("pacer-test-inference", detail["error"])

    def test_restart_window_needs_a_later_start_and_a_rearm(self):
        marker = {"host_finished_at": 100}
        self.assertEqual((101, 110), check.restart_window({"cpa_started_at": 101, "hosts_armed_at": 110}, marker))
        for bad in ({"cpa_started_at": 100, "hosts_armed_at": 110}, {"cpa_started_at": 101, "hosts_armed_at": 100},
                    {"cpa_started_at": 101}, {"cpa_started_at": "101", "hosts_armed_at": 110}, {}):
            with self.assertRaises(AssertionError, msg=bad):
                check.restart_window(bad, marker)

    def test_live_lab_defaults_to_the_shared_lab_directory_and_does_no_io(self):
        lab = check.Lab({})
        self.assertEqual(HERE.parents[3] / ".cache" / "lab" / "capacity-window" / "window.json", lab.window_path)
        self.assertEqual(("pacer-test-admin", "pacer-test-inference"), (lab.management_key, lab.inference_key))
        lab = check.Lab({"PACER_LAB_DIR": "/elsewhere", "PACER_MANAGEMENT_KEY": "m", "PACER_INFERENCE_KEY": "i"})
        self.assertEqual(pathlib.Path("/elsewhere/capacity-window/window.json"), lab.window_path)
        self.assertEqual(("m", "i"), (lab.management_key, lab.inference_key))


class Refusals(unittest.TestCase):
    def test_main_refuses_without_readiness_evidence_or_a_phase(self):
        with tempfile.TemporaryDirectory() as out:
            for environ, argv in (({"PACER_EVIDENCE": out}, ["phase1"]), ({"PACER_CAPACITY_READY": "1"}, ["phase1"]),
                                  ({"PACER_CAPACITY_READY": "yes", "PACER_EVIDENCE": out}, ["phase1"]),
                                  ({"PACER_CAPACITY_READY": "1", "PACER_EVIDENCE": out}, []),
                                  ({"PACER_CAPACITY_READY": "1", "PACER_EVIDENCE": out}, ["phase4"])):
                with self.assertRaises(SystemExit):
                    check.main(argv, environ, lab=FakeLab())
            self.assertEqual([], os.listdir(out))

    def test_scripts_refuse_before_any_lab_io(self):
        environ = {k: v for k, v in os.environ.items() if not k.startswith("PACER_")}
        done = subprocess.run([sys.executable, str(HERE / "check-capacity.py"), "phase1"], env=environ, capture_output=True, text=True, timeout=30)
        self.assertNotEqual(0, done.returncode)
        self.assertIn("PACER_CAPACITY_READY=1", done.stderr)
        for argv, extra in ((["up"], {}), (["bogus"], {"PACER_CAPACITY_WINDOW": "1"}), ([], {"PACER_CAPACITY_WINDOW": "1"})):
            done = subprocess.run(["sh", str(HERE / "capacity-window.sh")] + argv, env=dict(environ, **extra), capture_output=True, text=True, timeout=30)
            self.assertEqual(2, done.returncode, done.stderr)
            self.assertIn("PACER_CAPACITY_WINDOW=1", done.stderr)


class Phases(unittest.TestCase):
    def setUp(self):
        self.out = tempfile.TemporaryDirectory()
        self.dir = pathlib.Path(self.out.name)

    def tearDown(self):
        self.out.cleanup()

    def assertNoSecrets(self):
        for path in self.dir.iterdir():
            self.assertNotRegex(path.read_text(), r"sk-ant-|Bearer")

    def test_phase1_runs_every_case_in_parallel_and_writes_the_restart_marker(self):
        lab = FakeLab()
        code, receipt, printed = run("phase1", lab, self.dir)
        self.assertEqual(0, code, json.dumps(receipt, indent=1)[:3000])
        self.assertTrue(receipt["pass"])
        self.assertEqual(sorted(check.PHASE1), receipt["cases_ran"])
        self.assertEqual(ACCOUNTS, tuple(receipt["verification"]["request_profile"]))
        self.assertEqual({}, receipt["verification"]["forced_refresh"])
        cases = receipt["cases"]
        self.assertEqual(3, cases["a"]["requests_between_samples"])
        self.assertEqual(LOWER_BOUND, cases["a"]["intervals"][check.SEVEN_DAY]["value"])
        self.assertGreaterEqual(cases["b"]["requests_between_samples"], 3)
        self.assertLess(cases["b"]["last_request_before_s2_seconds"], check.BUSY_PERIOD + 1)
        self.assertEqual("released", cases["d"]["held_stream"]["outcome"])
        self.assertEqual(0, cases["d"]["requests_between_samples"])
        self.assertEqual({"five_hour": 8, "seven_day": 18}, {k: cases["e"]["usage"][k] for k in ("five_hour", "seven_day")})
        self.assertEqual(3600, cases["c"]["usage"]["five_hour_reset"] - int(T0) - 18000)
        for case in cases.values():
            s1, s2 = check.parse_time(case["s1"]), check.parse_time(case["s2"])
            self.assertGreater(s1, check.parse_time(receipt["ready_at"]))
            self.assertEqual(check.ACCOUNT_MIN_GAP, round(s2 - s1))
        marker = json.loads((self.dir / check.RESTART_MARKER).read_text())
        self.assertEqual(set(ACCOUNTS), set(marker["last_reads"]))
        self.assertIn("case d (cap-d): PASS", printed)
        self.assertNoSecrets()

    def test_phase1_fails_one_case_on_a_wrong_judgment_and_writes_no_marker(self):
        lab = FakeLab(phase1={"cap-b": ("lower_bound", [], LOWER_BOUND)})
        code, receipt, printed = run("phase1", lab, self.dir)
        self.assertEqual(1, code)
        self.assertFalse(receipt["pass"])
        self.assertEqual([False], [c["pass"] for c in receipt["cases"].values() if c["account"] == "cap-b"])
        self.assertIn("expected unknown, got lower_bound", receipt["cases"]["b"]["error"])
        self.assertTrue(all(c["pass"] for c in receipt["cases"].values() if c["account"] != "cap-b"))
        self.assertFalse((self.dir / check.RESTART_MARKER).exists())
        self.assertIn("case b (cap-b): FAIL", printed)

    def test_phase1_case_d_accepts_only_the_native_sandwich_reason_as_extra(self):
        native = ["carry_in_unproven", "cut_not_quiescent", "dispatch_freedom_unproven", "physical_proof_unavailable"]
        self.assertEqual(0, run("phase1", FakeLab(phase1={"cap-d": ("unknown", native, None)}), self.dir)[0])
        other = sorted(native + ["missing_exact_proof"])
        code, receipt, _ = run("phase1", FakeLab(phase1={"cap-d": ("unknown", other, None)}), self.dir)
        self.assertEqual(1, code)
        self.assertIn("plus optionally", receipt["cases"]["d"]["error"])
        missing = ["carry_in_unproven", "dispatch_freedom_unproven", "physical_proof_unavailable"]
        self.assertEqual(1, run("phase1", FakeLab(phase1={"cap-d": ("unknown", missing, None)}), self.dir)[0])
        self.assertEqual(1, run("phase1", FakeLab(phase1={"cap-c": ("unknown", ["dispatch_freedom_unproven", "reset_between_samples"], None)}), self.dir)[0])

    def test_a_failed_rerun_removes_the_old_restart_marker(self):
        self.assertEqual(0, run("phase1", FakeLab(), self.dir)[0])
        self.assertTrue((self.dir / check.RESTART_MARKER).exists())
        self.assertEqual(1, run("phase1", FakeLab(capacity_known=True), self.dir)[0])
        self.assertFalse((self.dir / check.RESTART_MARKER).exists())

    def test_a_receipt_that_would_carry_a_credential_is_withheld(self):
        leaky = dict(PHASE1, **{"cap-e": ("unknown", ["sk-ant-oat01-leak"], None)})
        code, receipt, _ = run("phase1", FakeLab(phase1=leaky), self.dir)
        self.assertEqual(1, code)
        self.assertEqual({"phase": "phase1", "pass": False, "error": "receipt withheld: it carried a credential"}, receipt)
        self.assertNoSecrets()

    def test_phase1_refuses_misrouted_requests(self):
        code, receipt, _ = run("phase1", FakeLab(misroute={"cap-a": "cap-b"}), self.dir)
        self.assertEqual(1, code)
        self.assertIn("did not reach their prefixed logins", receipt["error"])
        self.assertNotIn("cases", receipt)

    def test_phase1_fails_a_case_whose_requests_land_elsewhere(self):
        code, receipt, _ = run("phase1", FakeLab(misroute={"cap-a": "cap-e"}, misroute_after=1), self.dir)
        self.assertEqual(1, code)
        self.assertIn("0 of 3 requests reached cap-a", receipt["cases"]["a"]["error"])
        self.assertTrue(receipt["cases"]["e"]["pass"])  # cap-e's own samples came after the strays

    def test_phase1_refuses_a_readback_that_claims_known_capacity_before_any_case(self):
        code, receipt, _ = run("phase1", FakeLab(capacity_known=True), self.dir)
        self.assertEqual(1, code)
        self.assertIn("measurement.capacity_known is not false", receipt["error"])
        self.assertNotIn("verification", receipt)

    def test_phase1_refuses_unparked_original_logins(self):
        code, receipt, _ = run("phase1", FakeLab(extra_rows=[{"id": "fixture-a.json", "provider": "claude", "capacity": {}}]), self.dir)
        self.assertEqual(1, code)
        self.assertIn("unknown_account_ref_tuple", receipt["error"])
        self.assertIn("fixture-a.json", receipt["error"])

    def test_phase1_forces_a_refresh_when_logins_already_carry_account_uuid(self):
        lab = FakeLab(uuid_on_disk=True)
        code, receipt, _ = run("phase1", lab, self.dir)
        self.assertEqual(0, code, receipt.get("error"))
        self.assertEqual([], receipt["verification"]["request_profile"])
        self.assertEqual(set(ACCOUNTS), set(receipt["verification"]["forced_refresh"]))
        self.assertEqual(dict.fromkeys(ACCOUNTS, 1), lab.generations)

    def test_a_run_without_judged_intervals_fails_every_case(self):
        code, receipt, _ = run("phase1", FakeLab(judge=lambda *_: None), self.dir)
        self.assertEqual(1, code)
        self.assertEqual(sorted(check.PHASE1), receipt["cases_ran"])
        for case in receipt["cases"].values():
            self.assertFalse(case["pass"])
            self.assertIn("no_judged_interval", case["error"])

    def test_phase2_needs_phase1_and_a_restart_and_rearm(self):
        code, receipt, _ = run("phase2", FakeLab(), self.dir)
        self.assertEqual(1, code)
        self.assertIn("phase1 has not passed", receipt["error"])
        lab = FakeLab()
        self.assertEqual(0, run("phase1", lab, self.dir)[0])
        code, receipt, _ = run("phase2", lab, self.dir)
        self.assertEqual(1, code)
        self.assertIn("window.json lacks", receipt["error"])
        lab.window_doc = {"cpa_started_at": 1, "hosts_armed_at": 2}
        code, receipt, _ = run("phase2", lab, self.dir)
        self.assertIn("CPA has not restarted", receipt["error"])
        self.assertEqual(1, code)

    def test_phase3_needs_phase2(self):
        code, receipt, _ = run("phase3", FakeLab(), self.dir)
        self.assertEqual(1, code)
        self.assertIn("phase2 has not passed", receipt["error"])

    def test_all_three_phases_across_a_restart_and_a_refresh(self):
        lab = FakeLab()
        self.assertEqual(0, run("phase1", lab, self.dir)[0])
        lab.advance(300)
        lab.restart()
        lab.advance(60)
        code, receipt, _ = run("phase2", lab, self.dir)
        self.assertEqual(0, code, json.dumps(receipt, indent=1)[:3000])
        for case in receipt["cases"].values():
            across = case["across_restart"]
            self.assertLess(check.parse_time(across["s1"]), lab.restart_at)
            self.assertGreater(check.parse_time(across["s2"]), lab.restart_at)
            self.assertEqual(SPANNING, across["intervals"][check.SEVEN_DAY]["reasons"])
            self.assertEqual(UNVERIFIED, case["intervals"][check.FIVE_HOUR]["reasons"])
        self.assertEqual("check-capacity.py phase3", receipt["next"])
        code, receipt, _ = run("phase3", lab, self.dir)
        self.assertEqual(0, code, json.dumps(receipt, indent=1)[:3000])
        self.assertEqual(set(ACCOUNTS), set(receipt["refresh"]))
        for name, case in receipt["cases"].items():
            self.assertEqual(LOWER_BOUND, case["intervals"][check.FIVE_HOUR]["value"])
            self.assertEqual(name == "c", "refresh_inside" in case)
            if "into_s1" in case:
                self.assertLessEqual(set(REVERIFYING), set(case["into_s1"][check.SEVEN_DAY]["reasons"]))
        self.assertEqual(2, lab.generations["cap-c"])
        self.assertNoSecrets()

    def test_phase2_fails_if_cpa_reverified_a_login_after_the_restart(self):
        lab = FakeLab()
        self.assertEqual(0, run("phase1", lab, self.dir)[0])
        lab.advance(300)
        lab.restart()
        lab.on_disk.discard("cap-e")  # cap-e's next request profiles it again
        code, receipt, _ = run("phase2", lab, self.dir)
        self.assertEqual(1, code)
        self.assertIn("profiled or refreshed after the restart", receipt["cases"]["e"]["error"])
        self.assertTrue(all(c["pass"] for n, c in receipt["cases"].items() if n != "e"))


if __name__ == "__main__":
    unittest.main()

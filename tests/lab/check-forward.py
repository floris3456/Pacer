"""Native v8/feed/pin acceptance, fixed offline dummy lab only.

Requires explicit parent ownership/readiness. Adds one dummy inference; never
resets evidence, clears gaps, changes profiles or invokes real provider APIs.
"""
import base64
import csv
import hashlib
import io
import json
import os
import pathlib
import time
import urllib.error
import urllib.request
import uuid

BASE = "http://pacer-lab-cpa.orb.local:8317"
UPSTREAM = "http://pacer-upstream.orb.local:8080"
ROOT = "/v8/management/plugins/pacer/reports/forward/"
HTTP = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def call(path, body=None, key="pacer-test-admin", raw=None, method=None):
    headers = {"Content-Type": "application/json"}
    if key:
        headers["Authorization"] = "Bearer " + key
    data = raw if raw is not None else None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(BASE + path, data=data, headers=headers, method=method)
    try:
        with HTTP.open(req, timeout=35) as response:
            return response.status, response.read(8 * 1024 * 1024 + 1), dict(response.headers)
    except urllib.error.HTTPError as error:
        return error.code, error.read(8 * 1024 * 1024 + 1), dict(error.headers)


def api(op, body=None):
    status, raw, headers = call(ROOT + op, body)
    assert status == 200, (op, status)
    assert "no-store" in headers.get("Cache-Control", "").lower()
    assert len(raw) <= 8 * 1024 * 1024
    return json.loads(raw)


def fixture(path):
    with HTTP.open(UPSTREAM + "/_test/" + path, timeout=5) as response:
        return json.load(response)


def download(op, pin):
    body = {"pin": pin}
    if op == "export-csv":
        body["limit"] = 100
    response = api(op, body)
    suffix = "csv" if op == "export-csv" else "json"
    assert response["filename"] == "pacer-forward." + suffix
    assert response["encoding"] == "base64"
    return base64.b64decode(response["content"], validate=True)


def pages(pin):
    result, cursor, seen = [], "", set()
    for _ in range(20):
        page = api("requests", {"pin": pin, "limit": 100, "cursor": cursor})
        assert page["pin"] == pin
        result.extend(page["rows"])
        cursor = page.get("next_cursor", "")
        if not cursor:
            return result
        assert cursor not in seen
        seen.add(cursor)
    raise AssertionError("fixture pagination exceeded its bound")


def check_value(added, before, after, expected_tier):
    assert expected_tier in ("unknown", "standard")
    assert len(added["origins"]) == 1
    origin = added["origins"][0]
    usage = origin["normalized_source_usage"]
    assert usage["cache_create_5m"] == 25 and usage["cache_create_1h"] == 75
    if expected_tier == "unknown":
        assert added["value"] is None and "charged_tier_unknown" in added["issues"]
        assert not origin["native_provenance"].get("charged_service_tier")
        assert after["valued_count"] == before["valued_count"]
        assert after["valued_subset"] == before["valued_subset"]
        return None
    assert origin["native_provenance"]["charged_service_tier"] == "standard"
    value = added["value"]
    assert value is not None
    assert value["tokens"]["cache_write_5m"] == 25 and value["tokens"]["cache_write_1h"] == 75
    assert value["tokens"]["cache_write_unknown"] == 0
    # Independent PLAN/CACHE-TTL rates, USD per million -> nano USD.
    expected = 10 * 4000 + 4 * 20000 + 500 * 200 + 25 * 5000 + 75 * 8000
    assert value["low_nano_usd"] == value["high_nano_usd"] == expected
    assert after["valued_count"] == before["valued_count"] + 1
    for field in ("low_nano_usd", "high_nano_usd"):
        assert after["valued_subset"][field] - before["valued_subset"][field] == expected
    return expected


def main():
    if os.environ.get("PACER_FORWARD_READY") != "1":
        raise SystemExit("Parent must authorize a ready exclusive forward lab window")
    out = pathlib.Path(os.environ["PACER_EVIDENCE"])
    expected_tier = os.environ.get("PACER_FORWARD_EXPECT_TIER", "unknown")
    assert expected_tier in ("unknown", "standard")
    receipt = {"pass": False, "expected_response_tier": expected_tier, "scope": "isolated native-only forward publication; no complete billing/capacity claim"}
    before = fixture("attempts")
    assert fixture("activity")["active_messages"] == 0
    receipt["attempts_before"] = len(before)
    receipt["before_attempt_digest"] = hashlib.sha256(json.dumps(before, sort_keys=True).encode()).hexdigest()
    try:
        for key in (None, "pacer-test-inference", "pacer-test-invalid-management"):
            assert call(ROOT + "status", key=key)[0] == 401
        for root in ("/v0/management/plugins/pacer/reports/forward/", "/v0/resource/plugins/pacer/reports/forward/"):
            assert call(root + "status")[0] == 404
        assert call(ROOT + "pin", raw=b'{"filter":{},"filter":{}}')[0] == 400
        assert call(ROOT + "pin", raw=b" " * 65537)[0] == 413
        assert call(ROOT + "pin", {"filter": {}, "coverage_complete": True})[0] == 400
        assert call(ROOT + "report", {})[0] == 400
        receipt["authentication_alias_and_strict_input_refusals"] = True

        initial = api("status")
        assert initial["publisher"]["state"] == "ready" and initial["publisher"]["capacity"] == "unavailable"
        pin = api("pin", {"filter": {"domain": "native", "provider": "claude", "model": "claude-opus-5-5"}})
        report = api("report", {"pin": pin})
        assert report["capacity"] == "unavailable"
        old_rows = pages(pin)
        old_json, old_csv = download("export-json", pin), download("export-csv", pin)
        assert json.loads(old_json)["pin"] == pin
        assert list(csv.reader(io.StringIO(old_csv.decode())))
        if len(old_rows) > 1:
            first = api("requests", {"pin": pin, "limit": 1})
            assert first.get("next_cursor")
            status, raw, _ = call(ROOT + "requests", {"pin": pin, "limit": 2, "cursor": first["next_cursor"]})
            assert status == 409 and json.loads(raw) == {"error": "cursor_conflict"}
        receipt["old_pin"] = pin
        receipt["old_export_sha256"] = {"json": hashlib.sha256(old_json).hexdigest(), "csv_page": hashlib.sha256(old_csv).hexdigest()}
        receipt["phase"] = "dummy-inference"
        status, raw, _ = call("/v1/messages", {
            "model": "claude-opus-5-5", "max_tokens": 64,
            "messages": [{"role": "user", "content": "Native forward fixture " + uuid.uuid4().hex + ". No reasoning."}],
        }, key="pacer-test-inference")
        assert status == 200, status
        usage = json.loads(raw)["usage"]
        assert usage["input_tokens"] == 10 and usage["output_tokens"] == 4
        assert usage["cache_creation"]["ephemeral_5m_input_tokens"] == 25
        assert usage["cache_creation"]["ephemeral_1h_input_tokens"] == 75
        if expected_tier == "standard":
            assert usage.get("service_tier") == "standard"
        else:
            assert not usage.get("service_tier")

        deadline = time.monotonic() + 70
        while time.monotonic() < deadline:
            current = api("status")
            if int(current["projected_through"]) > int(initial["projected_through"]) and current["publisher"]["state"] == "ready":
                new_pin = api("pin", {"filter": pin["filter"]})
                new_report = api("report", {"pin": new_pin})
                if new_report["totals"]["canonical_rows"] == report["totals"]["canonical_rows"] + 1:
                    break
            time.sleep(.2)
        else:
            raise AssertionError("new native usage did not reach a sealed forward report")
        assert new_report["capacity"] == "unavailable"
        new_rows = pages(new_pin)
        old_ids = {row["id"] for row in old_rows}
        additions = [row for row in new_rows if row["id"] not in old_ids]
        assert len(additions) == 1 and len(new_rows) == len(old_rows) + 1
        added = additions[0]
        expected = check_value(added, report["totals"], new_report["totals"], expected_tier)
        evidence = api("evidence", {"pin": new_pin, "id": added["id"]})
        assert evidence == added and evidence["capacity"] == "unavailable" and evidence["coverage_reasons"]
        public = json.dumps(evidence)
        assert not any(secret in public for secret in ("pacer-test-inference", "pacer-test-admin", "X-Pacer-Cache-Capability", "Authorization", "raw_usage"))
        assert api("report", {"pin": pin}) == report
        assert download("export-json", pin) == old_json and download("export-csv", pin) == old_csv
        receipt.update(pass_checks=True, phase="completed", new_pin=new_pin, added_id=added["id"], value_nano_usd=expected,
                       observed_write_5m=25, observed_write_1h=75, original_pin_exports_unchanged=True,
                       evidence_coverage_reasons=evidence["coverage_reasons"])
        receipt["pass"] = True
    finally:
        after = fixture("attempts")
        receipt.update(attempts_after=len(after), baseline_preserved=after[:len(before)] == before,
                       activity=fixture("activity"), fixture_analytics=fixture("analytics")["calls"])
        (out / "native-forward.json").write_text(json.dumps(receipt, indent=2) + "\n")
    assert receipt["baseline_preserved"] and len(after) == len(before) + 1 and receipt["activity"]["active_messages"] == 0
    print(json.dumps(receipt, indent=2))


if __name__ == "__main__":
    main()

"""Original-CPAMP oracle checks through the real native, offline lab HTTP path.

Requires the extended 240/65 frozen synthetic artifact and an exclusive parent
window. Does not import, prepare, write metadata, reset fixtures or infer.
"""
import collections
import gzip
import hashlib
import json
import os
import pathlib
import urllib.error
import urllib.request

BASE = "http://pacer-lab-cpa.orb.local:8317"
UPSTREAM = "http://pacer-upstream.orb.local:8080"
ROOT = "/v0/management/plugins/pacer/history/legacy/"
ADMIN = "pacer-test-admin"  # Literal isolated-lab credential, never a live key.
MAX_OUTPUT = 8 << 20


def csv_records(text):
    """Keep complete original encoded row bytes; only row ordering may differ."""
    records, start, quoted, index = [], 0, False, 0
    while index < len(text):
        char = text[index]
        if char == '"':
            if quoted and index + 1 < len(text) and text[index + 1] == '"':
                index += 2
                continue
            quoted = not quoted
        elif char == "\n" and not quoted:
            records.append(text[start:index + 1])
            start = index + 1
        index += 1
    if quoted or start != len(text):
        raise ValueError("Incomplete oracle CSV record")
    return records


def same_csv_rows(left, right):
    a, b = csv_records(left), csv_records(right)
    return bool(a and b) and a[0] == b[0] and collections.Counter(a[1:]) == collections.Counter(b[1:])


def load_oracles():
    base = pathlib.Path(__file__).resolve().parents[2] / "internal/historyreports/testdata/historical-golden"
    provenance = json.loads((base / "provenance.json").read_bytes())
    assert provenance["source_commit"] == "e8492c245bf69144afdcf20b8b4ea8935f0476e5"
    result = []
    for name in ("historical-oracle.json", "historical-exports.json"):
        raw = gzip.decompress((base / (name + ".gz")).read_bytes())
        assert hashlib.sha256(raw).hexdigest() == provenance["outputs"][name], "Oracle digest mismatch"
        result.append(json.loads(raw))
    return (*result, provenance)


def analytics_calls(http, origin):
    with http.open(origin + "/_test/analytics", timeout=3) as response:
        data = json.load(response)
    assert isinstance(data.get("calls"), int) and data["calls"] >= 0, "Invalid fixture analytics counter"
    return data


def main():
    if os.environ.get("PACER_HISTORY_READY") != "1":
        raise SystemExit("Parent must load the extended artifact and grant an exclusive history window first")
    out = pathlib.Path(os.environ["PACER_EVIDENCE"])
    out.mkdir(parents=True, exist_ok=True)
    receipt = {"checks": [], "domain_responses": 0, "byte_exact_exports": 0,
               "row_exact_exports": 0, "scope": "Offline synthetic native HTTP acceptance, not real-data migration"}
    http = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def call(path, body=None, token=ADMIN, raw=None, method=None):
        data = raw if raw is not None else None if body is None else json.dumps(body).encode()
        headers = {"Content-Type": "application/json"}
        if token:
            headers["Authorization"] = "Bearer " + token
        request = urllib.request.Request(BASE + path, data=data, headers=headers, method=method)
        try:
            response = http.open(request, timeout=40)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            data = response.read(MAX_OUTPUT + 1)
            assert len(data) <= MAX_OUTPUT, "Unbounded native output"
            return response.code, data, response.headers

    def step(name, ok):
        receipt["checks"].append({"name": name, "pass": bool(ok)})
        assert ok, name

    def post(op, body):
        status, raw, headers = call(ROOT + op, body)
        step("200-no-store:" + op, status == 200 and headers.get("Cache-Control") == "no-store")
        assert b"/lab/" not in raw and b"synthetic-source-private-sentinel" not in raw
        return json.loads(raw)

    def stats():
        return analytics_calls(http, UPSTREAM)

    try:
        cases, exports, provenance = load_oracles()
        receipt["oracle_provenance"] = provenance
        before = stats()
        status, raw, _ = call(ROOT + "status")
        ready = json.loads(raw)
        step("explicit-extended-frozen-boundary", status == 200 and ready.get("historical_ready") is True
             and ready["default_pin"]["through"] == 240 and ready["default_pin"]["observations_through"] == 65
             and ready["default_pin"]["forecasts_through"] == 5 and ready["default_pin"]["verifications_through"] == 6)
        receipt["default_pin"] = ready["default_pin"]
        for case in cases:
            view, name = case["View"], case["Name"]
            query = {"pin": case["Quota"] if view == "quota" else case["Filter"]}
            body = {"query": query}
            if view in ("profile-proof", "history-proof", "forecasts"):
                body["window_id"] = case["Window"]
            if view == "history" and case["Kind"]:
                body["kind"] = case["Kind"]
            if view == "forecasts":
                body["limit"] = 100
            actual = post(view, body)
            step("original-domain:" + view + "/" + name,
                 (actual["forecasts"] if view == "forecasts" else actual) == case["Output"])
            receipt["domain_responses"] += 1
            if view == "forecasts":
                continue
            for format_ in ("json", "csv"):
                export = {"query": query, "format": format_}
                if view != "quota":
                    export["view"] = view
                    if case["Window"]:
                        export["window_id"] = case["Window"]
                    if case["Kind"]:
                        export["kind"] = case["Kind"]
                envelope = post("quota-export" if view == "quota" else "export", export)
                key = view + "/" + name + "/" + format_
                expected = exports[key]
                step("download-envelope:" + key, envelope["filename"] == "pacer-legacy-" + view + "." + format_
                     and envelope["media_type"] == ("application/json" if format_ == "json" else "text/csv"))
                if format_ == "csv" and view in ("profiles", "profile-proof", "history-proof"):
                    step("original-encoded-csv-rows:" + key, same_csv_rows(envelope["content"], expected))
                    receipt["row_exact_exports"] += 1
                else:
                    step("original-export-bytes:" + key, envelope["content"] == expected)
                    receipt["byte_exact_exports"] += 1

        page_body = {"query": {"pin": ready["default_pin"]}, "window_id": "forecast-window", "limit": 1}
        first = post("forecasts", page_body)
        step("saved-forecast-first-page", first["total"] == 2 and len(first["forecasts"]) == 1
             and first["forecasts"][0]["forecast"]["id"] == "saved-0" and bool(first["next_cursor"]))
        page_body["cursor"] = first["next_cursor"]
        second = post("forecasts", page_body)
        step("saved-unscorable-revision-survives", len(second["forecasts"]) == 1 and not second.get("next_cursor")
             and second["forecasts"][0]["forecast"]["id"] == "saved-1"
             and second["forecasts"][0]["results"][-1]["state"] == "unscorable")
        changed = dict(page_body, limit=2)
        step("cursor-filter-refusal", call(ROOT + "forecasts", changed)[0] == 409)
        changed = dict(page_body, cursor=page_body["cursor"] + "x")
        step("cursor-tamper-refusal", call(ROOT + "forecasts", changed)[0] == 409)
        for view in ("status", "profiles", "profile-proof", "history", "history-proof", "forecasts", "quota", "export", "quota-export"):
            for role, token in (("anonymous", ""), ("inference", "pacer-test-inference"), ("unknown", "pacer-test-unknown")):
                # A successful administrator request resets CPA's failed-login
                # counter; this is authorization testing, not a lockout/load test.
                assert call(ROOT + "status")[0] == 200
                status = call(ROOT + view, None if view == "status" else {}, token=token)[0]
                step(role + "-refused:" + view, status == 401)
            assert call(ROOT + "status")[0] == 200
            public = "/v0/resource/plugins/pacer/history/legacy/" + view
            step("no-public-history-alias:" + view, call(public, token="")[0] == 404)
        for op, raw in (("profiles", b'{"query":{},"query":{}}'), ("profiles", b'{"Query":{}}'),
                        ("profiles", b'{"query":{"selection":{"model":null}}}'),
                        ("profiles", b'{"archive_path":"/private/sentinel"}'),
                        ("quota", b'{"query":{"capacity_ratio":10001}}')):
            status, response, _ = call(ROOT + op, raw=raw)
            step("ambiguous-or-unsupported-input-refused:" + raw.decode(), status == 400 and b"sentinel" not in response)
        future = dict(ready["default_pin"], through=241)
        step("future-pin-refused", call(ROOT + "profiles", {"query": {"pin": future}})[0] == 400)
        step("input-byte-ceiling", call(ROOT + "profiles", raw=b"{}" + b" " * (64 * 1024 - 1))[0] == 413)
        step("no-offline-preparation-endpoint", call(ROOT + "prepare", {})[0] == 404)
        after = stats()
        receipt["provider_calls"] = {"before": before, "after": after}
        step("reads-never-poll-provider", before == after)
        step("complete-oracle-inventory", receipt["domain_responses"] == 53 and receipt["byte_exact_exports"] == 67
             and receipt["row_exact_exports"] == 37)
        receipt["pass"] = True
    except Exception as error:
        # Exceptions contain only named assertions; do not serialize transport
        # headers, bearer values, raw SQL or private artifact paths.
        receipt["pass"] = False
        receipt["failure_type"] = type(error).__name__
        raise
    finally:
        (out / "native-history.json").write_text(json.dumps(receipt, indent=2) + "\n")
    print(json.dumps({key: receipt[key] for key in ("pass", "domain_responses", "byte_exact_exports", "row_exact_exports")}))


if __name__ == "__main__":
    main()

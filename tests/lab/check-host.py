"""Offline native-host checks; fixed dummy credentials, no live provider use."""
import json
import os
import pathlib
import urllib.error
import urllib.request

BASE = os.environ.get("PACER_LAB", "http://pacer-lab-cpa.orb.local:8317")
if BASE not in ("http://pacer-lab-cpa.orb.local:8317", "http://pacer-lab-cpa:8317"):
    raise SystemExit("Only the isolated Pacer lab is allowed")
http = urllib.request.build_opener(urllib.request.ProxyHandler({}))
checks = []
for path, key, want in [
    ("/v0/resource/plugins/pacer/index.html", None, 200),
    ("/v0/management/plugins/pacer/state", None, 401),
    ("/v0/management/plugins/pacer/state", "pacer-test-inference", 401),
    ("/v0/management/plugins/pacer/state", "pacer-test-invalid-management", 401),
    ("/v0/management/plugins/pacer/state", "pacer-test-admin", 200),
    ("/v8/management/oauth/settings/models", "pacer-test-admin", 200),
    ("/v0/resource/plugins/pacer/state", None, 404),
    ("/v0/resource/plugins/pacer/events", None, 200),
    ("/v0/management/plugins/pacer/observations", "pacer-test-admin", 400),
    ("/v0/management/plugins/pacer/observations", "pacer-test-inference", 401),
]:
    request = urllib.request.Request(BASE + path, headers={} if key is None else {"Authorization": "Bearer " + key})
    try:
        with http.open(request, timeout=5) as response:
            status, body = response.status, response.read()
    except urllib.error.HTTPError as error:
        status, body = error.code, error.read()
    checks.append({"path": path, "auth": "none" if key is None else "inference" if key.endswith("inference") else "unknown-bearer" if key == "pacer-test-invalid-management" else "management",
                   "status": status, "expected": want, "pass": status == want})
    if path.endswith("/events") and status == 200:
        discovery = json.loads(body)
        assert discovery["mode"] == "off" and discovery["protocol"] == 1
request = urllib.request.Request(BASE + "/v0/management/plugins", headers={"Authorization": "Bearer pacer-test-admin"})
with http.open(request, timeout=5) as response:
    plugins = json.load(response)["plugins"]
pacer = next(plugin for plugin in plugins if plugin["id"] == "pacer")
checks.append({"name": "native-plugin-registered-and-enabled", "pass": pacer["registered"] and pacer["effective_enabled"]})
request = urllib.request.Request(BASE + "/v0/management/plugins/pacer/state", headers={"Authorization": "Bearer pacer-test-admin"})
with http.open(request, timeout=5) as response:
    accounts = json.load(response)["accounts"]
assert accounts, "lab must expose dummy subscription credentials"
request = urllib.request.Request(BASE + "/v0/management/plugins/pacer/observations?auth_index=" + accounts[0]["auth_index"], headers={"Authorization": "Bearer pacer-test-admin"})
with http.open(request, timeout=5) as response:
    observation = json.load(response)
checks.append({"name": "unreserved-native-observation-route", "pass": isinstance(observation["windows"], list) and "reconciliation" in observation})
out = pathlib.Path(os.environ.get("PACER_EVIDENCE", "../../evidence/implementation"))
out.mkdir(parents=True, exist_ok=True)
(out / "native-host-auth.json").write_text(json.dumps(checks, indent=2) + "\n")
print(json.dumps(checks, indent=2))
assert all(check["pass"] for check in checks)

"""Read-only production export into a PRIVATE, isolated preview snapshot.

Never writes to production or exports usable authentication. Data stays on the
server; only snapshot metadata is printed. Run as the operator on the CPA host.
"""
import argparse
import base64
import datetime
import hashlib
import json
import mmap
import os
from pathlib import Path
import re
import shutil
import sqlite3
import subprocess
import urllib.parse
import urllib.request

ADMIN = "pacer-preview-admin-not-production"
SECRET_FIELDS = {"api-key", "api_key", "access_token", "refresh_token", "id_token", "secret-key", "token"}


def yaml_text(value, indent=0):
    """Emit block YAML with JSON-quoted scalars (also accepted by older plugins)."""
    lines = []
    entries = value.items() if isinstance(value, dict) else [(None, item) for item in value]
    for key, item in entries:
        if key is None and isinstance(item, dict) and item:
            block = yaml_text(item, indent+2).splitlines(keepends=True)
            lines.append(" " * indent + "- " + block[0][indent+2:] + "".join(block[1:]))
            continue
        label = "-" if key is None else (key if re.fullmatch(r"[A-Za-z0-9_.-]+", key) else json.dumps(key)) + ":"
        if isinstance(item, (dict, list)) and item:
            lines.append(" " * indent + label + "\n" + yaml_text(item, indent+2))
        else:
            lines.append(" " * indent + label + " " + json.dumps(item) + "\n")
    return "".join(lines)


def secret_values(value, field=""):
    if isinstance(value, dict):
        return set().union(*(secret_values(v, k) for k, v in value.items())) if value else set()
    if isinstance(value, list):
        return set().union(*(secret_values(v, field) for v in value)) if value else set()
    return {value.encode()} if isinstance(value, str) and len(value) >= 16 and field in SECRET_FIELDS | {"api-keys"} else set()


def backup(source, target):
    src = sqlite3.connect("file:" + str(source) + "?mode=ro", uri=True, timeout=30)
    dst = sqlite3.connect(str(target))
    try:
        src.execute("BEGIN")
        generation = src.execute("PRAGMA schema_version").fetchone()[0]
        guarded = src.execute("SELECT count(*) FROM sqlite_master WHERE name='measurement_report_meta'").fetchone()[0]
        if guarded:
            assert src.execute("SELECT schema_cookie FROM measurement_report_meta WHERE id=1").fetchone()[0] == generation, "Source feed is not valid"
        src.backup(dst)
        # SQLite's backup API replaces the destination header generation. This
        # restores the generation captured from the SAME source read snapshot;
        # it never blesses an unknown migration or changes the saved feed cookie.
        dst.execute("PRAGMA schema_version=" + str(generation))
        assert dst.execute("PRAGMA quick_check").fetchone()[0] == "ok"
    finally:
        dst.close()
        src.close()
    os.chmod(target, 0o600)


def take_snapshot(root, production, key_file):
    root, production = root.resolve(), production.resolve()
    if root == production or production in root.parents:
        raise ValueError("Preview must be outside the production directory")
    inspect = lambda: subprocess.check_output(["docker", "inspect", "--format", "{{.Image}} {{.State.StartedAt}}", "cliproxyapi"], text=True).strip().split()
    before_process = inspect()
    key = re.search(r'Bearer\s+([^"\s]+)', key_file.read_text()).group(1)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def api(path):
        request = urllib.request.Request("http://127.0.0.1:8317" + path, headers={"Authorization": "Bearer " + key})
        with opener.open(request, timeout=60) as response:
            return json.load(response)

    config = api("/v8/management/config")
    inventory = api("/v0/management/auth-files")["files"]
    secrets = secret_values(config) | {key.encode()}
    stamp = datetime.datetime.now(datetime.timezone.utc)
    name = stamp.strftime("%Y%m%dT%H%M%SZ")
    target = root / "snapshots" / name
    target.mkdir(parents=True, mode=0o700)
    os.chmod(root, 0o700)
    for directory in ("data", "auths", "plugins/linux/amd64"):
        (target / directory).mkdir(parents=True, mode=0o700)
    data = production / "plugins/pacer"
    # Downstream stores first, authoritative state last: projections never point
    # beyond the copied source head. Each database is a consistent SQLite backup.
    paths = sorted(data.glob("*.sqlite"), key=lambda p: (p.name == "state.sqlite", p.name == "forward-report.sqlite", p.name))
    for path in paths:
        if path.is_symlink(): raise ValueError("Symlinked database refused")
        backup(path, target / "data" / path.name)
    state = sqlite3.connect(str(target / "data/state.sqlite"))
    fixtures = {}
    providers = {}
    identity_fields = {"type", "disabled", "email", "account_uuid", "organization_uuid", "organization_name", "account_id", "project_id", "plan_type", "websockets", "priority", "id", "label", "prefix"}
    for index, entry in enumerate(inventory):
        filename = entry["name"]
        if Path(filename).name != filename or not filename.endswith(".json"):
            raise ValueError("Unexpected credential filename")
        raw = api("/v0/management/auth-files/download?name=" + urllib.parse.quote(filename, safe=""))
        secrets |= secret_values(raw)
        provider = entry.get("provider") or entry.get("type")
        dummy = "preview-dummy-" + provider + "-" + hashlib.sha256(filename.encode()).hexdigest()[:16]
        auth = {k: v for k, v in raw.items() if k in identity_fields}
        for field in ("access_token", "refresh_token", "api_key"):
            if field in raw: auth[field] = dummy
        if raw.get("id_token"):
            # Retain identity claims, but use an unmistakable unsigned dummy JWT.
            try:
                payload = raw["id_token"].split(".")[1]
                claims = json.loads(base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4)))
            except (ValueError, IndexError):
                claims = {"email": auth.get("email", "preview@example.invalid")}
            claims = {k:v for k,v in claims.items() if k in ("sub", "email", "name", "iss", "aud", "https://api.openai.com/auth")}
            claims["exp"] = 4070908800
            encode = lambda v: base64.urlsafe_b64encode(json.dumps(v).encode()).decode().rstrip("=")
            auth["id_token"] = encode({"alg": "none", "typ": "JWT"}) + "." + encode(claims) + ".preview-not-a-signature"
        auth["expired"] = "2099-01-01T00:00:00Z"
        auth["last_refresh"] = stamp.isoformat()
        (target / "auths" / filename).write_text(json.dumps(auth, indent=2))
        ident = entry.get("id") or filename
        rows = state.execute("SELECT DISTINCT json_extract(body,'$.AccountID') FROM pacer_usage_credentials WHERE auth=?", (ident,)).fetchall()
        identity = "credential:" + ident
        account = raw.get("account_uuid") if provider == "claude" else raw.get("account_id") if provider == "codex" else None
        if account: identity = "account:" + account
        native = rows[0][0] if len(rows) == 1 else provider + ":" + hashlib.sha256((provider + ":" + identity).encode()).hexdigest()
        windows = [json.loads(r[0]) for r in state.execute("SELECT body FROM observation_windows WHERE account=? AND provider=?", (native, provider))]
        fixtures[dummy] = {"provider": provider, "windows": windows}
        providers[provider] = providers.get(provider, 0) + 1
    state.close()

    plugins = config["plugins"]["configs"]
    pacer = {"enabled": True, "priority": 1, "state_path": "/preview/runtime/data/state.sqlite", "report_path": "/preview/runtime/data/reports.sqlite", "forward_report_path": "/preview/runtime/data/forward-report.sqlite", "forward_report_book": plugins["pacer"]["forward_report_book"], "compact_on_switch": True, "routing": {"mode": "off"}}
    preview = {"config-version": 8, "server": {"host": "0.0.0.0", "port": 8317}, "management": {"allow-remote": True, "secret-key": ADMIN, "disable-control-panel": True}, "access": {"api-keys": ["pacer-preview-client-" + str(i+1) for i in range(len(config["access"]["api-keys"]))]}, "oauth": {"auth-dir": "/preview/runtime/auths", **{k: v for k, v in config["oauth"].items() if k in ("settings", "excluded-models", "model-alias")}}, "plugins": {"enabled": True, "dir": "/preview/runtime/plugins", "configs": {"pacer": pacer}}, "routing": {"strategy": "fill-first"}}
    if "opencode-go-cliproxyapi" in plugins:
        preview["plugins"]["configs"]["opencode-go-cliproxyapi"] = {"enabled": True, "api-keys": [{"value": token} for token, fixture in fixtures.items() if fixture["provider"] == "opencode-go"], "base-url": "https://opencode.ai/zen/go/v1", "model-prefix": plugins["opencode-go-cliproxyapi"].get("model-prefix", "")}
    compatibility = []
    for i, item in enumerate(config.get("api-keys", {}).get("openai-compatibility", [])):
        clean = {k:v for k,v in item.items() if k in ("name", "priority", "disabled", "prefix", "models", "excluded-models")}
        clean.update({"base-url":"https://opencode.ai/preview-unsupported", "keys":[{"api-key":"preview-compat-"+str(i)+"-"+str(j)} for j,_ in enumerate(item.get("keys", []))]})
        compatibility.append(clean)
    codex = []
    for i, item in enumerate(config.get("api-keys", {}).get("codex", [])):
        clean = {k:v for k,v in item.items() if k in ("name", "priority", "prefix", "models", "excluded-models")}
        clean.update({"keys": [{"api-key": "preview-codex-api-key-" + str(i) + "-" + str(j), **{k:v for k,v in key.items() if k in ("weight", "websockets")}} for j,key in enumerate(item.get("keys", []))], "base-url": "https://chatgpt.com/backend-api/codex"})
        codex.append(clean)
    preview["api-keys"] = {"openai-compatibility": compatibility, "codex": codex}
    for library in (production / "plugins/linux/amd64").glob("*.so"):
        if library.name.startswith(("pacer-", "opencode-go-")):
            shutil.copy2(library, target / "plugins/linux/amd64" / library.name)
    (target / "config.yaml").write_text(yaml_text(preview))
    (target / "fixtures.json").write_text(json.dumps(fixtures, indent=2))
    # Exact current secrets are never exported, including into copied raw blobs.
    # Keep a failed snapshot private for diagnosis; do not make it runnable.
    pattern = re.compile(b"|".join(re.escape(s) for s in sorted(secrets, key=len, reverse=True)))
    for path in target.rglob("*"):
        if not path.is_file() or not path.stat().st_size: continue
        with path.open("rb") as stream, mmap.mmap(stream.fileno(), 0, access=mmap.ACCESS_READ) as content:
            if pattern.search(content):
                raise ValueError("Snapshot contains a source credential; export refused (file: " + path.name + ")")
    assert inspect() == before_process, "Production runtime changed during snapshot; take a new snapshot"
    manifest = {"snapshot": name, "snapshot_at": stamp.isoformat(), "completed_at": datetime.datetime.now(datetime.timezone.utc).isoformat(), "cpa_image": before_process[0], "production_started_at": before_process[1], "providers": providers, "credential_scan": "passed", "source": "live read-only SQLite snapshots; no production restart", "dummy_auths": len(inventory)}
    (target / "manifest.json").write_text(json.dumps(manifest, indent=2))
    return target, manifest


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", type=Path, default=Path("/opt/pacer-dev"))
    parser.add_argument("--production", type=Path, default=Path("/opt/cliproxyapi"))
    parser.add_argument("--management-key-file", type=Path, default=Path("/etc/caddy/cliproxyapi-management-auth.Caddyfile"))
    args = parser.parse_args()
    _, manifest = take_snapshot(args.root, args.production, args.management_key_file)
    print(json.dumps(manifest))

#!/usr/bin/env python3
"""Swap the Pacer library on CT104 only after it registered against a copy of production's data.

Run on CT104 (python3 + PyYAML, root). The library directory is a build's dist/linux_amd64
(pacer-v<version>.so and native-probe, both from scripts/build.sh).

  python3 -B swap.py check <dist> [--rollback <old.so>] [--state <file>]   copy, register twice; nothing live changes
  python3 -B swap.py swap  <dist> --backup <dir>           check, back up, swap, restart CPA once

`check` takes an online backup of every database in Pacer's state directory (state_path's
directory holds the derived capacity, prefix and usage-record stores) and the configured report
stores, rewrites the production Pacer config onto the copy and registers the library through the
C ABI twice: the swap's start and the next restart. A schema change that only fails on an
existing database (2026-10-15: a new table tripped the report feed's schema witness and Pacer did
not register) fails here. `--rollback` then registers the old library on the upgraded copy, so a
rollback does not need the database backup.

`swap` refuses unless `check` passes moments before, so the restart is an ordinary one: CPA is
down for seconds and running OpenCodePlus chats retry, with Pacer answering when it is back. If
Pacer does not answer within the wait, the previous library is restored with one more restart.

Exit codes: 0 ok, 1 check failed (nothing changed), 2 swap failed and rolled back, 3 rollback
failed (act now).
"""
import argparse
import base64
import json
import re
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import time
import urllib.request
from pathlib import Path

import yaml

HOST = Path("/opt/cliproxyapi")
CONTAINER = "/CLIProxyAPI"  # where the CPA container sees HOST
CONFIG = HOST / "config.yaml"
LIBRARY_DIR = HOST / "plugins/linux/amd64"
CADDY_AUTH = Path("/etc/caddy/cliproxyapi-management-auth.Caddyfile")
MANAGEMENT = "http://127.0.0.1:8317"


def say(*parts):
    print(*parts, flush=True)


def host_path(path):
    if not path.startswith(CONTAINER + "/"):
        raise SystemExit(f"config path outside {CONTAINER}: {path}")
    return HOST / path[len(CONTAINER) + 1:]


def pacer_config():
    return yaml.safe_load(CONFIG.read_text())["plugins"]["configs"]["pacer"]


def library(dist):
    found = sorted(Path(dist).glob("pacer-v*.so"))
    if len(found) != 1 or not (Path(dist) / "native-probe").is_file():
        raise SystemExit(f"{dist}: need exactly one pacer-v*.so and native-probe")
    version = re.fullmatch(r"pacer-v(.+)\.so", found[0].name).group(1)
    return found[0], Path(dist) / "native-probe", version


def witness(path):
    c = sqlite3.connect(f"file:{path}?mode=ro", uri=True)
    try:
        return c.execute("SELECT schema_cookie,(SELECT schema_version FROM pragma_schema_version) "
                         "FROM measurement_report_meta WHERE id=1").fetchone()
    finally:
        c.close()


def backup_sqlite(source, target):
    """An online backup that keeps the source's schema version.

    SQLite's backup API restarts the copy's schema version at 1, and the report feed keeps the
    schema version as its witness: a copy made that way reads as a schema that went backwards, so
    Pacer refuses it, as it would refuse a restored rollback backup.
    """
    src = sqlite3.connect(f"file:{source}?mode=ro", uri=True, timeout=60)
    dst = sqlite3.connect(target)
    try:
        for _ in range(5):
            before = src.execute("PRAGMA schema_version").fetchone()[0]
            src.backup(dst)
            if src.execute("PRAGMA schema_version").fetchone()[0] == before:
                break
        else:
            raise SystemExit(f"{source}: schema kept changing during the backup")
        dst.execute(f"PRAGMA schema_version={int(before)}")
        dst.commit()
        if dst.execute("PRAGMA quick_check").fetchone()[0] != "ok":
            raise SystemExit(f"{target}: quick_check failed")
    finally:
        dst.close()
        src.close()


def copy_data(cfg, scratch, state_source=None):
    """Online backups of Pacer's databases; returns the config rewritten onto the copy."""
    state_dir = host_path(cfg["state_path"]).parent
    sources = {p for p in state_dir.iterdir() if p.is_file() and not p.name.endswith(("-wal", "-shm"))}
    sources |= {host_path(v) for k, v in cfg.items() if k.endswith("_path") and isinstance(v, str)}
    need = sum(p.stat().st_size for p in sources if p.exists())
    free = shutil.disk_usage(scratch).free
    if free < need * 1.2 + (1 << 30):
        raise SystemExit(f"not enough space for the copy: need {need >> 20} MiB, free {free >> 20} MiB")
    mapping = {}
    for source in sorted(sources):
        target = Path(scratch) / source.name
        if source.name in {t.name for t in mapping.values()}:
            raise SystemExit(f"two stores named {source.name}")
        mapping[source] = target
        if not source.exists():
            continue
        if source == host_path(cfg["state_path"]) and state_source:
            source = Path(state_source)
        # A read-only store (the frozen report artifact) is immutable and must stay a lone
        # read-only file; the live ones get an online backup with the source's mode.
        if source.suffix == ".sqlite" and source.stat().st_mode & 0o200:
            backup_sqlite(source, target)
            target.chmod(source.stat().st_mode & 0o777)
        else:
            shutil.copy2(source, target)
    rewritten = dict(cfg)
    for key, value in cfg.items():
        if key.endswith("_path") and isinstance(value, str):
            rewritten[key] = str(mapping[host_path(value)])
    return rewritten


def register(lib, probe, version, cfg, scratch, label):
    # The host schema CPA offers on CT104; a managed cache policy refuses anything older.
    payload = json.dumps({"config_yaml": base64.b64encode(yaml.safe_dump(cfg).encode()).decode(), "schema_version": 7})
    if len(payload) >= 8000:
        raise SystemExit("config too large for native-probe")
    request = Path(scratch) / "register.json"
    request.write_text(payload)
    run = subprocess.run([str(probe), str(lib), str(request), version], capture_output=True, text=True, timeout=300)
    ok = run.returncode == 0
    say(f"{label}: {'registered' if ok else 'FAILED'}", (run.stdout + run.stderr).strip()[-600:])
    return ok


def check(dist, rollback=None, keep=False, state_source=None):
    lib, probe, version = library(dist)
    cfg = pacer_config()
    scratch = tempfile.mkdtemp(prefix="pacer-check-", dir=HOST)
    try:
        say(f"copying Pacer data to {scratch}")
        copy = copy_data(cfg, scratch, state_source)
        state = copy["state_path"]
        say("schema witness before (saved, current):", witness(state))
        ok = all(register(lib, probe, version, copy, scratch, f"{lib.name} start {n}") for n in (1, 2))
        say("schema witness after (saved, current):", witness(state))
        if ok and rollback:
            old = Path(rollback)
            name = re.fullmatch(r"pacer-v(.+)\.so", old.name)
            ok = register(old, probe, name.group(1) if name else version, copy, scratch, f"rollback {old}")
        return ok
    finally:
        if keep:
            say(f"kept {scratch}")
        else:
            shutil.rmtree(scratch, ignore_errors=True)


def state():
    key = re.search(r"(?i)Bearer\s+([^\"\s]+)", CADDY_AUTH.read_text()).group(1)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    request = urllib.request.Request(MANAGEMENT + "/v0/management/plugins/pacer/state",
                                     headers={"Authorization": "Bearer " + key})
    return json.loads(opener.open(request, timeout=10).read())


def restart_and_wait(wait):
    started = time.monotonic()
    subprocess.run(["docker", "restart", "cliproxyapi"], check=True, capture_output=True)
    while time.monotonic() - started < wait:
        try:
            s = state()
            say(f"Pacer answers after {time.monotonic() - started:.1f}s: mode {s['routing']['mode']}, "
                f"decision write errors {s['decision_write_errors']}")
            return True
        except Exception:
            time.sleep(1)
    say(f"Pacer did not answer within {wait}s")
    return False


def install(source, target):
    staged = target.with_name(".pacer-swap.so")
    shutil.copy2(source, staged)
    shutil.chown(staged, 1000, 1000)
    staged.chmod(0o755)
    staged.replace(target)


def swap(dist, backup, wait):
    lib, _, _ = library(dist)
    target = LIBRARY_DIR / lib.name
    backup = Path(backup)
    backup.mkdir(mode=0o700, parents=True, exist_ok=True)
    previous = backup / lib.name
    if not previous.exists():
        shutil.copy2(target, previous)
    if not check(dist, rollback=previous):
        say("check failed: nothing changed")
        return 1
    state_path = host_path(pacer_config()["state_path"])
    backup_sqlite(state_path, backup / "state.sqlite")
    shutil.copy2(CONFIG, backup / "config.yaml")
    install(lib, target)
    say(f"installed {lib} -> {target}")
    if restart_and_wait(wait):
        return 0
    install(previous, target)
    say(f"restored {previous}")
    return 2 if restart_and_wait(wait) else 3


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)
    c = sub.add_parser("check")
    c.add_argument("dist")
    c.add_argument("--rollback")
    c.add_argument("--keep", action="store_true")
    c.add_argument("--state", help="register against a copy of this state.sqlite instead of the live one")
    s = sub.add_parser("swap")
    s.add_argument("dist")
    s.add_argument("--backup", required=True)
    s.add_argument("--wait", type=int, default=60)
    args = parser.parse_args()
    if args.command == "check":
        sys.exit(0 if check(args.dist, args.rollback, args.keep, args.state) else 1)
    sys.exit(swap(args.dist, args.backup, args.wait))


if __name__ == "__main__":
    main()

"""Reusable Pacer preview commands. No command restarts or promotes production."""
import argparse
import datetime
import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import sys
import tarfile

HERE = Path(__file__).resolve().parent


def run(*args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)


def compose(root, *args):
    return run("docker", "compose", "--env-file", str(root / "preview.env"), "-f", str(root / "source/dev/compose.yaml"), *args)


def sync(host, root):
    source = HERE.parent
    files = subprocess.check_output(["git", "ls-files", "-z"], cwd=source).decode().split("\0")
    run("ssh", host, "umask 077; mkdir -p " + shlex.quote(str(root / "source")))
    child = subprocess.Popen(["ssh", host, "tar -xf - -C " + shlex.quote(str(root / "source"))], stdin=subprocess.PIPE)
    try:
        with tarfile.open(fileobj=child.stdin, mode="w|") as archive:
            for name in files:
                if name and (source / name).is_file(): archive.add(source / name, arcname=name, recursive=False)
    finally:
        child.stdin.close()
    if child.wait(): raise RuntimeError("Source sync failed")
    print("Preview source synced; UI edits appear on browser reload.")


def activate(root, snapshot=None, fresh=False):
    current = root / "current.json"
    if current.exists() and not snapshot and not fresh:
        compose(root, "up", "-d", "--pull", "never")
        return
    if not snapshot:
        completed = sorted((root / "snapshots").glob("*/manifest.json"))
        if not completed: raise RuntimeError("Create a snapshot first")
        snapshot = completed[-1].parent.name
    if Path(snapshot).name != snapshot: raise ValueError("Snapshot must be a name")
    saved = root / "snapshots" / snapshot
    manifest = json.loads((saved / "manifest.json").read_text())
    assert manifest["credential_scan"] == "passed"
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    runtime = root / "runtimes" / (snapshot + "-" + stamp)
    shutil.copytree(saved, runtime)
    for path in [runtime, *runtime.rglob("*")]:
        os.chown(path, 1000, 1000)
        os.chmod(path, 0o700 if path.is_dir() else 0o600)
    for path in (runtime / "plugins").rglob("*.so"): os.chmod(path, 0o755)
    # The prepared report reader requires a sealed owner-read-only artifact.
    os.chmod(runtime / "data/reports.sqlite", 0o400)
    tls = root / "tls"
    tls.mkdir(mode=0o755, exist_ok=True)
    if not (tls / "cert.pem").exists():
        run("openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "3650", "-subj", "/CN=Pacer preview only", "-addext", "subjectAltName=DNS:api.anthropic.com,DNS:chatgpt.com,DNS:cloudcode-pa.googleapis.com,DNS:opencode.ai", "-keyout", str(tls / "key.pem"), "-out", str(tls / "cert.pem"), stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        os.chmod(tls / "key.pem", 0o600)
        os.chmod(tls / "cert.pem", 0o644)
    (root / "preview.env").write_text("ROOT=" + str(root) + "\nRUNTIME=" + str(runtime) + "\nCPA_IMAGE=" + manifest["cpa_image"] + "\n")
    compose(root, "up", "-d", "--pull", "never", "--force-recreate")
    current.write_text(json.dumps({"runtime": str(runtime), "snapshot": snapshot}, indent=2))
    print("Preview only: " + str(runtime))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", help="SSH host, e.g. ct104; omitted when running on the preview host")
    parser.add_argument("--root", type=Path, default=Path("/opt/pacer-dev"))
    parser.add_argument("command", choices=["sync", "snapshot", "up", "refresh", "build", "status", "down"])
    parser.add_argument("--snapshot", help="Saved snapshot name for up")
    args = parser.parse_args()
    root = args.root.resolve()
    if root == Path("/") or root == Path("/opt/cliproxyapi") or Path("/opt/cliproxyapi") in root.parents:
        raise ValueError("Preview root must be separate from production")
    if args.host:
        if args.command == "sync": return sync(args.host, root)
        command = ["python3", str(root / "source/dev/manage.py"), "--root", str(root), args.command]
        if args.snapshot: command += ["--snapshot", args.snapshot]
        return run("ssh", args.host, shlex.join(command))
    if args.command in ("snapshot", "refresh"):
        run(sys.executable, str(HERE / "snapshot.py"), "--root", str(root))
        if args.command == "refresh": activate(root, fresh=True)
    elif args.command == "up": activate(root, args.snapshot)
    elif args.command == "build":
        runtime = Path(json.loads((root / "current.json").read_text())["runtime"])
        run("docker", "run", "--rm", "--name", "pacer-preview-builder", "-v", str(root / "source") + ":/src", "-v", "pacer-preview-go:/go", "-v", "pacer-preview-build-cache:/root/.cache/go-build", "-w", "/src", "golang:1.26-bookworm", "sh", "scripts/build.sh")
        version = (root / "source/VERSION").read_text().strip()
        library = "pacer-v" + version + ".so"
        dest = runtime / "plugins/linux/amd64" / library
        # Replace the inode, never truncate a library mapped by the running CPA.
        temporary = dest.with_suffix(".candidate")
        shutil.copy2(root / "source/dist/linux_amd64" / library, temporary)
        os.chown(temporary, 1000, 1000)
        os.chmod(temporary, 0o755)
        os.replace(temporary, dest)
        compose(root, "restart", "cpa")
    elif args.command == "status":
        print((root / "current.json").read_text())
        compose(root, "ps")
    elif args.command == "down": compose(root, "stop")
    else: raise ValueError("sync requires --host")


if __name__ == "__main__":
    main()

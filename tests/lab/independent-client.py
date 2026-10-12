#!/usr/bin/env python3
"""Offline native CPA + Pacer + packed SDK smoke; no inference or provider keys.

Usage: independent-client.py <CPA binary> <Pacer .so> <SDK dist/index.js>
All server state lives in a temporary directory, with three dummy caller keys.
"""

import hashlib
import json
import pathlib
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request


def main():
    binary, plugin, sdk = (str(pathlib.Path(p).resolve()) for p in sys.argv[1:])
    with tempfile.TemporaryDirectory(prefix="pacer-native-client-") as directory:
        root = pathlib.Path(directory)
        (root / "plugins").mkdir()
        (root / "auths").mkdir()
        shutil.copyfile(plugin, root / "plugins" / pathlib.Path(plugin).name)
        with socket.socket() as reservation:
            reservation.bind(("127.0.0.1", 0))
            port = reservation.getsockname()[1]
        base = f"http://127.0.0.1:{port}"
        keys = ["local-caller-one", "local-caller-two", "local-caller-three"]
        grants = [
            {"caller_scope": hashlib.sha256(("cli-proxy-api:caller-scope:v1\0" + key).encode()).hexdigest(),
             "auth_ids": [f"dummy-auth-{i}"]}
            for i, key in enumerate(keys[:2])
        ]
        config = {
            "config-version": 8,
            "server": {"host": "127.0.0.1", "port": port},
            "management": {"disable-control-panel": True},
            "oauth": {"auth-dir": str(root / "auths")},
            "access": {"api-keys": keys},
            "plugins": {
                "enabled": True, "dir": str(root / "plugins"),
                "required-interceptors": ["pacer"],
                "configs": {"pacer": {
                    "enabled": True, "state_path": str(root / "pacer.sqlite"),
                    "usage_auth_url": base + "/v1/models",
                    "client_access": {"grants": grants},
                }},
            },
        }
        (root / "config.yaml").write_text(json.dumps(config))
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        def get(path, key=None):
            headers = {"Authorization": "Bearer " + key} if key else {}
            request = urllib.request.Request(base + path, headers=headers)
            try:
                with opener.open(request, timeout=2) as response:
                    return response.status, json.load(response)
            except urllib.error.HTTPError as error:
                with error:
                    return error.code, json.load(error)

        with (root / "server.log").open("w+") as log:
            process = subprocess.Popen([binary, "--config", str(root / "config.yaml"), "--local-model"],
                                       cwd=root, stdout=log, stderr=subprocess.STDOUT)
            try:
                deadline = time.monotonic() + 10
                while True:
                    try:
                        status, discovery = get("/v0/resource/plugins/pacer/events")
                        if status == 200 and discovery.get("api"):
                            break
                    except (OSError, ValueError):
                        pass
                    if process.poll() is not None or time.monotonic() >= deadline:
                        log.seek(0)
                        raise RuntimeError("Isolated CPA failed to start:\n" + log.read()[-8000:])
                    time.sleep(0.05)
                api = discovery["api"]
                assert api["access"] == {"mode": "granted", "enforcement": "required-interceptors"}, api
                assert api["features"]["account_access"]["enabled"] is True
                assert api["features"]["keepalive"]["enabled"] is False
                endpoint = "/v0/resource/plugins/pacer/events?auth=api-key&view=capabilities&v=1"
                for key in keys[:2]:
                    status, capabilities = get(endpoint, key)
                    assert status == 200 and capabilities == api, (status, capabilities)
                status, refused = get(endpoint, keys[2])
                assert status == 403 and refused["error"]["code"] == "account_access_denied", refused
                status, refused = get(endpoint, "invalid-local-key")
                assert status == 401, refused
                usage = "/v0/resource/plugins/pacer/events?auth=api-key&view=usage&all=true&v=1"
                status, view = get(usage, keys[0])
                assert status == 200 and view["credentials"] == [] and view["all"] is True, view
                script = """
import assert from 'node:assert/strict';
import {pathToFileURL} from 'node:url';
const [base, module] = process.argv.slice(1);
const {PacerClient, HttpError} = await import(pathToFileURL(module).href);
const client = new PacerClient({baseURL:base,key:'local-caller-one'});
const api = await client.capabilities();
assert.equal(api.features.account_access.enabled,true);
assert.equal(api.access.enforcement,'required-interceptors');
assert.deepEqual(api.requirements.managed_cache_models,[]);
assert.equal((await client.usage()).credentials.length,0);
const denied = new PacerClient({baseURL:base,key:'local-caller-three'});
await assert.rejects(denied.capabilities(), error => error instanceof HttpError
  && error.status === 403 && error.code === 'account_access_denied'
  && error.action === 'contact_operator' && error.retryable === false);
"""
                subprocess.run(["node", "--input-type=module", "-e", script, base, sdk],
                               check=True, timeout=10, cwd=root)
                print("Native registration, host attestation, three callers, scoped usage, invalid keys and packed SDK: passed (zero inference).")
            finally:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()


if __name__ == "__main__":
    if len(sys.argv) != 4:
        raise SystemExit(__doc__)
    main()

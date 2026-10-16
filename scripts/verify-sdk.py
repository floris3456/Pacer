#!/usr/bin/env python3
"""Verify the complete vendored SDK; historical provenance stays unchanged."""

import hashlib
import json
from pathlib import Path

root = Path(__file__).resolve().parents[1] / "third_party" / "cpa-sdk"
manifest = root / "SNAPSHOT.json"
expected = json.loads(manifest.read_text())["files"]
actual = {
    str(path.relative_to(root))
    for path in root.rglob("*")
    if path.is_file() and path != manifest
}
if actual != expected.keys():
    raise SystemExit(
        f"SDK inventory mismatch: missing={sorted(expected.keys() - actual)}, "
        f"unrecorded={sorted(actual - expected.keys())}"
    )
for name, digest in expected.items():
    if hashlib.sha256((root / name).read_bytes()).hexdigest() != digest:
        raise SystemExit(f"SDK checksum mismatch: {name}")
print(f"Verified {len(expected)} SDK files")

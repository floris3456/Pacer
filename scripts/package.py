#!/usr/bin/env python3
"""Package existing native builds with CPA's release asset naming convention."""
import hashlib
from pathlib import Path
import zipfile

root = Path(__file__).resolve().parent.parent
version = (root / "VERSION").read_text().strip()
dist = root / "dist"
archives = []
for arch in ("amd64", "arm64"):
    binary = dist / f"linux_{arch}" / f"pacer-v{version}.so"
    if not binary.exists():
        continue
    archive = dist / f"pacer_{version}_linux_{arch}.zip"
    with zipfile.ZipFile(archive, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as output:
        for path, name in (
            (binary, binary.name),
            (binary.parent / "pacer-state", "tools/pacer-state"),
            (binary.parent / "history-import", "tools/history-import"),
            (root / "LICENSE", "LICENSE"),
            (root / "third_party/cpa-sdk/LICENSE", "CPA-SDK-LICENSE"),
            (root / "internal/history/CPAMP-LICENSE", "CPAMP-LICENSE"),
            (root / "docs/pacer-installation.md", "INSTALLATION.md"),
        ):
            item = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
            item.create_system = 3
            item.external_attr = (0o100755 if path == binary or name.startswith("tools/") else 0o100644) << 16
            item.compress_type = zipfile.ZIP_DEFLATED
            output.writestr(item, path.read_bytes())
    archives.append(archive)
if not archives:
    raise SystemExit("No native builds found; run scripts/build.sh first.")
(dist / "checksums.txt").write_text("".join(
    f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.name}\n"
    for path in archives
))
print("Packaged:", ", ".join(path.name for path in archives))

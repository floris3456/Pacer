#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
version=$(cat VERSION)
revision=unknown
dirty=unknown
if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  revision=$(git rev-parse HEAD)
  dirty=false
  test -z "$(git status --porcelain --untracked-files=normal)" || dirty=true
fi
info=github.com/floris3456/cliproxyapi-quota-handoff/internal/buildinfo
linkflags="-s -w -buildid= -X $info.Version=$version -X $info.Revision=$revision -X $info.Dirty=$dirty"
platform=$(go env GOOS)
arch=$(go env GOARCH)
test "$platform" = linux || { echo 'Release builds require Linux.' >&2; exit 1; }
case "$arch" in amd64|arm64) ;; *) echo 'Unsupported release architecture.' >&2; exit 1 ;; esac
out="dist/linux_${arch}"
mkdir -p "$out"
CGO_ENABLED=1 go build -mod=readonly -trimpath -buildvcs=false \
  -ldflags="$linkflags" -buildmode=c-shared \
  -o "$out/pacer-v${version}.so" ./cmd/plugin
CGO_ENABLED=1 go build -mod=readonly -trimpath -buildvcs=false \
  -ldflags='-s -w -buildid=' -o "$out/pacer-state" ./cmd/pacer-state
CGO_ENABLED=1 go build -mod=readonly -tags sqlite_fts5 -trimpath -buildvcs=false \
  -ldflags='-s -w -buildid=' -o "$out/history-import" ./cmd/history-import
cc -Wall -Wextra -Werror -o "$out/native-probe" tests/native-probe.c -ldl
scratch=$(mktemp -d "${TMPDIR:-/tmp}/pacer-probe.XXXXXX")
trap 'rm -rf "$scratch"' EXIT HUP INT TERM
config=$(printf 'state_path: %s/state.sqlite\ncompact_on_switch: true\nrouting:\n  mode: off\n' "$scratch" | base64 | tr -d '\n')
printf '{"config_yaml":"%s"}' "$config" > "$scratch/register.json"
"$out/native-probe" "$PWD/$out/pacer-v${version}.so" "$scratch/register.json" "$version"
go version > "$out/toolchain.txt"
python3 - "$out" "$version" "$revision" "$dirty" <<'PY'
import hashlib, json, pathlib, sys
out, version, revision, dirty = sys.argv[1:]
root = pathlib.Path(out)
artifacts = {p.name: hashlib.sha256(p.read_bytes()).hexdigest()
             for p in sorted(root.iterdir()) if p.is_file() and p.name != "build.json"}
(root / "build.json").write_text(json.dumps({"version": version, "revision": revision,
    "dirty": {"true": True, "false": False}.get(dirty), "artifacts": artifacts}, indent=2) + "\n")
PY

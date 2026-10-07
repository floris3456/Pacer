#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
version=$(cat VERSION)
platform=$(go env GOOS)
arch=$(go env GOARCH)
test "$platform" = linux || { echo 'Release builds require Linux.' >&2; exit 1; }
case "$arch" in amd64|arm64) ;; *) echo 'Unsupported release architecture.' >&2; exit 1 ;; esac
out="dist/linux_${arch}"
mkdir -p "$out"
CGO_ENABLED=1 go build -mod=readonly -trimpath -buildvcs=false \
  -ldflags='-s -w -buildid=' -buildmode=c-shared \
  -o "$out/pacer-v${version}.so" ./cmd/plugin
cc -Wall -Wextra -Werror -o "$out/native-probe" tests/native-probe.c -ldl
scratch=$(mktemp -d "${TMPDIR:-/tmp}/pacer-probe.XXXXXX")
trap 'rm -rf "$scratch"' EXIT HUP INT TERM
config=$(printf 'state_path: %s/state.sqlite\ncompact_on_switch: true\nrouting:\n  mode: off\n' "$scratch" | base64 | tr -d '\n')
printf '{"config_yaml":"%s"}' "$config" > "$scratch/register.json"
"$out/native-probe" "$PWD/$out/pacer-v${version}.so" "$scratch/register.json" "$version"
go version > "$out/toolchain.txt"

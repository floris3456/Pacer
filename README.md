# CPA quota handoff

A native [CLIProxyAPI](https://github.com/floris3456/CLIProxyAPI) plugin that
tracks Claude/Codex credential quotas, warns active chats and coordinates quota
compaction and credential handoffs with
[OpenCodePlus](https://github.com/floris3456/OpenCodePlus).

**Start with shadow mode.** It shows reminders and collects forecasts while CPA
keeps its normal routing. Quota-triggered compaction and switching are disabled.
An optional compatible CPAMP backend supplies calibration for adaptive estimates.

Local maintenance build `0.1.3-local.1` adds `notifications_enabled: false` in
the plugin configuration. It mutes both inline stream warnings and polled chat
notices. Quota state, usage, calibration, thresholds and routing keep operating;
notices are written through CPA's host logger and coordinated events remain in
SQLite. Re-enable with `true` or omit the setting. This setting supports live
reconfiguration without a plugin reload. It does not disable enforcement actions
if enforce mode is selected. Existing client history is not removed.

## What it provides

- Five-hour and seven-day quota windows, shared-account accounting and reset times.
- `/usage` and `/usage --all` in OpenCodePlus 1.1.2 or newer, including exact active
  credential identity. Before the first request, `/usage` shows all credentials.
- Per-chat coordination: each chat completes its own compaction before switching;
  other chats may compact concurrently on the same credential.
- Return to a smaller account only after the relevant quota reset and a fresh or
  compacted context. Account rank describes capacity, not remaining percentage.
- Manual reminders for ordinary clients on supported HTTP text streams. Tool-only,
  reasoning-only, JSON, non-streaming and WebSocket responses have no inline fallback.
- Optional CPAMP forecasts account for model prices, input, cached input, cache
  writes, output, concurrent reservations and compaction headroom. Estimates count
  model requests, which can be several per user turn. Missing evidence is not zero usage.

## Downloads and compatibility

[Download v0.1.2](https://github.com/floris3456/cliproxyapi-quota-handoff/releases/tag/v0.1.2).
Linux **amd64** and **arm64** ZIPs include the versioned `.so` and licenses.
Verify them with the release's `checksums.txt`.

The host needs the native C ABI **1**, schema **6**, and the request lifecycle,
scheduler, usage, management-resource and stream-interceptor hooks. The tested
host is CPA `7.3.20-opus55.1`, source
[`ef9e712`](https://github.com/floris3456/CLIProxyAPI/commit/ef9e71201e0ca72b03a540d9b6e0862f8c053347).
An arbitrary older CPA build is not supported. Prebuilt libraries use Debian
Bookworm/glibc; Alpine/musl hosts need a matching source build. This plugin runs
on the CPA server; macOS clients only need OpenCodePlus.

## Setup

1. [Install and configure the CPA plugin](docs/installation.md).
2. [Configure Caddy and OpenCodePlus](docs/caddy.md).
3. Leave `adaptive.mode: shadow` while reviewing reminders and calibration.
   [Modes and CPAMP integration](docs/calibration.md) explains the optional backend
   and what changes when enforcement is enabled.

One public HTTPS origin serves every client. OpenCodePlus initiates requests to
CPA; clients need no inbound listener. Inference uses each user's normal CPA API
key. Event polling uses a private chat capability. Fresh-chat usage reads use the
CPA API key and expose the **whole configured quota pool** to any valid frontend
key. See the [usage resource contract](USAGE.md) before enabling it for tenants
that must not see each other's credential IDs, aliases or quota bars.

## Build and test

With Go 1.26, a C compiler and libc development headers:

```sh
go test -race ./...
sh scripts/build.sh
python3 scripts/package.py
```

Or use the pinned native builder (run on the desired architecture):

```sh
mkdir -p .cache/go
docker run --rm -v "$PWD:/src" -v "$PWD/.cache/go:/go" -w /src \
  -e GOCACHE=/go/build \
  golang:1.26-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d \
  sh scripts/build.sh
python3 scripts/package.py
```

The two CPA SDK packages are an unchanged, licensed
[source snapshot](third_party/cpa-sdk/README.md), with file hashes and commit
provenance. No private CPA checkout or server configuration is needed. Tests use
dummy credentials. The build runs a native ABI load/register/shutdown probe.
The release workflow builds both architectures and publishes store-compatible
ZIP names plus checksums. It never deploys to a server.

## License

MIT. The CPA SDK retains its original MIT license in `third_party/cpa-sdk/`.

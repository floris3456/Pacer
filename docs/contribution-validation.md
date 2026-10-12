# Quota consistency validation

These changes build on upstream `d727bc84962c3b86f12bc39a885c0ee959282088`.
They retain protocol 1 and the default shared credential pool and routing policy.

## Independent client contract (2026-10-12)

The additive API and account grants are documented in
[independent-clients.md](independent-clients.md). Explicit grants and mandatory
cache admission require the host's required-interceptor guarantee; the default
remains unchanged. Required-model tests include aliases, thinking suffixes,
forged headers, foreign callers, retries and real durable policy resolution.
Three-user regressions cover redaction, persisted ownership and key revocation
while a quota watch is held.

The full Go race suite, vet, vendored SDK tests, native build and C ABI probe pass
after these changes. Build receipts record version/revision/dirty status and
artifact SHA-256 hashes. The accompanying local CPA patch also passes its full
affected packages, targeted race tests and executable build; its explicit-cache
backport passes final-wire execute/stream/count and executor regression tests.

The offline integration smoke loads the **actual native plugin into the built
CPA executable**, then uses the packed generic SDK over localhost. It verifies
host capability delivery, two granted callers, a denied third caller, an invalid
key, public/authenticated discovery, structured errors and scoped usage:

```sh
python3 tests/lab/independent-client.py \
  /path/to/patched-CPA /path/to/pacer.so /path/to/sdk/dist/index.js
```

That smoke passes with zero inference calls. All keys, directories and state are
synthetic and temporary. Stock OpenCode/Pi checks and SDK package isolation are
recorded in the client repository. These checks do not establish live provider
billing or complete managed auxiliary support; neither production service was
changed. The recovered host's original build provenance remains bounded by the
[inspection receipt](live-build-2026-10-12.md).

## Earlier consistency changes

- Held usage reads reload their binding after waking. A revoked capability or
  changed binding key is rejected; the current account is read from the refreshed
  binding. Three simultaneous callers retain their own identity/model scope while
  receiving updates to shared quotas.
- Quota notifications follow changes to canonical observations, including meaningful
  freshness/status changes. Duplicate and rejected stale evidence does not repeatedly
  wake every viewer. Usage reads do not execute provider requests.
- Codex premium-to-main migration retires superseded cycles in either timestamp
  direction. Both reset anchors, rollback, reopen and delayed replay are covered;
  ambiguous evidence is not promoted into a proven retirement.
- Capability reads require a unique chat/route match. Legacy clients that reused
  one capability across callers or sessions receive the existing authorization
  error instead of another caller's current account or routing preview.

Validated on Linux amd64 with Go 1.26.9 and a C compiler:

```sh
CGO_ENABLED=1 go test -race -tags sqlite_fts5 ./...
go vet -tags sqlite_fts5 ./...
(cd third_party/cpa-sdk && go test ./...)
sh scripts/build.sh
```

The build includes the native C ABI load/register/buffer-ownership/shutdown probe.
Focused regressions live in `internal/pacer/usage_watch_test.go` and
`internal/observation/premium_test.go`; they include cancellation, 256-waiter
saturation, transactional failures and three overlapping caller views.
`internal/tracker/tracker_test.go` also covers unique, ambiguous, route-separated
and reopened capability lookups.

This is local automated validation with synthetic credentials. It does not assert
live provider behavior, identify a deployed CPA binary, or establish the schema-7
host capability required by the separate managed cache policy. No deployment or
routing-mode change is included.

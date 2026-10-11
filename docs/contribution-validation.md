# Quota consistency validation

These changes build on upstream `d727bc84962c3b86f12bc39a885c0ee959282088`.
They retain protocol 1, the shared credential pool and existing routing policy.

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

This is local automated validation with synthetic credentials. It does not assert
live provider behavior, identify a deployed CPA binary, or establish the schema-7
host capability required by the separate managed cache policy. No deployment or
routing-mode change is included.

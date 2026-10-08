# Native-only forward reporting publisher

This source increment connects accepted native usage to the forward ledger without adding an HTTP route or UI. It reports recorded attempts and original CPAMP API-equivalent value. Native quota normalization, physical coverage, billing-pool completeness, routing activation and historical/native migration acceptance remain unavailable.

## Explicit offline preparation

The existing `pacer-state` executable adds one reusable command:

```sh
pacer-state forward-init \
  --output /var/lib/pacer/forward.sqlite \
  --book-json /var/lib/pacer/approved-forward-book.json
```

Both paths must be absolute/canonical, distinct, regular files without symlink components; the output must **not exist** and its directory must exist. Creation is exclusive with mode `0600`. Existing files/hard links are never overwritten. Input is at most 2 MiB, exact-field typed JSON; duplicate/case-alias/unknown fields, trailing input and invalid prices are rejected. Errors do not echo paths, decoder context, raw input or SQL. If initialization fails after exclusive reservation, that newly reserved output is retained for inspection rather than reused/reset on a later invocation.

Example book shape, with deliberately nonfunctional identity placeholders:

```json
{
  "id": "approved-opus-reference-v1",
  "book": {
    "id": "approved-opus-reference-v1",
    "policy": "profile-api-value-v3",
    "prices": {
      "claude-opus-5-5": {"prompt": 4, "completion": 20, "cacheRead": 0.2}
    }
  },
  "qualifications": [{
    "SourceID": "EXISTING-APPROVED-SOURCE",
    "Provider": "claude",
    "Model": "claude-opus-5-5",
    "Route": "",
    "AccountID": "EXISTING-CAPTURED-ACCOUNT",
    "CredentialRef": "EXISTING-CAPTURED-CREDENTIAL-REFERENCE"
  }]
}
```

Use the exact approved source/immutable captured account/credential references and approved book bytes; these labels are not real identities or authority. Qualification field capitalization shown above is exact. Rates are USD per million tokens under the original v3 comparison-book semantics. Context/service-tier fields remain the original typed book fields. The command fetches nothing and accepts no provider-key/configuration inputs. Missing qualifications retain records as unvalued; absent books do not select a default or free pricing. An explicitly empty qualification list grants no valuation authority.

Successful output supplies the actual content digest:

```json
{
  "forward_report_path": "/var/lib/pacer/forward.sqlite",
  "forward_report_book": "<actual 64-character digest returned by the command>",
  "mode": "native-only",
  "profile_revision": "0",
  "capacity": "unavailable"
}
```

This command imports **no historical baseline**, opens no source database and invents no installation association. Historical books with a historical valuation cohort are refused by this minimal CLI. Existing F1 baseline/origin/profile preparation APIs remain separate offline migration work. The runtime refuses a store containing a frozen baseline unless that store passes the accepted-baseline gate below; it never silently omits a baseline. All recorded native revisions, including old rows, can be adopted asynchronously; qualification determines valuation, not admission.

## Accepted frozen baseline (frozen→live cutover)

Normative contract: `evidence/implementation/forward-continuity-contract.md` §1–§3. Offline library preparation, in order, once per store (a new cutover is a new store): `PrepareForwardBase`, `SeedFrozenForwardProfiles` (revision 1 = the frozen profile configuration at H, with a `profile_seed` provenance record: base digest, artifact digest, frozen revision, config digest), `RegisterForwardOrigins`, `PrepareForwardCutover(origin, declareCoverageGap)`. The last step records **H** (base `h_ms`, `events_through`, `observations_through`) and **N0** (the largest certificate `StartMS`). `N0 > H` is refused (`ErrForwardCoverageGap`) unless the gap is declared; a declared gap is recorded as `coverage_gap {from_ms: H, to_ms: N0, accounts}` and never filled. There is no `pacer-state` command for these steps yet.

The runtime (`ForwardSelection`, at startup and every cycle) accepts a store with a baseline only when: the base is complete; the cutover's origins are registered, ready and have `missing_count` 0; revision 1 was seeded from the same frozen artifact (`profile_seed` matches base and revision-1 digest); the recorded H/N0 matches the base and certificates with `N0 ≤ H` or a declared `coverage_gap`; and a historical book, if configured, matches H. Otherwise startup keeps the original refusal. No runtime path imports a baseline, seeds profiles or edits origins.

Generations sealed on such a store carry `manifest.cutover`. Combined domain: baseline `started < H`, native `started ≥ N0`; inside `[N0,H)` a qualifying linked native replaces its baseline event; any other event there is counted once on its own side, its evidence row has `dedup_unverified: unlinked_baseline|unlinked_native`, and every touched total/daily/model/account/profile bucket carries `dedup_unverified {unlinked_baseline, unlinked_native}`. The report's `cutover` block adds generation-level `pre_cutover_native` (native `started < N0`, still visible in the `native` domain) and `post_h_baseline` (excluded baseline rows). The `baseline` domain is also capped at H. Profile edits after cutover are forward revisions; profiles copied from the frozen configuration refuse edits/archive (`ErrForwardFrozenProfile`, HTTP 409). Stores and manifests without a recorded cutover keep the original behaviour and query.

## Runtime configuration

Example YAML (dummy paths; replace the digest with initializer output):

```yaml
enabled: true
state_path: /var/lib/pacer/state.sqlite
forward_report_path: /var/lib/pacer/forward.sqlite
forward_report_book: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
routing:
  mode: off
```

Omit both forward fields to disable this publisher. Neither this command nor the agent writes user configuration. A configured absent/empty/incompatible/unprepared file, unknown book, symlink or alias of the state/history/frozen-report file refuses plugin startup. Existing-file hard-link aliases are checked by file identity. Changing either forward field on a running instance is rejected; quiesce/close and instantiate a new plugin for idle reload. A closed/quiesced instance cannot be reconfigured back into service.

`OpenPreparedForward` opens with SQLite `mode=rw`: it has no create fallback and never calls the initializer. Startup validates schema/index metadata and referenced book/profile/head bytes, with zero-row column checks. It does not scan events or build indexes on retained tables. Reader connections use ordinary `mode=ro` WAL access, never `immutable=1` on the forward database. The original `OpenForward` offline initializer is unchanged.

## Worker and failure boundaries

- One owned goroutine; wake channel capacity one. `usage.handle` sends a nonblocking hint on return, including errors after primary persistence. Durable feed state remains authority. Requests and scheduler hooks do no new scans, provider waits or source RPCs.
- Each bootstrap/projection/feed-provenance turn handles at most 250 rows and 2 MiB with a 2-second context, then yields for 2 ms. Existing SQLite owner busy-timeout semantics still apply. Import cursors persist transactionally; after restart, provenance tuple replay resumes from the beginning in bounded background batches while projected records resume at their durable cursor.
- Each cycle freezes a feed head and explicit request-start horizon **T** before projection. A later callback with a start beyond T remains retained and can enter a later generation even if no new feed row arrives. The 30-second periodic refresh also handles missed notifications, source faults and append-only profile changes.
- The profile branch's current revision is captured per cycle. The book digest is configuration-authorized and immutable until idle reload. Adding a different book object alone does not authorize its adoption. Old pins retain their original profile/book/provenance.
- Exact captured membership/provider/account tuples are collected from the local immutable feed, capped at 128. `measurement.SourceSnapshot` is the only coverage read; no discovery mapping or `host.usage.checkpoint` call is made. One snapshot per turn, 512 KiB total snapshot budget. Missing, failed, omitted or conflicting proof remains explicit. Source faults supersede later generations' provenance without rewriting earlier pins.
- Seal/compile uses a separate **2-minute** cancellable deadline, not the short batch deadline. This accommodates the retained 100,001-attempt synthetic race fixture (~31 seconds for compilation). One publisher competes for the existing shared **two-slot** report admission. Native ingestion does not wait for compilation.
- Errors leave the previous sealed report available. The worker retains its fixed cycle/cursors, reports a fixed error code, waits two seconds and retries. Wake floods cannot bypass backoff. A source-head recheck follows successful publication; unchanged head/profile/proof normally avoids another generation.
- The authoritative atomic feed trigger remains the only additional synchronous ingestion work from F1. Its failure rolls back `PutRecord` and the callback returns an error. An asynchronous projection/seal failure affects lag only. A physical-consumer failure after the primary commit still returns a callback error while the recorded accounting remains publishable with unknown coverage.
- Quiesce/shutdown cancels and joins the publisher and active admitted report leases. `Close` then closes the forward handles before closing source state. The last sealed generation is exposed by metadata immediately on reopen while adoption resumes.

## Go API for the parent’s future v8 adapter

No new `/v0/management` route, field or registration is present.

```go
status := plugin.ForwardStatus() // detached metadata, no database read
plugin.WakeForward()            // coalesced hint after authorized metadata edits

err := plugin.WithForwardReports(ctx, func(ctx context.Context, s *historyreports.ForwardStore) error {
    pin, err := s.PinForward(ctx, generation, filter)
    if err != nil { return err }
    report, err := s.ForwardReport(ctx, pin)
    // Serialize/use report within the authorized adapter; honor ctx.
    _ = report
    return err
})
```

`WithForwardReports` acquires shared report admission and leases the store until the callback returns. The callback must honor cancellation, must not retain the store/database reference and must not close the owner. It can use existing pinned request/evidence/JSON/CSV methods and authorized profile CAS operations. Parent owns v8 authorization, exact input validation, safe error mapping, bounded response envelopes and output writer handling. Library errors are not an HTTP response schema. Saved pin plus page limit/cursor must remain fixed across pagination/exports.

`ForwardRuntimeStatus` JSON shape:

```json
{
  "state": "ready",
  "active": false,
  "phase": "",
  "source_head": "42",
  "projected": "42",
  "bootstrap_complete": true,
  "latest_generation": "<generation digest>",
  "report_at": "2026-10-08T01:00:03Z",
  "failures": "0",
  "capacity": "unavailable"
}
```

For an accepted baseline store, the status adds `"cutover": {"h_ms", "events_through", "observations_through", "n0_ms", "coverage_gap"?, "pre_cutover_native", "post_h_baseline"}`: the recorded H/N0 and the counts of the latest sealed generation (zero counts before the first seal). Native-only stores omit it.

States: `disabled`, `initializing`, `lag`, `error`, `ready`, `stopped`. Active phases: `bootstrap`, `projection`, `provenance_feed`, `provenance`, `seal`. Optional `target_at` is the frozen cycle horizon; optional `error_code` is one of `forward_bootstrap_failed`, `forward_source_unavailable`, `forward_selection_unavailable`, `forward_store_unavailable`, `forward_projection_failed`, `forward_provenance_feed_failed`, `forward_compute_unavailable`, `forward_seal_failed`, `forward_phase_invalid`. Error text/SQL/paths are never stored in status. Failure counters saturate. Heads/cursors are losslessly encoded sequence strings, **not attempt counts**. `source_head` is the last background-observed source head, not a synchronous current-database assertion.

Additive library helpers in `forward_store.go`: `CheckForwardPath`, `OpenPreparedForward`, `NativeForwardSelection`, `PreparedForwardHead`; in `forward_cutover.go`: `ForwardSelection`, `PreparedCutover`, `GenerationCutover` (runtime reads) and the offline-only `SeedFrozenForwardProfiles`, `PrepareForwardCutover`. They provide prepared-only opening/reference checks; no valuation, converter or identity algorithm changes are included.

## Acceptance boundary

Source/fake-host tests use actual `Plugin.Handle` registration, usage and shutdown/quiesce. They cover $7.25 mixed writes, primary rollback vs post-primary physical refusal, unknown aliases, profile/proof revisions, old exports after reopen, source decode faults, startup path refusal, one-batch bootstrap/resume, frozen T, failed/held publication, shared admissions and joined cancellation. Tests do not promote billing completeness, physical coverage, warmth or routing.

The library's 100,001-attempt test and retained historical 7/81 plus 53/104 oracle tests remain required regression checks. Runtime deployment, real producer/price authority, baseline migration, physical startup-inventory repair, v8 routes, native-host/browser acceptance and retirement are separate parent-owned gates.

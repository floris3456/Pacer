# Pacer offline integration lab

Use the existing CPA image or the task's candidate image with `--local-model`,
this config, and Pacer's native build. Mount **only** a subject-local lab directory
at `/lab`; put dummy OAuth credential JSON in `/lab/auths` pointing `base_url` to
`http://pacer-upstream:8080`. Never mount the production auth/config/state roots.

`upstream.py` runs on an internal-only Docker network, makes no outbound requests,
and accepts only the two literal fixture tokens in its source. Publish CPA only
on a loopback host port. This tests actual CPA/native-plugin dispatch and browser
authentication separately from source package tests. A fixture summary does not
establish OpenCode's committed-compaction behavior; that needs the client lab.

Keep all live services and the CPAMP database untouched.

The CPA host must be built with **`CGO_ENABLED=1`** on Linux for native dynamic
plugins. A `CGO_ENABLED=0` server can start and serve management endpoints while
rejecting every `.so`; its successful Go build is not native-plugin acceptance.
Verify the host build metadata and run `check-host.py` (including actual Pacer
registration/state) before inference tests or delegating an exclusive client window.
The isolated CPAMP server uses a separate pure-Go SQLite implementation and may
still be built with CGO disabled; these are different artifact requirements.

The Usage Quota page (`/usage/profiles|windows|inspect`, `PATCH /usage/exclusions`)
is checked by the manager repository's browser runner with `PACER_FOCUS=usage`
and `PACER_USAGE_READY=1`. It needs the prepared usage history (quota facts v2)
and the forward store; its exclusion step restores the window it excludes. The
former frozen-history and legacy CPAMP routes are removed and must answer 404.

`check-coverage.py` requires `PACER_COVERAGE_READY=1`, `PACER_EVIDENCE`, and a parent
window on the corrected scoped-hook candidate. It waits at most 45 seconds for the
30-second worker, requires two explicit **unknown** native reports, then verifies
they survive a real unchanged inventory refresh. Missing reports are failures, not
proof of unknown coverage. It neither infers nor changes provider/fixture state.
Native acceptance runners preserve accumulated fixture attempts; any reset must
be a separately authorized operation with prior evidence retention, not a preamble.

## Exact physical-cut acceptance

`internal/measurement/physical_lab_test.go` is an opt-in read-only check against a
private SQLite **backup snapshot**, not the live database. It is skipped in normal
source tests. Place copies in a task-local `physical-source-snapshots/` directory
and set `PACER_PHYSICAL_SNAPSHOT` to the positive snapshot before running:

```sh
go test -count=1 -timeout=45s -v ./internal/measurement -run '^TestNativePhysicalStoredSelections$'
```

The lab must have the matching physical-proof host/plugin. Observe a genuine first
quiescent lower cut before the managed 1h/5m stream/nonstream attempts being selected;
do not infer it from idle time or a snapshot row count. The check reopens current
consumer manifests through the real `Store.SelectPhysical` API, checks exact native
accounting joins and 25/75 split preservation, and refuses an unissued upper cut.
Billing membership and primary warmth must still be unknown.

Optionally set `PACER_PHYSICAL_REVOKED_SNAPSHOT` to a later backup after an actual
failed native dispatch; it must revoke a previously known nonempty exact selection
with an origin gap. This is not equivalent to manually inserting a gap. Preserve
both copies and failures. Never clear faults, reset fixture history, or restore an
older database to produce a positive result. Bind receipts to loaded artifact hashes;
source fixtures or a skipped test do not establish native acceptance.

The network-none, task-owned snapshot inspector can execute
`physical_snapshot.py anchor|positive|revoked <unique-name.sqlite>` with the live
state mounted **read-only** at `/state` and a private destination at `/snapshots`.
It waits at most 70 seconds, saves through SQLite's backup API using exclusive
0600 output, and reports facts from that saved copy. Its row/count readiness is
only a bounded wait condition; the Go selector still must validate actual cuts.
Failed copies are retained. `test_physical_snapshot.py` checks these SQL/readiness
conditions without a runtime or filesystem state.

Set `PACER_CHECK_LEGACY_MARKERS=1` for a lab retaining pre-fix native Activity:
the opt-in selector additionally checks public `Measure` readback before and
after the alleged expiry, requiring unknown warmth and no protection or prefix
authority, while retaining old canonical body digests byte-for-byte.

`check-forward.py` requires explicit `PACER_FORWARD_READY=1` and `PACER_EVIDENCE`.
It tests actual native ingestion, pinned reports/evidence and exact old export
replay, with one dummy request. Default `PACER_FORWARD_EXPECT_TIER=unknown` requires
an unvalued response; `standard` additionally requires observed response-tier
provenance and independently checks the qualified $0.000945 fixture value.
`PACER_FORWARD_STREAM=1` requires a complete Messages SSE stream; truncation fails.
Only the original fixture URL or the separately owned `pacer-upstream-tier` URL
is allowed via `PACER_FIXTURE_UPSTREAM`. These are distinct attempt/counter domains.
Never restart/reset the original fixture to load new source. Scope, preparation,
ownership, version checks and temporary routing cleanup stay the caller's duty.

## Capacity lower-bound window

Parent-run only. `capacity-window.sh` refuses without `PACER_CAPACITY_WINDOW=1` and
`check-capacity.py` without `PACER_CAPACITY_READY=1` and `PACER_EVIDENCE`. The fixture
`capacity_upstream.py` (`pacer-upstream-capacity`, image-free `python:3.12-slim`) is a
separate attempt domain; the original `upstream.py` is never edited or restarted.
`capacity-auths/cap-{a..e}.json` are dummy logins without `account_uuid`, with
`prefix: cap-x`, so a request for `cap-x/claude-opus-5-5` reaches exactly that login.

1. `PACER_CAPACITY_WINDOW=1 PACER_CAPACITY_PARK_ORIGINALS=1 capacity-window.sh up`
   (prints `STAGED`), `docker restart pacer-lab-cpa`, then `up` again within ~20 s.
   Any non-cap Claude login stays unverified and makes every interval
   `unknown_account_ref_tuple`, so the originals must be parked.
2. `check-capacity.py phase1` (~35 min): cases a–e in parallel, one account each.
3. On pass: `docker restart pacer-lab-cpa`, `capacity-window.sh up` within ~20 s,
   then `check-capacity.py phase2` within ~1.5 h (~35 min run; readback keeps 8 intervals).
   Phase 2's clean interval also expects `carry_in_unproven` + `source_not_enrolled`
   when no request reached the account between the restart and s1. CPA enrols a
   Claude source only on its first dispatch per process. A rerun after a failed
   phase 2 takes the other branch, because the first run's requests enrolled them.
4. `check-capacity.py phase3` (~35 min), then `capacity-window.sh down` and restore
   any Pacer state snapshot (the run adds `capacity-evidence.sqlite` rows).

Each interval spans two of Pacer's own scheduled usage reads, at least 15 min apart,
so nothing can be sped up. Receipts are `capacity-phase{1,2,3}.json` and are partial
on failure. Known residual risks, which surface as an explicit extra reason: a case-b
request or another account's request landing within the ~50 ms of a sample's cut
reads; a scheduler delay of more than 8 min past the 15 min gap, which expires
case d's 600 s hold. Interrupting the checker during case d's hold makes CPA record a
physical gap, which invalidates every proof in that CPA incarnation.
`test_capacity_upstream.py` and `test_capacity_checker.py` run offline.

## Pace routing lab

`pace-lab.sh` runs a real CPA host with the native Pacer library and
`capacity_upstream.py` (as `api.anthropic.com`, three dummy Claude logins) on an
internal Docker network, all in Docker volumes. `check-pace.py` sets the meters of
the docs/pace-routing.md example and checks, through CPA: the former `dry-run`
YAML loading as `off`, pace from live meters including a dormant 5-hour window,
switching routing on, new-chat placement, warm chats staying, reply headers
updating a meter, band and running-window tie-break, forced moves, the
`compact_on_switch` claim flow (409, then the compacted chat on the new account),
routing off and the decision trace. It takes about 8 minutes: Pacer's first meter
reads after startup are 1–5 minutes apart by design.

Build CPA and Pacer for the Docker host's architecture with CGO, e.g. CPA from its
checkout with `git archive HEAD | docker run -i golang:1.26-bookworm sh -c '... CGO_ENABLED=1
go build -o /tmp/CLIProxyAPI ./cmd/server ...'` and Pacer with `scripts/build.sh`:

```sh
tests/lab/pace-lab.sh up <CLIProxyAPI> dist/linux_<arch>/pacer-v$(cat VERSION).so
tests/lab/pace-lab.sh check
tests/lab/pace-lab.sh down
```

# Historical preservation and native keys/groups

`internal/history` and `cmd/history-import` implement a **separate historical
archive plus native display metadata**. Completion is `preservation:
"archival-only"`; it does not establish Usage Quota report parity or authorize
CPAMP retirement. There is no importer HTTP route or startup backfill.

## Ownership and frozen-input contract

Parent owns `internal/pacer`, management authorization, ABI and UI integration.
The measurement implementation owns normalized live usage, ratios and stretches.

`history.Open(path)` opens a dedicated Pacer history SQLite file, separate from
routing/measurement and CPAMP databases. New files are mode `0600`; existing files
must be owner-only and carry history schema version 1. Foreign DBs are rejected
before a writable connection is opened. Parent directories must already exist.

`Store.Import` is a trusted **offline maintenance** capability. Its input must be
a consistent SQLite backup or a cleanly stopped/checkpointed copy. The producer
must freeze the complete SQLite state; copying only an active WAL database's
main file is not a backup. This command does not create the backup, contact
CPAMP/CPA, or consume any event queue.

The source must be a regular non-symlink file, with no write permission bits and
no adjacent `-wal`, `-shm`, or `-journal`. Reads use
`mode=ro&immutable=1&_query_only=1`. File SHA256, SQLite integrity and schema are
checked before staging; file identity/stat is checked between batches and SHA256
again before publication. Continued immutability is the trusted producer's
contract, not protection against a hostile process able to rewrite owner files.

Keep the original CPAMP DB, consistent backup and encryption companion
(`data.key` where applicable) for rollback. Encrypted settings are preserved as
ciphertext; the archive neither transfers nor decrypts credentials.

### Explicit boundary manifest

Supply a JSON file containing these fields. Hashes are lowercase 64-character
SHA256 hex, never raw keys or paths.

| Field | Meaning |
| --- | --- |
| `schema` | `cpamp-usage-v2` or `cpamp-usage-v3` |
| `snapshot_sha256` | Complete frozen SQLite file hash |
| `lineage_sha256` | Hash of the operator's stable source-installation identity record |
| `checkpoint_sha256` | Hash of the explicit cutover/import checkpoint record |
| `h_ms` | Explicit historical upper bound H, Unix milliseconds |
| `events_through` | Cursor bounded by `analytics_state['events']` |
| `observations_through` | Cursor bounded by source maximum observation rowid |
| `report_pins_sha256` | Optional hash of additional frozen report/evidence JSON |

The first lineage/checkpoint identity is trusted operator provenance: CPAMP has
no installation-ID column that proves it. Reopening/retrying cannot change any
manifest field or substitute another snapshot/installation. One store admits
**one frozen snapshot**. Import into a new store before native edits; importing
over existing native metadata is rejected. Multi-snapshot reconciliation is a
future contract.

H and cursors are preserved, not inferred or used to truncate the archive. All
source rows survive, including evidence beyond a particular report pin. A later
report engine must apply saved bounds. This package does not activate
`PinHistoricalValuation`, infer canonical closure or assume cache-write TTLs.

### CLI

Build with SQLite FTS5 for CPAMP search-index schemas:

```sh
go build -tags sqlite_fts5 -o /trusted/task-cache/history-import ./cmd/history-import
/trusted/task-cache/history-import \
  --source /trusted/frozen/usage.sqlite \
  --manifest /trusted/frozen/boundary.json \
  --store /trusted/pacer/history.sqlite \
  --frozen --batch 250 --max-batches 100
```

Replace example paths with approved task-local paths. `--frozen` attests the
producer contract above. Repeat until safe JSON progress says `complete: true`;
omit `--max-batches` or use zero to finish in one invocation. Partial work is
durable but unavailable to keys/profile views.

Optional `--report-pins /trusted/frozen/report-pins.json` must match manifest
`report_pins_sha256`; both must be present on every retry. The artifact must be
read-only, valid JSON, at most 16MiB; bytes remain exact in the private archive.
Use it for issued window/report pagination or evidence pins absent from SQLite.
Without it only persisted pins are preserved: absent transient pins cannot be
reconstructed by this importer.

Each data transaction copies at most 1–1000 rows, with a 4MiB soft encoded-byte
limit and a 16MiB maximum encoded row. Row copies, projections, hash chain and
keyset cursor commit together. Interruption rolls back only that batch.
Concurrent retries serialize without duplicate rows/publication. Incompatible
rows fail without publication. A retry never replaces native alias/group edits.

Initial/final hashing and initial integrity checking scan the frozen file. This
is offline maintenance, not bounded startup work; small `--max-batches` repeat
that validation cost. `Open`, edits and views never trigger an import. Keep large
imports outside CPA startup and routing.

## Source contract and archival format

The SQL contract comes from actual CPAMP source at analytics checkpoint
`e8492c245bf69144afdcf20b8b4ea8935f0476e5`. Files:
`internal/history/cpamp-schema.sql`, `testdata/schema-provenance.json`,
`CPAMP-LICENSE`. Copied SQL and adapted profile DTOs retain CPAMP's MIT notice.
Regenerate with
`python3 internal/history/testdata/generate_schema.py /path/to/cpamp`.

Required tables, column declarations, primary-key positions and nullability are
checked. V3 also requires historical valuations/token-evidence revisions and
cannot be labeled v2. V2 keeps earlier books without inventing v3 provenance.
Missing/incompatible domains fail closed. Extra ordinary tables/columns are
copied, including private settings/raw bodies. Never expose the archive as a
management download or serialize source rows as public DTOs.

Preserved domains:

- exact aliases/current and historical key hashes/rotation lineage;
- events, immutable hashes, original IDs, identity ledger, account snapshots,
  analytics account identities and credential-inventory evidence;
- every complete profile revision JSON, plans, dated memberships, bindings and
  unknown fields;
- prices/context/service tiers, comparison books, normalization rules,
  observations/windows/activations/cycles/snapshots, forecasts/results and
  ingestion/checkpoint state;
- historical valuation cohorts, canonical closure payloads, token-evidence
  revisions, observed TTL splits and saved report-window pins;
- other retained tables, schema objects, `sqlite_sequence`, FTS5 backing tables
  and supplied external report artifacts.

Source SQL is archived, never executed on the destination. UTF-8 SQLite encoding
is required. Generated/hidden columns in ordinary tables, unsupported virtual
modules, unaddressable rowid tables and oversized rows fail closed. FTS5 virtual
tables retain schema while **all physical backing tables** are copied; their
zero logical-row manifest count is not lost history. Builds without FTS5 may
reject FTS-bearing sources; use the documented build tag.

### Private extraction schema

| Table | Contents |
| --- | --- |
| `history_import` | Canonical boundary JSON, publication flag |
| `history_source` | `user_version`, `application_id`, `encoding` |
| `history_schema` | Source type/name/table/SQL, including null SQL |
| `history_tables` | Ordered columns/keys, virtual flag, cursor, count, chain |
| `history_rows` | Source table, ordinal, key cells, complete ordered row cells |
| `history_artifacts` | Digest-bound byte-exact external report pins |
| `history_key_stage` | Private hash ownership/original aliases |
| `history_metadata` | Append-only native display revisions |

`Cell` JSON has SQLite storage class and data: signed decimal `integer`, hex
IEEE754 bits for `real`, base64 bytes for `text`/`blob`, or `null`. NULL/empty,
invalid UTF-8 TEXT, BLOBs and integer precision remain distinct. Keyset scans use
an unshadowed rowid or ordered composite primary key for `WITHOUT ROWID`; negative
and zero rowids survive. Chains are SHA256 of
`previousHex + "\n" + Go-encoded archiveRow JSON`, in source-key order, initially
`previousHex=""`. Schema and original content are also anchored by the whole-file
digest. Trusted extractors can page by `(tbl, ordinal)` and interpret cells using
`spec.Columns`. This is not a report replay or arbitrary-SQL management API.

## Parent Keys & Groups API mapping

These are Go methods; **no route is registered here**. Parent must require
management authorization, rejecting ordinary inference keys as administrators.
Source paths/imports/archive SQL never belong in a management request schema.

| Parent operation | Domain method |
| --- | --- |
| Keys & Groups GET | `Store.Keys(ctx)` -> `Metadata` |
| Edit | strict `Edit` body -> `Store.Edit(ctx, edit)` -> new `Metadata` |
| Imported profiles GET | `Store.Profiles(ctx, sourceRevision)`: `-1` latest, `0` initial |
| Preservation status GET | `Store.Status(ctx)` -> safe boundary, hashed table refs/counts/chains |

`Metadata` contains `revision`, `keys`, `groups`. Keys contain `reference`,
`alias`, `current_hash`, sorted `lineage_hashes` (including current), `imported`.
`key:<initial-current-hash>` stays stable after native rotations. Exact historical
CPAMP filtering is `keys:` plus sorted lineage joined by commas. This selects a
contribution to a shared quota pool, not a per-key allowance.

Public DTOs omit raw CPA keys, auth filenames, account snapshots, raw JSON,
artifact contents and DB paths. Credential-shaped/control/oversized labels are
redacted in imported views and rejected on edits; original bytes remain private.
Normal aliases retain exact source spelling.

Every edit supplies the latest native `revision`:

- `add-key`: SHA256 `hash`, optional friendly `name`, empty `reference`. Parent
  supplies the hash; this does not create/authorize a CPA credential.
- `alias`: stable key `reference`, `name` (empty clears display alias).
- `rotate-key`: stable `reference`, unused SHA256 `hash`. Retains reference,
  alias, old hashes and groups. Reusing any owned/retired hash conflicts; merges
  are unsupported. It never creates/revokes credentials or changes routing.
- `put-group`: `reference="group:<64 hex>"`, nonempty `name`, unique known
  `key_references`. Parent generates the stable opaque group ID. Replaces display
  membership without changing key lineage or authorization.
- `delete-group`: reference; keys, history and profiles remain.

Native revisions are append-only. Stale revisions conflict; after a lost response
reload before retrying. Nonempty duplicate native aliases conflict after
case-insensitive trimming; imported aliases remain exact. Bounds: 10,000 imported
hashes, 10,000 native keys, 1,000 groups, 10,000 members/edit, 8MiB edit snapshot.

Display groups are an explicit new multi-client-key feature, separate from CPAMP
account profiles. `Profiles` preserves source revision and membership dates,
including legacy bindings/selection profiles. Profile refs hash original CPAMP
profile IDs; account refs hash exact opaque account IDs. They are display refs,
not new measurement identities. Original IDs/plans/all fields remain archived.
Display edits never modify imported profiles or their source revisions.

Suggested errors: `ErrInput` -> 400, `ErrConflict`/`ErrPending` -> 409,
`ErrStorage` -> 503; `ErrSource`/`ErrDigest` are offline failures. Return constant
errors without driver strings/source paths. Auth/browser wiring belongs to parent.

## Verification and retirement gates

Synthetic schema-matching fixtures test exact cells and preserved domains,
read-only/no-sidecar input, schema/digest/lineage/checkpoint/H failures,
contradictory revisions/lineage, mid-batch rollback, reopen, repeat/concurrent
imports, secret-safe views, edit conflicts/rotation/groups, report artifacts,
FTS5 backing rows, and **100,001 events** with bounded interrupted/resumed copy.
CLI tests cover import/resume/repeat and strict input. Row-content and chain
checks supplement counts.

Still required before retirement:

1. Extract native Usage Quota calculations/evidence/pagination/exports with saved
   v2/v3 valuation, identity semantics, revisions, books and source bounds.
   Connect measurement ingestion without duplicates or lost attribution.
2. Validate a real approved frozen dataset and external report pins; compare exact
   fixed-interval totals/allocation/evidence (early resets, shared-pool key filters,
   gaps, invalid/missing prices, observed vs assumed TTL). Test real rollback.
3. Wire authenticated keys/groups/profile UI and verify CPA/CPAMP navigation and
   CPAMP-unavailable operation in real browsers.
4. Independently preserve original CPAMP history/metadata and approve cutover.
   Synthetic archival success does not establish live migration, native report
   parity, deployment or retirement.

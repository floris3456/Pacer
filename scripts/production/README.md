# Pacer Phase 1+2 on CT104

## Swapping the Pacer library (current)

`swap.py` (run on CT104) registers a built library against a copy of production's Pacer data before
anything live changes, then swaps it with one ordinary CPA restart:

```sh
python3 -B swap.py check <release-dir> --rollback /opt/cliproxyapi/plugins/linux/amd64/pacer-v0.2.0-dev.so
python3 -B swap.py swap  <release-dir> --backup /opt/cliproxyapi/backups/pacer-<name>
```

`<release-dir>` holds a `scripts/build.sh` output (`pacer-v*.so`, `native-probe`). `check` copies every
store in the state directory and the configured report stores, registers twice through the C ABI with
CPA's schema 7, and then registers the rollback library on the upgraded copy. On 2026-10-15 a new table
tripped the report feed's schema witness on the existing database and Pacer did not register; `check`
reproduces that failure for `3cc4004` against the pre-swap database and passes for `7061a35`. `swap`
re-runs `check`, backs up the library, `state.sqlite` and `config.yaml`, installs atomically, restarts
CPA and restores the previous library (one more restart) if Pacer does not answer within `--wait`.

A database backup made with SQLite's backup API restarts the copy's schema version at 1; the report feed
reads that as a schema that went backwards and refuses it. `swap.py` keeps the source's schema version;
an older backup must get `PRAGMA schema_version=<live value at backup time>` before it is restored.


> Historical. Pace routing removed the dry-run mode and its report route
> (`/v0/management/plugins/pacer/dry-run-report` now answers 404), so `dry-run-daily.py` and
> `pacer-dry-run-daily.timer` are retired: disable the timer when deploying pace routing.

`phase1.py` swaps CT104 from quota-handoff 0.1.5-local.1 to Pacer `55fa5ac` in routing `dry-run` on the
pacer-boundary CPA host `bb27d121`. `dry-run-daily.py` archives Pacer's authoritative daily report route
(`GET /v0/management/plugins/pacer/dry-run-report`, see `docs/dry-run-report.md`) verbatim. `rehearse.sh`
proves both against a local replica. Evidence: `pacer/evidence/implementation/phase1-rehearsal/`.

## Operator sequence (from the Mac, in this worktree; `<pve>` is the Proxmox host)

```sh
P=/Users/bliss/Agent/homeserverr/pacer; B=$P/.cache/phase1-release; R=/opt/cliproxyapi/releases/pacer-phase1-55fa5ac-bb27d121
python3 -B scripts/production/phase1.py bundle --out "$B" --cpa-binary "$P/.cache/native/cpa-bb27d121-cgo-linux-amd64" \
  --pacer-dist "$P/.cache/native/src-pacer-55fa5ac-1/dist/linux_amd64" \
  --qh-lib "$P/../quota-switch-compaction/artifacts/release-0.1.5-local.1/quota-handoff-v0.1.5-local.1.so"
COPYFILE_DISABLE=1 tar -C "$B" -cf - . | ssh <pve> "pct exec 104 -- sh -c 'umask 077 && mkdir -p $R && tar --no-same-owner -C $R -xf -'"
ssh <pve> 'pct exec 104 -- python3 -B - recon' < scripts/production/phase1.py   # read-only; review edit_plan, layout, pyyaml
ssh <pve> 'pct exec 104 -- python3 -B - stage' < scripts/production/phase1.py   # image, backups, state init; no runtime change
# Only inside a window the user approved (CPA restarts once, typically under a minute):
ssh <pve> 'pct exec 104 -- python3 -B - cutover --i-have-user-approval' < scripts/production/phase1.py
ssh <pve> 'pct exec 104 -- python3 -B - verify' < scripts/production/phase1.py
ssh <pve> "pct exec 104 -- python3 -B $R/dry-run-daily.py --out /opt/cliproxyapi/reports/pacer-dry-run --latest"
# Back to quota-handoff at any time (idle-gated; add --force only in an emergency):
ssh <pve> 'pct exec 104 -- python3 -B - rollback' < scripts/production/phase1.py
```

The timer `pacer-dry-run-daily.timer` (00:15 UTC) runs the archiver with `--out`; it fetches and stores
`pacer-dry-run-<day>.{json,md}` for every completed UTC day not yet on disk, starting from the newest
archive (or the recorded cutover day, else 31 days back). The first report is the cutover day, whose
`meta.reasons` includes `dry_run_coverage_partial`.

Exit codes: 0 ok, 1 error, 2 refused (nothing changed), 3 not idle (nothing changed), 4 verification failed
and rolled back, 5 rollback incomplete (act now), 6 `verify` found failures.

The archiver mirrors phase1's auth and CLI: `--out DIR` (timer), `--day D`, `--latest`, `--check`; it reads
the management key exactly as `phase1.py` does (`PHASE1_CADDY_DIR`, `PHASE1_CPA_URL`) and never prints it.
Exit codes: 0 ok, 1 error / no route (nothing written), 2 refused / bad request (nothing written). A 429
response honours `Retry-After` up to 5 tries; 503 or a connection failure writes nothing.

## What the rehearsal does not prove

No systemd (the timer is rendered and its `ExecStart` is executed, but not scheduled); no PocketID/TLS on the
public site; no CPAMP container; no other production plugins; production `config.yaml`/compose layouts are
modelled, so `recon` on CT104 must show every `edit_plan` entry and `layout` as applying before `stage`.

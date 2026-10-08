# Pacer Phase 1+2 on CT104

`phase1.py` swaps CT104 from quota-handoff 0.1.5-local.1 to Pacer `ab311f1` in routing `dry-run` on the
pacer-boundary CPA host `bb27d121`. `dry-run-daily.py` writes the Phase 2 daily report. `rehearse.sh`
proves both against a local replica. Evidence: `pacer/evidence/implementation/phase1-rehearsal/`.

## Operator sequence (from the Mac, in this worktree; `<pve>` is the Proxmox host)

```sh
P=/Users/bliss/Agent/homeserverr/pacer; B=$P/.cache/phase1-release; R=/opt/cliproxyapi/releases/pacer-phase1-ab311f1-bb27d121
python3 -B scripts/production/phase1.py bundle --out "$B" --cpa-binary "$P/.cache/native/cpa-bb27d121-cgo-linux-amd64" \
  --pacer-dist "$P/.cache/native/src-pacer-ab311f1-1/dist/linux_amd64" \
  --qh-lib "$P/../quota-switch-compaction/artifacts/release-0.1.5-local.1/quota-handoff-v0.1.5-local.1.so"
COPYFILE_DISABLE=1 tar -C "$B" -cf - . | ssh <pve> "pct exec 104 -- sh -c 'umask 077 && mkdir -p $R && tar --no-same-owner -C $R -xf -'"
ssh <pve> 'pct exec 104 -- python3 -B - recon' < scripts/production/phase1.py   # read-only; review edit_plan, layout, pyyaml
ssh <pve> 'pct exec 104 -- python3 -B - stage' < scripts/production/phase1.py   # image, backups, state init; no runtime change
# Only inside a window the user approved (CPA restarts once, typically under a minute):
ssh <pve> 'pct exec 104 -- python3 -B - cutover --i-have-user-approval' < scripts/production/phase1.py
ssh <pve> 'pct exec 104 -- python3 -B - verify' < scripts/production/phase1.py
ssh <pve> "pct exec 104 -- python3 -B $R/dry-run-daily.py --state /opt/cliproxyapi/plugins/pacer/state.sqlite --out /opt/cliproxyapi/reports/pacer-dry-run --latest"
# Back to quota-handoff at any time (idle-gated; add --force only in an emergency):
ssh <pve> 'pct exec 104 -- python3 -B - rollback' < scripts/production/phase1.py
```

The timer `pacer-dry-run-daily.timer` (00:15 UTC) writes `pacer-dry-run-<day>.{json,md}` for each completed
UTC day; the first is the cutover day with `coverage.partial = true`.

Exit codes: 0 ok, 1 error, 2 refused (nothing changed), 3 not idle (nothing changed), 4 verification failed
and rolled back, 5 rollback incomplete (act now), 6 `verify` found failures.

## What the rehearsal does not prove

No systemd (the timer is rendered and its `ExecStart` is executed, but not scheduled); no PocketID/TLS on the
public site; no CPAMP container; no other production plugins; production `config.yaml`/compose layouts are
modelled, so `recon` on CT104 must show every `edit_plan` entry and `layout` as applying before `stage`.

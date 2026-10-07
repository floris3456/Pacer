# Pacer observation subsystem

Self-contained provider evidence store and persistent reconciliation scheduler, implemented
on `pacer-observation` from `402bd7b`. Integration API and behavior:
[observation contract](../../docs/observation-contract.md).

- `window.go`, `parse.go`: provenance, confidence, cycle merge, pure Claude/Codex parsers.
- `store.go`: shared SQLite persistence and append-only source history.
- `scheduler.go`, `audit.go`: coalesced jobs, persistent request budgets and request accounting.
- `*_test.go`: real SQLite, injected clocks, concurrent pools, reset/backoff/restart regressions.

No external filesystem changes or provider requests. Task caches and Go test temporary files
are under `/Users/bliss/Agent/homeserverr/pacer/.cache/observer`; missing modules are installed
only in that task-local module cache. Production bridge integration is owned by the parent
worktree. The existing module path is retained for cherry-picking.

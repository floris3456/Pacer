# Native marker observations are not prefix proof

The current native payload establishes selected policy and observed caller/CPA
marker TTL. Aggregate cache-read/create counts do not identify an entry, its
relationship to a reusable primary snapshot, or all remaining relevant prefixes.
A scope/epoch hash is not a prefix identity. Successful dispatch, positive counts,
matching selected/effective TTL and breakpoint presence do not fill this gap.

## New records

`FromNative` retains `Policy.SelectedTTL`, `Policy.EffectiveTTL`, policy kind,
primary/auxiliary role, captured attribution and immutable accounting. It always
leaves native `Activity.PrimaryPrefix=false`, `PrefixRef=""`,
`ProtectionTTL=""`, `Partial=true`. No entry-attribution proof is currently supported.
The exact 25/75 write split is still valued independently of the selected marker TTL.
Historical marker readback remains valid; warmth, billing membership completeness
and numeric-time physical coverage do not follow from it.

## Retained pre-fix records

Older canonical native bodies can contain the unsupported positive Activity fields.
They remain historical bytes: no migration, backfill, identity rewrite or revision
is performed. `qualifies` rejects all current records with non-nil `Native`,
regardless of `Activity.Partial`. `buildStretches` ignores that Activity on a local
record copy before selecting a protection TTL, naming a prefix or admitting a
warming call. Native primary activity remains unknown and cannot create qualifying
requests, protection estimates or eligible priors. Marker TTL can still label the
policy cohort; it cannot supply a protection timestamp by itself.

Native warming calls cannot create/rebind a primary stretch or extend even a genuine
generic entry's protection on the same epoch. Unknown native primary bindings stay
conservatively warm after the marker duration; a false TTL expiry cannot make them
cold. Raw record reads and physical fact joins still expose the original historical
record, not a rewritten record. Such Activity fields are not prefix authority.

This guard is independent of source-time/physical checkpoint proof. Exact physical
selection still proves its original local causal domain; it does not confer entry
attribution. Future native entry evidence requires an explicit versioned host proof
and corresponding validation, not clearing `Partial` or reusing an old scope hash.

## Generic executor evidence

Non-native records (`Native=nil`) may carry genuinely qualified explicit executor
Activity. Existing complete-entry behavior is preserved. Generic partial Activity
means an observed qualified entry plus unknown remainder: its request-start-based
protection estimate can be valid for that entry while full warmth remains unknown.
Partial alone grants no positive evidence; the native gate remains closed.

## Replay and parent acceptance

Activity is part of immutable record identity. Re-converting a callback whose old
body contains the removed positives keeps its event ID and revision but changes
that identity; insertion must return `ErrConflict`. A revision bump also cannot
rewrite it. Preserve the refusal and retained evidence rather than renaming the
event, clearing faults or mutating the old row. New callbacks repeat idempotently.

Parent acceptance should build this commit on the pinned startup/physical branch
and review/cherry-pick it onto integration main. Preserve old false-Activity rows
and faults. Use a new consumer/listener for the authorized native rerun, with warmup,
an explicit first lower quiescent cut, then selected attempts and the matching upper
cut. Check corrected new Activity and guarded old projections separately. Marker
history and exact accounting should still pass; no prefix-positive acceptance is
available from the current native wire evidence.

`native_marker_safety_test.go` freezes a real pre-fix `FromNative` body (dummy inputs),
checks its SHA-256/current and revision bytes through public `Measure` and refused
replays, and tests fresh conversion, legacy warming, false expiry and independent
complete/partial generic controls. Source-only red/green receipts live in the task's
`evidence/implementation/native-marker-safety/README.md`.

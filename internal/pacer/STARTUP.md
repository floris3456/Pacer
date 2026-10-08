# Initial catalog enrollment

The optional `host.auth.list.catalog` version-1 descriptor distinguishes a legacy
disk snapshot from a runtime auth catalog and the usage listener's actual lifecycle.
Wire fields: `version`, `domain` (`cpa.auth.disk` / `cpa.auth.runtime`), `state`,
uint64 `generation`, and `listener_state`. The descriptor is parsed locally; no
vendored physical SDK types or native schema/ABI changes are required.

Pacer defers only before its first runtime/legacy catalog observation and only for
explicit `uninitialized` / `loading` states or a `registering` / `installing` listener.
No credential enrollment, checkpoint request, coverage write or fault clearing occurs
on that path. A prior failure prevents deferral from masking it. `ready` requires a
runtime catalog, positive load generation and installed listener. Even an empty ready
catalog is remembered, so subsequent loading/loss cannot be relabeled as startup.

Unknown versions, failed/unavailable/unsupported catalogs, retired listeners,
generation regression and later missing indices retain sticky failure handling.
Old hosts without the descriptor keep their existing conservative validation.
This is a lifecycle boundary, not an account-completeness or physical-trust flag.

The CPA implementation derives state from actual `Manager.Load`, not a setter. It
preserves early CLI plugin loading, which is needed for flags and plugin auth parsers.
CPA's source contract is documented in `docs/plugin-auth-catalog.md` in the matching
CPA commit; both commits must be reviewed/integrated together for this startup fix.

Ordered physical acceptance still requires: installed durable listener; optional
warmup to enroll the source; observed explicit first quiescent lower cut; selected
attempts; acknowledged usage plus actual body/producer closure; known upper cut;
selection by exact boundary identities. Warmup accounting remains recorded outside
that selected interval. Old faults remain stored, `Complete` remains false, and
numeric-time `PhysicalCarryInProven` remains false.

`startup_catalog_test.go` exercises real `Plugin.Handle` registration, all deferred
states, later identity loss, failed/unknown/retired descriptors, empty-ready regression,
and exact selected facts excluding the warmup event. No timer or provider call drives
the tests; the separate CPA fixture proves descriptor transitions with actual loading.

# Pinned CPA native SDK — mixed, independently recorded snapshots

Only `sdk/pluginapi` and `sdk/pluginabi` are vendored. `PROVENANCE.json` records
the **original import**, not the current hashes of every file. Later additions
and replacements mean this is a mixed snapshot; it must not be described as an
unchanged copy of that original commit. Both packages use only the Go standard
library. Retained tests and MIT license remain, and the plugin imports these
packages through the portable `go.mod` replacement at the repository root.

This keeps a clean clone buildable without the full CPA service or a private
checkout. The historical source/archive equivalence applied to the original
import; it is not a current whole-SDK assertion. Native C ABI remains 1; schema-7
contracts were added subsequently. This is not a Go `-buildmode=plugin` module.
Review per-file provenance and the ABI before updating this snapshot.

`INVENTORY-PROVENANCE.json` records two additive files for the
`host.auth.inventory_checkpoint` RPC (`sdk/pluginabi/auth_inventory.go`,
`sdk/pluginapi/auth_inventory.go`). Both are byte-for-byte copies from CPA
`pacer-boundary` `bb27d121a88230bbf25fa9238be1ba4018a48952`.

The later additive schema-7 physical checkpoint subset is pinned separately in
`PHYSICAL-PROVENANCE.json`: `usage_checkpoint.go` and `usage_physical.go` are copied
byte-for-byte from CPA `df794f421de43c861272b2a6b5247286be3c4f07`. This supplement
does not relabel the original snapshot's provenance or change C ABI 1.

`V8-PROVENANCE.json` separately records the exact replacement of
`sdk/pluginapi/types.go` from CPA `9af81ead` to obtain the optional
`PluginManagementBasePath` advertisement. The imported upstream file also
includes existing Path fields and cooldown-reset **type declarations** relative
to the previous vendored file; this import adds no implementation or invocation
of those operations. No active-mode or cooldown-clear call is authorized by this
SDK refresh. Original and physical manifests are preserved as historical records;
the V8 supplement does not rewrite their hashes or claim a whole-SDK refresh.
The retained `sdk/pluginabi/types.go` still declares its historical SchemaVersion
constant as 6. The V8 supplement's schema 7 names the upstream host contract, not
a replacement of that constant. Pacer's runtime registration decides its schema;
the v8 host requires actual schema >=7 registration before advertising the prefix.

`AUTH-POOL-PROVENANCE.json` records the subsequent local additive `AuthPool`,
`RequestInterceptResponse.AuthPool`, and host-only `AuthPoolValidate` declarations.
Its hash describes the current `sdk/pluginapi/types.go`; the V8 manifest retains
the original upstream hash. The matching CPA-host contribution enforces this
optional JSON field before credential selection. This patch changes neither the
native C ABI nor the registration schema; required filtering is separately
attested by the host. It is not an unchanged upstream file.

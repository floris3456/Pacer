# Pacer candidate setup

No live activation is implied by these instructions. Use an isolated CPA instance
and copied state for acceptance. Do not unload or overwrite a library while an
owned runtime has outstanding requests. Stop only owned, confirmed-idle runtimes.

## Host and client

- Linux glibc, C ABI 1. Basic observation/compatibility views support host schema 6.
- Managed Claude TTL requires CPA schema 7 with `cache_policy.handle`, `resolve`,
  synchronous `attempt` begin/validate/finish and deferred logical completion.
- The tested candidate implements protocol 1 / contract revision 2 checkpoint receipts.
- Unsupported providers, transports, gateways and missing/fused policy state fail
  closed for managed requests. Existing unmanaged traffic retains CPA behavior.

Example plugin configuration inside CPA `plugins.configs.pacer`:

```yaml
enabled: true
priority: 1
state_path: /var/lib/pacer/state.sqlite
compact_on_switch: true
routing:
  mode: off
  providers: [claude]
cache_policy:
  enabled: true
  routes:
    - id: claude
      models: [claude-opus-5-5]
  trusted_empty_installations: []
# Optional, pre-created by the trusted offline importer:
# history_path: /var/lib/pacer/history.sqlite
# report_path: /var/lib/pacer/reports/historyreports-INSTANCE/reports.sqlite
```

Use exact host-resolved built-in Claude model IDs, not wildcard/compatible-gateway
names. The current OpenCodePlus client uses route `claude`. Route changes require
an idle reload; model selection is not permission to change provider/transport.

## Explicit initial installation

With CPA stopped and the target Pacer state backed up, initialize once:

```sh
./pacer-state init --state /var/lib/pacer/state.sqlite --routes claude
```

The helper refuses an already initialized policy schema. It is not a repair/reset
command. Normal startup only opens the correctness store, including outstanding
guards. If lost or corrupt, restore it or explicitly reconcile stopped dispatch;
never recreate it to make used context look new. Keep SQLite files owner-only.

## Empty-history authority

By default no installation may claim a fresh empty context. Existing/copied history
uses prepare → actual portable compaction → transition-bound checkpoint receipt →
commit. Cancel does not turn existing history into a fresh chat.

An operator may explicitly trust an authenticated controlled OpenCodePlus installation
to attest **its own Core empty-history state** by adding both `caller_digest` and
`installation_id` under `trusted_empty_installations`. The digest is the host's
SHA256 of `cli-proxy-api:caller-scope:v1` + a NUL byte + authenticated principal.
Never record the raw principal/key in configuration examples or evidence. Matching
only a caller or only an installation is insufficient; no wildcards are supported.
This is a trust grant, not independent server inspection of the private client DB.
Protect that client's credentials and capability. Do not grant this to arbitrary
clients. Configuration changes require an idle reload.

## Management, observation and history

- Public shell: `/v0/resource/plugins/pacer/index.html` (no credentials in its URL).
- The page connects through the authenticated reverse-proxy session; the proxy keeps
  the CPA management key server-side. There is no browser management-key prompt.
  Apply the same SSO gate and server-side key injection to both `/v0/management/*`
  and `/v8/management/*` (including their bare prefix paths). The latter serves
  model settings and forward reports. Never inject the key into inference routes.
- CPAMP embedding uses the restricted same-origin frame bridge. No administrator
  credential is posted into the frame. The existing management API owns model settings
  through `/v8/management/oauth/settings/models` and `/v8/management/models/config`.
- Quota reads return the shared observation cache; refresh requests join the durable
  paced reconciliation scheduler. They do not directly call a provider endpoint.
- Scoped cooldown recovery requires fresh applicable positive new-cycle evidence and
  exact host CAS. It never falls back to a broad administrative cooldown reset.
- Keys/groups edits are display metadata with revision checks. Replacement lineage
  never creates or authorizes an inference credential. Dated account profiles are
  distinct from display groups.
- Import only a frozen SQLite backup through `history-import`; no arbitrary path or
  importer is exposed through management. Archive preservation is not report parity.
  CPAMP remains required until all named parity/rollback gates pass.

Prepare native frozen reports explicitly, outside CPA startup and routing:

```sh
./pacer-state prepare-reports --archive /var/lib/pacer/history.sqlite --output-dir /var/lib/pacer/reports
```

The output directory must already exist. The command creates an owned child directory
and prints its read-only artifact path and default pin. Set `report_path` to that path
at the next confirmed-idle reload. Normal plugin startup only opens preparation metadata;
it never imports/materializes history. Keep the original archive and CPAMP backup.
The Usage Quota page reads the prepared quota facts in memory (no report slots):
`POST /plugins/pacer/usage/{profiles,windows,inspect}` and `PATCH
/plugins/pacer/usage/exclusions` under the CPA-authenticated management namespace.
Exclusions (windows left out of a profile's average) are append-only revisions in
the forward store; without a forward store they are empty and read-only. Facts
older than `pacer-quota-facts-v2` answer 409: re-run offline `prepare-reports`.
The former `history/*` frozen-report and `history/legacy/*` CPAMP routes are
removed. A missing artifact never yields invented totals.

## Build and evidence

`scripts/build.sh` builds the library, `pacer-state`, `history-import`, and runs the
native C ABI probe. `scripts/package.py` packages built artifacts and licenses; it
never installs, restarts, promotes or publishes them. Use focused race tests for
`internal/pacer`, `policyadapter`, `cachepolicy`, `measurement`, `observation`,
`routing`, `tracker`, and the relevant history/CLI tests. Native registration,
authentication, final TTL and browser/TUI acceptance remain distinct checks.

Active routing is unavailable in this candidate until the required observed dry-run
period and integrated acceptance are actually completed. A fake clock is appropriate
for unit tests, not a substitute for the 24–48-hour rollout observation.

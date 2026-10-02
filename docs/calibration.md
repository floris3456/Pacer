# Modes and optional CPAMP calibration

Set `adaptive.mode` explicitly:

| Mode | Behavior |
| --- | --- |
| `shadow` | Observe routing, collect estimates and show manual reminders; no quota-triggered compaction, handoff or admission refusals |
| `enforce` | Use adaptive reserves when fresh calibrated evidence exists; coordinate compaction and safe credential handoff |
| `off` (also omitted) | Use the original fixed-threshold handoff policy; this does **not** disable the plugin |

To disable the plugin, use its CPA `enabled` setting. Keep shadow enabled while
checking the pool, windows, observed costs and reset behavior. Without calibration,
shadow reminders use 20% then 10% for five hours, and 15% then 5% for seven days.
A fresh forecast may warn earlier when estimated requests or compaction headroom
are tighter. Reset times and stale/missing readings remain visible in `/usage`.

## Optional manager backend

Quota views and conservative shadow reminders work without CPAMP. Adaptive
cost-per-percentage calibration needs a **compatible CPAMP implementation** of
`POST /internal/quota-handoff/exchange`. A stock CPAMP installation is not assumed
to expose it. This repository packages the CPA plugin, not the manager patch.

If available, set `adaptive.cpamp_url` and exactly one token source:
`token_file` (absolute, regular, mode 0600) or `token_env`. Use the same dedicated
backend token on CPAMP, at least 32 characters. This token is separate from CPA
frontend and management keys. HTTP is allowed only on private/loopback addresses;
otherwise use HTTPS. Keep the endpoint on your private network and out of Caddy's
public bypass. Do not put real token values in the plugin YAML or Git.

The exchange uses protocol 1. CPA sends `readings` and `projections`; CPAMP returns
`models` describing each account/window's calibration and model-specific token
weights/profiles. See `internal/bridge/adaptive.go` for the wire payload and
`internal/adaptive/model.go` for validation and units. Prices are converted to
API-equivalent cost, then divided by the observed cost per quota percentage point.
Snapshots expire; insufficient, stale or invalid samples are refused. The plugin
exchanges at most once per five seconds while processing traffic; it is not an
independent continuous CPAMP collector.

Estimates include cached input, cache writes, normal input, output, concurrent
reservations, observation lag and compaction reserve. Ordinary clients contribute
usage without a trustworthy distinct-chat count. No estimate guarantees that a
provider's billing or reset behavior will remain constant. Review calibration
with representative chats before enabling enforcement.

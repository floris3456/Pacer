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

The candidate stays routing-off until integrated acceptance and the documented
24–48 hour dry run. Keep all live services and the CPAMP database untouched.

The CPA host must be built with **`CGO_ENABLED=1`** on Linux for native dynamic
plugins. A `CGO_ENABLED=0` server can start and serve management endpoints while
rejecting every `.so`; its successful Go build is not native-plugin acceptance.
Verify the host build metadata and run `check-host.py` (including actual Pacer
registration/state) before inference tests or delegating an exclusive client window.
The isolated CPAMP server uses a separate pure-Go SQLite implementation and may
still be built with CGO disabled; these are different artifact requirements.

`check-history.py` compares the native historical management routes with the
checked-in original CPAMP oracles (53 domain results; 67 byte-exact exports and
37 encoded-row-exact legacy CSVs). It requires `PACER_HISTORY_READY=1`, an explicit
`PACER_EVIDENCE` directory, the extended synthetic 240-event/65-observation artifact,
and a serialized parent acceptance window. It also checks authorization, absent
public aliases, pin/cursor/input refusals and unchanged fake-provider call counts.
Do not run it while a client/browser worker owns the lab. `test_history_checker.py`
tests only the offline comparison logic and never contacts the lab.

`check-coverage.py` requires `PACER_COVERAGE_READY=1`, `PACER_EVIDENCE`, and a parent
window on the corrected scoped-hook candidate. It waits at most 45 seconds for the
30-second worker, requires two explicit **unknown** native reports, then verifies
they survive a real unchanged inventory refresh. Missing reports are failures, not
proof of unknown coverage. It neither infers nor changes provider/fixture state.
Native acceptance runners preserve accumulated fixture attempts; any reset must
be a separately authorized operation with prior evidence retention, not a preamble.

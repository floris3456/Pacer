# Installation and updates

Use a CPA host with the [documented ABI](../README.md#downloads-and-compatibility).
Keep persistent plugin state and token files in a private directory writable by
the CPA service account. In Docker, mount the plugin directory into CPA; copying
a library only onto the host does not make it visible inside the container.

## Download

On the Linux server, choose `amd64` for x86-64 or `arm64` for AArch64:

```sh
version=0.1.2
arch=amd64
asset="quota-handoff_${version}_linux_${arch}.zip"
base="https://github.com/floris3456/cliproxyapi-quota-handoff/releases/download/v${version}"
mkdir -p "quota-handoff-${version}"
cd "quota-handoff-${version}"
curl -fLO "$base/$asset"
curl -fLO "$base/checksums.txt"
sha256sum --check --ignore-missing checksums.txt
unzip "$asset"
```

Verify the chosen archive reports `OK`. The ZIP contains
`quota-handoff-v0.1.2.so`, `LICENSE` and `CPA-SDK-LICENSE`.
Install the library under `<plugins-dir>/linux/<arch>/` with the CPA service's
ownership and read/execute permissions. Merge [the example](../examples/plugin.yaml)
into your existing CPA configuration. Do not replace the rest of your configuration.

Get exact `auth_id` and `auth_index` values from CPA's authenticated
`GET /v0/management/auth-files` response. Only Claude and Codex are supported.
Give credentials for one underlying account the same stable `account` value;
host-owned provider identity also folds aliases. `rank: 1` is smaller capacity
than `rank: 2`. At least two usable accounts for the same provider are needed for
a handoff. One account still supports quota views and reminders.

`usage_auth_url` must reach CPA's own authenticated `/v1/models` endpoint from
inside CPA. Its default is `http://127.0.0.1:8317/v1/models`. Only an HTTP loopback
literal with an explicit port and that exact path is accepted. The endpoint must
reject anonymous access. It must not redirect. Frontend keys are checked afresh;
the plugin never caches them or uses a management key for a usage read.

Use the host's normal plugin management/reload mechanism, then check
`GET /v0/management/plugins` with your existing management authentication. Look
for `id: quota-handoff`, `registered: true`, `effective_enabled: true`, and
`metadata.version: 0.1.2`. Do not send management credentials to clients.

The optional [registry.json](../registry.json) can be added as a custom CPA plugin
store source at the raw GitHub URL. Release asset names match CPA's plugin-store
format. This does not add the plugin to CPA's official catalog.

## Upgrade safely

1. Drain model requests using your existing maintenance procedure. Disabling a
   plugin is not proof that an inference request has finished. Do not interrupt
   busy chats or replace a mapped library in place.
2. Save the old library and the exact per-plugin configuration outside the
   discovered plugin directory. Back up SQLite with its online backup API (or
   after a clean unload); copying only a live `.sqlite` file can miss its WAL.
3. Disable the plugin, wait for the host to observe the disabled state, then
   physically unload it with the host's supported mechanism. On the tested host,
   `DELETE /v0/management/plugins/quota-handoff` unloads **and deletes both the
   selected library and its saved config**. Only use it after the backups above.
4. Wait until the plugin is unregistered. Restore its saved config with
   `enabled: false` via `PUT /v0/management/plugins/quota-handoff/config` and wait
   for that state. Place the new versioned library in the platform directory.
5. Enable it via `PATCH /v0/management/plugins/quota-handoff/enabled` with
   `{"enabled":true}`. Wait for registered/enabled/version checks to succeed.
   Config reloads are asynchronous: do not fire the whole sequence without
   checking each state. If needed, repeat a separate disable/reload/enable cycle.
6. Confirm the same state path, credential pool and adaptive mode; verify the
   public handshake and `/usage`. The tested host supports this without restarting
   CPA. Other hosts may require their own documented restart procedure.

Retain the private state directory. For rollback, drain and unload the candidate,
restore the old library and saved configuration, then enable and verify it. Keep
newer SQLite state unless a specific recovery requires the snapshot. No database
schema change was introduced between plugin 0.1.1 and 0.1.2.

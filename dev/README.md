# Pacer development preview

The preview runs a real CPA and Pacer against **private copies** of the current
production databases. OAuth identities/labels are retained to connect the copied
history, but access tokens, refresh tokens, API keys and JWT signatures are dummy
values. Export refuses if a current production credential appears in the copy.

Provider quota endpoints are local TLS fixtures initialized from the snapshot's
meter readings. The Docker network has no external route. Provider inference is
deliberately not simulated; the fixture returns an explicit error for it.

Preview: **https://cliproxy.boe.moe/pacer-dev/**, behind the same PocketID and email
allowlist as management. A development banner shows the snapshot time. Preview
requests go to its own CPA, not the production management endpoints.

## Daily workflow (from this worktree)

```sh
# Upload tracked working-tree source, including uncommitted edits.
python3 dev/manage.py --host ct104 sync

# HTML/CSS/JS changes are now visible on browser reload; no CPA restart.

# Go/backend changes: build with the normal native ABI checks; restart DEV only.
python3 dev/manage.py --host ct104 build

# Copy today's production data and start a fresh DEV runtime from that snapshot.
# Previous snapshots/runtimes remain available; production is not restarted.
python3 dev/manage.py --host ct104 refresh

python3 dev/manage.py --host ct104 status
python3 dev/manage.py --host ct104 down  # stops only the preview
python3 dev/manage.py --host ct104 up    # resumes its current runtime
```

New source files must be `git add`ed before `sync`; it sends only Git-tracked
paths. Source is mounted read-only into the web/provider containers. The web
adapter changes only the page's API transport prefix and adds the banner; API
validation and UI behavior are the actual working-tree code.

`snapshot` creates a copy without switching the preview. `up --snapshot NAME`
starts a new mutable runtime from a named saved snapshot. `refresh` resets preview
edits to a newly captured copy, so use `snapshot` first if keeping the existing
preview edits is important. Snapshots are never modified by the running preview.

## Host layout and isolation

All runtime material is under **`/opt/pacer-dev`**, outside this repository:

- `source/`: synced development source and build artifacts.
- `snapshots/<time>/`: private completed data exports, dummy auths/config and
  their manifest; incomplete/failed exports have no manifest and cannot start.
- `runtimes/<snapshot>-<time>/`: writable dev database/config/auth copies.
- `tls/`: local fixture certificate/key, trusted only by the preview container.
- `current.json`, `preview.env`: the selected dev runtime and pinned host image.

Compose project `pacer-preview` owns `cpa`, `providers`, `web` and an internal-only
network. Only web port **127.0.0.1:18319** is published. Production directories are
never mounted. The CPA image is pinned by immutable image ID at snapshot time.
Builder caches have their own `pacer-preview-*` volumes. The database exporter
uses SQLite read snapshots and preserves the copied schema generation required
by the measurement feed; it does not reset feed identities or clear evidence.

The data remains private and contains real account/history information. Do not
commit snapshots, runtime files or fixture credentials to Git.

## Review and final swap

Review UI changes here; exercise settings against the dev copy; verify backend
changes and upgrades against a fresh snapshot. Candidate restarts affect only
`pacer-preview-cpa-1`.

After explicit approval, use the existing production artifact-install procedure:
back up the incumbent, stop **only production CPA**, install the exact tested
Pacer library, then start it and verify registration/data/model parity. That is
one production restart. Never copy preview databases, auths or config back into
production, and never promote automatically. This tool has no production restart
or promotion command.

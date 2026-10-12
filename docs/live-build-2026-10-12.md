# Live CPA build provenance — 2026-10-12

The production executable is identified, and its accompanying source snapshot has been recovered locally. The original full Git commit and exact build invocation remain unverified. The detailed, sanitized receipt is [live-build-2026-10-12.json](live-build-2026-10-12.json).

Verified facts:

- Production uses `cliproxyapi-opus55:pacer-7333067`, Docker image ID `sha256:d37bfc8d132aa6128b59632ec8228000b62aaadce7a91c477552723aec310eb7`. No registry manifest digest or OCI source/revision labels are recorded.
- The running executable, preserved `pacer-management-data/image/CLIProxyAPI`, and deployment receipt share SHA-256 `5ed81b093ba4e6e726d97c2e9d0b1064b56d2134f553a27a38d1fbc4c43eb266`. The receipt names short revision `7333067`. The binary identifies Go 1.26.8, module `github.com/router-for-me/CLIProxyAPI/v8`, CGO enabled, and no embedded VCS settings.
- The image has all eight filesystem layers of `cliproxyapi-opus55:pacer-71fb2ac4`, followed by a `CLIProxyAPI` copy. Its first seven layers match `cliproxyapi-opus55:20261007-13`. Those older releases identify their own source revisions; they do not identify the overwritten live executable.
- The source tree at `/opt/cliproxyapi/releases/pacer-management-data/cpa` has no `.git` metadata. Its location beside the matching binary and receipt associates it with this release; a rebuild has not proven source-to-binary equivalence.

The isolated local source copy is `/home/viktor/Projects/pacer-contributions/CPA-host`: 1,800 files, 23,801,302 bytes. Local snapshot commit `a646821df554ec4bb11c61c522892b836db5b176` has Git tree `ea6e5cf02732f334c56a39d2c217e8b05196a16f`. This is a new preservation commit, not the original CPA revision. Every copied file and the selected-source fingerprint were verified against the remote tree; the receipt records SHA-256 `a283673d8814109e1e8cd018cd990582c529374baee6d270a647d43730bf716c` and its algorithm.

The initial copy excluded runtime configuration, auth stores, environment files, logs, data, compiled artifacts, marketing assets and CI files. Source packages such as `internal/config` and `sdk/auth` remain. It included the three embedded model JSON catalogs. The receipt preserves the source Dockerfile's build recipe, but that recipe is not a verified reproduction command for production.

At 12:39 UTC, five specifically requested source files were restored unchanged: `config.example.yaml` and the four `internal/home/testdata` JSON concurrency fixtures. The example YAML has empty management/TLS secret fields and explicit placeholder API keys; the fixtures contain dummy identifiers. No actual `config.yaml`, auth store or environment file was copied. All five SHA-256 values and review details appear in the JSON receipt's `source_recovery_addendum`. This adds 71,814 bytes, bringing recovered source inputs to 1,805 files and 23,873,116 bytes. The original 1,800-file snapshot commit and fingerprint remain historical facts; concurrent host edits and later commits are separate work.

Pacer is a separately mounted shared library. Its current file SHA-256, `65a516eea2e476819fba8a3597e4d5d24ff1dd0093f4c05f6e6518152de2bff5`, matches the activation receipt naming Pacer source `f4a9e2bd0ceedc909b947a561b921927ba9470ae`.

Inspection and copying made no production changes, restarts or provider inference calls. The production PID, image and start time remained unchanged during the original inspection. No copied CPA code was built or run as part of provenance inspection or fixture recovery. Further host work can use the preserved local snapshot, while retaining the uncertainty about its original full commit and reproducibility.

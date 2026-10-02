# Pinned CPA native SDK snapshot

Only `sdk/pluginapi` and `sdk/pluginabi` are copied, unchanged, from the public
CPA source commit recorded in `PROVENANCE.json`. Both use only the Go standard
library. Original tests and MIT license are retained. The plugin imports these
packages through the portable `go.mod` replacement at the repository root.

This keeps a clean clone buildable without the full CPA service or a private
checkout. The installed host source archive and public commit have identical SDK
bytes. Native ABI 1 / schema 6 compatibility is required; this is a C ABI plugin,
not a Go `-buildmode=plugin` module. Review the ABI before updating this snapshot.

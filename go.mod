module github.com/floris3456/cliproxyapi-quota-handoff

go 1.26.0

require (
	github.com/mattn/go-sqlite3 v1.14.32
	github.com/router-for-me/CLIProxyAPI/v7 v7.0.0
	gopkg.in/yaml.v3 v3.0.1
)

// CPA native ABI 1 / schema 6, pinned to the tested host source.
replace github.com/router-for-me/CLIProxyAPI/v7 => ./third_party/cpa-sdk

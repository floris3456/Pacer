// Package buildinfo identifies an artifact without exposing machine-local paths.
package buildinfo

// Release builds set these with -ldflags. Unknown remains explicit in local builds.
var Version = "0.2.0-dev"
var Revision = "unknown"
var Dirty = "unknown"

type Info struct {
	Version  string `json:"version"`
	Revision string `json:"revision"`
	Dirty    *bool  `json:"dirty"`
}

func Current() Info {
	info := Info{Version: Version, Revision: Revision}
	if Dirty == "true" || Dirty == "false" {
		dirty := Dirty == "true"
		info.Dirty = &dirty
	}
	return info
}

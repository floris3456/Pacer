// Package management serves a public, credential-free shell. Its data and
// mutations use CPA's management-authenticated routes, never resource handlers.
package management

import _ "embed"

//go:embed index.html
var Page []byte

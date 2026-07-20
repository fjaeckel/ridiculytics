// Package web embeds the tracking script so a single binary can serve it,
// giving self-hosters a zero-third-party-request option.
package web

import _ "embed"

//go:embed counter.js
var CounterJS []byte

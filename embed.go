// Package sheetdrop exposes the embedded web frontend to cmd/server.
package sheetdrop

import "embed"

//go:embed web
var WebFS embed.FS

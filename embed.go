// Package ziga exposes the embedded web frontend to cmd/server.
package ziga

import "embed"

//go:embed all:web/dist
var WebFS embed.FS

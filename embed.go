// Package ziga exposes the embedded web frontend to cmd/server.
package ziga

import "embed"

//go:embed web
var WebFS embed.FS

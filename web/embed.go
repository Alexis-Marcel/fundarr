// Package web holds the built UI (web/dist, produced by `npm run build`).
package web

import "embed"

//go:embed all:dist
var Dist embed.FS

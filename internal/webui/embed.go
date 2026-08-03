// Package webui embeds the built dashboard SPA so pm ships as a single binary.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the built SPA rooted at dist/.
func FS() (fs.FS, error) {
	return fs.Sub(dist, "dist")
}

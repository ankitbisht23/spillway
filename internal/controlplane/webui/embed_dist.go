//go:build webui

package webui

import (
	"embed"
	"io/fs"
)

// dist is the Vite build (make web-build), which images always embed.
//
//go:embed all:dist
var dist embed.FS

// Files are the UI's files: the Vite build.
func Files() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // a constant, valid path
	}
	return sub
}

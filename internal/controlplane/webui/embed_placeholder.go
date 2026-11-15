//go:build !webui

package webui

import (
	"embed"
	"io/fs"
)

// placeholder is a page that says the binary was built without the UI.
//
//go:embed placeholder
var placeholder embed.FS

// Files are the UI's files: without the webui build tag, the placeholder.
func Files() fs.FS {
	sub, err := fs.Sub(placeholder, "placeholder")
	if err != nil {
		panic(err) // a constant, valid path
	}
	return sub
}

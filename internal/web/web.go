// Package web embeds the UI assets.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var files embed.FS

// Assets is the UI root (index.html, app.js, app.css).
func Assets() fs.FS {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	return sub
}

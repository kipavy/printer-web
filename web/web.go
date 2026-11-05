// Package web embeds the front-end assets served by the application.
package web

import (
	"embed"
	"io/fs"
)

// Add new asset directories (for example vendor) to the embed line.
//
//go:embed *.html
var files embed.FS

// FS returns the embedded assets, rooted at the web directory.
func FS() fs.FS { return files }

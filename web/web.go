// Package web embeds the front-end assets served by the application.
package web

import (
	"embed"
	"io/fs"
)

// Pages, scripts, styles, manifest, icon and the vendored libraries and fonts.
//
//go:embed *.html *.js *.css *.webmanifest *.svg vendor
var files embed.FS

// FS returns the embedded assets, rooted at the web directory.
func FS() fs.FS { return files }

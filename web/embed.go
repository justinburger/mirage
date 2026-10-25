// Package web embeds the management UI: a dependency-free single-page app
// (vanilla JavaScript ES modules, no build step) in web/static.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var static embed.FS

// FS returns the UI files rooted at web/static.
func FS() fs.FS {
	sub, err := fs.Sub(static, "static")
	if err != nil {
		panic(err)
	}
	return sub
}

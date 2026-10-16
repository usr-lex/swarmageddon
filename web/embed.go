// Package web embeds the control center's built UI (web/dist).
//
// The React app is built into web/dist by Vite. When dist holds no build,
// the server shows Placeholder instead, so the Go binary still builds and runs.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

const placeholder = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>swarmageddon</title></head>
<body><h1>swarmageddon control center</h1><p>The web UI was not built into this binary. The API is served under /api.</p></body></html>
`

// Dist returns the UI's files, rooted at dist, and whether a real build (an
// index.html) is present.
func Dist() (fs.FS, bool) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, false
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return sub, false
	}
	return sub, true
}

// Placeholder is the page served when no UI was built.
func Placeholder() []byte { return []byte(placeholder) }

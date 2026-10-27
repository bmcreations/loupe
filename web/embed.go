// Package web embeds the browser client so the server ships as a single file.
package web

import "embed"

// The patterns are globs rather than a list of filenames because a module that
// app.js imports and the binary does not embed is a 404 at runtime and nothing
// at build time: the page loads, the module fails, and the client dies part
// way through evaluation. Adding a module should not mean remembering three
// separate lists (this one, clientModules, and the //go:embed above it).
//
// The *.mjs test files are deliberately not matched -- they are run by node
// from the source tree and have no business in the binary.
//
//go:embed *.html *.css *.js *.svg *.png
var FS embed.FS

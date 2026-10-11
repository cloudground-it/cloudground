// Package ui carries the panel's web interface inside the panel-api binary.
//
// ui/dist is the Nuxt build of ui/app, committed so that `go build` needs no
// Node; `make ui` or `make docker-dist` rebuilds it.
package ui

import (
	"embed"
	"io/fs"
)

// The "all:" prefix keeps the build's files whose names start with "_" or
// ".", which Nuxt writes (the _nuxt folder).
//
//go:embed all:dist
var built embed.FS

// FS is the built interface with dist/ as its root: index.html at the top.
func FS() fs.FS {
	root, err := fs.Sub(built, "dist")
	if err != nil {
		// fs.Sub fails only on an invalid name, and "dist" is valid.
		panic("ui: " + err.Error())
	}
	return root
}

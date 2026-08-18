// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package web embeds the built dashboard into the LoadWave binary.
//
// Shipping the UI inside the binary is what keeps deployment to a single file:
// there is no asset directory to place correctly, and no way for the dashboard
// to end up a different version from the coordinator serving it.
//
// The embedded directory is the frontend's build output. It is not checked in,
// so a fresh clone embeds only a placeholder and the server explains how to
// build the real thing. Run `make ui` — or `npm --prefix web ci && npm --prefix
// web run build` — to populate it.
package web

import (
	"embed"
	"io/fs"
)

// The `all:` prefix is required: without it the placeholder that keeps the
// directory present in a fresh clone would be skipped as a dotfile, and the
// embed would fail to compile.
//
//go:embed all:dist
var dist embed.FS

// Assets returns the built dashboard rooted at its index, and whether a real
// build is present.
//
// A clone that has never built the frontend yields ok=false rather than an
// error, so `go build ./...` and `go test ./...` work with no Node toolchain
// installed at all — which matters for contributors working only on the Go
// side.
func Assets() (fs.FS, bool) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, false
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil, false
	}
	return sub, true
}

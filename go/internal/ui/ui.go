// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package ui embeds the management UI: the build of web/ (make web), which
// Vite writes into dist/. Only dist/.keep is committed, so that the module
// builds without npm; such a build serves no UI (Built reports it).
package ui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the built UI, rooted at its index.html.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// "dist" is a fixed, valid path of the embedded tree.
		return dist
	}
	return sub
}

// Built reports whether the UI was built into this binary: whether
// dist/index.html exists.
func Built() bool {
	_, err := fs.Stat(FS(), "index.html")
	return err == nil
}

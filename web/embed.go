// Package web holds strand's embedded front-end assets.
package web

import (
	"embed"
	"io/fs"
)

//go:embed index.html static
var assets embed.FS

// FS is the front-end file tree, rooted so index.html sits at "/".
var FS fs.FS = assets

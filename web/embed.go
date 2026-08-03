// Package web exposes the browser files embedded in the scimux binary.
package web

import "embed"

// Files contains only production browser assets. Tests and package metadata
// remain on disk and are intentionally excluded.
//
//go:embed index.html assets css js
var Files embed.FS

// Package legal exposes the third-party notices embedded in scimux.
package legal

import "embed"

// Files contains the license notices served by the local About sheet.
//
//go:embed *.LICENSE
var Files embed.FS

// Command scimux supervises tmux-wrapped agent chats from a local web page.
package main

import "codeberg.org/chrberger/scimux/internal/app"

// version is stamped at build time: go build -ldflags "-X main.version=v0.5.0".
var version = "dev"

func main() {
	app.SetVersion(version)
	app.Run()
}

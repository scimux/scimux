module github.com/scimux/scimux

go 1.26.0

// The minimum *patched* toolchain, which the go directive cannot express: that
// one is the language floor and is satisfied by any 1.26.x, including patches
// with known standard-library defects. CI resolves this version and builds what
// users download on a patched toolchain; a developer running GOTOOLCHAIN=local
// may still use theirs because the release gate is enforced in the workflows.
toolchain go1.26.9

// builds/ is the gitignored directory the workflows write release binaries
// into, and review sandboxes have parked whole GOCACHE trees under it. It
// never holds a package, but `./...` walks it anyway — the go command does
// not read .gitignore — so a stale sandbox turns every `go test ./...` into
// tens of thousands of extra directory round-trips. Needs the go directive
// above: the ignore directive is go 1.25.
ignore builds

require (
	github.com/coder/acp-go-sdk v0.13.5
	github.com/pion/webrtc/v4 v4.2.18
)

require (
	github.com/google/uuid v1.6.0 // indirect
	github.com/pion/datachannel v1.6.2 // indirect
	github.com/pion/dtls/v3 v3.1.5 // indirect
	github.com/pion/ice/v4 v4.4.0 // indirect
	github.com/pion/interceptor v0.1.47 // indirect
	github.com/pion/logging v0.2.4 // indirect
	github.com/pion/mdns/v2 v2.1.0 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtcp v1.2.17 // indirect
	github.com/pion/rtp v1.10.5 // indirect
	github.com/pion/sctp v1.11.1 // indirect
	github.com/pion/sdp/v3 v3.0.19 // indirect
	github.com/pion/srtp/v3 v3.0.12 // indirect
	github.com/pion/stun/v3 v3.1.6 // indirect
	github.com/pion/transport/v4 v4.0.2 // indirect
	github.com/pion/turn/v5 v5.0.12 // indirect
	github.com/wlynxg/anet v0.0.5 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/time v0.14.0 // indirect
)

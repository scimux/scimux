module codeberg.org/chrberger/scimux

go 1.25.0

// The minimum *patched* toolchain, which the go directive cannot express: that
// one is the language floor and is satisfied by any 1.25.x, including patches
// with known defects in crypto/x509, net/http and os. 1.25.13 is the highest
// fix version govulncheck reports for the stdlib paths this binary reaches.
//
// Advisory by design. CI resolves it and builds what users download on a
// patched toolchain; a developer running GOTOOLCHAIN=local falls back to
// theirs rather than being stopped, because the gate that must not be dodged
// is in the workflows, not on the contributor's machine.
toolchain go1.25.13

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
	golang.org/x/crypto v0.55.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/time v0.14.0 // indirect
)

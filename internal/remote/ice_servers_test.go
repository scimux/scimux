package remote

// S6 — ICE servers on the laptop's answering path.
//
// Every production peer connection except acceptSessionOffer is created with
// a bare webrtc.Configuration{} on purpose: those sites are in-process, the
// device half, or fault injection. The answering path is the one that has to
// traverse NAT. These rows pin the origin→STUN derivation that path uses.

import (
	"net"
	"net/url"
	"testing"

	"github.com/pion/webrtc/v4"
)

func TestICEServersFromOrigin(t *testing.T) {
	defaultHost, err := url.Parse(DefaultOrigin)
	if err != nil || defaultHost.Hostname() == "" {
		t.Fatalf("DefaultOrigin %q must parse to a host", DefaultOrigin)
	}
	defaultSTUN := "stun:" + net.JoinHostPort(defaultHost.Hostname(), "3478")

	tests := []struct {
		name   string
		origin string
		want   []string
	}{
		{
			name:   "https host",
			origin: "https://my.scimux.eu",
			want:   []string{"stun:my.scimux.eu:3478"},
		},
		{
			name:   "https host:port",
			origin: "https://my.scimux.eu:8443",
			want:   []string{"stun:my.scimux.eu:3478"},
		},
		{
			name:   "http loopback with a port",
			origin: "http://127.0.0.1:8099",
			want:   []string{"stun:127.0.0.1:3478"},
		},
		{
			name:   "IPv6 literal",
			origin: "https://[2001:db8::1]",
			want:   []string{"stun:[2001:db8::1]:3478"},
		},
		{
			name:   "empty origin uses DefaultOrigin host",
			origin: "",
			want:   []string{defaultSTUN},
		},
		{
			name:   "malformed origin yields no servers",
			origin: "not a url",
			want:   nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := iceServersFromOrigin(tt.origin)
			urls := iceServerURLs(got)
			if tt.want == nil {
				if len(urls) != 0 {
					t.Fatalf("iceServersFromOrigin(%q) = %v, want an empty server list, not an error", tt.origin, urls)
				}
				return
			}
			if len(urls) != len(tt.want) {
				t.Fatalf("iceServersFromOrigin(%q) = %v, want %v", tt.origin, urls, tt.want)
			}
			for i := range tt.want {
				if urls[i] != tt.want[i] {
					t.Fatalf("iceServersFromOrigin(%q) = %v, want %v", tt.origin, urls, tt.want)
				}
			}
		})
	}
}

func iceServerURLs(servers []webrtc.ICEServer) []string {
	var urls []string
	for _, s := range servers {
		urls = append(urls, s.URLs...)
	}
	return urls
}

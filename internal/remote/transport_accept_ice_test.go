package remote

// S6 — the answering path must actually ask STUN.
//
// acceptSessionOffer is the one peer connection that carries real traffic: a
// paired device's sealed §12.1 offer arrives, and the computer has to traverse
// NAT to reach a phone on another network. A bare Configuration{} gathers
// host candidates only. AT-int-a cannot catch that: it joins two pion peers
// in one process on one host, where host candidates are enough.
//
// These rows drive a real offer through acceptSessionOffer pointed at the
// in-test Binding responder. The required proof is that the responder saw a
// well-formed Binding Request — that is the configuration reaching pion.
// An srflx candidate, if the stack emits one, must match the address the
// responder replied; its absence is not a failure, because on loopback the
// reflexive address equals the host address and pion may drop it as
// redundant.

import (
	"bytes"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"
)

func TestAcceptSessionOfferSendsSTUNBindingRequest(t *testing.T) {
	stun := startTestSTUN(t)

	ctx, cancel := ctxTO(t)
	defer cancel()

	dev := newDevicePeer(t, ctx)
	ice := []webrtc.ICEServer{{URLs: []string{"stun:" + stun.Addr()}}}

	s, answer, err := acceptSessionOffer(ctx, dev.offer, tunnelEcho(nil), ice)
	if s != nil {
		t.Cleanup(func() { _ = s.Close() })
	}
	if err != nil {
		t.Fatalf("acceptSessionOffer: %v", err)
	}

	if !stun.SawBindingRequest() {
		t.Fatal("acceptSessionOffer sent no STUN Binding Request; ICE servers did not reach pion")
	}

	// On loopback the reflexive address equals the host address, and pion
	// may suppress the candidate as redundant — so a missing srflx is not
	// a failure. The Binding Request above is the required proof.
	mapped := stun.MappedAddrs()
	for _, c := range sdpSRFLX(answer.SDP) {
		if !udpAddrIn(c, mapped) {
			t.Fatalf("srflx candidate %s does not match any address the STUN responder replied (%v)", c.String(), mapped)
		}
	}
}

func TestTransportAcceptHasNoBareICEConfiguration(t *testing.T) {
	src, err := os.ReadFile("transport_accept.go")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(src, []byte("webrtc.Configuration{}")) {
		t.Fatal("transport_accept.go creates a peer connection with a bare webrtc.Configuration{}; the answering path must take ICE servers derived from the rendezvous origin")
	}
}

func sdpSRFLX(sdp string) []net.UDPAddr {
	var out []net.UDPAddr
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimRight(line, "\r")
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		if !strings.HasPrefix(fields[0], "a=candidate:") {
			continue
		}
		if fields[6] != "typ" || fields[7] != "srflx" {
			continue
		}
		ip := net.ParseIP(fields[4])
		if ip == nil {
			continue
		}
		port, err := strconv.Atoi(fields[5])
		if err != nil {
			continue
		}
		out = append(out, net.UDPAddr{IP: ip, Port: port})
	}
	return out
}

func udpAddrIn(got net.UDPAddr, have []net.UDPAddr) bool {
	for _, a := range have {
		if a.Port == got.Port && a.IP.Equal(got.IP) {
			return true
		}
	}
	return false
}

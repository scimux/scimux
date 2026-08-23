package remote

// Hermetic STUN Binding responder for the answering-path ICE test.
//
// Tests in this repository must not depend on the scimux-rv binary. This is
// a stdlib UDP Binding-only responder (rendezvous-v1 §17) bound to
// 127.0.0.1:0. It answers a well-formed Binding Request with a Binding
// Success carrying XOR-MAPPED-ADDRESS and nothing else, and drops everything
// else. The vendored vectors in testdata/vectors/stun.json are the authority
// for that encoding; they are read through the same helpers
// protocol_vectors_test.go already uses.

import (
	"encoding/binary"
	"encoding/hex"
	"net"
	"sync"
	"testing"
)

const stunRecvCap = 1280

// testSTUN is a loopback Binding-only responder. Addr is the UDP host:port
// an ICE server URL can point at.
type testSTUN struct {
	conn net.PacketConn

	mu     sync.Mutex
	reqs   [][]byte
	mapped []*net.UDPAddr
}

func startTestSTUN(t *testing.T) *testSTUN {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen STUN: %v", err)
	}
	s := &testSTUN{conn: conn}
	done := make(chan struct{})
	t.Cleanup(func() {
		_ = conn.Close()
		<-done
	})
	go func() {
		defer close(done)
		buf := make([]byte, stunRecvCap)
		for {
			n, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			pkt := append([]byte(nil), buf[:n]...)
			reply := stunBindingReply(pkt, addr)
			if !stunIsBindingRequest(pkt) {
				continue
			}
			s.mu.Lock()
			s.reqs = append(s.reqs, pkt)
			if ua, ok := addr.(*net.UDPAddr); ok {
				cp := *ua
				s.mapped = append(s.mapped, &cp)
			}
			s.mu.Unlock()
			if len(reply) == 0 {
				continue
			}
			if _, err := conn.WriteTo(reply, addr); err != nil {
				return
			}
		}
	}()
	return s
}

func (s *testSTUN) Addr() string {
	return s.conn.LocalAddr().String()
}

func (s *testSTUN) SawBindingRequest() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, req := range s.reqs {
		if stunIsBindingRequest(req) {
			return true
		}
	}
	return false
}

func (s *testSTUN) MappedAddrs() []net.UDPAddr {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]net.UDPAddr, 0, len(s.mapped))
	for _, a := range s.mapped {
		if a == nil {
			continue
		}
		out = append(out, *a)
	}
	return out
}

func stunIsBindingRequest(pkt []byte) bool {
	if len(pkt) < specSTUNHeaderLen || len(pkt) > stunRecvCap {
		return false
	}
	if binary.BigEndian.Uint16(pkt[0:2]) != specSTUNBindingRequest {
		return false
	}
	if int(binary.BigEndian.Uint16(pkt[2:4])) != len(pkt)-specSTUNHeaderLen {
		return false
	}
	return binary.BigEndian.Uint32(pkt[4:8]) == specSTUNMagicCookie
}

func stunBindingReply(pkt []byte, src net.Addr) []byte {
	if !stunIsBindingRequest(pkt) {
		return nil
	}
	ua, ok := src.(*net.UDPAddr)
	if !ok || ua.IP == nil {
		return nil
	}
	return specXORMapped(append([]byte(nil), pkt[8:20]...), ua.IP, ua.Port)
}

func TestSTUNResponderMatchesVendoredVectors(t *testing.T) {
	reqV := mustStunVector(t, "stun-binding-request")
	executeSTUN(t, reqV)
	req, err := hex.DecodeString(reqV.StunHex)
	if err != nil {
		t.Fatalf("stun-binding-request stun_hex: %v", err)
	}
	if !stunIsBindingRequest(req) {
		t.Fatalf("responder rejected the vendored Binding Request")
	}

	v4 := mustStunVector(t, "stun-binding-success-v4")
	executeSTUN(t, v4)
	src4 := &net.UDPAddr{IP: net.ParseIP(v4.MappedIP), Port: v4.MappedPort}
	got4 := stunBindingReply(req, src4)
	want4, err := hex.DecodeString(v4.ResponseHex)
	if err != nil {
		t.Fatalf("stun-binding-success-v4 response_hex: %v", err)
	}
	if string(got4) != string(want4) {
		t.Fatalf("IPv4 Binding Success\n got %x\nwant %x", got4, want4)
	}

	v6 := mustStunVector(t, "stun-binding-success-v6")
	executeSTUN(t, v6)
	src6 := &net.UDPAddr{IP: net.ParseIP(v6.MappedIP), Port: v6.MappedPort}
	got6 := stunBindingReply(req, src6)
	want6, err := hex.DecodeString(v6.ResponseHex)
	if err != nil {
		t.Fatalf("stun-binding-success-v6 response_hex: %v", err)
	}
	if string(got6) != string(want6) {
		t.Fatalf("IPv6 Binding Success\n got %x\nwant %x", got6, want6)
	}

	drop := mustStunVector(t, "stun-drop-truncated")
	executeSTUNDrop(t, drop)
	trunc, err := hex.DecodeString(drop.StunHex)
	if err != nil {
		t.Fatalf("stun-drop-truncated stun_hex: %v", err)
	}
	if reply := stunBindingReply(trunc, src4); reply != nil {
		t.Fatalf("truncated Binding Request produced a reply: %x", reply)
	}
}

func mustStunVector(t *testing.T, id string) vector {
	t.Helper()
	for _, v := range loadVectors(t, "stun-responder") {
		if v.ID == id {
			return v
		}
	}
	t.Fatalf("vendored vector %s is missing from testdata/vectors", id)
	return vector{}
}

package remote

import (
	"net/http"
	"strings"
	"testing"
)

// The live accept path and the value-only session helpers share checkInner.
// Pin every rejection rung so a future field addition cannot accidentally
// turn a malformed envelope into a usable offer.
func TestSessionInnerAdmissionTable(t *testing.T) {
	valid := SessionInner{
		V:           ProtocolVersion,
		Type:        sessionOfferType,
		SDP:         "v=0\r\n",
		Fingerprint: "sha-256 AA:BB",
	}
	tests := []struct {
		name string
		in   SessionInner
		word string
	}{
		{"valid", valid, ""},
		{"version", func() SessionInner { v := valid; v.V++; return v }(), "version"},
		{"type", func() SessionInner { v := valid; v.Type = sessionAnswerType; return v }(), "offer"},
		{"sdp", func() SessionInner { v := valid; v.SDP = ""; return v }(), "description"},
		{"fingerprint", func() SessionInner { v := valid; v.Fingerprint = ""; return v }(), "fingerprint"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkInner(tt.in, sessionOfferType)
			if tt.word == "" {
				if err != nil {
					t.Fatalf("valid inner rejected: %v", err)
				}
				return
			}
			requireClass(t, err, ClassHandshake)
			if !strings.Contains(strings.ToLower(guidanceOf(err)), tt.word) {
				t.Fatalf("guidance %q does not identify %s rejection", guidanceOf(err), tt.name)
			}
		})
	}
}

func TestTunnelResponseWriterFlushPublishesHeaders(t *testing.T) {
	w := newTunnelResponseWriter()
	defer w.finish()
	w.Header().Set("X-Test", "ready")
	w.Flush()
	select {
	case <-w.headerCh:
	default:
		t.Fatal("Flush did not make response headers available to the tunnel")
	}
	if w.status != http.StatusOK {
		t.Fatalf("Flush status = %d, want %d", w.status, http.StatusOK)
	}
	if got := w.snapshot.Get("X-Test"); got != "ready" {
		t.Fatalf("flushed header = %q, want ready", got)
	}
	// A second flush must be harmless: handlers may flush after each chunk.
	w.Flush()
}

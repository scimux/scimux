package acp

import (
	"math"
	"strconv"
	"testing"

	sdk "github.com/coder/acp-go-sdk"
)

func TestParseDeliverToken(t *testing.T) {
	overflowStr := strconv.FormatUint(math.MaxUint64, 10) + "0"

	cases := []struct {
		name   string
		tok    string
		incarn string
		seq    uint64
		opt    sdk.PermissionOptionId
		ok     bool
	}{
		{"valid", "inc-a:42:allow-once", "inc-a", 42, "allow-once", true},
		{"option id with colons", "inc-a:7:ns:opt:id", "inc-a", 7, "ns:opt:id", true},
		{"empty token", "", "", 0, "", false},
		{"missing incarnation", ":1:opt", "", 0, "", false},
		{"empty incarnation", ":5:opt", "", 0, "", false},
		{"missing sequence", "inc:opt", "", 0, "", false},
		{"empty sequence", "inc::opt", "", 0, "", false},
		{"non-decimal sequence", "inc:1x:opt", "", 0, "", false},
		{"negative sequence", "inc:-3:opt", "", 0, "", false},
		{"overflowing sequence", "inc:" + overflowStr + ":opt", "", 0, "", false},
		{"missing option id", "inc:9:", "", 0, "", false},
		{"no separators", "incarnonly", "", 0, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			incarn, seq, opt, ok := parseDeliverToken(tc.tok)
			if ok != tc.ok || incarn != tc.incarn || seq != tc.seq || opt != tc.opt {
				t.Fatalf("parseDeliverToken(%q) = (%q, %d, %q, %v), want (%q, %d, %q, %v)",
					tc.tok, incarn, seq, opt, ok, tc.incarn, tc.seq, tc.opt, tc.ok)
			}
		})
	}
}

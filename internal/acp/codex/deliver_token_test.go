package codex

import (
	"math"
	"strconv"
	"testing"
)

func TestParseDeliverToken(t *testing.T) {
	overflowStr := strconv.FormatUint(math.MaxUint64, 10) + "0"

	cases := []struct {
		name   string
		tok    string
		incarn string
		seq    uint64
		idx    int
		ok     bool
	}{
		{"valid", "inc-a:42:1", "inc-a", 42, 1, true},
		{"zero index", "inc-a:3:0", "inc-a", 3, 0, true},
		{"negative index", "inc-a:3:-2", "inc-a", 3, -2, true},
		{"empty token", "", "", 0, 0, false},
		{"missing incarnation", ":1:0", "", 0, 0, false},
		{"empty incarnation", ":5:0", "", 0, 0, false},
		{"missing sequence", "inc:0", "", 0, 0, false},
		{"empty sequence", "inc::0", "", 0, 0, false},
		{"non-decimal sequence", "inc:1x:0", "", 0, 0, false},
		{"negative sequence", "inc:-3:0", "", 0, 0, false},
		{"overflowing sequence", "inc:" + overflowStr + ":0", "", 0, 0, false},
		{"missing index", "inc:9:", "", 0, 0, false},
		{"non-integer index", "inc:9:x", "", 0, 0, false},
		{"extra colon in index", "inc:9:1:2", "", 0, 0, false},
		{"no separators", "incarnonly", "", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			incarn, seq, idx, ok := parseDeliverToken(tc.tok)
			if ok != tc.ok || incarn != tc.incarn || seq != tc.seq || idx != tc.idx {
				t.Fatalf("parseDeliverToken(%q) = (%q, %d, %d, %v), want (%q, %d, %d, %v)",
					tc.tok, incarn, seq, idx, ok, tc.incarn, tc.seq, tc.idx, tc.ok)
			}
		})
	}
}

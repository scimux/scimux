package codex

import "testing"

func TestMapKeyToDecision(t *testing.T) {
	mixed := []Decision{
		{Key: "decline"},
		{Key: "accept"},
		{Key: "cancel"},
	}
	allReject := []Decision{{Key: "decline"}, {Key: "cancel"}, {Key: "denied"}}
	noneReject := []Decision{{Key: "accept"}, {Key: "acceptForSession"}, {Key: "allowForTurn"}}

	cases := []struct {
		name string
		key  string
		ds   []Decision
		idx  int
		ok   bool
	}{
		{"empty list", "y", nil, 0, false},
		{"numeric first", "1", mixed, 0, true},
		{"numeric middle", "2", mixed, 1, true},
		{"numeric last", "3", mixed, 2, true},
		{"numeric zero", "0", mixed, 0, false},
		{"numeric negative", "-1", mixed, 0, false},
		{"numeric out of range", "4", mixed, 0, false},
		{"y first non-rejection", "y", mixed, 1, true},
		{"Enter first non-rejection", "Enter", mixed, 1, true},
		{"y all reject falls back to first", "y", allReject, 0, true},
		{"Enter all reject falls back to first", "Enter", allReject, 0, true},
		{"n first rejection", "n", mixed, 0, true},
		{"Escape first rejection", "Escape", mixed, 0, true},
		{"n none reject falls back to last", "n", noneReject, 2, true},
		{"Escape none reject falls back to last", "Escape", noneReject, 2, true},
		{"unknown key", "x", mixed, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idx, ok := mapKeyToDecision(tc.key, tc.ds)
			if idx != tc.idx || ok != tc.ok {
				t.Fatalf("mapKeyToDecision(%q) = (%d, %v), want (%d, %v)", tc.key, idx, ok, tc.idx, tc.ok)
			}
		})
	}
}

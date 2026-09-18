package codex

import (
	"reflect"
	"testing"

	"github.com/scimux/scimux/internal/asset"
	"github.com/scimux/scimux/internal/sessionlog"
)

// TestScanAssetsLocked mirrors the ACP transport's Phase-4 turn-append gate:
// nil hook / unsupported / empty scans never call through; assistant markdown
// and write-shaped tool output do, with the session's node ID and directory.
func TestScanAssetsLocked(t *testing.T) {
	type call struct {
		nodeID string
		dir    string
		cands  []asset.Candidate
	}
	cases := []struct {
		name string
		hook bool
		ev   Event
		want *call
	}{
		{
			name: "nil hook",
			hook: false,
			ev:   Event{T: "assistant", Text: "see ![img](chart.png)"},
		},
		{
			name: "unsupported event",
			hook: true,
			ev:   Event{T: "user", Text: "![img](chart.png)"},
		},
		{
			name: "assistant text without assets",
			hook: true,
			ev:   Event{T: "assistant", Text: "no links here"},
		},
		{
			name: "assistant markdown with asset",
			hook: true,
			ev:   Event{T: "assistant", Text: "see ![a chart](out/chart.png)"},
			want: &call{
				nodeID: "node-a",
				dir:    "/work",
				cands:  []asset.Candidate{{Ref: "out/chart.png", Alt: "a chart", IsImage: true}},
			},
		},
		{
			name: "tool event with nil tool data",
			hook: true,
			ev:   Event{T: "tool"},
		},
		{
			name: "tool output with asset",
			hook: true,
			ev: Event{
				T: "tool",
				Tool: &sessionlog.ToolEvent{
					Kind:     "edit",
					Title:    "Write",
					RawInput: map[string]any{"path": "notes/out.md"},
				},
			},
			want: &call{
				nodeID: "node-a",
				dir:    "/work",
				cands:  []asset.Candidate{{Ref: "notes/out.md"}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []call
			s := &Session{nodeID: "node-a", dir: "/work"}
			if tc.hook {
				s.assetHook = func(nodeID, dir string, cands []asset.Candidate) {
					got = append(got, call{nodeID: nodeID, dir: dir, cands: append([]asset.Candidate(nil), cands...)})
				}
			}
			s.scanAssetsLocked(tc.ev)
			if tc.want == nil {
				if len(got) != 0 {
					t.Fatalf("callback count = %d, want 0; got %#v", len(got), got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("callback count = %d, want 1; got %#v", len(got), got)
			}
			if got[0].nodeID != tc.want.nodeID || got[0].dir != tc.want.dir {
				t.Errorf("node/dir = (%q, %q), want (%q, %q)", got[0].nodeID, got[0].dir, tc.want.nodeID, tc.want.dir)
			}
			if !reflect.DeepEqual(got[0].cands, tc.want.cands) {
				t.Errorf("cands = %#v, want %#v", got[0].cands, tc.want.cands)
			}
		})
	}
}

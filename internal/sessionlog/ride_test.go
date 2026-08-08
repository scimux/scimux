package sessionlog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeRideLog writes synthetic JSONL events for ride fold tests.
func writeRideLog(t *testing.T, name string, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name+".jsonl")
	var b []byte
	for _, ln := range lines {
		b = append(b, ln...)
		b = append(b, '\n')
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadRidesBySegment_OverlappingToolsAndWait(t *testing.T) {
	// One segment: real = 20s (10:00:00 → 10:00:20).
	// Tools: id=a pending@0 completed@5; id=b pending@3 completed@8 → union 8s.
	// Wait: start@10 end@14 → 4s. Agent residual = 8s.
	// agent+tools+wait ≡ real.
	path := writeRideLog(t, "overlap",
		`{"t":"meta","time":"2026-08-01T10:00:00Z","meta":{"node":"r1","uid":"u1","agent":"grok","created":"2026-08-01T10:00:00Z"}}`,
		`{"t":"user","time":"2026-08-01T10:00:00Z","text":"go"}`,
		`{"t":"tool","time":"2026-08-01T10:00:00Z","tool":{"id":"a","title":"Bash","status":"pending"}}`,
		`{"t":"tool","time":"2026-08-01T10:00:03Z","tool":{"id":"b","title":"Read","status":"pending"}}`,
		`{"t":"tool","time":"2026-08-01T10:00:05Z","tool":{"id":"a","status":"completed"}}`,
		`{"t":"tool","time":"2026-08-01T10:00:08Z","tool":{"id":"b","status":"completed"}}`,
		`{"t":"attention","time":"2026-08-01T10:00:10Z","attention":{"kind":"approval","status":"start"}}`,
		`{"t":"attention","time":"2026-08-01T10:00:14Z","attention":{"kind":"approval","status":"end"}}`,
		`{"t":"assistant","time":"2026-08-01T10:00:20Z","text":"done"}`,
		`{"t":"usage","time":"2026-08-01T10:00:20Z","usage":{"inputTokens":10,"outputTokens":5,"turnId":"t1","costAmount":0.01}}`,
	)
	rides := ReadRidesBySegment(path)
	if len(rides) != 1 {
		t.Fatalf("segments = %d, want 1", len(rides))
	}
	r := rides[0]
	if !r.HasSplit {
		t.Fatal("HasSplit = false, want true")
	}
	if r.Real != 20*time.Second {
		t.Errorf("Real = %v, want 20s", r.Real)
	}
	if r.Tools != 8*time.Second {
		t.Errorf("Tools = %v, want 8s (union of overlapping)", r.Tools)
	}
	if r.Wait != 4*time.Second {
		t.Errorf("Wait = %v, want 4s", r.Wait)
	}
	if r.Agent != 8*time.Second {
		t.Errorf("Agent = %v, want 8s residual", r.Agent)
	}
	if r.Agent+r.Tools+r.Wait != r.Real {
		t.Errorf("agent+tools+wait = %v ≠ real %v", r.Agent+r.Tools+r.Wait, r.Real)
	}
	if r.Totals.Turns != 1 || r.Totals.FreshIn != 10 {
		t.Errorf("totals Turns/FreshIn = %d/%d, want 1/10", r.Totals.Turns, r.Totals.FreshIn)
	}
}

func TestReadRidesBySegment_CompleteOnlyTools_SplitAbsent(t *testing.T) {
	// codex app-server shape: single completed stamp per tool id, no start.
	// Tools split must be absent (not zero); Real still present.
	path := writeRideLog(t, "complete-only",
		`{"t":"meta","time":"2026-08-01T10:00:00Z","meta":{"node":"r2","uid":"u2","agent":"codex","created":"2026-08-01T10:00:00Z"}}`,
		`{"t":"user","time":"2026-08-01T10:00:00Z","text":"run"}`,
		`{"t":"tool","time":"2026-08-01T10:00:05Z","tool":{"id":"item-1","title":"shell","status":"completed","kind":"commandExecution"}}`,
		`{"t":"assistant","time":"2026-08-01T10:00:10Z","text":"ok"}`,
		`{"t":"usage","time":"2026-08-01T10:00:10Z","usage":{"inputTokens":5,"outputTokens":2,"turnId":"c1"}}`,
	)
	rides := ReadRidesBySegment(path)
	if len(rides) != 1 {
		t.Fatalf("segments = %d, want 1", len(rides))
	}
	r := rides[0]
	if r.HasSplit {
		t.Error("HasSplit = true, want false (complete-only → tools absent)")
	}
	if r.Real != 10*time.Second {
		t.Errorf("Real = %v, want 10s", r.Real)
	}
	if r.Tools != 0 || r.Agent != 0 || r.Wait != 0 {
		t.Errorf("split parts must be zero when absent, got agent=%v tools=%v wait=%v",
			r.Agent, r.Tools, r.Wait)
	}
}

func TestReadRidesBySegment_HistoricalNoTools_RealOnly(t *testing.T) {
	// Pre-V2-P2 claude mirror shape: user/assistant/usage only, no tools,
	// no attention edges → Real only, no split.
	path := writeRideLog(t, "historical",
		`{"t":"meta","time":"2026-08-01T10:00:00Z","meta":{"node":"r3","uid":"u3","agent":"claude","created":"2026-08-01T10:00:00Z"}}`,
		`{"t":"source","time":"2026-08-01T10:00:00Z","source":{"path":"/tmp/s.jsonl","sessionId":"s1"}}`,
		`{"t":"user","time":"2026-08-01T10:00:01Z","text":"hi"}`,
		`{"t":"assistant","time":"2026-08-01T10:00:05Z","text":"hello"}`,
		`{"t":"usage","time":"2026-08-01T10:00:05Z","usage":{"inputTokens":2,"outputTokens":4,"turnId":"h1"}}`,
	)
	rides := ReadRidesBySegment(path)
	if len(rides) < 1 {
		t.Fatalf("segments = %d, want ≥1", len(rides))
	}
	found := false
	for _, ride := range rides {
		if ride.Totals.Turns > 0 || ride.Real > 0 {
			if ride.HasSplit {
				t.Error("historical segment HasSplit = true, want false (real-only)")
			}
			if ride.Real <= 0 {
				t.Error("Real missing on historical segment")
			}
			found = true
		}
	}
	if !found {
		last := rides[len(rides)-1]
		if last.HasSplit {
			t.Error("last segment HasSplit = true, want false")
		}
	}
}

func TestReadRidesBySegment_PerSegmentSplit(t *testing.T) {
	// Segment 0 (pre-source empty) + source → seg A (tools+wait) + clear → seg B (real only).
	path := writeRideLog(t, "per-seg",
		`{"t":"meta","time":"2026-08-01T10:00:00Z","meta":{"node":"r4","uid":"u4","agent":"pi","created":"2026-08-01T10:00:00Z"}}`,
		`{"t":"source","time":"2026-08-01T10:00:00Z","source":{"path":"","sessionId":"s-a"}}`,
		`{"t":"user","time":"2026-08-01T10:00:00Z","text":"a"}`,
		`{"t":"tool","time":"2026-08-01T10:00:01Z","tool":{"id":"t1","title":"x","status":"pending"}}`,
		`{"t":"tool","time":"2026-08-01T10:00:03Z","tool":{"id":"t1","status":"completed"}}`,
		`{"t":"attention","time":"2026-08-01T10:00:03Z","attention":{"kind":"approval","status":"start"}}`,
		`{"t":"attention","time":"2026-08-01T10:00:04Z","attention":{"kind":"approval","status":"end"}}`,
		`{"t":"assistant","time":"2026-08-01T10:00:05Z","text":"done a"}`,
		`{"t":"usage","time":"2026-08-01T10:00:05Z","usage":{"inputTokens":10,"outputTokens":1,"turnId":"a1","costAmount":0.1}}`,
		`{"t":"source","time":"2026-08-01T10:10:00Z","source":{"path":"","sessionId":"s-b","reason":"clear"}}`,
		`{"t":"user","time":"2026-08-01T10:10:00Z","text":"b"}`,
		`{"t":"assistant","time":"2026-08-01T10:10:02Z","text":"done b"}`,
		`{"t":"usage","time":"2026-08-01T10:10:02Z","usage":{"inputTokens":20,"outputTokens":2,"turnId":"b1","costAmount":0.2}}`,
	)
	rides := ReadRidesBySegment(path)
	if len(rides) < 2 {
		t.Fatalf("segments = %d, want ≥2", len(rides))
	}
	// Find the two content segments by turns.
	var withSplit, realOnly int
	for _, r := range rides {
		if r.HasSplit {
			withSplit++
			if r.Agent+r.Tools+r.Wait != r.Real {
				t.Errorf("split segment partition broken: %v+%v+%v ≠ %v",
					r.Agent, r.Tools, r.Wait, r.Real)
			}
			if r.Tools != 2*time.Second {
				t.Errorf("tools = %v, want 2s", r.Tools)
			}
			if r.Wait != 1*time.Second {
				t.Errorf("wait = %v, want 1s", r.Wait)
			}
		} else if r.Totals.Turns > 0 {
			realOnly++
			if r.Tools != 0 || r.Wait != 0 || r.Agent != 0 {
				t.Errorf("real-only segment leaked split: agent=%v tools=%v wait=%v",
					r.Agent, r.Tools, r.Wait)
			}
		}
	}
	if withSplit != 1 {
		t.Errorf("withSplit segments = %d, want 1", withSplit)
	}
	if realOnly != 1 {
		t.Errorf("realOnly content segments = %d, want 1", realOnly)
	}
}

func TestReadRidesBySegment_OpenToolIntervalOmitted(t *testing.T) {
	// Pending without completed → no tools interval; without wait edges → no split.
	path := writeRideLog(t, "open-tool",
		`{"t":"meta","time":"2026-08-01T10:00:00Z","meta":{"node":"r5","uid":"u5","agent":"grok","created":"2026-08-01T10:00:00Z"}}`,
		`{"t":"tool","time":"2026-08-01T10:00:00Z","tool":{"id":"open","status":"pending"}}`,
		`{"t":"assistant","time":"2026-08-01T10:00:05Z","text":"still working"}`,
	)
	rides := ReadRidesBySegment(path)
	if len(rides) == 0 {
		t.Fatal("ReadRidesBySegment returned no segments")
	}
	if rides[0].HasSplit {
		t.Error("open tool alone must not enable split (no closed interval)")
	}
}

func TestReadRide_WholeJourneyMatchesSegments(t *testing.T) {
	path := writeRideLog(t, "whole",
		`{"t":"meta","time":"2026-08-01T10:00:00Z","meta":{"node":"r6","uid":"u6","agent":"claude","created":"2026-08-01T10:00:00Z"}}`,
		`{"t":"tool","time":"2026-08-01T10:00:00Z","tool":{"id":"x","status":"pending"}}`,
		`{"t":"tool","time":"2026-08-01T10:00:04Z","tool":{"id":"x","status":"completed"}}`,
		`{"t":"usage","time":"2026-08-01T10:00:04Z","usage":{"inputTokens":1,"outputTokens":1,"turnId":"w1"}}`,
	)
	whole := ReadRide(path)
	if !whole.HasSplit {
		t.Fatal("whole HasSplit = false")
	}
	if whole.Tools != 4*time.Second {
		t.Errorf("whole Tools = %v, want 4s", whole.Tools)
	}
	if whole.Totals.Turns != ReadFare(path).Turns {
		t.Errorf("whole turns ≠ ReadFare turns")
	}
}

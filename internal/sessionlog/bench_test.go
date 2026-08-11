package sessionlog

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Synthetic large log for benchmarks. Never load real ~/.scimux content
// (AGENTS.md fixtures-and-privacy). Deterministic, generated in b.TempDir.

func synthLog(b *testing.B, turns int) string {
	b.Helper()
	dir := b.TempDir()
	path := filepath.Join(dir, "bench.jsonl")
	w := &Writer{Path: path}
	if err := w.Append(NewMeta("bench", "claude", "sonnet", "", "/tmp")); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < turns; i++ {
		evs := []Event{
			{T: "user", Time: fmt.Sprintf("2026-07-15T09:%02d:%02dZ", i/60, i%60), Text: fmt.Sprintf("user turn %d with some padding text for realism", i)},
			{T: "assistant", Time: fmt.Sprintf("2026-07-15T09:%02d:%02dZ", i/60, i%60), Text: fmt.Sprintf("assistant reply %d with a longer body so the log is not tiny: lorem ipsum dolor sit amet", i)},
			{T: "usage", Time: fmt.Sprintf("2026-07-15T09:%02d:%02dZ", i/60, i%60), Usage: &UsageEvent{
				Used: 1000 + i, Size: 200_000,
				InputTokens: 10 + i%50, OutputTokens: 20 + i%30,
				TurnID: fmt.Sprintf("t%d", i), CostAmount: 0.001,
			}},
		}
		for _, ev := range evs {
			if err := w.Append(ev); err != nil {
				b.Fatal(err)
			}
		}
		if i > 0 && i%200 == 0 {
			if err := w.Append(NewClearSource(fmt.Sprintf("s%d", i))); err != nil {
				b.Fatal(err)
			}
		}
	}
	return path
}

// BenchmarkColdParse measures a full LogCache load of a large synthetic log.
func BenchmarkColdParse(b *testing.B) {
	path := synthLog(b, 2000) // ~6k records
	st, err := os.Stat(path)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(st.Size()), "bytes")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := &LogCache{}
		_ = c.Segment(path)
		_, _ = c.FareAndRides(path)
		_ = c.AnchoredAssets(path)
		_ = c.Assets(path)
	}
}

// BenchmarkAppendOneRecord measures the steady-state cost of re-reading after
// one appended record (the poll hot path for an actively streaming agent).
func BenchmarkAppendOneRecord(b *testing.B) {
	path := synthLog(b, 2000)
	// Warm once.
	c := &LogCache{}
	_ = c.Segment(path)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		// Append a unique record so size/mtime advance every iteration.
		w := &Writer{Path: path}
		if err := w.Append(Event{
			T: "user", Time: fmt.Sprintf("2026-08-01T00:00:%02dZ", i%60),
			Text: fmt.Sprintf("append-%d", i),
		}); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		_ = c.Segment(path)
		_, _ = c.FareAndRides(path)
		_ = c.AnchoredAssets(path)
		_ = c.Assets(path)
	}
}

package sessionlog

import (
	"os"
	"sync"
	"time"

	"codeberg.org/chrberger/scimux/internal/fare"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// LogCache memoizes all four poll-path products of one session log under a
// single (path, size, mtime) key: the current Segment, whole-journey fare
// totals + per-segment rides, path-anchored assets, and the ID-keyed asset
// index. One os.Stat and (on change) one ReadEvents walk feed every product.
//
// The parsed []Event slice is NOT retained: the four products are what callers
// want, and Segment.Turns already holds the turn text. Keeping the raw events
// would roughly double the per-node footprint for no gain on the poll path.
//
// Stage A walks the whole file on every size/mtime change (invalidate and
// re-derive). Stage B keeps this API and switches the walk to a tail-only
// resume when the file only grows.
type LogCache struct {
	mu    sync.Mutex
	path  string
	size  int64
	mtime time.Time

	seg      Segment
	fare     fare.FareTotals
	rides    []fare.Ride
	anchored []AnchoredAsset
	assets   map[string]AssetEvent

	walks int // test spy: times the file was actually parsed
}

// Segment returns the log's current conversation surface.
func (c *LogCache) Segment(path string) Segment {
	if !c.ensure(path) {
		return Segment{Turns: []transcript.Turn{}}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seg
}

// FareAndRides returns whole-journey totals and per-segment rides.
func (c *LogCache) FareAndRides(path string) (fare.FareTotals, []fare.Ride) {
	if !c.ensure(path) {
		return fare.FareTotals{ReportedCostComplete: true}, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fare, c.rides
}

// AnchoredAssets returns path-sourced assets tagged with their owning turn
// record index (render-time projection).
func (c *LogCache) AnchoredAssets(path string) []AnchoredAsset {
	if !c.ensure(path) {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.anchored
}

// Assets returns the ID-keyed asset index.
func (c *LogCache) Assets(path string) map[string]AssetEvent {
	if !c.ensure(path) {
		return map[string]AssetEvent{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.assets
}

// Walks is the number of times this cache actually parsed the file.
// Used by Stage A tests to prove one walk per (path, size, mtime).
func (c *LogCache) Walks() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.walks
}

// ensure loads or refreshes the four products when the file's identity key
// (path, size, mtime) has changed. Returns false when the path is missing or
// unreadable — callers then return the same defensive zeros as the standalone
// readers, without counting a walk.
func (c *LogCache) ensure(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.path == path && c.size == st.Size() && c.mtime.Equal(st.ModTime()) {
		return true
	}
	// One walk; derive all four; drop the event slice.
	evs := ReadEvents(path)
	whole, rides := foldRidesFromEvents(evs)
	c.seg = segmentOf(evs)
	c.fare = whole.Totals
	c.rides = rides
	c.anchored = anchoredAssetsFromEvents(evs)
	c.assets = assetsFromEvents(evs)
	c.path, c.size, c.mtime = path, st.Size(), st.ModTime()
	c.walks++
	return true
}

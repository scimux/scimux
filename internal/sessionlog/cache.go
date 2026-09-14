package sessionlog

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"codeberg.org/chrberger/scimux/internal/fare"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// LogCache memoizes all four poll-path products of one session log under a
// single (path, size, mtime) key: the current Segment, whole-journey fare
// totals + per-segment rides, path-anchored assets, and the ID-keyed asset
// index.
//
// Session logs are append-only (AGENTS.md). Between calls the cache retains
// accumulator state and a watermark — the byte offset immediately after the
// last newline actually consumed — and on growth seeks to that offset to
// parse only the appended tail. The watermark is deliberately NOT st.Size():
// a torn final line (no trailing '\n') is left unconsumed so the next read
// resumes at a record boundary, never mid-JSON.
//
// The raw []Event slice is NOT retained: products and compact accumulators
// (fare hits, ride timing, live turns) are enough. Keeping every event would
// roughly double the per-node footprint for no poll-path gain.
//
// Fare is not monotonically foldable (a mechanical source seam retroactively
// un-counts no-TurnID usage via lastSegInEpoch). The cache retains the hit
// list and re-runs markCounted + the fold on every refresh — O(usage records),
// not O(bytes).
type LogCache struct {
	mu    sync.Mutex
	path  string
	size  int64
	mtime time.Time

	// Products (read-out snapshots; turns are a copy of the live accumulator).
	seg      Segment
	fare     fare.FareTotals
	rides    []fare.Ride
	anchored []AnchoredAsset
	assets   map[string]AssetEvent

	// Incremental parse state.
	watermark int64 // byte offset after last consumed '\n'
	recCount  int   // successfully parsed records (matches ReadEvents indices)
	haveState bool  // true after at least one successful parse of this path
	// prefixSig is the last up-to-64 bytes immediately before watermark. On
	// growth we re-read those bytes; a mismatch means the prefix was rewritten
	// under our feet (same-size clobber the size/mtime key missed) and we must
	// cold-parse. True appends leave the prefix intact.
	prefixSig []byte

	// Segment raw accumulator (StartTime is pre-R20.9 backdating).
	uid           string
	recSeg        int
	priorTurns    int
	turns         []transcript.Turn
	decisions     []DecisionSurface
	rawStartTime  string
	used, sizeOcc int64
	clearTimes    []string
	stations      map[string]StationLabel

	// Fare intermediate (hits + openReasons); totals re-folded each refresh.
	agent       string
	hits        []usageHit
	openReasons []string
	fareSegIdx  int

	// Ride timing intermediate, parallel to openReasons segments.
	rideSegs []rideSegAccum

	// Asset intermediate.
	lastTurnAnchor int // record index of last non-whitespace turn; -1 = none
	anchAcc        []AnchoredAsset
	assetIdx       map[string]AssetEvent

	walks       int   // test spy: times the file was actually parsed
	bytesParsed int64 // test spy: bytes read from the file on the last parse
}

// rideSegAccum holds the per-segment time material for the v2 ride model.
// Mirrors the local segTime struct in foldRidesFromEvents.
type rideSegAccum struct {
	first, last  time.Time
	hasFirst     bool
	toolStarts   map[string]time.Time
	toolEnds     map[string]time.Time
	hasToolStart bool
	toolIvs      []fare.Interval
	waitOpen     time.Time
	waitOpenSet  bool
	waitIvs      []fare.Interval
	hasWaitEdge  bool
}

func newRideSeg() rideSegAccum {
	return rideSegAccum{
		toolStarts: map[string]time.Time{},
		toolEnds:   map[string]time.Time{},
	}
}

// Segment returns the log's current conversation surface.
func (c *LogCache) Segment(path string) Segment {
	if !c.ensure(path) {
		return Segment{Turns: []transcript.Turn{}}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return copySegmentProv(c.seg)
}

// copySegmentProv returns seg with each turn's Prov copied so a caller cannot
// mutate the cache's retained provenance bytes. Other Segment fields keep
// existing snapshot semantics.
func copySegmentProv(seg Segment) Segment {
	if len(seg.Turns) == 0 {
		return seg
	}
	turns := make([]transcript.Turn, len(seg.Turns))
	for i, t := range seg.Turns {
		t.Prov = copyRaw(t.Prov)
		turns[i] = t
	}
	seg.Turns = turns
	return seg
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
func (c *LogCache) Walks() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.walks
}

// BytesParsed is the number of file bytes scanned on the most recent parse
// (cold or incremental). Used by Stage B tests to prove tail-only reads.
func (c *LogCache) BytesParsed() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytesParsed
}

// ensure loads or refreshes the four products when the file's identity key
// (path, size, mtime) has changed. Returns false when the path is missing or
// unreadable — callers then return the same defensive zeros as the standalone
// readers, without counting a walk.
//
// Caller must NOT hold c.mu. ensure takes c.mu across the whole refresh so
// concurrent poller/handler access serializes (same discipline as the old
// Cache/FareCache: a.mu is only for get-or-create of the pointer).
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

	// Cold when: first load, path changed, file shrank past the watermark, or
	// mtime went backwards (replacement). Growth on the same path resumes
	// from the watermark when the prefix signature still matches.
	cold := !c.haveState || c.path != path || st.Size() < c.watermark ||
		(!c.mtime.IsZero() && st.ModTime().Before(c.mtime))

	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	if !cold && c.watermark > 0 {
		if !c.prefixIntact(f) {
			cold = true
		}
	}

	var startOff int64
	if cold {
		c.resetAccum()
		startOff = 0
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return false
		}
	} else {
		startOff = c.watermark
		if _, err := f.Seek(startOff, io.SeekStart); err != nil {
			// Seek failed — fall back to cold.
			c.resetAccum()
			startOff = 0
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return false
			}
		}
	}

	newOff, nBytes, err := c.consume(f, startOff)
	if err != nil {
		// I/O error mid-parse: drop state so the next call cold-parses.
		c.resetAccum()
		c.haveState = false
		return false
	}
	c.watermark = newOff
	c.bytesParsed = nBytes
	c.capturePrefixSig(f, newOff)
	c.snapshotProducts()
	c.path, c.size, c.mtime = path, st.Size(), st.ModTime()
	c.haveState = true
	c.walks++
	return true
}

const prefixSigLen = 64

// prefixIntact reports whether the bytes immediately before watermark still
// match prefixSig. A same-size rewrite the size/mtime key missed will fail
// this check when the file later grows, forcing a cold re-parse.
func (c *LogCache) prefixIntact(f *os.File) bool {
	if len(c.prefixSig) == 0 {
		return c.watermark == 0
	}
	off := c.watermark - int64(len(c.prefixSig))
	if off < 0 {
		return false
	}
	buf := make([]byte, len(c.prefixSig))
	n, err := f.ReadAt(buf, off)
	if err != nil || n != len(c.prefixSig) {
		return false
	}
	for i := range buf {
		if buf[i] != c.prefixSig[i] {
			return false
		}
	}
	return true
}

// capturePrefixSig stores the last up-to-prefixSigLen bytes before off.
func (c *LogCache) capturePrefixSig(f *os.File, off int64) {
	if off <= 0 {
		c.prefixSig = nil
		return
	}
	n := int64(prefixSigLen)
	if n > off {
		n = off
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, off-n); err != nil {
		// Signature unavailable — next growth will cold-parse (len 0 + watermark>0).
		c.prefixSig = nil
		return
	}
	c.prefixSig = buf
}

// resetAccum clears all incremental state for a cold re-parse.
func (c *LogCache) resetAccum() {
	c.watermark = 0
	c.recCount = 0
	c.prefixSig = nil
	c.uid = ""
	c.recSeg = 0
	c.priorTurns = 0
	c.turns = c.turns[:0]
	c.decisions = c.decisions[:0]
	c.rawStartTime = ""
	c.used, c.sizeOcc = 0, 0
	c.clearTimes = c.clearTimes[:0]
	c.stations = map[string]StationLabel{}
	c.agent = ""
	c.hits = c.hits[:0]
	c.openReasons = []string{""} // segment 0 (pre-first-source), matches collectFareHits
	c.fareSegIdx = 0
	c.rideSegs = []rideSegAccum{newRideSeg()}
	c.lastTurnAnchor = -1
	c.anchAcc = c.anchAcc[:0]
	c.assetIdx = map[string]AssetEvent{}
}

// consume reads newline-terminated records from r starting at baseOff,
// feeding each successfully parsed event into the accumulators. The returned
// watermark is baseOff + bytes through the last complete '\n'; a final line
// without a trailing newline is left unconsumed (torn-tail safety) and re-read
// next time. It walks the file with the same forEachRecord as ReadEvents and
// the search readers on purpose: this cache is the chat read path, so a record
// one of them skips and this one ingests is two readers showing the user two
// different conversations -- and record ordinals that no longer line up.
func (c *LogCache) consume(r io.Reader, baseOff int64) (newOff int64, nBytes int64, err error) {
	n, err := forEachRecord(r, func(rec []byte, _ int64) bool {
		var ev Event
		if json.Unmarshal(rec, &ev) == nil {
			c.ingest(ev, c.recCount)
			c.recCount++
		}
		return true
	})
	return baseOff + n, n, err
}

// ingest updates every accumulator for one successfully parsed event at
// record index i (the durable ReadEvents index).
func (c *LogCache) ingest(ev Event, i int) {
	// --- segment (raw; backdating applied at snapshot) ---
	if ev.T == "meta" && ev.Meta != nil {
		c.uid = ev.Meta.UID
		if ev.Meta.Agent != "" {
			c.agent = ev.Meta.Agent
		}
	}
	if c.rawStartTime == "" && ev.Time != "" {
		c.rawStartTime = ev.Time
	}
	switch ev.T {
	case "source":
		c.priorTurns += len(c.turns)
		c.turns = c.turns[:0]
		c.decisions = c.decisions[:0]
		c.rawStartTime = ev.Time
		c.used, c.sizeOcc = 0, 0
		if ev.Source != nil && ev.Source.Reason == "clear" {
			c.clearTimes = append(c.clearTimes, ev.Time)
		}
		c.recSeg++
	case "user", "assistant":
		if strings.TrimSpace(ev.Text) != "" {
			c.turns = append(c.turns, chatTurn(ev, c.uid, c.agent, c.recSeg, i))
		}
	case "usage":
		if ev.Usage != nil {
			if ev.Usage.Used > 0 {
				c.used = int64(ev.Usage.Used)
			}
			if ev.Usage.Size > 0 {
				c.sizeOcc = int64(ev.Usage.Size)
			}
		}
	case "station":
		if ev.Station != nil && ev.Station.Seam != "" {
			if c.stations == nil {
				c.stations = map[string]StationLabel{}
			}
			c.stations[ev.Station.Seam] = StationLabel{Title: ev.Station.Title, Desc: ev.Station.Desc}
		}
	case "decision":
		if ev.Decision != nil {
			c.decisions = append(c.decisions, DecisionSurface{
				Record: i, Time: ev.Time, Decision: *ev.Decision,
			})
		}
	}

	// --- fare hits ---
	switch ev.T {
	case "meta":
		if ev.Meta != nil && ev.Meta.Agent != "" {
			c.agent = ev.Meta.Agent
		}
	case "source":
		reason := ""
		if ev.Source != nil {
			reason = ev.Source.Reason
		}
		c.openReasons = append(c.openReasons, reason)
		c.fareSegIdx = len(c.openReasons) - 1
		// Grow ride segs in lockstep with openReasons.
		c.rideSegs = append(c.rideSegs, newRideSeg())
	case "usage":
		if ev.Usage != nil && hasFareData(ev.Usage) {
			c.hits = append(c.hits, usageHit{u: *ev.Usage, segIdx: c.fareSegIdx})
		}
	}

	// --- ride timing (current fareSegIdx) ---
	if c.fareSegIdx >= 0 && c.fareSegIdx < len(c.rideSegs) {
		c.ingestRide(ev, &c.rideSegs[c.fareSegIdx])
	}

	// --- assets ---
	switch ev.T {
	case "user", "assistant":
		if strings.TrimSpace(ev.Text) != "" {
			c.lastTurnAnchor = i
		}
	case "asset":
		if ev.Asset != nil && ev.Asset.ID != "" {
			if _, exists := c.assetIdx[ev.Asset.ID]; !exists {
				if c.assetIdx == nil {
					c.assetIdx = map[string]AssetEvent{}
				}
				c.assetIdx[ev.Asset.ID] = *ev.Asset
			}
			if ev.Asset.SourcePath != "" {
				c.anchAcc = append(c.anchAcc, AnchoredAsset{Anchor: c.lastTurnAnchor, Asset: *ev.Asset})
			}
		}
	}
}

func (c *LogCache) ingestRide(ev Event, s *rideSegAccum) {
	if countsTowardReal(ev.T) {
		if t, ok := parseEventTime(ev.Time); ok {
			if !s.hasFirst || t.Before(s.first) {
				s.first, s.hasFirst = t, true
			}
			if t.After(s.last) {
				s.last = t
			}
		}
	}
	switch ev.T {
	case "tool":
		if ev.Tool == nil || ev.Tool.ID == "" {
			return
		}
		if strings.EqualFold(ev.Tool.Kind, "approval") {
			return
		}
		t, ok := parseEventTime(ev.Time)
		if !ok {
			return
		}
		id := ev.Tool.ID
		status := strings.ToLower(ev.Tool.Status)
		if isTerminalToolStatus(status) {
			if _, seen := s.toolEnds[id]; !seen {
				s.toolEnds[id] = t
			}
		} else {
			s.hasToolStart = true
			if _, seen := s.toolStarts[id]; !seen {
				s.toolStarts[id] = t
			}
		}
	case "attention":
		if ev.Attention == nil {
			return
		}
		s.hasWaitEdge = true
		t, ok := parseEventTime(ev.Time)
		if !ok {
			return
		}
		status := strings.ToLower(ev.Attention.Status)
		switch status {
		case "start":
			if !s.waitOpenSet {
				s.waitOpen, s.waitOpenSet = t, true
			}
		case "end":
			if s.waitOpenSet {
				s.waitIvs = append(s.waitIvs, fare.Interval{Start: s.waitOpen, End: t})
				s.waitOpenSet = false
			}
		}
	}
}

// snapshotProducts derives the four read-out products from accumulators.
// Segment backdating (R20.9) is applied to a copy; live turns are copied so a
// later seam's turns[:0] cannot clobber a caller's held slice.
func (c *LogCache) snapshotProducts() {
	// Segment
	seg := Segment{
		PriorTurns: c.priorTurns,
		StartTime:  c.rawStartTime,
		Used:       c.used,
		Size:       c.sizeOcc,
		Turns:      append([]transcript.Turn(nil), c.turns...),
		Decisions:  append([]DecisionSurface(nil), c.decisions...),
		ClearTimes: append([]string(nil), c.clearTimes...),
		Stations:   make(map[string]StationLabel, len(c.stations)),
	}
	if seg.Turns == nil {
		seg.Turns = []transcript.Turn{}
	}
	if seg.Decisions == nil {
		seg.Decisions = []DecisionSurface{}
	}
	for k, v := range c.stations {
		seg.Stations[k] = v
	}
	if seg.Stations == nil {
		seg.Stations = map[string]StationLabel{}
	}
	// R20.9 read-out only — never mutate rawStartTime.
	if len(seg.Turns) > 0 {
		if t0 := seg.Turns[0].Time; t0 != "" && earlier(t0, seg.StartTime) {
			seg.StartTime = t0
		}
	}
	c.seg = seg

	// Fare: re-run markCounted over retained hits (handles retroactive un-count).
	c.fare, _ = foldFareFromHits(c.agent, c.hits, c.openReasons)

	// Rides: rebuild tool intervals from starts/ends, then BuildRide per seg.
	c.rides = buildRidesFromAccum(c.agent, c.hits, c.openReasons, c.rideSegs)

	// Assets
	c.anchored = append([]AnchoredAsset(nil), c.anchAcc...)
	c.assets = make(map[string]AssetEvent, len(c.assetIdx))
	for k, v := range c.assetIdx {
		c.assets[k] = v
	}
}

// foldFareFromHits is markCounted + addCanonical over a retained hit list.
// Same answer as foldFareFromEvents, without re-walking the log.
func foldFareFromHits(agent string, hits []usageHit, openReasons []string) (fare.FareTotals, []fare.FareTotals) {
	nSeg := len(openReasons)
	if nSeg == 0 {
		z := fare.FareTotals{ReportedCostComplete: true}
		return z, nil
	}
	count := markCounted(hits, openReasons)
	segs := make([]fare.FareTotals, nSeg)
	for i := range segs {
		segs[i].ReportedCostComplete = true
	}
	whole := fare.FareTotals{ReportedCostComplete: true}
	for i, h := range hits {
		if !count[i] {
			continue
		}
		raw := rawFromUsage(h.u)
		can := fare.Normalize(agent, raw)
		addCanonical(&whole, can, h.u)
		addCanonical(&segs[h.segIdx], can, h.u)
	}
	return whole, segs
}

// buildRidesFromAccum produces per-segment rides from fare hits + ride timing
// accumulators, matching foldRidesFromEvents output.
func buildRidesFromAccum(agent string, hits []usageHit, openReasons []string, st []rideSegAccum) []fare.Ride {
	totals, segs := foldFareFromHits(agent, hits, openReasons)
	nSeg := len(segs)
	if nSeg == 0 {
		return nil
	}
	// Close tool intervals per id (same as foldRidesFromEvents).
	// Work on copies of interval slices so repeated snapshots don't append
	// duplicate closed intervals onto retained state.
	type closed struct {
		toolIvs      []fare.Interval
		waitIvs      []fare.Interval
		hasFirst     bool
		first, last  time.Time
		hasWaitEdge  bool
		hasToolStart bool
	}
	cl := make([]closed, nSeg)
	for i := 0; i < nSeg && i < len(st); i++ {
		s := st[i]
		var toolIvs []fare.Interval
		for id, start := range s.toolStarts {
			end, ok := s.toolEnds[id]
			if !ok || !end.After(start) {
				continue
			}
			toolIvs = append(toolIvs, fare.Interval{Start: start, End: end})
		}
		cl[i] = closed{
			toolIvs:      toolIvs,
			waitIvs:      append([]fare.Interval(nil), s.waitIvs...),
			hasFirst:     s.hasFirst,
			first:        s.first,
			last:         s.last,
			hasWaitEdge:  s.hasWaitEdge,
			hasToolStart: s.hasToolStart,
		}
		_ = s.hasToolStart
	}

	rides := make([]fare.Ride, nSeg)
	for i := 0; i < nSeg; i++ {
		s := cl[i]
		var real time.Duration
		if s.hasFirst && s.last.After(s.first) {
			real = s.last.Sub(s.first)
		}
		hasToolTiming := len(s.toolIvs) > 0
		hasWaitTiming := s.hasWaitEdge
		toolIvs, waitIvs := s.toolIvs, s.waitIvs
		if s.hasFirst && s.last.After(s.first) {
			toolIvs = clipIntervals(toolIvs, s.first, s.last)
			waitIvs = clipIntervals(waitIvs, s.first, s.last)
		}
		rides[i] = fare.BuildRide(segs[i], real, toolIvs, waitIvs, hasToolTiming, hasWaitTiming)
	}
	_ = totals // whole-journey ride is not stored on LogCache products (only segs + totals.Totals)
	return rides
}

package sessionlog

import (
	"encoding/json"
	"os"
	"sort"
	"strings"

	"github.com/scimux/scimux/internal/transcript"
)

// PageMode selects the turn range returned by LogCache.Page.
type PageMode string

const (
	PageBefore PageMode = "before"
	PageAfter  PageMode = "after"
	PageAround PageMode = "around"
)

// WindowHead identifies the first agent turn in a prompt cycle cut by a page.
type WindowHead struct {
	UID     string `json:"uid"`
	Segment int    `json:"segment"`
	Record  int    `json:"record"`
}

// HistorySlice is one segment's selected turn range.
type HistorySlice struct {
	HistorySegment
	SegmentOffset int            `json:"segment_offset"`
	SegmentTurns  int            `json:"segment_turns"`
	WindowHead    *WindowHead    `json:"window_head,omitempty"`
	LeaseBase     map[string]int `json:"lease_base,omitempty"`
}

// PageResult contains selected history surfaces and whole-log range counts.
type PageResult struct {
	Segments    []HistorySlice   `json:"segments"`
	Empty       []HistorySegment `json:"empty_segments"`
	BeforeCount int              `json:"before_count"`
	AfterCount  int              `json:"after_count"`
	Current     int              `json:"current_segment"`
	Found       bool             `json:"-"`
}

// Page returns a bounded turn range from the cached log.
func (c *LogCache) Page(path string, mode PageMode, at, limit int) (PageResult, bool) {
	if !c.ensure(path) {
		return PageResult{Segments: []HistorySlice{}, Empty: []HistorySegment{}}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	result := PageResult{
		Segments: []HistorySlice{}, Empty: []HistorySegment{}, Current: c.recSeg,
	}
	if limit < 1 || limit > 200 || (mode != PageBefore && mode != PageAfter && mode != PageAround) {
		return result, false
	}
	all := make([]pageTurnIndex, 0)
	for i := range c.pageSegments {
		all = append(all, c.pageSegments[i].turns...)
	}
	start, end := 0, 0 // half-open range in all
	switch mode {
	case PageBefore:
		end = firstTurnAtOrAfter(all, at)
		start = end - limit
		if start < 0 {
			start = 0
		}
	case PageAfter:
		start = firstTurnAfter(all, at)
		end = start + limit
		if end > len(all) {
			end = len(all)
		}
	case PageAround:
		target := turnByRecord(all, at)
		if target < 0 {
			return result, true
		}
		result.Found = true
		start = target - limit/2
		if start < 0 {
			start = 0
		}
		end = start + limit
		if end > len(all) {
			end = len(all)
			start = end - limit
			if start < 0 {
				start = 0
			}
		}
	}
	result.BeforeCount, result.AfterCount = start, len(all)-end
	if start == end {
		return result, true
	}

	open := os.Open
	if c.pageOpen != nil {
		open = c.pageOpen
	}
	f, err := open(path)
	if err != nil {
		return result, false
	}
	defer f.Close()
	selected := all[start:end]
	rangeLow, rangeHigh := selected[0].record, selected[len(selected)-1].record
	if mode == PageBefore {
		rangeHigh = at - 1
	}
	if mode == PageAfter {
		rangeLow = at + 1
		if end == len(all) {
			rangeHigh = c.recCount - 1
		}
	}
	represented := map[int]bool{}
	for pos := 0; pos < len(selected); {
		segIndex := selected[pos].segment
		segStart := pos
		for pos < len(selected) && selected[pos].segment == segIndex {
			pos++
		}
		seg := c.pageSegments[segIndex]
		represented[segIndex] = true
		offset := selected[segStart].offset
		slice := HistorySlice{
			HistorySegment: HistorySegment{
				UID: seg.uid, Index: seg.index, Start: seg.start, Seam: seg.seam,
				Reason: seg.reason, Turns: []transcript.Turn{}, Decisions: []DecisionSurface{},
			},
			SegmentOffset: offset, SegmentTurns: len(seg.turns),
		}
		if seg.firstTurnTime != "" && earlier(seg.firstTurnTime, slice.Start) {
			slice.Start = seg.firstTurnTime
		}
		for i := segStart; i < pos; i++ {
			ev, readErr := c.readIndexedEvent(f, selected[i].ref)
			if readErr != nil {
				return result, false
			}
			if (ev.T != "user" && ev.T != "assistant") || strings.TrimSpace(ev.Text) == "" {
				return result, false
			}
			slice.Turns = append(slice.Turns, chatTurn(ev, selected[i].uid, selected[i].agent, selected[i].segment, selected[i].record))
		}
		for _, d := range seg.decisions {
			low, high := pageRecordBounds(c.pageSegments, seg, rangeLow, rangeHigh)
			if offset == 0 {
				low = seg.seamRecord + 1
				if seg.seamRecord < 0 {
					low = 0
				}
			}
			if d.record < low || d.record > high {
				continue
			}
			ev, readErr := c.readIndexedEvent(f, d.ref)
			if readErr != nil || ev.Decision == nil {
				return result, false
			}
			slice.Decisions = append(slice.Decisions, DecisionSurface{Record: d.record, Time: ev.Time, Decision: *ev.Decision})
		}
		applyPageWindow(&slice, seg, offset)
		result.Segments = append(result.Segments, slice)
	}
	for _, seg := range c.pageSegments {
		if represented[seg.index] {
			continue
		}
		low, high := pageRecordBounds(c.pageSegments, seg, rangeLow, rangeHigh)
		decisions := []DecisionSurface{}
		for _, d := range seg.decisions {
			if d.record < low || d.record > high {
				continue
			}
			ev, readErr := c.readIndexedEvent(f, d.ref)
			if readErr != nil || ev.Decision == nil {
				return result, false
			}
			decisions = append(decisions, DecisionSurface{Record: d.record, Time: ev.Time, Decision: *ev.Decision})
		}
		if len(decisions) == 0 {
			continue
		}
		offset := 0
		for _, turn := range seg.turns {
			if turn.record < low {
				offset++
			}
		}
		slice := HistorySlice{
			HistorySegment: HistorySegment{
				UID: seg.uid, Index: seg.index, Start: seg.start, Seam: seg.seam,
				Reason: seg.reason, Turns: []transcript.Turn{}, Decisions: decisions,
			},
			SegmentOffset: offset, SegmentTurns: len(seg.turns),
		}
		if len(seg.turns) > 0 && seg.firstTurnTime != "" && earlier(seg.firstTurnTime, slice.Start) {
			slice.Start = seg.firstTurnTime
		}
		result.Segments = append(result.Segments, slice)
	}
	sort.Slice(result.Segments, func(i, j int) bool { return result.Segments[i].Index < result.Segments[j].Index })
	firstRecord, lastRecord := rangeLow, rangeHigh
	for _, seg := range c.pageSegments {
		initialEmpty := seg.seamRecord < 0 && firstRecord <= 0
		if len(seg.turns) != 0 || (!initialEmpty && seg.seamRecord < firstRecord) || seg.seamRecord > lastRecord {
			continue
		}
		result.Empty = append(result.Empty, HistorySegment{
			UID: seg.uid, Index: seg.index, Start: seg.start, Seam: seg.seam,
			Reason: seg.reason, Turns: []transcript.Turn{}, Decisions: []DecisionSurface{},
		})
	}
	return result, true
}

func pageRecordBounds(segs []pageSegmentIndex, seg pageSegmentIndex, low, high int) (int, int) {
	segmentLow := 0
	if seg.seamRecord >= 0 {
		segmentLow = seg.seamRecord + 1
	}
	segmentHigh := high
	if nextStart := nextSegmentStartRecord(segs, seg.index); nextStart >= 0 && segmentHigh >= nextStart {
		segmentHigh = nextStart - 1
	}
	if low < segmentLow {
		low = segmentLow
	}
	return low, segmentHigh
}

func nextSegmentStartRecord(segs []pageSegmentIndex, index int) int {
	if index+1 < len(segs) {
		return segs[index+1].seamRecord
	}
	return -1
}

func firstTurnAtOrAfter(turns []pageTurnIndex, record int) int {
	for i, turn := range turns {
		if turn.record >= record {
			return i
		}
	}
	return len(turns)
}

func firstTurnAfter(turns []pageTurnIndex, record int) int {
	for i, turn := range turns {
		if turn.record > record {
			return i
		}
	}
	return len(turns)
}

func turnByRecord(turns []pageTurnIndex, record int) int {
	for i, turn := range turns {
		if turn.record == record {
			return i
		}
	}
	return -1
}

func (c *LogCache) readIndexedEvent(f *os.File, ref recordRef) (Event, error) {
	c.pageReads++
	buf := make([]byte, ref.len)
	start := ref.end - int64(ref.len) - 1 // exclude trailing newline
	_, err := f.ReadAt(buf, start)
	if err != nil {
		return Event{}, err
	}
	buf = bytesTrimCR(buf)
	var ev Event
	err = json.Unmarshal(buf, &ev)
	return ev, err
}

func bytesTrimCR(buf []byte) []byte {
	if len(buf) > 0 && buf[len(buf)-1] == '\r' {
		return buf[:len(buf)-1]
	}
	return buf
}

func applyPageWindow(slice *HistorySlice, seg pageSegmentIndex, offset int) {
	if offset > 0 {
		userStart := 0
		for i := 0; i < offset; i++ {
			if seg.turns[i].role == "user" {
				userStart = i
			}
		}
		for i := userStart; i < offset; i++ {
			if seg.turns[i].role == "assistant" {
				head := seg.turns[i]
				slice.WindowHead = &WindowHead{UID: head.uid, Segment: head.segment, Record: head.record}
				break
			}
		}
	}
	if offset == 0 {
		return
	}
	kept := make(map[string]bool)
	first := slice.Turns[0].Record
	for _, d := range slice.Decisions {
		if d.Decision.LeaseID != "" && d.Record >= first {
			kept[d.Decision.LeaseID] = true
		}
	}
	for _, d := range seg.decisions {
		if d.record < first && kept[d.leaseID] && d.leaseID != "" {
			if slice.LeaseBase == nil {
				slice.LeaseBase = map[string]int{}
			}
			slice.LeaseBase[d.leaseID]++
		}
	}
}

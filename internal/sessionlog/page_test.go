package sessionlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/transcript"
)

func appendRawEvent(t *testing.T, b *strings.Builder, ev Event, ending string) {
	t.Helper()
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	b.Write(raw)
	b.WriteString(ending)
}

func pagingFixture(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	appendRawEvent(t, &b, NewMeta("node", "codex", "", "", "/tmp"), "\r\n")
	b.WriteString("   \n")                                       // ignored physical line, no record ordinal
	b.WriteString(strings.Repeat("x", maxRecordBytes+10) + "\n") // oversized physical line, no record ordinal
	appendRawEvent(t, &b, Event{T: "user", Time: "2025-01-01T00:00:00Z", Text: "first"}, "\n")
	b.WriteString("{not json}\n") // ignored parsed record ordinal
	appendRawEvent(t, &b, Event{T: "assistant", Time: "2025-01-01T00:00:01Z", Text: "answer", Prov: json.RawMessage(`{"p":1}`)}, "\r\n")
	appendRawEvent(t, &b, NewDecision(DecisionEvent{LeaseID: "lease-a", RequestID: "d1"}), "\n")
	for i := 0; i < 5; i++ {
		role := "assistant"
		if i == 0 {
			role = "user"
		}
		appendRawEvent(t, &b, Event{T: role, Time: "2025-01-01T00:01:00Z", Text: "cycle"}, "\n")
		if i == 2 {
			appendRawEvent(t, &b, NewDecision(DecisionEvent{LeaseID: "lease-a", RequestID: "d2"}), "\n")
		}
	}
	appendRawEvent(t, &b, Event{T: "assistant", Time: "2025-01-01T00:02:00Z", Text: "   \t"}, "\n")
	appendRawEvent(t, &b, NewDecision(DecisionEvent{LeaseID: "lease-a", RequestID: "d5"}), "\n")
	appendRawEvent(t, &b, NewClearSource("s2"), "\n")                    // intentionally empty clear surface
	appendRawEvent(t, &b, NewSource("/mirror/rebind.jsonl", "s3"), "\n") // mechanical seam after empty surface
	appendRawEvent(t, &b, NewDecision(DecisionEvent{LeaseID: "lease-b", RequestID: "d3"}), "\n")
	appendRawEvent(t, &b, Event{T: "user", Time: "2024-01-01T00:00:00Z", Text: "backdated"}, "\n")
	appendRawEvent(t, &b, Event{T: "assistant", Time: "2024-01-01T00:00:01Z", Text: "new answer"}, "\n")
	appendRawEvent(t, &b, NewSource("/mirror/rebind-again.jsonl", "s4"), "\n")
	appendRawEvent(t, &b, NewDecision(DecisionEvent{LeaseID: "lease-c", RequestID: "d4"}), "\n")
	appendRawEvent(t, &b, Event{T: "user", Time: "2025-02-01T00:00:00Z", Text: "last"}, "\n")
	appendRawEvent(t, &b, NewClearSource("s5"), "\n") // trailing empty surface
	path := filepath.Join(t.TempDir(), "page.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPageBeforeMatchesHistoryOracle(t *testing.T) {
	path := pagingFixture(t)
	want := ReadHistory(path)
	c := &LogCache{}
	all, ok := c.Page(path, PageBefore, 1<<30, 200)
	if !ok {
		t.Fatal("page not found")
	}
	if len(all.Segments) != len(want) {
		t.Fatalf("page surfaces=%d, want ReadHistory surfaces=%d", len(all.Segments), len(want))
	}
	for i, got := range all.Segments {
		if got.SegmentOffset != 0 || got.SegmentTurns != len(want[i].Turns) {
			t.Errorf("slice %d offset/total=%d/%d", i, got.SegmentOffset, got.SegmentTurns)
		}
		if !reflect.DeepEqual(got.HistorySegment, want[i]) {
			t.Errorf("slice %d mismatch:\n got=%+v\nwant=%+v", i, got.HistorySegment, want[i])
		}
	}
	if all.BeforeCount != 0 || all.AfterCount != 0 {
		t.Errorf("full range counts=%d/%d, want zero/zero", all.BeforeCount, all.AfterCount)
	}

	// Paging from the end backwards must preserve exact durable turn identity,
	// provenance, decisions, and segment headers.
	at := 1 << 30
	var reversePages []PageResult
	walks := c.Walks()
	for {
		page, found := c.Page(path, PageBefore, at, 3)
		if !found || len(page.Segments) == 0 {
			break
		}
		reversePages = append(reversePages, page)
		at = page.Segments[0].Turns[0].Record
	}
	if c.Walks() != walks {
		t.Fatalf("page reads reparsed log: walks %d -> %d", walks, c.Walks())
	}
	if len(reversePages) == 0 {
		t.Fatal("backward paging returned no slices")
	}
	merged := mergePageSegments(reversePages)
	if !reflect.DeepEqual(merged, want) {
		t.Fatalf("merged backward pages differ from ReadHistory:\n got=%+v\nwant=%+v", merged, want)
	}
	var pageEmpty []HistorySegment
	for _, p := range reversePages {
		pageEmpty = append(pageEmpty, p.Empty...)
	}
	if !reflect.DeepEqual(uniqueHistorySegments(pageEmpty), emptyTurnSurfaces(ReadHistoryIncludingEmpty(path))) {
		t.Fatalf("empty surfaces across pages differ: got=%+v", pageEmpty)
	}

	// Forward paging is the same oracle in chronological order.
	at = -1
	var forwardPages []PageResult
	for {
		page, found := c.Page(path, PageAfter, at, 3)
		if !found || len(page.Segments) == 0 {
			break
		}
		forwardPages = append(forwardPages, page)
		last := page.Segments[len(page.Segments)-1].Turns
		at = last[len(last)-1].Record
	}
	if got := mergePageSegments(forwardPages); !reflect.DeepEqual(got, want) {
		t.Fatalf("merged forward pages differ from ReadHistory:\n got=%+v\nwant=%+v", got, want)
	}
}

func TestPageModesAndBoundedRead(t *testing.T) {
	path := pagingFixture(t)
	c := &LogCache{}
	all, ok := c.Page(path, PageBefore, 1<<30, 200)
	if !ok || len(all.Segments) < 2 {
		t.Fatalf("baseline page unavailable: %+v", all)
	}
	target := all.Segments[1].Turns[1]
	around, found := c.Page(path, PageAround, target.Record, 4)
	if !found || !around.Found {
		t.Fatal("around did not find readable target")
	}
	if around.BeforeCount+around.AfterCount+countPageTurns(around) != countPageTurns(all) {
		t.Errorf("counts do not cover log: before=%d after=%d selected=%d all=%d", around.BeforeCount, around.AfterCount, countPageTurns(around), countPageTurns(all))
	}
	missing, ok := c.Page(path, PageAround, target.Record+1, 4)
	if !ok || missing.Found {
		t.Error("around found a non-turn record")
	}
	page, found := c.Page(path, PageAfter, target.Record, 2)
	if !found || countPageTurns(page) > 2 {
		t.Errorf("after page=%+v, found=%v", page, found)
	}

	// The around range is the target plus the corresponding before/after
	// neighbours, with absolute segment offsets retained on each slice.
	target = all.Segments[0].Turns[4]
	before, _ := c.Page(path, PageBefore, target.Record, 2)
	after, _ := c.Page(path, PageAfter, target.Record, 1)
	around, _ = c.Page(path, PageAround, target.Record, 4)
	var composed []int
	for _, p := range []PageResult{before} {
		composed = appendTurnRecords(composed, p)
	}
	composed = append(composed, target.Record)
	composed = appendTurnRecords(composed, after)
	var gotRecords []int
	for _, s := range around.Segments {
		for _, turn := range s.Turns {
			gotRecords = append(gotRecords, turn.Record)
		}
	}
	if !reflect.DeepEqual(gotRecords, composed) {
		t.Errorf("around records=%v, before+target+after=%v", gotRecords, composed)
	}
	if len(around.Segments) == 0 || around.Segments[0].SegmentOffset != 2 || around.Segments[0].SegmentTurns != len(all.Segments[0].Turns) {
		t.Errorf("around slice offset/total incorrect: %+v", around.Segments)
	}
	firstAround, _ := c.Page(path, PageAround, all.Segments[0].Turns[0].Record, 4)
	if countPageTurns(firstAround) != 4 || firstAround.BeforeCount != 0 {
		t.Errorf("around at beginning did not fill available range: %+v", firstAround)
	}
	lastTurn := all.Segments[len(all.Segments)-1].Turns[len(all.Segments[len(all.Segments)-1].Turns)-1]
	lastAround, _ := c.Page(path, PageAround, lastTurn.Record, 4)
	if countPageTurns(lastAround) != 4 || lastAround.AfterCount != 0 {
		t.Errorf("around at end did not fill available range: %+v", lastAround)
	}
	oneTurn := writeLog(t, []Event{{T: "assistant", Time: "2025-01-01T00:00:00Z", Text: "only"}})
	oneCache := &LogCache{}
	oneAll, _ := oneCache.Page(oneTurn, PageBefore, 100, 5)
	oneAround, _ := oneCache.Page(oneTurn, PageAround, oneAll.Segments[0].Turns[0].Record, 5)
	if countPageTurns(oneAround) != 1 || oneAround.BeforeCount != 0 || oneAround.AfterCount != 0 {
		t.Errorf("around one-turn edge incorrect: %+v", oneAround)
	}
	cycleAssistant := all.Segments[0].Turns[3]
	window, ok := c.Page(path, PageAfter, cycleAssistant.Record, 3)
	if !ok || len(window.Segments) == 0 {
		t.Fatal("window slice unavailable")
	}
	w := window.Segments[0]
	if w.SegmentOffset != 4 || w.WindowHead == nil || w.WindowHead.Record != cycleAssistant.Record {
		t.Errorf("window metadata offset/head=%d/%+v", w.SegmentOffset, w.WindowHead)
	}
	if w.LeaseBase["lease-a"] != 1 || len(w.Decisions) != 1 || w.Decisions[0].Decision.RequestID != "d2" {
		t.Errorf("window decision lease metadata=%+v lease_base=%v", w.Decisions, w.LeaseBase)
	}
	// A decision in the physical range after the last selected turn of one
	// segment is returned as an audit-only slice when the page starts in the
	// following segment.
	audit, ok := c.Page(path, PageAfter, all.Segments[0].Turns[len(all.Segments[0].Turns)-1].Record, 1)
	if !ok || len(audit.Segments) < 2 || len(audit.Segments[0].Turns) != 0 || len(audit.Segments[0].Decisions) != 1 {
		t.Errorf("intervening decision surface missing: %+v", audit)
	}
}

func appendTurnRecords(dst []int, page PageResult) []int {
	for _, s := range page.Segments {
		for _, turn := range s.Turns {
			dst = append(dst, turn.Record)
		}
	}
	return dst
}

func TestPageReadsOnlySelectedRecordsFromLargeLog(t *testing.T) {
	var b strings.Builder
	appendRawEvent(t, &b, NewMeta("large", "codex", "", "", "/tmp"), "\n")
	for i := 0; i < 20_000; i++ {
		if i == 19_990 {
			appendRawEvent(t, &b, NewDecision(DecisionEvent{LeaseID: "last", RequestID: "in-range"}), "\n")
		}
		appendRawEvent(t, &b, Event{T: "assistant", Time: "2025-01-01T00:00:00Z", Text: "synthetic turn"}, "\n")
	}
	path := filepath.Join(t.TempDir(), "large.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &LogCache{}
	all, ok := c.Page(path, PageBefore, 1<<30, 31)
	if !ok || countPageTurns(all) != 31 {
		t.Fatalf("large page returned %d turns, ok=%v", countPageTurns(all), ok)
	}
	if got, want := c.pageReads, countPageTurns(all)+1; got != want {
		t.Errorf("ReadAt records=%d, want selected turns + in-range decision = %d", got, want)
	}
	walks := c.Walks()
	if _, ok := c.Page(path, PageBefore, 1<<30, 31); !ok || c.Walks() != walks {
		t.Errorf("repeat page changed parse walks: %d -> %d", walks, c.Walks())
	}
}

func TestPageReadsFirstRecordFromZeroOffset(t *testing.T) {
	path := writeLog(t, []Event{{T: "user", Time: "2025-01-01T00:00:00Z", Text: "first record"}})
	c := &LogCache{}
	page, ok := c.Page(path, PageBefore, 100, 1)
	if !ok || countPageTurns(page) != 1 || page.Segments[0].Turns[0].Text != "first record" {
		t.Fatalf("page first record=%+v ok=%v", page, ok)
	}
}

func TestPageIndexAppendAndColdReset(t *testing.T) {
	path := richLog(t)
	c := &LogCache{}
	if _, ok := c.Page(path, PageBefore, 1<<30, 200); !ok {
		t.Fatal("initial page failed")
	}
	initialBytes, initialWalks := c.BytesParsed(), c.Walks()
	if err := (&Writer{Path: path}).Append(Event{T: "user", Time: "2026-08-01T00:00:00Z", Text: "appended"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Page(path, PageBefore, 1<<30, 200); !ok {
		t.Fatal("page after append failed")
	}
	if c.Walks() != initialWalks+1 || c.BytesParsed() >= initialBytes {
		t.Errorf("append did not extend incrementally: walks %d -> %d parsed=%d initial=%d", initialWalks, c.Walks(), c.BytesParsed(), initialBytes)
	}

	// Shrinking the same path forces a cold parse and discards stale entries.
	short := []byte("{\"t\":\"user\",\"time\":\"2025-01-01T00:00:00Z\",\"text\":\"fresh\"}\n")
	if err := os.WriteFile(path, short, 0o600); err != nil {
		t.Fatal(err)
	}
	page, ok := c.Page(path, PageBefore, 1<<30, 10)
	if !ok || countPageTurns(page) != 1 || page.Segments[0].Turns[0].Text != "fresh" {
		t.Fatalf("cold parse retained stale page index: %+v", page)
	}
}

func TestPageDefensiveInputsAndReaderErrors(t *testing.T) {
	c := &LogCache{}
	if _, ok := c.Page(filepath.Join(t.TempDir(), "missing.jsonl"), PageBefore, 10, 5); ok {
		t.Fatal("missing path unexpectedly succeeded")
	}
	path := pagingFixture(t)
	if _, ok := c.Page(path, PageBefore, 10, 0); ok {
		t.Fatal("zero limit unexpectedly succeeded")
	}
	if _, ok := c.Page(path, PageMode("sideways"), 10, 5); ok {
		t.Fatal("invalid mode unexpectedly succeeded")
	}
	if page, ok := c.Page(path, PageBefore, 0, 5); !ok || len(page.Segments) != 0 {
		t.Fatalf("before first record should be empty: %+v ok=%v", page, ok)
	}

	filePath := filepath.Join(t.TempDir(), "damaged.jsonl")
	if err := os.WriteFile(filePath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filePath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
}

func TestPageOpenFailure(t *testing.T) {
	path := pagingFixture(t)
	c := &LogCache{}
	_ = c.Segment(path)
	openCalled := false
	c.pageOpen = func(string) (*os.File, error) {
		openCalled = true
		return nil, errors.New("synthetic open failure")
	}
	if _, ok := c.Page(path, PageBefore, 1<<30, 5); ok || !openCalled {
		t.Fatalf("page/open result ok/called=%v/%v", ok, openCalled)
	}
}

func TestPageReturnsFalseWhenIndexedRecordCanNoLongerBeDecoded(t *testing.T) {
	path := writeLog(t, []Event{{T: "user", Time: "2025-01-01T00:00:00Z", Text: "indexed"}})
	c := &LogCache{}
	_ = c.Segment(path) // establish the cache key and index
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), original...)
	corrupt[0] = '!'
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Page(path, PageBefore, 100, 5); ok || c.pageReads != 1 {
		t.Fatalf("corrupt indexed record result ok/reads=%v/%d, want false/1", ok, c.pageReads)
	}
}

func TestPageRejectsIndexedRecordsWithUnexpectedShape(t *testing.T) {
	t.Run("wrong turn type", func(t *testing.T) {
		path := writeLog(t, []Event{{T: "user", Time: "2025-01-01T00:00:00Z", Text: "indexed"}})
		c := &LogCache{}
		_ = c.Segment(path)
		mutateCachedLog(t, path, []byte(`"user"`), []byte(`"tool"`))
		if _, ok := c.Page(path, PageBefore, 100, 5); ok || c.pageReads != 1 {
			t.Fatalf("wrong-role page result ok/reads=%v/%d", ok, c.pageReads)
		}
	})
	t.Run("decision without payload", func(t *testing.T) {
		path := writeLog(t, []Event{
			{T: "user", Time: "2025-01-01T00:00:00Z", Text: "indexed"},
			NewDecision(DecisionEvent{LeaseID: "lease", RequestID: "decision"}),
		})
		c := &LogCache{}
		_ = c.Segment(path)
		mutateCachedLog(t, path, []byte(`"decision":`), []byte(`"whatever":`))
		if _, ok := c.Page(path, PageBefore, 100, 5); ok || c.pageReads < 2 {
			t.Fatalf("missing-decision page result ok/reads=%v/%d", ok, c.pageReads)
		}
	})
}

func TestPageHandlesUnreadableDecisionInAuditOnlySlice(t *testing.T) {
	path := pagingFixture(t)
	c := &LogCache{}
	want := ReadHistory(path)
	_ = c.Segment(path)
	anchor := want[0].Turns[len(want[0].Turns)-1].Record
	c.mu.Lock()
	decisions := c.pageSegments[0].decisions
	if len(decisions) == 0 {
		c.mu.Unlock()
		t.Fatal("fixture has no first-segment decisions")
	}
	decisions[len(decisions)-1].ref.end += 10_000
	c.mu.Unlock()
	readsBefore := c.pageReads
	if _, ok := c.Page(path, PageAfter, anchor, 1); ok || c.pageReads <= readsBefore {
		t.Fatalf("unreadable audit page ok/read count=%v/%d (before %d)", ok, c.pageReads, readsBefore)
	}
}

func mutateCachedLog(t *testing.T, path string, old, replacement []byte) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mtime, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	index := bytes.Index(data, old)
	if index < 0 || len(old) != len(replacement) {
		t.Fatalf("could not replace %q with same-length bytes", old)
	}
	copy(data[index:index+len(old)], replacement)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime.ModTime(), mtime.ModTime()); err != nil {
		t.Fatal(err)
	}
}

func mergePageSegments(pages []PageResult) []HistorySegment {
	bySegment := map[int]*HistorySegment{}
	for pi := range pages {
		p := pages[pi]
		for si := range p.Segments {
			mergeOnePageSegment(bySegment, p.Segments[si])
		}
	}
	indices := make([]int, 0, len(bySegment))
	for index := range bySegment {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	out := make([]HistorySegment, 0, len(indices))
	for _, index := range indices {
		out = append(out, *bySegment[index])
	}
	return out
}

func mergeOnePageSegment(bySegment map[int]*HistorySegment, slice HistorySlice) {
	cur := bySegment[slice.Index]
	if cur == nil {
		copy := slice.HistorySegment
		copy.Turns = append([]transcript.Turn(nil), slice.Turns...)
		copy.Decisions = append([]DecisionSurface(nil), slice.Decisions...)
		bySegment[slice.Index] = &copy
		return
	}
	cur.Turns = append(cur.Turns, slice.Turns...)
	cur.Decisions = append(cur.Decisions, slice.Decisions...)
	// Slices share boundary decisions by definition; retain each durable record once.
	seen := map[int]bool{}
	unique := cur.Decisions[:0]
	for _, d := range cur.Decisions {
		if !seen[d.Record] {
			seen[d.Record] = true
			unique = append(unique, d)
		}
	}
	cur.Decisions = unique
	sort.Slice(cur.Turns, func(i, j int) bool { return cur.Turns[i].Record < cur.Turns[j].Record })
	sort.Slice(cur.Decisions, func(i, j int) bool { return cur.Decisions[i].Record < cur.Decisions[j].Record })
}

func emptyTurnSurfaces(in []HistorySegment) []HistorySegment {
	out := []HistorySegment{}
	for _, s := range in {
		if len(s.Turns) == 0 {
			out = append(out, s)
		}
	}
	return out
}

func uniqueHistorySegments(in []HistorySegment) []HistorySegment {
	seen := map[int]bool{}
	out := []HistorySegment{}
	for _, s := range in {
		if !seen[s.Index] {
			seen[s.Index] = true
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

func countPageTurns(p PageResult) int {
	n := 0
	for _, s := range p.Segments {
		n += len(s.Turns)
	}
	return n
}

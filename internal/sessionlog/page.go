package sessionlog

import "github.com/scimux/scimux/internal/transcript"

type PageMode string

const (
	PageBefore PageMode = "before"
	PageAfter  PageMode = "after"
	PageAround PageMode = "around"
)

type WindowHead struct {
	UID     string `json:"uid"`
	Segment int    `json:"segment"`
	Record  int    `json:"record"`
}

type HistorySlice struct {
	HistorySegment
	SegmentOffset int            `json:"segment_offset"`
	SegmentTurns  int            `json:"segment_turns"`
	WindowHead    *WindowHead    `json:"window_head,omitempty"`
	LeaseBase     map[string]int `json:"lease_base,omitempty"`
}

type PageResult struct {
	Segments    []HistorySlice   `json:"segments"`
	Empty       []HistorySegment `json:"empty_segments"`
	BeforeCount int              `json:"before_count"`
	AfterCount  int              `json:"after_count"`
	Current     int              `json:"current_segment"`
	Found       bool             `json:"-"`
}

func (c *LogCache) Page(path string, mode PageMode, at, limit int) (PageResult, bool) {
	return PageResult{Segments: []HistorySlice{}, Empty: []HistorySegment{transcriptHistoryEmpty()}}, false
}

func transcriptHistoryEmpty() HistorySegment {
	return HistorySegment{Turns: []transcript.Turn{}, Decisions: []DecisionSurface{}}
}

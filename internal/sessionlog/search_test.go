package sessionlog

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A cancelled context abandons the scan mid-file (marking it partial) rather than
// reading to EOF — the server-side half of the search abort machinery.
func TestScanLogCtx_StopsOnCancel(t *testing.T) {
	lines := []string{metaU1}
	for i := 0; i < 600; i++ {
		lines = append(lines, userLine("2026-07-01T10:00:00Z", "needle here"))
	}
	p := writeLines(t, lines...)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := ScanLogCtx(ctx, p, "needle", ScanOptions{Before: 10, After: 10})
	if !res.Partial {
		t.Error("partial = false, want true when the context is cancelled mid-scan")
	}
	if len(res.Hits) >= 600 {
		t.Errorf("hits = %d, want fewer than the full 600 (scan abandoned early)", len(res.Hits))
	}
}

// writeLines writes the given raw JSONL lines to a temp log file and returns its
// path. Lines are written verbatim (with a trailing newline) so a test can feed
// malformed or truncated records to exercise the defensive contract. (The
// existing writeLog helper takes structured Events; search tests need raw text.)
func writeLines(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "log.jsonl")
	body := strings.Join(lines, "\n")
	if len(lines) > 0 {
		body += "\n"
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeLogRaw writes bytes verbatim (no trailing newline) — for the torn-tail case.
func writeRaw(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "log.jsonl")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const (
	metaU1 = `{"t":"meta","time":"2026-07-01T10:00:00Z","meta":{"node":"n","uid":"U1","agent":"claude","created":"2026-07-01T10:00:00Z"}}`
)

func userLine(time, text string) string {
	return `{"t":"user","time":"` + time + `","text":` + jsonStr(text) + `}`
}
func asstLine(time, text string) string {
	return `{"t":"assistant","time":"` + time + `","text":` + jsonStr(text) + `}`
}
func jsonStr(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

var wideOpt = ScanOptions{Before: 40, After: 40, MaxHits: 100}

func TestScanLog_BasicMatchRolesOrdinalsUID(t *testing.T) {
	p := writeLines(t,
		metaU1,
		userLine("2026-07-01T10:01:00Z", "please fix the login handler"),
		asstLine("2026-07-01T10:02:00Z", "the login flow works now"),
	)
	res := ScanLog(p, "login", wideOpt)
	if res.UID != "U1" {
		t.Fatalf("UID = %q, want U1", res.UID)
	}
	if len(res.Hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(res.Hits))
	}
	if res.Hits[0].Role != "user" || res.Hits[0].Record != 1 || res.Hits[0].Segment != 0 {
		t.Errorf("hit0 = %+v, want role=user record=1 segment=0", res.Hits[0])
	}
	if res.Hits[1].Role != "assistant" || res.Hits[1].Record != 2 {
		t.Errorf("hit1 = %+v, want role=assistant record=2", res.Hits[1])
	}
	for _, h := range res.Hits {
		if h.Match != "login" {
			t.Errorf("Match = %q, want login", h.Match)
		}
		if h.UID != "U1" {
			t.Errorf("hit UID = %q, want U1", h.UID)
		}
	}
}

func TestScanLog_CaseInsensitivePreservesOriginal(t *testing.T) {
	p := writeLines(t, metaU1, userLine("2026-07-01T10:01:00Z", "LOGIN failed twice"))
	res := ScanLog(p, "login", wideOpt)
	if len(res.Hits) != 1 || res.Hits[0].Match != "LOGIN" {
		t.Fatalf("got %+v, want one hit with Match=LOGIN", res.Hits)
	}
}

func TestScanLog_SegmentOrdinalAcrossClearSeams(t *testing.T) {
	seam := `{"t":"source","time":"2026-07-01T10:05:00Z","source":{"reason":"clear"}}`
	p := writeLines(t,
		metaU1,
		userLine("2026-07-01T10:01:00Z", "alpha login"), // rec 1, seg 0
		seam, // rec 2 (source)
		userLine("2026-07-01T10:06:00Z", "beta login"), // rec 3, seg 1
		seam, // rec 4
		userLine("2026-07-01T10:07:00Z", "gamma login"), // rec 5, seg 2
	)
	res := ScanLog(p, "login", wideOpt)
	if len(res.Hits) != 3 {
		t.Fatalf("got %d hits, want 3", len(res.Hits))
	}
	want := []struct{ seg, rec int }{{0, 1}, {1, 3}, {2, 5}}
	for i, w := range want {
		if res.Hits[i].Segment != w.seg || res.Hits[i].Record != w.rec {
			t.Errorf("hit %d = seg %d rec %d, want seg %d rec %d",
				i, res.Hits[i].Segment, res.Hits[i].Record, w.seg, w.rec)
		}
	}
}

func TestScanLog_DuplicateTextAndTimeStayDistinct(t *testing.T) {
	dup := userLine("2026-07-01T10:01:00Z", "same login line")
	p := writeLines(t, metaU1, dup, dup)
	res := ScanLog(p, "login", wideOpt)
	if len(res.Hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(res.Hits))
	}
	if res.Hits[0].Record == res.Hits[1].Record {
		t.Errorf("duplicate records share record ordinal %d", res.Hits[0].Record)
	}
}

func TestScanLog_MalformedAndBlankSkippedDoNotShiftOrdinals(t *testing.T) {
	p := writeLines(t,
		metaU1,               // rec 0
		"",                   // blank — not a record
		"{ this is not json", // malformed — not a record
		userLine("2026-07-01T10:01:00Z", "login found"), // rec 1
	)
	res := ScanLog(p, "login", wideOpt)
	if len(res.Hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(res.Hits))
	}
	if res.Hits[0].Record != 1 {
		t.Errorf("Record = %d, want 1 (blank/malformed must not count)", res.Hits[0].Record)
	}
}

func TestScanLog_AssetNameMatchesNotSourcePath(t *testing.T) {
	asset := `{"t":"asset","time":"2026-07-01T10:01:00Z","asset":{"id":"a_1","name":"plot.png","storage":"blob","sourcePath":"/home/secret/plot.png","sourceKind":"upload"}}`
	p := writeLines(t,
		metaU1, // rec 0
		asset,  // rec 1
		userLine("2026-07-01T10:02:00Z", "secret notes"), // rec 2
	)
	// filename matches, as an "asset" hit
	if res := ScanLog(p, "plot", wideOpt); len(res.Hits) != 1 ||
		res.Hits[0].Role != "asset" || res.Hits[0].Record != 1 {
		t.Fatalf("plot: got %+v, want one asset hit at record 1", res.Hits)
	}
	// SourcePath is never searched; only the user record ("secret notes") matches,
	// and it sits at record 2 — proving the asset record still counts in the ordinal
	res := ScanLog(p, "secret", wideOpt)
	if len(res.Hits) != 1 || res.Hits[0].Role != "user" || res.Hits[0].Record != 2 {
		t.Fatalf("secret: got %+v, want one user hit at record 2 (SourcePath not searched)", res.Hits)
	}
}

func TestScanLog_ToolRecordSkippedButCounted(t *testing.T) {
	tool := `{"t":"tool","time":"2026-07-01T10:01:00Z","tool":{"id":"t1","title":"Edit","rawInput":"do the login thing"}}`
	p := writeLines(t,
		metaU1, // rec 0
		tool,   // rec 1 — contains "login" but must not match
		userLine("2026-07-01T10:02:00Z", "login please"), // rec 2
	)
	res := ScanLog(p, "login", wideOpt)
	if len(res.Hits) != 1 || res.Hits[0].Role != "user" || res.Hits[0].Record != 2 {
		t.Fatalf("got %+v, want one user hit at record 2 (tool not searched, still counted)", res.Hits)
	}
}

func TestScanLog_TornFinalRecordTolerated(t *testing.T) {
	body := metaU1 + "\n" +
		userLine("2026-07-01T10:01:00Z", "login one") + "\n" +
		`{"t":"user","text":"login tw` // truncated, no closing — a torn tail
	p := writeRaw(t, body)
	res := ScanLog(p, "login", wideOpt)
	if len(res.Hits) != 1 || res.Hits[0].Match != "login" {
		t.Fatalf("got %+v, want the one intact hit; torn tail must be skipped", res.Hits)
	}
}

func TestScanLog_HitCapPartial(t *testing.T) {
	p := writeLines(t, metaU1,
		userLine("2026-07-01T10:01:00Z", "login a"),
		userLine("2026-07-01T10:02:00Z", "login b"),
	)
	res := ScanLog(p, "login", ScanOptions{Before: 10, After: 10, MaxHits: 1})
	if len(res.Hits) != 1 {
		t.Fatalf("got %d hits, want 1 (cap)", len(res.Hits))
	}
	if !res.Partial {
		t.Errorf("Partial = false, want true when the hit cap trips")
	}
}

func TestScanLog_ByteCapPartial(t *testing.T) {
	lines := []string{metaU1}
	for i := 0; i < 200; i++ {
		lines = append(lines, userLine("2026-07-01T10:01:00Z", "filler line without the word"))
	}
	lines = append(lines, userLine("2026-07-01T10:59:00Z", "login at the very end"))
	p := writeLines(t, lines...)
	res := ScanLog(p, "login", ScanOptions{Before: 10, After: 10, MaxHits: 100, MaxBytes: 200})
	if !res.Partial {
		t.Errorf("Partial = false, want true when the byte cap stops the scan early")
	}
	if len(res.Hits) != 0 {
		t.Errorf("got %d hits, want 0 (scan should stop before the trailing match)", len(res.Hits))
	}
}

func TestScanLog_ExcerptWindowBoundedAndRaw(t *testing.T) {
	long := strings.Repeat("x", 100) + " login " + strings.Repeat("y", 100)
	p := writeLines(t, metaU1, userLine("2026-07-01T10:01:00Z", long))
	res := ScanLog(p, "login", ScanOptions{Before: 10, After: 10, MaxHits: 10})
	if len(res.Hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(res.Hits))
	}
	h := res.Hits[0]
	if len([]rune(h.Before)) > 10 || len([]rune(h.After)) > 10 {
		t.Errorf("window not bounded: before=%d after=%d runes", len([]rune(h.Before)), len([]rune(h.After)))
	}
	if h.Match != "login" {
		t.Errorf("Match = %q, want login", h.Match)
	}
}

func TestScanLog_SpansAreNotEscaped(t *testing.T) {
	// The scanner returns raw text; escaping is the client's job.
	p := writeLines(t, metaU1, userLine("2026-07-01T10:01:00Z", "<script>login</script>"))
	res := ScanLog(p, "login", wideOpt)
	if len(res.Hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(res.Hits))
	}
	joined := res.Hits[0].Before + res.Hits[0].Match + res.Hits[0].After
	if !strings.Contains(joined, "<script>") {
		t.Errorf("spans should carry raw markup, got %q", joined)
	}
}

func TestScanLog_UnicodeMatch(t *testing.T) {
	p := writeLines(t, metaU1,
		userLine("2026-07-01T10:01:00Z", "café 🎉 LOGIN 🎉 done"),
	)
	res := ScanLog(p, "login", wideOpt)
	if len(res.Hits) != 1 || res.Hits[0].Match != "LOGIN" {
		t.Fatalf("got %+v, want one hit Match=LOGIN through emoji/accents", res.Hits)
	}
}

func TestScanLog_NoMetaUIDLeftEmpty(t *testing.T) {
	// no meta record → caller maps "" to a legacy identity; scanner never invents one
	p := writeLines(t, userLine("2026-07-01T10:01:00Z", "login without meta"))
	res := ScanLog(p, "login", wideOpt)
	if res.UID != "" {
		t.Errorf("UID = %q, want empty for a log with no meta uid", res.UID)
	}
	if len(res.Hits) != 1 || res.Hits[0].Record != 0 {
		t.Fatalf("got %+v, want one hit at record 0", res.Hits)
	}
}

func TestScanLog_WhitespaceOnlyTextNotMatched(t *testing.T) {
	p := writeLines(t, metaU1, userLine("2026-07-01T10:01:00Z", "   "))
	if res := ScanLog(p, " ", wideOpt); len(res.Hits) != 0 {
		t.Errorf("got %d hits, want 0 (whitespace-only record must not match)", len(res.Hits))
	}
}

func TestScanLog_MissingFileNoError(t *testing.T) {
	res := ScanLog(filepath.Join(t.TempDir(), "nope.jsonl"), "login", wideOpt)
	if len(res.Hits) != 0 || res.Partial {
		t.Errorf("missing file should yield no hits and not partial, got %+v", res)
	}
}

func assetLine(time, id, name string) string {
	return `{"t":"asset","time":"` + time + `","asset":{"id":"` + id + `","name":` + jsonStr(name) + `,"storage":"inline"}}`
}
func seamLine(time string) string {
	return `{"t":"source","time":"` + time + `","source":{"reason":"clear"}}`
}

// An asset filename hit anchors on the turn that owns it (the nearest preceding
// turn in the same segment), never the asset record's own time — that record is
// never rendered as a turn, so anchoring on its time would never resolve.
func TestScanLog_AssetHitAnchorsOwningTurnTime(t *testing.T) {
	p := writeLines(t,
		metaU1,
		asstLine("2026-07-01T10:02:00Z", "here is the chart you asked for"),
		assetLine("2026-07-01T10:02:05Z", "a_1", "revenue-plot.png"),
	)
	res := ScanLog(p, "revenue-plot", wideOpt)
	if len(res.Hits) != 1 || res.Hits[0].Role != "asset" {
		t.Fatalf("hits = %+v, want one asset hit", res.Hits)
	}
	if res.Hits[0].Time != "2026-07-01T10:02:00Z" {
		t.Errorf("asset hit time = %q, want the owning assistant turn's time", res.Hits[0].Time)
	}
	// The stable identity still points at the asset record itself (record 2).
	if res.Hits[0].Record != 2 || res.Hits[0].Segment != 0 {
		t.Errorf("asset hit identity = seg %d rec %d, want seg 0 rec 2", res.Hits[0].Segment, res.Hits[0].Record)
	}
}

// The owning-turn search never crosses a /clear seam: an asset that opens a fresh
// segment has no owner there, so it falls back to its own time (not a stale turn
// from the closed segment).
func TestScanLog_AssetOwnerDoesNotCrossSeam(t *testing.T) {
	p := writeLines(t,
		metaU1,
		asstLine("2026-07-01T10:02:00Z", "old segment answer"),
		seamLine("2026-07-01T11:00:00Z"),
		assetLine("2026-07-01T11:00:05Z", "a_9", "fresh-start.png"),
	)
	res := ScanLog(p, "fresh-start", wideOpt)
	if len(res.Hits) != 1 {
		t.Fatalf("hits = %+v, want one", res.Hits)
	}
	if res.Hits[0].Time != "2026-07-01T11:00:05Z" {
		t.Errorf("asset hit time = %q, want its own time (no owner across the seam)", res.Hits[0].Time)
	}
	if res.Hits[0].Segment != 1 {
		t.Errorf("asset hit segment = %d, want 1 (after the seam)", res.Hits[0].Segment)
	}
}

// An asset filename hit that PRECEDES its owning turn (the upload flow: the asset
// record is appended before the prompt that references it) anchors on the *next*
// turn's time, not the asset record's own time — so a live show-to-chat, which
// jumps by turn time, can land on the owning turn instead of dead-ending.
func TestScanLog_AssetHitBeforeOwnerGetsNextTurnTime(t *testing.T) {
	p := writeLines(t,
		metaU1,
		assetLine("2026-07-01T10:00:30Z", "a_1", "upload-xyz.png"),   // rec 1: no preceding turn
		userLine("2026-07-01T10:01:00Z", "please render the upload"), // rec 2: the owning turn
	)
	res := ScanLog(p, "upload-xyz", wideOpt)
	if len(res.Hits) != 1 || res.Hits[0].Role != "asset" {
		t.Fatalf("hits = %+v, want one asset hit", res.Hits)
	}
	if res.Hits[0].Time != "2026-07-01T10:01:00Z" {
		t.Errorf("asset hit time = %q, want the following turn's time (the owner)", res.Hits[0].Time)
	}
	// The stable identity still points at the asset record itself (record 1).
	if res.Hits[0].Record != 1 || res.Hits[0].Segment != 0 {
		t.Errorf("asset hit identity = seg %d rec %d, want seg 0 rec 1", res.Hits[0].Segment, res.Hits[0].Record)
	}
}

// anchorText resolves (seg, rec) through ReadTurnWindow and returns the anchor
// turn's text, failing the test if the identity does not resolve.
func anchorText(t *testing.T, path string, seg, rec int) string {
	t.Helper()
	w, anchor, _, _, ok := ReadTurnWindow(path, seg, rec, 8, 8)
	if !ok {
		t.Fatalf("ReadTurnWindow(%d,%d) did not resolve", seg, rec)
	}
	if anchor < 0 || anchor >= len(w) {
		t.Fatalf("anchor %d out of range for window len %d", anchor, len(w))
	}
	return w[anchor].Text
}

// ReadTurnWindow maps a hit's (segment, record) to the turn it owns, counting
// records exactly as ScanLog does and mapping an asset record to its owning turn.
func TestReadTurnWindow(t *testing.T) {
	// records: 0 meta, 1 user, 2 assistant, 3 asset, 4 seam, 5 user, 6 asset(no owner)
	p := writeLines(t,
		metaU1, // rec 0, seg 0
		userLine("2026-07-01T10:01:00Z", "question one"),    // rec 1 → turn 0
		asstLine("2026-07-01T10:02:00Z", "answer one"),      // rec 2 → turn 1
		assetLine("2026-07-01T10:02:05Z", "a_1", "one.png"), // rec 3 → owns turn 1
		seamLine("2026-07-01T11:00:00Z"),                    // rec 4, seg→1
		userLine("2026-07-01T11:01:00Z", "question two"),    // rec 5 → turn 2
		assetLine("2026-07-01T11:00:30Z", "a_2", "pre.png"), // rec 6, seg 1: preceding turn rec 5
	)
	// A user turn resolves to itself.
	if got := anchorText(t, p, 0, 1); got != "question one" {
		t.Errorf("user turn: anchor = %q, want %q", got, "question one")
	}
	if got := anchorText(t, p, 1, 5); got != "question two" {
		t.Errorf("second-segment user: anchor = %q, want %q", got, "question two")
	}
	// An asset resolves to the nearest preceding turn in its segment.
	if got := anchorText(t, p, 0, 3); got != "answer one" {
		t.Errorf("asset owner: anchor = %q, want %q", got, "answer one")
	}
	// An asset whose preceding turn is in the same segment owns that turn.
	if got := anchorText(t, p, 1, 6); got != "question two" {
		t.Errorf("asset in-segment owner: anchor = %q, want %q", got, "question two")
	}
	// Out of range resolves to nothing.
	if _, _, _, _, ok := ReadTurnWindow(p, 9, 99, 8, 8); ok {
		t.Error("out-of-range identity should not resolve")
	}
}

// A record that precedes every turn in its segment (the upload flow: the asset is
// appended before the prompt that references it) resolves forward to the next turn
// in that segment.
func TestReadTurnWindow_ForwardOwnerNextTurn(t *testing.T) {
	p := writeLines(t,
		metaU1, // rec 0
		assetLine("2026-07-01T10:00:30Z", "a_1", "pre.png"), // rec 1, seg 0: no preceding turn
		userLine("2026-07-01T10:01:00Z", "the prompt"),      // rec 2 → turn 0, the owner
	)
	if got := anchorText(t, p, 0, 1); got != "the prompt" {
		t.Errorf("forward owner: anchor = %q, want %q", got, "the prompt")
	}
}

// ReadTurnWindow never crosses a /clear seam to find a "next turn" owner: an asset
// that precedes every turn in its segment, followed by a seam before any owner
// turn appears, resolves to nothing rather than claiming the next segment's turn.
func TestReadTurnWindow_SeamTerminatesForwardOwner(t *testing.T) {
	p := writeLines(t,
		metaU1, // rec 0
		userLine("2026-07-01T10:01:00Z", "seg zero turn"),   // rec 1 → turn 0
		seamLine("2026-07-01T11:00:00Z"),                    // rec 2, seg→1
		assetLine("2026-07-01T11:00:05Z", "a_1", "pre.png"), // rec 3, seg 1: no preceding turn
		seamLine("2026-07-01T12:00:00Z"),                    // rec 4, seg→2 (before any seg-1 turn)
		userLine("2026-07-01T12:01:00Z", "seg two turn"),    // rec 5 → turn 1 (must NOT be claimed)
	)
	if w, anchor, _, _, ok := ReadTurnWindow(p, 1, 3, 8, 8); ok {
		t.Errorf("seam should terminate the forward-owner search, got ok with anchor %d (%q)", anchor, w[anchor].Text)
	}
}

// ReadTurnWindow keeps only a bounded window: for an anchor with many turns on
// either side it returns before+1+after turns and flags both truncations, never
// materializing the whole log.
func TestReadTurnWindow_BoundedWindow(t *testing.T) {
	lines := []string{metaU1}
	for i := 0; i < 20; i++ {
		lines = append(lines, userLine("2026-07-01T10:00:00Z", "turn"))
	}
	p := writeLines(t, lines...)
	// Anchor turn 10 → parsed record 11 (meta is rec 0). before=2, after=3.
	w, anchor, bt, af, ok := ReadTurnWindow(p, 0, 11, 2, 3)
	if !ok {
		t.Fatal("expected resolution")
	}
	if len(w) != 6 { // 2 before + anchor + 3 after
		t.Errorf("window len = %d, want 6", len(w))
	}
	if anchor != 2 {
		t.Errorf("anchor index = %d, want 2 (after 2 before-turns)", anchor)
	}
	if !bt || !af {
		t.Errorf("truncation flags = before %v after %v, want both true", bt, af)
	}
}

// ReadTurnWindow's ordinals stay in lockstep with ScanLog even when malformed and
// blank lines appear — the same defensive stream, so a hit resolves to its turn.
func TestReadTurnWindow_LockstepWithScanLog(t *testing.T) {
	p := writeLines(t,
		metaU1,
		userLine("2026-07-01T10:01:00Z", "alpha"),
		`{ this is not json`, // skipped, no ordinal shift
		``,                   // blank, no ordinal shift
		asstLine("2026-07-01T10:02:00Z", "beta"),
	)
	res := ScanLog(p, "beta", wideOpt)
	if len(res.Hits) != 1 {
		t.Fatalf("hits = %+v", res.Hits)
	}
	if got := anchorText(t, p, res.Hits[0].Segment, res.Hits[0].Record); got != "beta" {
		t.Errorf("resolve = %q, want beta", got)
	}
}

func asstLineProv(time, text, src string) string {
	return `{"t":"assistant","time":"` + time + `","text":` + jsonStr(text) + `,"prov":[{"loc":"message","key":"_meta","v":{"src":"` + src + `"}}]}`
}

func TestScanStringExcerptAndEmptyQuery(t *testing.T) {
	got := ScanString("please FIX the login handler today", "fix", 8, 8)
	if !got.OK || got.Match != "FIX" {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(strings.ToLower(got.Before), "please") {
		t.Fatalf("before=%q", got.Before)
	}
	if !strings.Contains(strings.ToLower(got.After), "the") {
		t.Fatalf("after=%q", got.After)
	}
	if ScanString("nope", "missing", 4, 4).OK {
		t.Fatal("absent query reported a hit")
	}
	if ScanString("hello", "  ", 4, 4).OK {
		t.Fatal("whitespace query reported a hit")
	}
	if ScanString("", "x", 4, 4).OK {
		t.Fatal("empty haystack reported a hit")
	}
}

func TestScanLogRetainsAgentAndProvForAssistant(t *testing.T) {
	p := writeLines(t,
		metaU1,
		userLine("2026-07-01T10:01:00Z", "please login"),
		asstLineProv("2026-07-01T10:02:00Z", "login is fixed", "search-asst"),
	)
	res := ScanLog(p, "login", wideOpt)
	if len(res.Hits) != 2 {
		t.Fatalf("hits = %+v", res.Hits)
	}
	if res.Hits[0].Role != "user" || res.Hits[0].Agent != "" || len(res.Hits[0].Prov) != 0 {
		t.Errorf("user hit must not carry agent/prov: %+v", res.Hits[0])
	}
	h := res.Hits[1]
	if h.Role != "assistant" || h.Agent != "claude" {
		t.Errorf("assistant hit agent = role=%q agent=%q, want assistant/claude", h.Role, h.Agent)
	}
	assertRawEqual(t, h.Prov, synthProvJSON("search-asst"))
}

func TestReadTurnWindowRetainsAgentAndProv(t *testing.T) {
	p := writeLines(t,
		metaU1,
		userLine("2026-07-01T10:01:00Z", "please login"),
		asstLineProv("2026-07-01T10:02:00Z", "login is fixed", "window-asst"),
	)
	res := ScanLog(p, "fixed", wideOpt)
	if len(res.Hits) != 1 {
		t.Fatalf("hits = %+v", res.Hits)
	}
	win, anchor, _, _, ok := ReadTurnWindow(p, res.Hits[0].Segment, res.Hits[0].Record, 1, 0)
	if !ok || anchor < 0 || anchor >= len(win) {
		t.Fatalf("window ok=%v anchor=%d len=%d", ok, anchor, len(win))
	}
	if win[anchor].Role != "assistant" || win[anchor].Text != "login is fixed" {
		t.Fatalf("anchor turn = %+v", win[anchor])
	}
	if win[anchor].Agent != "claude" {
		t.Errorf("window Agent = %q, want claude", win[anchor].Agent)
	}
	assertRawEqual(t, win[anchor].Prov, synthProvJSON("window-asst"))
	if win[0].Role != "user" || win[0].Agent != "" {
		t.Errorf("user window turn misattributed: %+v", win[0])
	}
}

func TestSearchOwnershipUnchangedWithProvenance(t *testing.T) {
	// Asset hits still own the nearest preceding turn; seams still bound
	// ownership. Provenance on the assistant must not change ordinals.
	p := writeLines(t,
		metaU1, // rec 0
		userLine("2026-07-01T10:01:00Z", "question one"),                                                      // rec 1
		asstLineProv("2026-07-01T10:02:00Z", "answer one", "own-asst"),                                        // rec 2
		`{"t":"asset","time":"2026-07-01T10:02:30Z","asset":{"id":"a_1","name":"plot.png","storage":"blob"}}`, // rec 3
		`{"t":"source","time":"2026-07-01T11:00:00Z","source":{"reason":"clear"}}`,                            // rec 4
		userLine("2026-07-01T11:01:00Z", "question two"),                                                      // rec 5
	)
	res := ScanLog(p, "plot", wideOpt)
	if len(res.Hits) != 1 || res.Hits[0].Role != "asset" || res.Hits[0].Record != 3 {
		t.Fatalf("asset hit = %+v", res.Hits)
	}
	if got := anchorText(t, p, 0, 3); got != "answer one" {
		t.Errorf("asset owner = %q, want answer one", got)
	}
	win, anchor, _, _, ok := ReadTurnWindow(p, 0, 3, 0, 0)
	if !ok || win[anchor].Text != "answer one" || win[anchor].Agent != "claude" {
		t.Fatalf("asset window = ok=%v %+v", ok, win)
	}
	assertRawEqual(t, win[anchor].Prov, synthProvJSON("own-asst"))
	if _, _, _, _, ok := ReadTurnWindow(p, 1, 3, 8, 8); ok {
		t.Error("asset record 3 is in segment 0; segment 1 must not resolve it as an owner search across the seam")
	}
}

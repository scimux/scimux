package sessionlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

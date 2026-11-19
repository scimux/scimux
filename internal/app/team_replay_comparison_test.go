package app

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestReplayComparisonRequiresExactSyntheticFields(t *testing.T) {
	cases := []struct {
		name             string
		expected, actual any
	}{
		{"added nested", map[string]any{"a": map[string]any{"x": true}}, map[string]any{"a": map[string]any{"x": true, "y": false}}},
		{"deleted nested", map[string]any{"a": map[string]any{"x": true}}, map[string]any{"a": map[string]any{}}},
		{"type", map[string]any{"a": true}, map[string]any{"a": "true"}},
		{"null", map[string]any{"a": nil}, map[string]any{"a": false}},
		{"order", map[string]any{"a": []any{1., 2.}}, map[string]any{"a": []any{2., 1.}}},
		{"value", map[string]any{"a": "one"}, map[string]any{"a": "two"}},
		{"timestamp", map[string]any{"at": "2026-01-01T00:00:00Z"}, map[string]any{"at": "2026-01-01T00:00:01Z"}},
		{"unrelated url", map[string]any{"url": "https://example.invalid/a"}, map[string]any{"url": "http://127.0.0.1:1234"}},
		{"identity", map[string]any{"people_copy_instance": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, map[string]any{"people_copy_instance": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}},
		{"unrelated id", map[string]any{"text": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, map[string]any{"text": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if seatReplayDiff(tc.expected, tc.actual, "$") == "" {
				t.Fatal("different values matched")
			}
		})
	}
	if d := seatReplayDiff(map[string]any{"a": []any{1., nil}}, map[string]any{"a": []any{1., nil}}, "$"); d != "" {
		t.Fatal("equal values differ")
	}
}

func TestReplayResponseComparisonSyntheticCases(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record seatReplayRecord
		status int
		body   string
		fail   bool
	}{
		{"same", seatReplayRecord{Status: 200, Res: []byte(`{"nested":{"x":true}}`)}, 200, `{"nested":{"x":true}}`, false},
		{"status", seatReplayRecord{Status: 200, Res: []byte(`{"x":true}`)}, 201, `{"x":true}`, true},
		{"field", seatReplayRecord{Status: 200, Res: []byte(`{"x":true}`)}, 200, `{"x":false}`, true},
		{"missing", seatReplayRecord{Status: 200, ResMissing: true}, 200, `{"x":true}`, true},
		{"invalid expected", seatReplayRecord{Status: 200, Res: []byte(`{`)}, 200, `null`, true},
		{"invalid actual", seatReplayRecord{Status: 200, Res: []byte(`null`)}, 200, `{`, true},
		{"bodyless", seatReplayRecord{Status: 204}, 204, ``, false},
		{"bodyless changed", seatReplayRecord{Status: 204}, 204, `{"x":true}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			diff := seatReplayResponseDiff(tc.record, tc.status, []byte(tc.body))
			if (diff != "") != tc.fail {
				t.Fatalf("comparison returned %q", diff)
			}
		})
	}
}

func TestReplayResponseExactRawSyntheticBytes(t *testing.T) {
	cases := []struct {
		name              string
		status            int
		jsonBody, rawBody string
		gotStatus         int
		got               []byte
		fail              bool
	}{
		{"plain text", 404, "", `"bm90IGZvdW5kCg=="`, 404, []byte("not found\n"), false},
		{"binary", 200, "", `"AP9/"`, 200, []byte{0, 255, 127}, false},
		{"byte mutation", 200, "", `"AP9/"`, 200, []byte{0, 255, 126}, true},
		{"empty raw", 200, "", `""`, 200, []byte{}, false},
		{"missing", 200, "", "", 200, []byte{}, true},
		{"json null", 200, `null`, "", 200, []byte(`null`), false},
		{"json null vs empty", 200, `null`, "", 200, []byte{}, true},
		{"ambiguous", 200, `null`, `""`, 200, []byte{}, true},
		{"corrupt base64", 200, "", `"%%%"`, 200, []byte{}, true},
		{"raw null", 200, "", `null`, 200, []byte{}, true},
		{"invalid JSON", 200, `{`, "", 200, []byte(`{`), true},
		{"wrong status", 200, "", `""`, 201, []byte{}, true},
		{"bodyless", 204, "", "", 204, []byte{}, false},
		{"bodyless raw", 204, "", `""`, 204, []byte{}, true},
		{"bodyless JSON", 204, `null`, "", 204, []byte{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := seatReplayRecord{Status: tc.status, Res: []byte(tc.jsonBody), ResBytes: []byte(tc.rawBody)}
			diff := seatReplayResponseDiff(r, tc.gotStatus, tc.got)
			if (diff != "") != tc.fail {
				t.Fatalf("comparison returned %q", diff)
			}
		})
	}
}

func TestReplayLargeJSONNumbersRemainDistinct(t *testing.T) {
	record := seatReplayRecord{Status: 200, Res: []byte(`{"anchor":9007199254740992}`)}
	if d := seatReplayResponseDiff(record, 200, []byte(`{"anchor":9007199254740993}`)); d == "" {
		t.Fatal("different JSON integers compared equal")
	}
}

func TestReplayTimesSyntheticSequenceBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		seq  any
		want bool
	}{
		{"uint64 maximum", json.Number("18446744073709551615"), true},
		{"overflow", json.Number("18446744073709551616"), false},
		{"fractional", json.Number("1.5"), false},
		{"negative", json.Number("-1"), false},
		{"wrong type", "1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"entries": []any{map[string]any{"seq": tc.seq, "at": "2000-01-01T00:00:00Z"}}}
			if got := seatReplayTimes(body, "synthetic", map[string]map[uint64]time.Time{}); got != tc.want {
				t.Fatalf("result=%v want=%v", got, tc.want)
			}
		})
	}
	body := map[string]any{"entries": []any{map[string]any{"seq": json.Number("9007199254740992"), "at": "2000-01-01T00:00:01Z"}, map[string]any{"seq": json.Number("9007199254740993"), "at": "2000-01-01T00:00:00Z"}}}
	if seatReplayTimes(body, "synthetic", map[string]map[uint64]time.Time{}) {
		t.Fatal("reversed adjacent large sequences accepted")
	}
}

func TestReplayBodylessSyntheticStatuses(t *testing.T) {
	for _, status := range []int{100, 199, 204, 304} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			for _, tc := range []struct {
				name        string
				record      seatReplayRecord
				actual      []byte
				wantFailure bool
			}{
				{"absent", seatReplayRecord{Status: status}, nil, false},
				{"represented JSON", seatReplayRecord{Status: status, Res: []byte(`null`)}, nil, true},
				{"represented raw", seatReplayRecord{Status: status, ResBytes: []byte(`""`)}, nil, true},
				{"actual bytes", seatReplayRecord{Status: status}, []byte("x"), true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if failed := seatReplayResponseDiff(tc.record, status, tc.actual) != ""; failed != tc.wantFailure {
						t.Fatalf("failed=%v want=%v", failed, tc.wantFailure)
					}
				})
			}
		})
	}
	if _, _, err := selectReplayBody(seatReplayRecord{Status: 200, ResBytes: []byte(" \n null \t")}); err == nil {
		t.Fatal("whitespace-wrapped null accepted as raw bytes")
	}
}

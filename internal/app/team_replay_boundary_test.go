package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReplayApplicationHeadersSynthetic(t *testing.T) {
	base := http.Header{"Content-Type": {"application/json"}, "X-Values": {"one", "two"}}
	cases := []struct {
		name      string
		want, got http.Header
		equal     bool
	}{
		{"identical", base, http.Header{"content-type": {"application/json"}, "x-values": {"one", "two"}}, true},
		{"added", base, http.Header{"Content-Type": {"application/json"}, "X-Values": {"one", "two"}, "X-Extra": {"yes"}}, false},
		{"deleted", base, http.Header{"Content-Type": {"application/json"}}, false},
		{"changed", base, http.Header{"Content-Type": {"text/plain"}, "X-Values": {"one", "two"}}, false},
		{"order", base, http.Header{"Content-Type": {"application/json"}, "X-Values": {"two", "one"}}, false},
		{"duplicate case keys", http.Header{"X-Values": {"one"}, "x-values": {"one"}}, http.Header{"X-Values": {"one", "one"}}, false},
		{"empty", http.Header{}, http.Header{}, true},
		{"missing", nil, http.Header{}, false},
		{"missing actual", http.Header{}, nil, false},
		{"duplicate actual case keys", http.Header{"X-Values": {"one", "one"}}, http.Header{"X-Values": {"one"}, "x-values": {"one"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := seatReplayHeadersEqual(tc.want, tc.got); got != tc.equal {
				t.Fatalf("equal=%v want=%v", got, tc.equal)
			}
		})
	}
}

func TestReplayPageBoundarySynthetic(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Add("X-Synthetic", "one")
		w.Header().Add("X-Synthetic", "two")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	got, host, headers := seatReplayPageRequest(handler, "GET", "/synthetic", "")
	if host != "127.0.0.1" || headers == nil || len(headers) != 0 {
		t.Fatal("request boundary missing")
	}
	if got.Code != 200 || !seatReplayHeadersEqual(http.Header{"Content-Type": {"application/json"}, "X-Content-Type-Options": {"nosniff"}, "X-Synthetic": {"one", "two"}}, got.Header()) {
		t.Fatal("response headers not captured")
	}
	for _, h := range []http.Header{{"Content-Type": {"application/json"}, "X-Content-Type-Options": {"unsafe"}, "X-Synthetic": {"one", "two"}}, {"Content-Type": {"application/json"}, "X-Content-Type-Options": {"nosniff"}}, nil, {}, {"Content-Type": {"application/json"}}, {"Content-Type": {"text/plain"}, "X-Content-Type-Options": {"nosniff"}, "X-Synthetic": {"one", "two"}}, {"Content-Type": {"application/json"}, "X-Content-Type-Options": {"nosniff"}, "X-Synthetic": {"one", "two"}, "X-Extra": {"x"}}} {
		if seatReplayHeadersEqual(h, got.Header()) {
			t.Error("changed or missing page header accepted")
		}
	}
}

func TestReplayWireMatchesApplicationRequestBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, host       string
		recorded, actual http.Header
		match            bool
	}{
		{"equal", "127.0.0.1:23456", http.Header{"X-Synthetic": {"one", "two"}}, http.Header{"x-synthetic": {"one", "two"}}, true},
		{"host", "127.0.0.1:23457", http.Header{}, http.Header{}, false},
		{"added", "127.0.0.1:23456", http.Header{}, http.Header{"X-Extra": {"x"}}, false},
		{"deleted", "127.0.0.1:23456", http.Header{"X-Synthetic": {"one"}}, http.Header{}, false},
		{"changed", "127.0.0.1:23456", http.Header{"X-Synthetic": {"one"}}, http.Header{"X-Synthetic": {"two"}}, false},
		{"reordered", "127.0.0.1:23456", http.Header{"X-Synthetic": {"one", "two"}}, http.Header{"X-Synthetic": {"two", "one"}}, false},
		{"missing map", "127.0.0.1:23456", nil, http.Header{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := seatReplayRecord{Method: "GET", Path: "/wire", RequestHost: "127.0.0.1:23456", RequestHeaders: tc.recorded, Status: 200, Res: []byte(`null`), Headers: http.Header{}}
			wire := &seatReplayWire{records: []seatReplayRecord{record}, used: make([]bool, 1)}
			r := httptest.NewRequest("GET", "http://127.0.0.1:23456/wire", nil)
			r.Host = tc.host
			r.Header = tc.actual
			w := httptest.NewRecorder()
			wire.ServeHTTP(w, r)
			if (w.Code == 200) != tc.match || wire.used[0] != tc.match {
				t.Fatal("request boundary mismatch")
			}
		})
	}
}

func TestReplayPageResponseCapturesSentHeaders(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Sent", "yes")
		w.WriteHeader(200)
		w.Header().Set("X-Late", "not-sent")
	})
	got, _, _ := seatReplayPageRequest(h, "GET", "/synthetic", "")
	headers := seatReplayPageResponseHeaders(got)
	if headers.Get("X-Sent") != "yes" || headers.Get("X-Late") != "" {
		t.Fatal("response headers do not match sent snapshot")
	}
}

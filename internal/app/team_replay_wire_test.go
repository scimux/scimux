package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/team"
)

func TestReplayWirePreservesSyntheticHeaders(t *testing.T) {
	const body = `{"protocol":1,"server":{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","name":"Synthetic"}}`
	for _, tc := range []struct {
		name   string
		values []string
		want   bool
	}{
		{"present", []string{"1"}, true}, {"absent", nil, false}, {"false", []string{"0"}, false}, {"malformed", []string{"other"}, false}, {"repeated", []string{"1", "other"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string][]string{"X-Synthetic": {"first", "second"}, "Connection": {"X-Hop, keep-alive"}, "X-Hop": {"private"}, "Keep-Alive": {"timeout=5"}, "Content-Length": {"999"}}
			if tc.values != nil {
				headers["Scimux-Blob-Generation"] = tc.values
			}
			raw, err := json.Marshal(map[string]any{"method": "GET", "path": "/v1/hello", "status": 200, "res": json.RawMessage(body), "headers": headers})
			if err != nil {
				t.Fatal(err)
			}
			var record seatReplayRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				t.Fatal(err)
			}
			wire := &seatReplayWire{records: []seatReplayRecord{record}, used: make([]bool, 1)}
			const logical = "http://127.0.0.1:23456"
			wire.records[0].RequestHost = "127.0.0.1:23456"
			wire.records[0].RequestHeaders = http.Header{"Accept": {"application/json"}}
			client := team.NewClient()
			client.Transport = seatReplayLogicalTransport(logical, wire, map[string]bool{})
			got, err := client.Hello(t.Context(), logical)
			if err != nil {
				t.Fatal(err)
			}
			if got.BlobGeneration != tc.want {
				t.Errorf("capability=%v, want %v", got.BlobGeneration, tc.want)
			}
			if !wire.used[0] || wire.cursor != 1 {
				t.Error("record not consumed")
			}
			wire.used[0] = false
			wire.cursor = 0
			req := httptest.NewRequest("GET", "http://127.0.0.1/v1/hello", nil)
			wire.records[0].RequestHost = req.Host
			wire.records[0].RequestHeaders = req.Header.Clone()
			w := httptest.NewRecorder()
			wire.ServeHTTP(w, req)
			if got := w.Header().Values("X-Synthetic"); len(got) != 2 || got[0] != "first" || got[1] != "second" {
				t.Errorf("repeated header lost: %v", got)
			}
			for _, key := range []string{"X-Hop", "Connection", "Keep-Alive", "Content-Length"} {
				if w.Header().Get(key) != "" {
					t.Errorf("transport header %s retained", key)
				}
			}
			if got := w.Header().Values("Scimux-Blob-Generation"); strings.Join(got, ",") != strings.Join(tc.values, ",") {
				t.Errorf("capability header values changed: %v", got)
			}
		})
	}
}

func TestReplayWireOldHeadersAbsentAndBodyless(t *testing.T) {
	for _, tc := range []struct {
		name   string
		raw    string
		status int
		want   string
	}{
		{"legacy", `{"method":"GET","path":"/old","status":200,"res":{"ok":true}}`, 200, `{"ok":true}`},
		{"bodyless", `{"method":"GET","path":"/old","status":204,"headers":{"X-Synthetic":["one","two"],"Content-Length":["99"]}}`, 204, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var record seatReplayRecord
			if err := json.Unmarshal([]byte(tc.raw), &record); err != nil {
				t.Fatal(err)
			}
			wire := &seatReplayWire{records: []seatReplayRecord{record}, used: make([]bool, 1)}
			srv := httptest.NewServer(wire)
			defer srv.Close()
			wire.records[0].RequestHost = strings.TrimPrefix(srv.URL, "http://")
			wire.records[0].RequestHeaders = http.Header{"User-Agent": {"Go-http-client/1.1"}, "Accept-Encoding": {"gzip"}}
			r, err := http.Get(srv.URL + "/old")
			if err != nil {
				t.Fatal(err)
			}
			b, err := io.ReadAll(r.Body)
			_ = r.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if r.StatusCode != tc.status || string(b) != tc.want {
				t.Errorf("unexpected response status/body: %d %q", r.StatusCode, b)
			}
			if r.Header.Get("Scimux-Blob-Generation") != "" {
				t.Error("legacy capability appeared")
			}
			if tc.status == 204 && len(r.Header.Values("X-Synthetic")) != 2 {
				t.Error("bodyless headers lost")
			}
			if !wire.used[0] || wire.cursor != 1 {
				t.Error("record not consumed")
			}
		})
	}
}

func TestReplayWireRecomputesFramingForRecordedBody(t *testing.T) {
	token := strings.Repeat("a", 64)
	raw := `{"method":"GET","path":"/token","status":200,"res":{"token":"` + token + `"},"headers":{"Content-Length":["19"],"Content-Type":["application/json"]}}`
	var record seatReplayRecord
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		t.Fatal(err)
	}
	wire := &seatReplayWire{records: []seatReplayRecord{record}, used: make([]bool, 1)}
	srv := httptest.NewServer(wire)
	defer srv.Close()
	wire.records[0].RequestHost = strings.TrimPrefix(srv.URL, "http://")
	wire.records[0].RequestHeaders = http.Header{"User-Agent": {"Go-http-client/1.1"}, "Accept-Encoding": {"gzip"}}
	resp, err := http.Get(srv.URL + "/token")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(body, &decoded) != nil || decoded.Token != token {
		t.Fatal("recorded token changed")
	}
	if resp.ContentLength != int64(len(body)) {
		t.Fatalf("framing length=%d body=%d", resp.ContentLength, len(body))
	}
	if !wire.used[0] || wire.cursor != 1 {
		t.Fatal("record not consumed")
	}
}

func TestReplayWireMatchesRecordedConditionalDelete(t *testing.T) {
	path := "/v1/channels/" + strings.Repeat("a", 32) + "/" + strings.Repeat("b", 16)
	gen := strings.Repeat("c", 16)
	records := []seatReplayRecord{{Method: "DELETE", Path: path + "?generation=" + gen, Status: 204}}
	wire := &seatReplayWire{records: records, used: make([]bool, 1), generations: map[string]string{path: gen}}
	req := httptest.NewRequest("DELETE", "http://127.0.0.1"+path+"?generation="+gen, nil)
	wire.records[0].RequestHost = req.Host
	wire.records[0].RequestHeaders = req.Header.Clone()
	w := httptest.NewRecorder()
	wire.ServeHTTP(w, req)
	if w.Code != 204 || !wire.used[0] {
		t.Fatal("recorded conditional request was not consumed")
	}
}

func TestReplayWireServesRawAndRejectsAmbiguousSyntheticRecords(t *testing.T) {
	for _, tc := range []struct {
		name, encoded string
		status        int
		expected      []byte
		consumed      bool
	}{
		{"raw", `{"method":"GET","path":"/wire","status":200,"res_bytes":"AP9/"}`, 200, []byte{0, 255, 127}, true},
		{"ambiguous", `{"method":"GET","path":"/wire","status":200,"res":null,"res_bytes":""}`, 500, nil, false},
		{"invalid", `{"method":"GET","path":"/wire","status":200,"res_bytes":"%%%"}`, 500, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rec seatReplayRecord
			if err := json.Unmarshal([]byte(tc.encoded), &rec); err != nil {
				t.Fatal(err)
			}
			rec.RequestHost = "127.0.0.1"
			rec.RequestHeaders = http.Header{}
			wire := &seatReplayWire{records: []seatReplayRecord{rec}, used: make([]bool, 1)}
			w := httptest.NewRecorder()
			wire.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1/wire", nil))
			if w.Code != tc.status || !bytes.Equal(w.Body.Bytes(), tc.expected) || wire.used[0] != tc.consumed {
				t.Fatal("unexpected wire response or consumption")
			}
		})
	}
}

func TestReplayWireDistinguishesAbsentRequestAndJSONNull(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded json.RawMessage
		actual   string
		want     int
	}{
		{"absent matches absent", nil, "", 200}, {"null matches null", json.RawMessage(`null`), "null", 200},
		{"null is not absent", json.RawMessage(`null`), "", 500}, {"absent is not null", nil, "null", 500},
		{"invalid request", json.RawMessage(`null`), "{", 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := seatReplayRecord{Method: "POST", Path: "/wire", Status: 200, Req: tc.recorded, Res: json.RawMessage(`null`), RequestHost: "127.0.0.1", RequestHeaders: http.Header{}}
			f := &seatReplayWire{records: []seatReplayRecord{rec}, used: make([]bool, 1)}
			w := httptest.NewRecorder()
			f.ServeHTTP(w, httptest.NewRequest("POST", "http://127.0.0.1/wire", strings.NewReader(tc.actual)))
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d", w.Code, tc.want)
			}
		})
	}
}

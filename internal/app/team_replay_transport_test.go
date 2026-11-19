package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/team"
)

func TestReplayLogicalTransportSyntheticBoundary(t *testing.T) {
	logical := "http://127.0.0.1:23456"
	sum := sha256.Sum256([]byte("token-1"))
	token := hex.EncodeToString(sum[:])
	calls := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Scheme != "http" || r.URL.Host != "127.0.0.1:23456" || r.Host != "127.0.0.1:23456" || r.Method != "POST" || r.URL.RequestURI() != "/v1/example?a=1" || r.Header.Get("X-Synthetic") != "example" || r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("forwarded request changed")
		}
		b, _ := io.ReadAll(r.Body)
		if string(b) != "synthetic" {
			t.Error("body changed")
		}
		w.Header().Set("Scimux-Blob-Generation", "1")
		w.Header().Add("X-Synthetic", "first")
		w.Header().Add("X-Synthetic", "second")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	tr := seatReplayLogicalTransport(logical, handler, map[string]bool{token: true})
	makeReq := func(target, auth string) *http.Request {
		req, err := http.NewRequest("POST", target+"/v1/example?a=1", bytes.NewBufferString("synthetic"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Synthetic", "example")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		return req
	}
	req := makeReq(logical, "Bearer "+token)
	response, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if calls != 1 || string(body) != `{"ok":true}` || response.Header.Get("Scimux-Blob-Generation") != "1" || len(response.Header.Values("X-Synthetic")) != 2 {
		t.Fatal("response or capability changed")
	}
	if req.URL.String() != logical+"/v1/example?a=1" {
		t.Fatal("original URL mutated")
	}
	for _, tc := range []struct{ name, target, auth string }{{"scheme", "https://127.0.0.1:23456", "Bearer " + token}, {"host", "http://127.0.0.2:23456", "Bearer " + token}, {"port", "http://127.0.0.1:23458", "Bearer " + token}, {"credential", logical, "Bearer " + strings.Repeat("b", 64)}, {"bad scheme", logical, "Basic synthetic"}} {
		t.Run(tc.name, func(t *testing.T) {
			before := calls
			if _, err := tr.RoundTrip(makeReq(tc.target, tc.auth)); err == nil {
				t.Fatal("unexpected acceptance")
			}
			if calls != before {
				t.Fatal("unexpected outbound call")
			}
		})
	}
	for _, target := range []string{"http://user@127.0.0.1:23456", "http://127.0.0.1:23456#synthetic"} {
		before := calls
		if _, err := tr.RoundTrip(makeReq(target, "Bearer "+token)); err == nil || calls != before {
			t.Error("userinfo or fragment accepted")
		}
	}
	before := calls
	multiple := makeReq(logical, "Bearer "+token)
	multiple.Header.Add("Authorization", "Bearer "+strings.Repeat("b", 64))
	if _, err := tr.RoundTrip(multiple); err == nil || calls != before {
		t.Error("multiple authorization values accepted")
	}
	empty := makeReq(logical, "")
	empty.Header["Authorization"] = []string{""}
	if _, err := tr.RoundTrip(empty); err == nil || calls != before {
		t.Error("empty authorization accepted")
	}
}

func TestReplayBindingSyntheticExact(t *testing.T) {
	entry := team.Entry{ID: strings.Repeat("a", 32), URL: "http://127.0.0.1:23456", Token: strings.Repeat("b", 64)}
	captured := peopleServerFingerprint(entry)
	if captured != peopleServerFingerprint(entry) {
		t.Fatal("same configuration binding changed")
	}
	changed := entry
	changed.Token = strings.Repeat("c", 64)
	if seatReplayDiff(map[string]any{"people_file_binding": captured}, map[string]any{"people_file_binding": peopleServerFingerprint(changed)}, "$") == "" {
		t.Fatal("mutated binding accepted")
	}
}

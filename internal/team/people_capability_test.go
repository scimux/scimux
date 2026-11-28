package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestPeopleBlobCapabilityDiscoveryIsExplicit(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		supported    bool
	}{{"current", "1", true}, {"legacy", "", false}, {"unknown", "2", false}} {
		t.Run(tc.name, func(t *testing.T) {
			srv, got := serveTeam(t, func(w http.ResponseWriter, _ *http.Request, _ *capturedRequest) {
				if tc.header != "" {
					w.Header().Set("Scimux-Blob-Generation", tc.header)
				}
				_, _ = io.WriteString(w, helloBody(helloID, helloName))
			})
			server, err := clientOn(srv).Hello(context.Background(), srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			field := reflect.ValueOf(server).FieldByName("BlobGeneration")
			if !field.IsValid() || field.Kind() != reflect.Bool {
				t.Fatal("hello did not expose explicit verified blob capability")
			}
			if field.Bool() != tc.supported {
				t.Errorf("capability %v; want %v", field.Bool(), tc.supported)
			}
			if len(*got) != 1 || (*got)[0].method != http.MethodGet || (*got)[0].path != "/v1/hello" {
				t.Fatal("capability discovery issued unexpected request")
			}
		})
	}
}

type guardedPeopleBlobClient interface {
	PutPeopleBlob(context.Context, string, string, string, string, string, string, string, []byte) error
}

func TestPeopleBlobNeverPutsWithoutVerifiedCapability(t *testing.T) {
	for _, tc := range []struct {
		name, cap, server string
		wantPut           bool
	}{{"old-relay", "", helloID, false}, {"changed-server", "1", "11111111111111111111111111111111", false}, {"guarded", "1", helloID, true}} {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte("synthetic bytes")
			digest := sha256.Sum256(payload)
			hash := hex.EncodeToString(digest[:])
			srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
				if r.URL.Path == "/v1/hello" {
					if tc.cap != "" {
						w.Header().Set("Scimux-Blob-Generation", tc.cap)
					}
					_, _ = io.WriteString(w, helloBody(tc.server, helloName))
					return
				}
				if r.Method == http.MethodPut && r.URL.Query().Get("generation") == "0001020304050607" {
					w.WriteHeader(http.StatusCreated)
					_, _ = io.WriteString(w, `{"id":"`+hash+`","size":15}`)
					return
				}
				http.Error(w, "unexpected", http.StatusBadRequest)
			})
			client, ok := any(clientOn(srv)).(guardedPeopleBlobClient)
			if !ok {
				t.Fatal("client has no generation-fenced People blob operation")
			}
			err := client.PutPeopleBlob(context.Background(), srv.URL, regToken, helloID, seatID, "main", "0001020304050607", hash, payload)
			var puts int
			for _, req := range *got {
				if req.method == http.MethodPut {
					puts++
				}
			}
			if (err == nil) != tc.wantPut || (puts == 1) != tc.wantPut {
				t.Errorf("err=%v PUTs=%d wantPut=%v", err, puts, tc.wantPut)
			}
		})
	}
}

type guardedPeopleBlobReader interface {
	GetPeopleBlob(context.Context, string, string, string, string, string, string, string, int64) ([]byte, error)
}

func TestPeopleBlobReadRequiresCapabilityAndValidatesBytes(t *testing.T) {
	for _, tc := range []struct {
		name, cap        string
		corrupt          bool
		wantGET, success bool
	}{{"old-relay", "", false, false, false}, {"valid", "1", false, true, true}, {"corrupt", "1", true, true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte("synthetic bytes")
			digest := sha256.Sum256(payload)
			hash := hex.EncodeToString(digest[:])
			srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
				if r.URL.Path == "/v1/hello" {
					if tc.cap != "" {
						w.Header().Set("Scimux-Blob-Generation", tc.cap)
					}
					_, _ = io.WriteString(w, helloBody(helloID, helloName))
					return
				}
				if r.Method == http.MethodGet && r.URL.Query().Get("generation") == "0001020304050607" {
					if tc.corrupt {
						_, _ = w.Write([]byte("bad bytes"))
					} else {
						_, _ = w.Write(payload)
					}
					return
				}
				http.Error(w, "unexpected", http.StatusBadRequest)
			})
			client, ok := any(clientOn(srv)).(guardedPeopleBlobReader)
			if !ok {
				t.Fatal("client has no generation-fenced People blob read")
			}
			data, err := client.GetPeopleBlob(context.Background(), srv.URL, regToken, helloID, seatID, "main", "0001020304050607", hash, int64(len(payload)))
			var gets int
			for _, req := range *got {
				if strings.Contains(req.path, "/blobs/") {
					gets++
				}
			}
			if (gets == 1) != tc.wantGET || (err == nil) != tc.success {
				t.Errorf("GETs=%d err=%v", gets, err)
			}
			if tc.success && string(data) != string(payload) {
				t.Fatal("verified bytes differ")
			}
			if !tc.success && len(data) != 0 {
				t.Fatal("unverified bytes returned")
			}
		})
	}
}

func TestPeopleSmallBlobPreservesHTTPStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   error
	}{{"unauthorized", 401, ErrUnauthorized}, {"forbidden", 403, ErrForbidden}, {"missing", 404, ErrNoChannel}, {"stale", 409, ErrStaleGeneration}} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
				if r.URL.Path == "/v1/hello" {
					w.Header().Set("Scimux-Blob-Generation", "1")
					_, _ = io.WriteString(w, helloBody(helloID, helloName))
					return
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"error":"synthetic"}`)
			})
			sum := sha256.Sum256([]byte("x"))
			data, err := clientOn(srv).GetPeopleBlob(context.Background(), srv.URL, regToken, helloID, seatID, "main", "0001020304050607", hex.EncodeToString(sum[:]), 1)
			if !errors.Is(err, tc.want) || len(data) != 0 {
				t.Fatalf("status %d yielded bytes=%d err=%v", tc.status, len(data), err)
			}
		})
	}
}

func TestPeopleTinyBlobSuccessRequiresExactBytes(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		success    bool
	}{{"valid", "x", true}, {"corrupt", "y", false}, {"oversize", "xy", false}, {"empty", "", false}} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
				if r.URL.Path == "/v1/hello" {
					w.Header().Set("Scimux-Blob-Generation", "1")
					_, _ = io.WriteString(w, helloBody(helloID, helloName))
					return
				}
				_, _ = io.WriteString(w, tc.body)
			})
			sum := sha256.Sum256([]byte("x"))
			data, err := clientOn(srv).GetPeopleBlob(context.Background(), srv.URL, regToken, helloID, seatID, "main", "0001020304050607", hex.EncodeToString(sum[:]), 1)
			if (err == nil) != tc.success || (tc.success && string(data) != "x") || (!tc.success && len(data) != 0) {
				t.Fatalf("bytes=%q err=%v", data, err)
			}
		})
	}
}

func TestPeopleBlobRejectsUnsafeRequestsBeforeTransfer(t *testing.T) {
	payload := []byte("x")
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])
	generation := "0001020304050607"
	srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		t.Errorf("unexpected network request: %s", r.Method)
		http.Error(w, "unexpected", 500)
	})
	c := clientOn(srv)
	for _, tc := range []struct {
		name, server, owner, chat, gen, hash string
		data                                 []byte
		size                                 int64
	}{
		{"server", "bad", seatID, "main", generation, hash, payload, 1}, {"owner", helloID, "bad", "main", generation, hash, payload, 1},
		{"chat", helloID, seatID, "../bad", generation, hash, payload, 1}, {"generation", helloID, seatID, "main", "bad", hash, payload, 1},
		{"hash", helloID, seatID, "main", generation, "bad", payload, 1}, {"empty", helloID, seatID, "main", generation, hash, nil, 0},
		{"oversize", helloID, seatID, "main", generation, hash, make([]byte, (4<<20)+1), (4 << 20) + 1}, {"digest", helloID, seatID, "main", generation, strings.Repeat("a", 64), payload, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := c.PutPeopleBlob(context.Background(), srv.URL, regToken, tc.server, tc.owner, tc.chat, tc.gen, tc.hash, tc.data); err == nil {
				t.Fatal("invalid upload accepted")
			}
			if tc.name != "digest" {
				if data, err := c.GetPeopleBlob(context.Background(), srv.URL, regToken, tc.server, tc.owner, tc.chat, tc.gen, tc.hash, tc.size); err == nil || len(data) != 0 {
					t.Fatal("invalid read accepted")
				}
			}
		})
	}
	if len(*got) != 0 {
		t.Fatal("invalid association reached the network")
	}
}

func TestPeopleBlobReceiptAcceptsAnUnknownField(t *testing.T) {
	payload := []byte("synthetic bytes")
	digest := sha256.Sum256(payload)
	hash := hex.EncodeToString(digest[:])
	srv, _ := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			_, _ = io.WriteString(w, helloBody(helloID, helloName))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"`+hash+`","size":15,"extra":true}`)
	})
	if err := clientOn(srv).PutPeopleBlob(context.Background(), srv.URL, regToken, helloID, seatID, "main", "0001020304050607", hash, payload); err != nil {
		t.Fatal(err)
	}
}

func TestPeopleBlobUploadRejectsRelayFailuresAndInvalidReceipts(t *testing.T) {
	payload := []byte("x")
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])
	gen := "0001020304050607"
	for _, tc := range []struct {
		name                   string
		helloStatus, putStatus int
		helloID                string
		cap                    bool
		receipt                string
		want                   error
	}{
		{"hello-unavailable", 503, 0, helloID, true, "", ErrProtocol}, {"server-mismatch", 200, 0, strings.Repeat("1", 32), true, "", ErrProtocol},
		{"unsupported", 200, 0, helloID, false, "", ErrBlobUnsupported}, {"forbidden", 200, 403, helloID, true, `{"error":"forbidden"}`, ErrForbidden},
		{"bad-json", 200, 201, helloID, true, `not-json`, ErrProtocol}, {"wrong-id", 200, 201, helloID, true, `{"id":"wrong","size":1}`, ErrProtocol}, {"wrong-size", 200, 201, helloID, true, `{"id":"` + hash + `","size":2}`, ErrProtocol},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
				if r.URL.Path == "/v1/hello" {
					if tc.cap {
						w.Header().Set("Scimux-Blob-Generation", "1")
					}
					w.WriteHeader(tc.helloStatus)
					if tc.helloStatus == 200 {
						_, _ = io.WriteString(w, helloBody(tc.helloID, helloName))
					}
					return
				}
				w.WriteHeader(tc.putStatus)
				_, _ = io.WriteString(w, tc.receipt)
			})
			err := clientOn(srv).PutPeopleBlob(context.Background(), srv.URL, regToken, helloID, seatID, "main", gen, hash, payload)
			if err == nil || (tc.name != "hello-unavailable" && !errors.Is(err, tc.want)) {
				t.Fatalf("unexpected refusal: %v", err)
			}
		})
	}
}

func TestPeopleBlobReportsInterruptedTransfersAndInvalidHello(t *testing.T) {
	payload := []byte("x")
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])
	gen := "0001020304050607"
	for _, tc := range []struct {
		name      string
		hello     string
		failHello bool
	}{
		{"transfer-interrupted", helloID, false}, {"server-mismatch", strings.Repeat("1", 32), false}, {"hello-offline", helloID, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := NewClient()
			client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/v1/hello" && !tc.failHello {
					return &http.Response{StatusCode: 200, Header: http.Header{"Scimux-Blob-Generation": []string{"1"}}, Body: io.NopCloser(strings.NewReader(helloBody(tc.hello, helloName)))}, nil
				}
				return nil, errors.New("synthetic interruption")
			})
			data, err := client.GetPeopleBlob(context.Background(), "http://127.0.0.1", regToken, helloID, seatID, "main", gen, hash, 1)
			if err == nil || len(data) != 0 {
				t.Fatalf("interrupted read returned data=%q err=%v", data, err)
			}
			if tc.name == "transfer-interrupted" {
				if err = client.PutPeopleBlob(context.Background(), "http://127.0.0.1", regToken, helloID, seatID, "main", gen, hash, payload); err == nil {
					t.Fatal("interrupted upload accepted")
				}
			}
		})
	}
}

package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type exactPeoplePublisher interface {
	AppendPeopleComment(context.Context, string, string, string, string, string, string, string, json.RawMessage) (uint64, error)
}

func TestPeoplePublicationNeverConfirmsUnverifiableRelayResult(t *testing.T) {
	for _, tc := range []struct {
		name, author, appendBody, readBody string
		appendStatus, readStatus, calls    int
	}{
		{"invalid author", "bad", "", "", 0, 0, 0},
		{"append refused", chOther, "", "", http.StatusForbidden, 0, 1},
		{"wrong append generation", chOther, fmt.Sprintf(`{"generation":"fedcba9876543210","results":[{"key":"c/0123456789abcdef","seq":3,"duplicate":true}],"last":3}`), "", 200, 0, 1},
		{"read refused", chOther, "", "", 200, http.StatusForbidden, 2},
		{"empty read", chOther, "", fmt.Sprintf(`{"generation":"%s","entries":[],"last":3,"more":false}`, chGen), 200, 200, 2},
		{"wrong read sequence", chOther, "", fmt.Sprintf(`{"generation":"%s","entries":[{"seq":4,"key":"c/0123456789abcdef","author":"%s","at":"2026-11-14T09:30:00.000Z","body":{"v":1}}],"last":4,"more":false}`, chGen, chOther), 200, 200, 2},
		{"wrong read key", chOther, "", fmt.Sprintf(`{"generation":"%s","entries":[{"seq":3,"key":"c/fedcba9876543210","author":"%s","at":"2026-11-14T09:30:00.000Z","body":{"v":1}}],"last":3,"more":false}`, chGen, chOther), 200, 200, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := &Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				status, raw := tc.readStatus, tc.readBody
				if r.Method == http.MethodPost {
					status, raw = tc.appendStatus, tc.appendBody
					if raw == "" {
						raw = fmt.Sprintf(`{"generation":"%s","results":[{"key":"c/0123456789abcdef","seq":3,"duplicate":true}],"last":3}`, chGen)
					}
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(raw)), Header: make(http.Header)}, nil
			})}
			seq, err := c.AppendPeopleComment(context.Background(), "http://127.0.0.1", chToken, chOwner, chChat, chGen, "c/0123456789abcdef", tc.author, json.RawMessage(`{"v":1}`))
			if err == nil || seq != 0 || calls != tc.calls {
				t.Fatalf("unverified result: seq=%d err=%v calls=%d", seq, err, calls)
			}
			if tc.name == "empty read" && !errors.Is(err, ErrPeoplePublicationConflict) {
				t.Fatalf("empty read must remain a conflict: %v", err)
			}
		})
	}
}

func TestPeoplePublicationVerifiesDuplicateBody(t *testing.T) {
	for _, tc := range []struct {
		name, body, author, gen string
		success                 bool
	}{
		{"same", `{"v":1}`, chOther, chGen, true},
		{"different-body", `{"v":2}`, chOther, chGen, false},
		{"different-author", `{"v":1}`, chOwner, chGen, false},
		{"different-generation", `{"v":1}`, chOther, "fedcba9876543210", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			c := &Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				var raw string
				if r.Method == http.MethodPost {
					raw = fmt.Sprintf(`{"generation":"%s","results":[{"key":"c/0123456789abcdef","seq":3,"duplicate":true}],"last":3}`, chGen)
				} else {
					if r.URL.Query().Get("after") != "2" {
						t.Error("wrong reconciliation cursor")
					}
					raw = fmt.Sprintf(`{"generation":"%s","entries":[{"seq":3,"key":"c/0123456789abcdef","author":"%s","at":"2026-11-14T09:30:00.000Z","body":%s}],"last":3,"more":false}`, tc.gen, tc.author, tc.body)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(raw)), Header: make(http.Header)}, nil
			})}
			publisher, ok := any(c).(exactPeoplePublisher)
			if !ok {
				t.Fatal("no exact publication reconciliation")
			}
			seq, err := publisher.AppendPeopleComment(context.Background(), "http://127.0.0.1", chToken, chOwner, chChat, chGen, "c/0123456789abcdef", chOther, json.RawMessage(`{"v":1}`))
			if (err == nil) != tc.success || (tc.success && seq != 3) {
				t.Errorf("seq=%d err=%v", seq, err)
			}
			if calls != 2 {
				t.Errorf("calls=%d want append plus exact read", calls)
			}
		})
	}
}

func TestPeoplePublicationRelayEscapingAndExactNumbers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		echoed  json.RawMessage
		success bool
	}{
		{"escaped", json.RawMessage(`{"v":1,"text":"Synthetic \u003cfile\u003e \u0026 name","anchor":9007199254740993,"files":["a","b"]}`), true},
		{"changed-large-integer", json.RawMessage(`{"v":1,"text":"Synthetic \u003cfile\u003e \u0026 name","anchor":9007199254740992,"files":["a","b"]}`), false},
		{"reordered-array", json.RawMessage(`{"v":1,"text":"Synthetic \u003cfile\u003e \u0026 name","anchor":9007199254740993,"files":["b","a"]}`), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := json.RawMessage(`{"v":1,"text":"Synthetic <file> & name","anchor":9007199254740993,"files":["a","b"]}`)
			c := &Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				var raw string
				if r.Method == http.MethodPost {
					raw = fmt.Sprintf(`{"generation":"%s","results":[{"key":"c/0123456789abcdef","seq":3,"duplicate":true}],"last":3}`, chGen)
				} else {
					raw = fmt.Sprintf(`{"generation":"%s","entries":[{"seq":3,"key":"c/0123456789abcdef","author":"%s","at":"2026-11-14T09:30:00.000Z","body":%s}],"last":3,"more":false}`, chGen, chOther, tc.echoed)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(raw)), Header: make(http.Header)}, nil
			})}
			seq, err := c.AppendPeopleComment(context.Background(), "http://127.0.0.1", chToken, chOwner, chChat, chGen, "c/0123456789abcdef", chOther, original)
			if (err == nil) != tc.success || (tc.success && seq != 3) {
				t.Fatalf("seq=%d err=%v", seq, err)
			}
		})
	}
}

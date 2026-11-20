package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestPeopleBlobCancellationBeforeUpload(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	c := &Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path == "/v1/hello" {
			cancel()
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Scimux-Blob-Generation": []string{"1"}}, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"protocol":1,"server":{"id":"%s","name":"Synthetic relay"}}`, chOther)))}, nil
		}
		return &http.Response{StatusCode: http.StatusInternalServerError, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}
	data := []byte("synthetic data")
	sum := sha256.Sum256(data)
	err := c.PutPeopleBlob(ctx, "http://127.0.0.1", chToken, chOther, chOwner, chChat, chGen, hex.EncodeToString(sum[:]), data)
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("cancelled upload: error=%v calls=%d", err, calls)
	}
	calls = 0
	err = c.PutPeopleBlob(ctx, "http://127.0.0.1", chToken, chOther, chOwner, chChat, chGen, hex.EncodeToString(sum[:]), data)
	if !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("already cancelled upload: error=%v calls=%d", err, calls)
	}
}

func TestPeopleAppendCancellationBeforeVerification(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	c := &Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method == http.MethodPost {
			cancel()
			raw := fmt.Sprintf(`{"generation":"%s","results":[{"key":"c/0123456789abcdef","seq":1,"duplicate":false}],"last":1}`, chGen)
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw))}, nil
		}
		return &http.Response{StatusCode: http.StatusInternalServerError, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}
	seq, err := c.AppendPeopleComment(ctx, "http://127.0.0.1", chToken, chOwner, chChat, chGen, "c/0123456789abcdef", chOther, []byte(`{"v":1}`))
	if !errors.Is(err, context.Canceled) || calls != 1 || seq != 0 {
		t.Fatalf("cancelled verification: seq=%d error=%v calls=%d", seq, err, calls)
	}
	calls = 0
	seq, err = c.AppendPeopleComment(ctx, "http://127.0.0.1", chToken, chOwner, chChat, chGen, "c/0123456789abcdef", chOther, []byte(`{"v":1}`))
	if !errors.Is(err, context.Canceled) || calls != 0 || seq != 0 {
		t.Fatalf("already cancelled append: seq=%d error=%v calls=%d", seq, err, calls)
	}
}

func TestPeoplePublicationAcceptsNilContext(t *testing.T) {
	data := []byte("synthetic data")
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	c := &Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		header := make(http.Header)
		raw := ""
		switch {
		case r.URL.Path == "/v1/hello":
			header.Set("Scimux-Blob-Generation", "1")
			raw = fmt.Sprintf(`{"protocol":1,"server":{"id":"%s","name":"Synthetic relay"}}`, chOther)
		case r.Method == http.MethodPut:
			raw = fmt.Sprintf(`{"id":"%s","size":%d}`, hash, len(data))
		case r.Method == http.MethodPost:
			raw = fmt.Sprintf(`{"generation":"%s","results":[{"key":"c/0123456789abcdef","seq":1,"duplicate":false}],"last":1}`, chGen)
		default:
			raw = fmt.Sprintf(`{"generation":"%s","entries":[{"seq":1,"key":"c/0123456789abcdef","author":"%s","at":"2026-01-01T00:00:00.000Z","body":{"v":1}}],"last":1,"more":false}`, chGen, chOther)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(raw))}, nil
	})}
	if err := c.PutPeopleBlob(nil, "http://127.0.0.1", chToken, chOther, chOwner, chChat, chGen, hash, data); err != nil {
		t.Fatalf("nil context upload: %v", err)
	}
	if seq, err := c.AppendPeopleComment(nil, "http://127.0.0.1", chToken, chOwner, chChat, chGen, "c/0123456789abcdef", chOther, []byte(`{"v":1}`)); err != nil || seq != 1 {
		t.Fatalf("nil context publication: seq=%d error=%v", seq, err)
	}
}

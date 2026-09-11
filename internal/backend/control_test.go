package backend

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistrationRoundTripAndOwnership(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	link := Link{Socket: "/tmp/scimux-test.sock", Token: "capability"}
	registration, err := Register(data, link)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(data, locatorName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("locator mode = %o, want 0600", info.Mode().Perm())
	}
	got, err := Discover(data)
	if err != nil {
		t.Fatal(err)
	}
	if got != link {
		t.Fatalf("discovered = %#v, want %#v", got, link)
	}

	// An older muxer must not remove a newer muxer's registration while its
	// own graceful shutdown is completing.
	newLink := Link{Socket: "/tmp/scimux-new.sock", Token: "new-capability"}
	newRegistration, err := Register(data, newLink)
	if err != nil {
		t.Fatal(err)
	}
	if err := registration.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := Discover(data); err != nil || got != newLink {
		t.Fatalf("old close disturbed new registration: %#v, %v", got, err)
	}
	if err := newRegistration.Close(); err != nil {
		t.Fatal(err)
	}
	if err := newRegistration.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := Discover(data); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("locator survives close: %v", err)
	}
	var nilRegistration *Registration
	if err := nilRegistration.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRegistrationRejectsUnsafeOrMalformedState(t *testing.T) {
	link := Link{Socket: "/tmp/muxer.sock", Token: "cap"}
	if _, err := Register("", link); err == nil {
		t.Fatal("registered in an empty data directory")
	}
	if _, err := Register(filepath.Join(t.TempDir(), "missing"), link); err == nil {
		t.Fatal("registered in a missing data directory")
	}
	if _, err := Register(t.TempDir(), Link{Token: "cap"}); err == nil {
		t.Fatal("registered an incomplete link")
	}
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Register(file, link); err == nil {
		t.Fatal("registered beneath a regular file")
	}
	public := t.TempDir()
	if err := os.Chmod(public, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Register(public, link); err == nil {
		t.Fatal("registered beneath a non-owner-only directory")
	}

	data := t.TempDir()
	path := filepath.Join(data, locatorName)
	tests := []struct {
		name string
		body string
		mode os.FileMode
	}{
		{"malformed", `{`, 0o600},
		{"empty-socket", `{"socket":"","token":"cap"}`, 0o600},
		{"empty-token", `{"socket":"/tmp/muxer.sock","token":""}`, 0o600},
		{"trailing-json", `{"socket":"/tmp/muxer.sock","token":"cap"}{}`, 0o600},
		{"public", `{"socket":"/tmp/muxer.sock","token":"cap"}`, 0o644},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.body), tc.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatal(err)
			}
			if _, err := Discover(data); err == nil {
				t.Fatal("discovered unsafe or malformed locator")
			}
		})
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxLocatorSize+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover(data); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized locator error = %v", err)
	}

	blocked := t.TempDir()
	if err := os.Chmod(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(blocked, locatorName), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Register(blocked, link); err == nil {
		t.Fatal("published a locator over a directory")
	}
	entries, err := os.ReadDir(blocked)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != locatorName {
		t.Fatalf("failed publish leaked temporary files: %v", entries)
	}
}

func TestRegistrationCloseAbsentAndReadFailure(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	registration, err := Register(data, Link{Socket: "/tmp/muxer.sock", Token: "cap"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(data, locatorName)); err != nil {
		t.Fatal(err)
	}
	if err := registration.Close(); err != nil {
		t.Fatalf("close after external removal: %v", err)
	}
	broken := &Registration{path: data}
	if err := broken.Close(); err == nil {
		t.Fatal("close accepted an unreadable registration path")
	}
}

func TestPublishLocatorCreationFailure(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if err := publishLocator(missing, filepath.Join(missing, locatorName), []byte("x")); err == nil {
		t.Fatal("published beneath a missing directory")
	}
}

func TestRegistrationRejectsLiveOwner(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	server, err := Listen(t.TempDir(), http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	registration, err := Register(data, server.Link())
	if err != nil {
		t.Fatal(err)
	}
	defer registration.Close()
	if _, err := Register(data, Link{Socket: "/tmp/other.sock", Token: "other"}); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second registration error = %v", err)
	}
	if got, err := Discover(data); err != nil || got != server.Link() {
		t.Fatalf("second registration disturbed owner: %#v, %v", got, err)
	}
}

func TestClientRequestStop(t *testing.T) {
	requested := make(chan struct{}, 1)
	s, err := Listen(t.TempDir(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/_scimux/stop" {
			t.Fatalf("stop request = %s %s", r.Method, r.URL.Path)
		}
		requested <- struct{}{}
		w.WriteHeader(http.StatusAccepted)
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := NewClient(s.Link())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.RequestStop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requested:
	default:
		t.Fatal("stop request did not reach muxer")
	}

	for _, status := range []int{http.StatusNoContent, http.StatusInternalServerError} {
		c.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return response(status, req), nil
		})
		if err := c.RequestStop(context.Background()); err == nil {
			t.Fatalf("accepted status %d", status)
		}
	}
	c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed")
	})
	if err := c.RequestStop(context.Background()); err == nil {
		t.Fatal("accepted transport failure")
	}
	var nilClient *Client
	if err := nilClient.RequestStop(context.Background()); err == nil {
		t.Fatal("nil client requested stop")
	}
}

func response(status int, req *http.Request) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}
}

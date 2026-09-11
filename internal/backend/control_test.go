package backend

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestRegistrationRoundTripAndOwnership(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	link := Link{Socket: "/tmp/scimux-test.sock", Token: "capability"}
	if err := os.WriteFile(filepath.Join(data, locatorName), []byte(`{"socket":"/stale","token":"stale"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	registration, err := Claim(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Discover(data); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("claim retained stale locator: %v", err)
	}
	if err := registration.Publish(link); err != nil {
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

	if _, err := Claim(data); !errors.Is(err, ErrMuxerOwned) {
		t.Fatalf("second owner claim = %v, want ErrMuxerOwned", err)
	}

	// A successor can claim and publish only after the lifetime owner exits.
	newLink := Link{Socket: "/tmp/scimux-new.sock", Token: "new-capability"}
	if err := registration.Close(); err != nil {
		t.Fatal(err)
	}
	newRegistration, err := Register(data, newLink)
	if err != nil {
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
	lockBlocked := t.TempDir()
	if err := os.Chmod(lockBlocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(lockBlocked, lockName), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Claim(lockBlocked); err == nil || !strings.Contains(err.Error(), "ownership lock") {
		t.Fatalf("lock open error = %v", err)
	}
	var nilRegistration *Registration
	if err := nilRegistration.Publish(link); err == nil {
		t.Fatal("nil owner published a locator")
	}
	unpublishedData := t.TempDir()
	if err := os.Chmod(unpublishedData, 0o700); err != nil {
		t.Fatal(err)
	}
	unpublished, err := Claim(unpublishedData)
	if err != nil {
		t.Fatal(err)
	}
	if err := unpublished.Publish(Link{Token: "cap"}); err == nil {
		t.Fatal("published an incomplete link")
	}
	if err := unpublished.Close(); err != nil {
		t.Fatal(err)
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
	if len(entries) != 2 || entries[0].Name() != locatorName || entries[1].Name() != lockName {
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
	broken := &Registration{locatorPath: data, payload: []byte("published")}
	if err := broken.Close(); err == nil {
		t.Fatal("close accepted an unreadable registration path")
	}
}

func TestConcurrentOwnershipHasExactlyOneWinner(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	const contenders = 32
	start := make(chan struct{})
	owners := make(chan *Registration, contenders)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			owner, err := Claim(data)
			if err == nil {
				wins.Add(1)
				owners <- owner
				return
			}
			if !errors.Is(err, ErrMuxerOwned) {
				t.Errorf("claim error = %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(owners)
	if got := wins.Load(); got != 1 {
		t.Fatalf("live owners = %d, want exactly 1", got)
	}
	for owner := range owners {
		if err := owner.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPublishLocatorCreationFailure(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if err := publishLocator(missing, filepath.Join(missing, locatorName), []byte("x")); err == nil {
		t.Fatal("published beneath a missing directory")
	}
}

func TestRegistrationFilesystemFailures(t *testing.T) {
	link := Link{Socket: "/tmp/muxer.sock", Token: "cap"}
	t.Run("remove stale locator", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory write permissions")
		}
		data := t.TempDir()
		if err := os.Chmod(data, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(data, lockName), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(data, locatorName), []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(data, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(data, 0o700) })
		if _, err := Claim(data); err == nil || !strings.Contains(err.Error(), "remove stale") {
			t.Fatalf("claim error = %v, want stale-locator removal failure", err)
		}
	})

	t.Run("publish locator", func(t *testing.T) {
		data := t.TempDir()
		if err := os.Chmod(data, 0o700); err != nil {
			t.Fatal(err)
		}
		owner, err := Claim(data)
		if err != nil {
			t.Fatal(err)
		}
		defer owner.Close()
		owner.locatorPath = filepath.Join(data, "missing", locatorName)
		if err := owner.Publish(link); err == nil || !strings.Contains(err.Error(), "create muxer locator") {
			t.Fatalf("publish error = %v, want locator creation failure", err)
		}
	})

	t.Run("rename locator", func(t *testing.T) {
		data := t.TempDir()
		path := filepath.Join(data, "occupied")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := publishLocator(data, path, []byte("payload")); err == nil || !strings.Contains(err.Error(), "publish muxer locator") {
			t.Fatalf("publish error = %v, want rename failure", err)
		}
	})

	t.Run("read locator", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses file read permissions")
		}
		data := t.TempDir()
		path := filepath.Join(data, locatorName)
		if err := os.WriteFile(path, []byte(`{"socket":"/tmp/muxer.sock","token":"cap"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
		if _, err := Discover(data); err == nil || !strings.Contains(err.Error(), "read muxer locator") {
			t.Fatalf("discover error = %v, want locator read failure", err)
		}
	})

	t.Run("remove locator on close", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory write permissions")
		}
		data := t.TempDir()
		if err := os.Chmod(data, 0o700); err != nil {
			t.Fatal(err)
		}
		owner, err := Register(data, link)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(data, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(data, 0o700) })
		if err := owner.Close(); err == nil {
			t.Fatal("close accepted an unremovable locator")
		}
	})
}

func TestRegistrationRejectsLiveOwner(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	link := Link{Socket: "/tmp/scimux-owner.sock", Token: "owner"}
	registration, err := Register(data, link)
	if err != nil {
		t.Fatal(err)
	}
	defer registration.Close()
	if _, err := Register(data, Link{Socket: "/tmp/other.sock", Token: "other"}); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second registration error = %v", err)
	}
	if got, err := Discover(data); err != nil || got != link {
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

package sessionworker

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRegistrationPublishesDiscoverableWorkerForItsLifetime(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	reg, err := Claim(data, "chat-one")
	if err != nil {
		t.Fatal(err)
	}
	locator := Locator{
		Identity: Identity{WorkerID: "instance-a", Agent: "opencode", Build: "v1"},
		NodeID:   "chat-one", PID: os.Getpid(),
		Link: Link{Socket: "/tmp/scimux-a.sock", Token: "secret"},
	}
	if err := reg.Publish(locator); err != nil {
		t.Fatal(err)
	}
	got, err := Discover(data, "chat-one")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, locator) {
		t.Fatalf("Discover = %#v, want %#v", got, locator)
	}
	if info, err := os.Stat(workerLocatorPath(data, "chat-one")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("locator mode = %v, %v", info, err)
	}
	if _, err := Claim(data, "chat-one"); !errors.Is(err, ErrWorkerOwned) {
		t.Fatalf("second Claim = %v, want ErrWorkerOwned", err)
	}
	if err := reg.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reg.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := Discover(data, "chat-one"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Discover after close = %v", err)
	}
	if _, err := os.Stat(workerLockPath(data, "chat-one")); err != nil {
		t.Fatalf("lifetime lock inode was removed: %v", err)
	}
}

func TestRegistrationReplacesStaleLocatorOnlyAfterClaim(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(workerLocatorPath(data, "chat-one"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workerLocatorPath(data, "chat-one"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := Claim(data, "chat-one")
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	if _, err := os.Stat(workerLocatorPath(data, "chat-one")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale locator remains after lifetime claim: %v", err)
	}
	locator := Locator{Identity: Identity{WorkerID: "fresh", Agent: "pi"}, NodeID: "chat-one", PID: 7, Link: Link{Socket: "/tmp/fresh.sock", Token: "new"}}
	if err := reg.Publish(locator); err != nil {
		t.Fatal(err)
	}
	if got, err := Discover(data, "chat-one"); err != nil || !reflect.DeepEqual(got, locator) {
		t.Fatalf("replacement = %#v, %v", got, err)
	}
}

func TestRegistrationValidationAndList(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Claim("", "node"); err == nil {
		t.Fatal("Claim accepted empty data dir")
	}
	if _, err := Claim(data, "../node"); err == nil {
		t.Fatal("Claim accepted unsafe node id")
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := Claim(missing, "node"); err == nil {
		t.Fatal("Claim accepted missing data dir")
	}
	notDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Claim(notDir, "node"); err == nil {
		t.Fatal("Claim accepted a non-directory")
	}
	insecure := t.TempDir()
	if err := os.Chmod(insecure, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Claim(insecure, "node"); err == nil {
		t.Fatal("Claim accepted group/world-accessible data dir")
	}
	reg, err := Claim(data, "one")
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	var nilRegistration *Registration
	if err := nilRegistration.Publish(Locator{}); err == nil {
		t.Fatal("nil registration published")
	}
	if err := nilRegistration.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reg.Publish(Locator{}); err == nil {
		t.Fatal("Publish accepted incomplete locator")
	}
	one := Locator{Identity: Identity{WorkerID: "i1", Agent: "grok"}, NodeID: "one", PID: 1, Link: Link{Socket: "/tmp/one", Token: "t1"}}
	if err := reg.Publish(one); err != nil {
		t.Fatal(err)
	}
	reg2, err := Claim(data, "two")
	if err != nil {
		t.Fatal(err)
	}
	defer reg2.Close()
	two := Locator{Identity: Identity{WorkerID: "i2", Agent: "codex"}, NodeID: "two", PID: 2, Link: Link{Socket: "/tmp/two", Token: "t2"}}
	if err := reg2.Publish(two); err != nil {
		t.Fatal(err)
	}
	got, err := List(data)
	if err != nil || !reflect.DeepEqual(got, []Locator{one, two}) {
		t.Fatalf("List = %#v, %v", got, err)
	}

	if err := os.WriteFile(filepath.Join(workerControlDir(data), "broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = List(data)
	if err == nil || !reflect.DeepEqual(got, []Locator{one, two}) {
		t.Fatalf("partial List = %#v, %v", got, err)
	}
	if got, err := List(filepath.Join(t.TempDir(), "absent")); err != nil || got != nil {
		t.Fatalf("List absent = %#v, %v", got, err)
	}
}

func TestRegistrationRejectsUnsafeStaleLocatorAndPreservesReplacement(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := workerControlDir(data)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := workerLocatorPath(data, "chat")
	if err := os.Symlink(filepath.Join(data, "elsewhere"), stale); err != nil {
		t.Fatal(err)
	}
	if _, err := Claim(data, "chat"); err == nil {
		t.Fatal("Claim accepted a stale locator symlink")
	}
	if err := os.Remove(stale); err != nil {
		t.Fatal(err)
	}
	reg, err := Claim(data, "chat")
	if err != nil {
		t.Fatal(err)
	}
	one := Locator{Identity: Identity{WorkerID: "one", Agent: "pi"}, NodeID: "chat", PID: 1, Link: Link{Socket: "/tmp/one", Token: "one"}}
	two := Locator{Identity: Identity{WorkerID: "two", Agent: "pi"}, NodeID: "chat", PID: 2, Link: Link{Socket: "/tmp/two", Token: "two"}}
	if err := reg.Publish(one); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(two)
	if err := os.WriteFile(stale, append(payload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reg.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := Discover(data, "chat"); err != nil || !reflect.DeepEqual(got, two) {
		t.Fatalf("Close removed another publisher: %#v, %v", got, err)
	}
}

func TestDiscoverRejectsEveryUntrustedLocatorShape(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := workerControlDir(data)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover(data, "../bad"); err == nil {
		t.Fatal("Discover accepted unsafe node id")
	}
	if _, err := Discover(data, "missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing locator = %v", err)
	}
	path := workerLocatorPath(data, "node")
	tests := []struct {
		name string
		make func() error
	}{
		{"directory", func() error { return os.Mkdir(path, 0o700) }},
		{"public", func() error { return os.WriteFile(path, []byte("{}"), 0o644) }},
		{"oversized", func() error { return os.WriteFile(path, make([]byte, maxLocatorSize+1), 0o600) }},
		{"bad-json", func() error { return os.WriteFile(path, []byte("{"), 0o600) }},
		{"incomplete", func() error { return os.WriteFile(path, []byte(`{"node_id":"node"}`), 0o600) }},
		{"mismatch", func() error {
			locator := Locator{Identity: Identity{WorkerID: "w", Agent: "pi"}, NodeID: "other", PID: 1, Link: Link{Socket: "/tmp/x", Token: "t"}}
			payload, _ := json.Marshal(locator)
			return os.WriteFile(path, payload, 0o600)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.RemoveAll(path)
			if err := tc.make(); err != nil {
				t.Fatal(err)
			}
			if _, err := Discover(data, "node"); err == nil {
				t.Fatal("untrusted locator was accepted")
			}
		})
	}
}

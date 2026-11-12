package sessionworker

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

func TestInteractionDiscoveryPreservesLocatorReadError(t *testing.T) {
	// Use a direct temporary directory so every ancestor is searchable even
	// when a root-run test temporarily drops filesystem privilege on this thread.
	data, err := os.MkdirTemp("", "scimux-unreadable-locator-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(data) })
	for _, dir := range []string{data, filepath.Join(data, "control"), workerControlDir(data)} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	path := workerLocatorPath(data, "synthetic")
	if err := os.WriteFile(path, []byte("{}"), 0000); err != nil {
		t.Fatal(err)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if os.Geteuid() == 0 {
		if err := syscall.Setfsuid(65534); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := syscall.Setfsuid(0); err != nil {
				t.Fatal(err)
			}
		}()
	}
	got, err := Discover(data, "synthetic")
	if !errors.Is(err, os.ErrPermission) || got != (Locator{}) {
		t.Fatalf("unreadable locator must preserve permission failure, got %#v %v", got, err)
	}
}

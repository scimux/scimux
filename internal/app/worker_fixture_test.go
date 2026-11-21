package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/scimux/scimux/internal/sessionworker"
	"github.com/scimux/scimux/internal/testowned/workerfixture"
)

// forcedWorkerFixture keeps the original direct wait owner across manager detach.
type forcedWorkerFixture struct {
	parent *workerfixture.Parent
	path   string
	entry  *workerEntry
	client *sessionworker.Client
}

func prepareForcedWorker(t *testing.T, m *workerManager) *forcedWorkerFixture {
	t.Helper()
	parent, path, err := workerfixture.New(filepath.Clean(os.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	f := &forcedWorkerFixture{parent: parent, path: path}
	m.startOptions.env = append(m.startOptions.env, "TMPDIR="+path)
	t.Cleanup(func() {
		if f.entry == nil || f.entry.process == nil {
			t.Error("worker fixture has no registered direct child")
			return
		}
		p := f.entry.process
		result := func() error { p.mu.Lock(); defer p.mu.Unlock(); return p.err }
		if err := f.parent.CleanupWait(p.wait, result, f.client.Close); err != nil {
			t.Error(err)
		}
	})
	return f
}

func (f *forcedWorkerFixture) register(t *testing.T, m *workerManager, node string) {
	t.Helper()
	entry := m.entry(node)
	if entry == nil || entry.process == nil {
		t.Fatal("worker fixture child unavailable")
	}
	locator, err := sessionworker.Discover(m.dataDir, node)
	if err != nil || locator.PID != entry.process.pid || locator.Identity != entry.identity {
		t.Fatal("worker fixture locator mismatch")
	}
	if err = f.parent.Register(locator.Link.Socket, entry.process.pid); err != nil {
		t.Fatal(err)
	}
	f.entry = entry
	f.client = entry.client
}

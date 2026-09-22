package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/storagebudget"
)

func TestManagedArchiveMovesShareTheBudgetLock(t *testing.T) {
	for _, kind := range []string{"session", "attachments", "assets"} {
		t.Run(kind, func(t *testing.T) {
			data := t.TempDir()
			a := &app{
				sessionsDir:    filepath.Join(data, "sessions"),
				attachmentsDir: filepath.Join(data, "attachments"),
				assetsDir:      filepath.Join(data, "assets"),
			}
			var src string
			var archive func()
			switch kind {
			case "session":
				src = filepath.Join(a.sessionsDir, "n1.jsonl")
				archive = func() { a.archiveSessionLog("n1") }
			case "attachments":
				src = filepath.Join(a.attachmentsDir, "n1", "upload.bin")
				archive = func() { a.archiveAttachments("n1") }
			case "assets":
				src = filepath.Join(a.assetsDir, "n1", "asset.bin")
				archive = func() { a.archiveAssets("n1") }
			}
			if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(src, []byte("managed bytes"), 0o600); err != nil {
				t.Fatal(err)
			}

			release, err := storagebudget.Lock(data)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			started := make(chan struct{})
			go func() {
				close(started)
				archive()
				close(done)
			}()
			<-started
			select {
			case <-done:
				release()
				t.Fatal("archive completed while the budget lock was held")
			case <-time.After(100 * time.Millisecond):
			}
			if _, err := os.Stat(src); err != nil {
				release()
				t.Fatalf("live path moved before lock release: %v", err)
			}
			release()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("archive remained blocked after budget lock release")
			}
			if _, err := os.Stat(src); !os.IsNotExist(err) {
				t.Fatalf("live path after archive: %v", err)
			}
		})
	}
}

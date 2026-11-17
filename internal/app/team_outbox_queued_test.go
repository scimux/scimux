package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/team"
)

func TestQueuedOutboxSnapshotCannotSendAfterStopping(t *testing.T) {
	box, dir, calls, _ := newShareBox(t, &outboxScript{})
	armActiveShare(t, dir, nil)
	old, ok := storedShare(t, dir, shareUID)
	if !ok {
		t.Fatal("missing synthetic share")
	}
	box.shareOps = &sync.Mutex{}
	box.files = &sync.Mutex{}
	box.shareOps.Lock()
	locked := true
	defer func() {
		if locked {
			box.shareOps.Unlock()
		}
	}()
	loaded := make(chan struct{})
	box.readNodes = func(dir string) ([]byte, error) {
		close(loaded) // tick has read shares already, before it waits for shareOps.
		return os.ReadFile(filepath.Join(dir, "nodes.jsonl"))
	}
	done := make(chan struct{})
	go func() { box.tick(context.Background()); close(done) }()
	select {
	case <-loaded:
	case <-time.After(3 * time.Second):
		t.Fatal("tick did not capture its pre-gate snapshot")
	}
	// The mutation ahead of this queued tick completes while owning shareOps.
	old.State = "stopping"
	mustSaveShare(t, dir, old)
	box.shareOps.Unlock()
	locked = false
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("queued tick did not finish")
	}
	for _, call := range snapshotCalls(calls) {
		if strings.HasSuffix(call.path, "/mirror") {
			t.Fatal("stale pre-gate active snapshot uploaded after the share became stopping")
		}
	}
}

func TestQueuedOutboxSnapshotSkipsMissingReplacedAndDamagedShares(t *testing.T) {
	for _, kind := range []string{"missing", "generation", "server", "node", "damaged"} {
		t.Run(kind, func(t *testing.T) {
			box, dir, calls, _ := newShareBox(t, &outboxScript{})
			armActiveShare(t, dir, nil)
			old, ok := storedShare(t, dir, shareUID)
			if !ok {
				t.Fatal("missing share")
			}
			box.shareOps = &sync.Mutex{}
			box.files = &sync.Mutex{}
			switch kind {
			case "missing":
				if err := team.RemoveShare(dir, shareUID); err != nil {
					t.Fatal(err)
				}
			case "generation":
				current := old
				current.Generation = "0000000000000000"
				mustSaveShare(t, dir, current)
			case "server":
				current := old
				current.Server = "00000000000000000000000000000000"
				mustSaveShare(t, dir, current)
			case "node":
				current := old
				current.Node = "another-node"
				mustSaveShare(t, dir, current)
			case "damaged":
				if err := os.WriteFile(filepath.Join(dir, team.SharesFile), []byte("damaged"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			box.one(context.Background(), nil, "", old, nil)
			if got := snapshotCalls(calls); len(got) != 0 {
				t.Fatalf("stale work made wire calls: %+v", got)
			}
			if kind == "damaged" {
				raw, err := os.ReadFile(filepath.Join(dir, team.SharesFile))
				if err != nil || string(raw) != "damaged" {
					t.Fatalf("damaged store altered: %q %v", raw, err)
				}
			}
		})
	}
}

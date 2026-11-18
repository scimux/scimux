package app

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/scimux/scimux/internal/sessionworker"
)

func TestDeleteRejectsStaleChatInstance(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.byID["n1"] = &Node{ID: "n1", InstanceID: "replacement", Title: "Replacement"}
	req := httptest.NewRequest("DELETE", "/api/nodes/n1?chat_instance=old", nil)
	req.SetPathValue("id", "n1")
	rec := httptest.NewRecorder()
	a.handleDeleteNode(rec, req)
	if rec.Code != 409 || a.byID["n1"] == nil {
		t.Fatalf("stale delete status=%d replacement=%v", rec.Code, a.byID["n1"])
	}
}

func TestLegacyNodeIdentityPersistsAcrossReload(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := a.appendRecord(storeRecord{Type: "node", Node: &Node{ID: "n1", Title: "Synthetic"}}); err != nil {
		t.Fatal(err)
	}
	read := func() string {
		b := newTestApp(t, &fakeTmux{})
		b.storePath = a.storePath
		if err := b.loadStore(); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(b.byID["n1"])
		if err != nil {
			t.Fatal(err)
		}
		var view map[string]any
		if err := json.Unmarshal(encoded, &view); err != nil {
			t.Fatal(err)
		}
		id, _ := view["instance_id"].(string)
		return id
	}
	first := read()
	if first == "" || read() != first {
		t.Fatal("legacy chat must retain its unique identity across reloads")
	}
}

func TestLegacyClosedNodeIdentityPersistsAcrossReload(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	old := &Node{ID: "legacy-closed", Title: "Synthetic legacy closed", Agent: "codex", EndedAt: "2000-01-01T00:00:00Z", SessionID: "synthetic-session"}
	if err := a.appendRecord(storeRecord{Type: "node", Node: old}); err != nil {
		t.Fatal(err)
	}
	read := func() *Node {
		b := newTestApp(t, &fakeTmux{})
		b.storePath = a.storePath
		if err := b.loadStore(); err != nil {
			t.Fatal(err)
		}
		return b.byID[old.ID]
	}
	first := read()
	if first == nil || first.InstanceID == "" {
		t.Fatalf("closed legacy chat lacks identity: %#v", first)
	}
	if first.EndedAt != old.EndedAt || first.SessionID != old.SessionID || first.Title != old.Title {
		t.Fatalf("migration changed closed record: %#v", first)
	}
	if next := read(); next == nil || next.InstanceID != first.InstanceID || next.EndedAt != old.EndedAt {
		t.Fatalf("identity did not survive reload: %#v", next)
	}
}

func TestLegacyClosedMigrationPreservesExistingIdentityAndErrors(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	old := &Node{ID: "ended", EndedAt: "2000-01-01T00:00:00Z"}
	known := &Node{ID: "known", InstanceID: "existing", EndedAt: old.EndedAt}
	a.nodes = []*Node{known, old}
	if err := a.migrateLegacyChatIdentities(func() (string, error) { return "", errors.New("entropy") }, func(storeRecord) error { t.Fatal("unexpected write"); return nil }); err == nil || old.InstanceID != "" || known.InstanceID != "existing" {
		t.Fatalf("allocation failure: %v", err)
	}
	if err := a.migrateLegacyChatIdentities(func() (string, error) { return "new", nil }, func(storeRecord) error { return errors.New("write") }); err == nil || old.InstanceID != "" || known.InstanceID != "existing" {
		t.Fatalf("persistence failure: %v", err)
	}
}

func TestRecoverWorkerIdentityFailureDoesNotPublish(t *testing.T) {
	data := ownedWorkerLockDir(t)
	state := sessionworker.State{HasSession: true, SessionID: "synthetic", Launch: &sessionworker.LaunchRequest{
		NodeID: "n1", Title: "Synthetic", Agent: "claude", Transport: "tmux", Dir: data, CreatedAt: "2026-01-01T00:00:00Z",
	}}
	_, closeWorker := publishRetirementWorker(t, data, "n1", state)
	t.Cleanup(closeWorker)
	a := newTestApp(t, &fakeTmux{})
	m := newWorkerManager("", data, "new")
	orig := randSource
	randSource = iotest.ErrReader(errors.New("synthetic entropy failure"))
	t.Cleanup(func() { randSource = orig })
	err := m.RecoverUnknown(a)
	if err == nil || !strings.Contains(err.Error(), "recover worker n1 identity") || a.byID["n1"] != nil || m.manages("n1") {
		t.Fatalf("recovery after identity failure: err=%v node=%v managed=%v", err, a.byID["n1"], m.manages("n1"))
	}
}

func TestChatIdentityAllocationFailureDoesNotPublishNode(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	orig := randSource
	randSource = iotest.ErrReader(errors.New("synthetic entropy failure"))
	t.Cleanup(func() { randSource = orig })
	n := &Node{Title: "Synthetic", Agent: "codex", Dir: a.home}
	status, _, err := a.createNode(n, nil)
	if status != 500 || err == nil || !strings.Contains(err.Error(), "allocate chat identity") || len(a.nodes) != 0 {
		t.Fatalf("failed identity allocation: status=%d err=%v nodes=%d", status, err, len(a.nodes))
	}
}

func TestLegacyChatIdentityAllocationFailureStopsReplay(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := a.appendRecord(storeRecord{Type: "node", Node: &Node{ID: "n1", Title: "Synthetic"}}); err != nil {
		t.Fatal(err)
	}
	b := newTestApp(t, &fakeTmux{})
	b.storePath = a.storePath
	orig := randSource
	randSource = iotest.ErrReader(errors.New("synthetic entropy failure"))
	t.Cleanup(func() { randSource = orig })
	if err := b.loadStore(); err == nil || !strings.Contains(err.Error(), "allocate legacy chat identity") {
		t.Fatalf("legacy identity failure = %v", err)
	}
}

func TestLegacyChatIdentityPersistenceFailureDoesNotPublishIdentity(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := &Node{ID: "n1", Title: "Synthetic"}
	a.nodes = []*Node{n}
	err := a.migrateLegacyChatIdentities(func() (string, error) { return "synthetic-id", nil }, func(storeRecord) error { return errors.New("synthetic write failure") })
	if err == nil || !strings.Contains(err.Error(), "persist legacy chat identity") || n.InstanceID != "" {
		t.Fatalf("legacy identity persistence failure: err=%v identity=%q", err, n.InstanceID)
	}
}

package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/sessionlog"
)

func compactChatGET(t *testing.T, a *app, id string) (map[string]any, string) {
	t.Helper()
	rec := chatGET(t, a, id, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("chat code = %d body %q", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body, rec.Header().Get("ETag")
}

func writeCompactMarker(t *testing.T, bundle, sid, trigger, at string) {
	t.Helper()
	if err := writeClaudePermFile(claudeCompactActivePath(bundle), claudeCompactMarker{
		SessionID: sid,
		Trigger:   trigger,
		At:        at,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestHandleChatReportsCompactingFromPreCompactMarker(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.live[n.ID] = "quiet"
	if err := RunClaudeCompactHook(bundle, bytes.NewReader(compactEventJSON("PreCompact", hookSIDOwn, "auto", nil)), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	body, etag1 := compactChatGET(t, a, n.ID)
	if body["compacting"] != true {
		t.Fatalf("compacting = %v, want true", body["compacting"])
	}
	if body["reply_ready"] != false {
		t.Fatalf("reply_ready = %v, want false during compaction", body["reply_ready"])
	}
	if body["attention"] != nil && body["attention"] != "" {
		t.Fatalf("attention = %v, compaction must not create attention", body["attention"])
	}
	if body["live"] != "quiet" {
		t.Fatalf("live = %v, compaction must not alter mechanical liveness", body["live"])
	}
	if body["fallback"] != false {
		t.Fatalf("fallback = %v", body["fallback"])
	}
	if body["supervision"] != string(claudeSupStrict) {
		t.Fatalf("supervision = %v, want strict", body["supervision"])
	}

	if err := RunClaudeCompactHook(bundle, bytes.NewReader(compactEventJSON("PostCompact", hookSIDOwn, "auto", nil)), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	body2, etag2 := compactChatGET(t, a, n.ID)
	if body2["compacting"] == true {
		t.Fatal("PostCompact must clear compacting on the next chat request")
	}
	if etag1 == "" || etag1 == etag2 {
		t.Fatalf("ETag must change when compaction ends: %q -> %q", etag1, etag2)
	}
}

func TestHandleChatCompactingForcesReplyReadyFalse(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}}
	a := newTestApp(t, f)
	tx := filepath.Join(t.TempDir(), "claude.jsonl")
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	appendLines(t, tx,
		`{"type":"user","timestamp":"`+stamp+`","message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","timestamp":"`+stamp+`","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"done"}]}}`)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	n.Transcript = tx
	a.live[n.ID] = "active"

	idle, _ := compactChatGET(t, a, n.ID)
	if idle["reply_ready"] != true {
		t.Fatalf("baseline reply_ready = %v, want true before compaction", idle["reply_ready"])
	}
	etagIdle := chatGET(t, a, n.ID, "").Header().Get("ETag")

	if err := RunClaudeCompactHook(bundle, bytes.NewReader(compactEventJSON("PreCompact", hookSIDOwn, "manual", nil)), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	busy, etagBusy := compactChatGET(t, a, n.ID)
	if busy["compacting"] != true {
		t.Fatalf("compacting = %v", busy["compacting"])
	}
	if busy["reply_ready"] != false {
		t.Fatal("reply_ready must be false while compacting, even after end_turn")
	}
	if busy["live"] != "active" {
		t.Fatalf("live = %v, want unchanged active", busy["live"])
	}
	if etagIdle == etagBusy {
		t.Fatal("ETag must change when compaction starts even if turns are unchanged")
	}
}

func TestHandleChatIgnoresNonCurrentCompactMarkers(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.live[n.ID] = "quiet"
	now := time.Now().UTC().Format(time.RFC3339Nano)

	assertQuiet := func(name string) {
		t.Helper()
		body, _ := compactChatGET(t, a, n.ID)
		if body["compacting"] == true {
			t.Fatalf("%s: compacting true, want ignored", name)
		}
	}

	writeCompactMarker(t, bundle, hookSIDSuccessor, "auto", now)
	assertQuiet("wrong session")

	if err := os.WriteFile(claudeCompactActivePath(bundle), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertQuiet("malformed")

	writeCompactMarker(t, bundle, hookSIDOwn, "auto", time.Now().Add(-31*time.Minute).UTC().Format(time.RFC3339Nano))
	assertQuiet("expired")

	writeCompactMarker(t, bundle, hookSIDOwn, "auto", now)
	n.EndedAt = time.Now().UTC().Format(time.RFC3339)
	assertQuiet("ended")
	n.EndedAt = ""

	a.live[n.ID] = "exited"
	assertQuiet("exited")
	a.live[n.ID] = "quiet"

	caps := mustRead(t, filepath.Join(bundle, "capabilities.json"))
	var doc map[string]any
	if err := json.Unmarshal(caps, &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc, "compact")
	stripped, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "capabilities.json"), stripped, 0o600); err != nil {
		t.Fatal(err)
	}
	a.noteClaudeStrictCapability(a.claudeHookID(n.ID))
	assertQuiet("unsupported")

	other := &Node{ID: "p1", Title: "p1", Agent: "pi", CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	a.nodes = append(a.nodes, other)
	a.byID[other.ID] = other
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	w := &sessionlog.Writer{Path: a.sessionLogPath(other.ID)}
	if err := w.Append(sessionlog.NewMeta(other.ID, "pi", "", "", a.home)); err != nil {
		t.Fatal(err)
	}
	body, _ := compactChatGET(t, a, other.ID)
	if body["compacting"] == true {
		t.Fatal("non-Claude chat must not report compacting")
	}
}

func TestClaudeSessionStartCompactStillIdentityPreserving(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cur := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn, claudeUserLine("keep", 0))
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	n.Transcript = cur
	n.Dir = "/w/proj"
	writeSeedLog(t, a, n.ID, "history")
	gen := a.claudeGeneration(n.ID)

	err := a.processClaudeHookEvent(n.ID, claudeSessionStartEvent{
		HookEventName: "SessionStart", Source: "compact",
		SessionID: hookSIDOwn, TranscriptPath: cur, Cwd: "/w/proj",
	})
	if err != nil {
		t.Fatalf("SessionStart compact: %v", err)
	}
	if n.Transcript != cur || n.SessionID != hookSIDOwn || a.claudeGeneration(n.ID) != gen {
		t.Fatalf("compact mutated node %+v gen=%d", n, a.claudeGeneration(n.ID))
	}
	if clearSeamCount(t, a, n.ID) != 0 {
		t.Fatal("SessionStart source compact must not create a clear seam")
	}
	if _, err := os.Stat(claudeCompactActivePath(bundle)); !os.IsNotExist(err) {
		t.Fatal("SessionStart compact must not write compact/active.json")
	}
	body, _ := compactChatGET(t, a, n.ID)
	if body["compacting"] == true {
		t.Fatal("SessionStart compact is binding, not PreCompact UX state")
	}
}

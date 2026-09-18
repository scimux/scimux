package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/sessionlog"
)

func elicitationChatGET(t *testing.T, a *app, id string) (map[string]any, string) {
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

func TestHandleChatReportsElicitationMetadata(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.live[n.ID] = "quiet"
	first := elicitationEventJSON("Elicitation", hookSIDOwn, "docs-server", "Need a token", map[string]any{
		"mode": "form", "elicitation_id": "elicit-a",
		"requested_schema": map[string]any{"type": "object"},
	})
	if err := RunClaudeElicitationHook(bundle, bytes.NewReader(first), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	second := elicitationEventJSON("Elicitation", hookSIDOwn, "auth", "Please authenticate", map[string]any{
		"mode": "url", "url": "https://auth.example.com/login", "elicitation_id": "elicit-b",
	})
	if err := RunClaudeElicitationHook(bundle, bytes.NewReader(second), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	body, etag1 := elicitationChatGET(t, a, n.ID)
	if body["elicitation_waiting"] != true {
		t.Fatalf("elicitation_waiting = %v", body["elicitation_waiting"])
	}
	if body["elicitation_count"] != float64(2) {
		t.Fatalf("elicitation_count = %v, want 2", body["elicitation_count"])
	}
	if body["reply_ready"] != false {
		t.Fatal("reply_ready must be false while elicitation is waiting")
	}
	items, _ := body["elicitations"].([]any)
	if len(items) != 2 {
		t.Fatalf("elicitations = %v", body["elicitations"])
	}
	firstItem, _ := items[0].(map[string]any)
	secondItem, _ := items[1].(map[string]any)
	if firstItem["server"] != "docs-server" || firstItem["message"] != "Need a token" || firstItem["mode"] != "form" {
		t.Fatalf("oldest first: %v", firstItem)
	}
	if _, ok := firstItem["url"]; ok {
		t.Fatalf("form item must not invent a url: %v", firstItem)
	}
	if secondItem["server"] != "auth" || secondItem["url"] != "https://auth.example.com/login" {
		t.Fatalf("url item = %v", secondItem)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if strings.Contains(s, "requested_schema") || strings.Contains(s, "elicit-a") || strings.Contains(s, "elicit-b") || strings.Contains(s, "claude-hooks") {
		t.Fatalf("chat leaked internal elicitation fields: %s", s)
	}
	if _, ok := body["perm_dialog_id"]; ok {
		t.Fatal("elicitation must not reuse perm_dialog_id")
	}

	stateRec := httptest.NewRecorder()
	a.handleState(stateRec, httptest.NewRequest("GET", "/api/state", nil))
	if strings.Contains(stateRec.Body.String(), "elicitation_waiting") {
		t.Fatal("/api/state must not carry elicitation fields")
	}

	post := elicitationEventJSON("ElicitationResult", hookSIDOwn, "docs-server", "", map[string]any{
		"action": "accept", "elicitation_id": "elicit-a",
	})
	if err := RunClaudeElicitationHook(bundle, bytes.NewReader(post), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	body2, etag2 := elicitationChatGET(t, a, n.ID)
	if body2["elicitation_count"] != float64(1) {
		t.Fatalf("after one result, count = %v", body2["elicitation_count"])
	}
	if etag1 == "" || etag1 == etag2 {
		t.Fatalf("ETag must change when elicitation changes: %q -> %q", etag1, etag2)
	}
}

func TestHandleChatIgnoresNonCurrentElicitations(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.live[n.ID] = "quiet"
	foreign := elicitationEventJSON("Elicitation", hookSIDSuccessor, "docs-server", "other session", map[string]any{"mode": "form"})
	if err := RunClaudeElicitationHook(bundle, bytes.NewReader(foreign), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	body, _ := elicitationChatGET(t, a, n.ID)
	if body["elicitation_waiting"] == true {
		t.Fatal("foreign-session elicitation must not appear on the current chat")
	}

	expired := claudeElicitationRecord{
		Nonce:      "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		SessionID:  hookSIDOwn,
		Server:     "docs-server",
		ServerHash: digestElicitationValue("docs-server"),
		Message:    "stale",
		Mode:       "form",
		At:         time.Now().Add(-5 * time.Hour).UTC().Format(time.RFC3339Nano),
	}
	if err := writeClaudePermFile(filepath.Join(bundle, "elicitation", "active", expired.Nonce+".json"), expired); err != nil {
		t.Fatal(err)
	}
	body, _ = elicitationChatGET(t, a, n.ID)
	if body["elicitation_waiting"] == true {
		t.Fatal("expired elicitation must not appear")
	}

	n.EndedAt = time.Now().UTC().Format(time.RFC3339)
	current := elicitationEventJSON("Elicitation", hookSIDOwn, "docs-server", "now", map[string]any{"mode": "form"})
	if err := RunClaudeElicitationHook(bundle, bytes.NewReader(current), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	body, _ = elicitationChatGET(t, a, n.ID)
	if body["elicitation_waiting"] == true {
		t.Fatal("ended node must not report elicitation")
	}
}

func TestHandleChatElicitationBoundsListAndStrings(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.live[n.ID] = "quiet"
	for i := 0; i < claudeElicitationListMax+2; i++ {
		body := elicitationEventJSON("Elicitation", hookSIDOwn, "docs-server", strings.Repeat("m", claudeElicitationMessageMax+50), map[string]any{
			"mode": "form", "elicitation_id": "id-" + strings.Repeat("x", i+1),
		})
		if err := RunClaudeElicitationHook(bundle, bytes.NewReader(body), io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	chat, _ := elicitationChatGET(t, a, n.ID)
	if chat["elicitation_count"] != float64(claudeElicitationListMax+2) {
		t.Fatalf("count = %v, want the unbounded current total", chat["elicitation_count"])
	}
	items, _ := chat["elicitations"].([]any)
	if len(items) != claudeElicitationListMax {
		t.Fatalf("displayed list = %d, want bound %d", len(items), claudeElicitationListMax)
	}
	first, _ := items[0].(map[string]any)
	msg, _ := first["message"].(string)
	if len(msg) > claudeElicitationMessageMax {
		t.Fatalf("message length %d exceeds bound", len(msg))
	}
}

func TestHandleChatElicitationDoesNotAppearOnNonClaude(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"p1": true}}
	a := newTestApp(t, f)
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
	body, _ := elicitationChatGET(t, a, other.ID)
	if body["elicitation_waiting"] == true {
		t.Fatal("non-Claude chat must not report elicitation")
	}
}

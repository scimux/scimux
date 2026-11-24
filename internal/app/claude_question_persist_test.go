package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStandingStoreBoundsRecordsAndPrunesOnlyResolvedQuestions(t *testing.T) {
	newRecord := func(t *testing.T, bundle string, session string) {
		t.Helper()
		model := claudeInteractionModel{Tool: "AskUserQuestion", Digest: strings.Repeat("a", 64)}
		if err := writeStandingQuestion(bundle, claudeInteractionRequest{ID: "incoming", SessionID: session}, model); err != nil {
			t.Fatal(err)
		}
	}
	fill := func(t *testing.T, bundle string, status string) string {
		t.Helper()
		dir := filepath.Join(bundle, "perm", "interact", "standing")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 256; i++ {
			st := claudeStandingQuestion{
				ID: fmt.Sprintf("record-%03d", i), SessionID: fmt.Sprintf("session-%03d", i),
				Tool: "AskUserQuestion", Digest: strings.Repeat("b", 64), Status: status,
			}
			if err := writeClaudePermFile(filepath.Join(dir, st.ID+".json"), st); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	full := t.TempDir()
	dir := fill(t, full, standingOutstanding)
	if err := writeStandingQuestion(full, claudeInteractionRequest{ID: "incoming", SessionID: "new-session"}, claudeInteractionModel{Tool: "AskUserQuestion", Digest: strings.Repeat("a", 64)}); !errors.Is(err, errInteractionIO) {
		t.Fatalf("full outstanding store accepted another question: %v", err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 256 {
		t.Fatalf("failed budget write changed prior records: entries=%d err=%v", len(entries), err)
	}

	resolved := t.TempDir()
	dir = fill(t, resolved, standingExternal)
	newRecord(t, resolved, "new-session")
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 256 {
		t.Fatalf("resolved entries were not reclaimed within cap: entries=%d err=%v", len(entries), err)
	}
}

// letterQuestionPane is a synthetic screen-reader AskUserQuestion menu.
// Labels match letterQuestionRaw, plus the trailing Other row Claude draws.
// It is not a copy of any live session.
const letterQuestionPane = "Pick a letter?\n\n  1. Alpha\n  2. Beta\n  3. Other\n\nEnter selection [1-3], or Escape to cancel:"

// TestOutstandingQuestionSurvivesHelperExpiryAndAskedTTL is the combined
// failure: the helper deadline and the asked-notice TTL both elapse, the
// chat is polled, and the same unanswered question must still be rendered
// with native choices taken from the current menu. No deadline is lengthened.
func TestOutstandingQuestionSurvivesHelperExpiryAndAskedTTL(t *testing.T) {
	if interactionHelperDeadline != 150*time.Second || interactionAnswerWindow != 135*time.Second || interactionHookTimeoutSec != 180 {
		t.Fatalf("deadlines changed helper=%s window=%s hook=%d", interactionHelperDeadline, interactionAnswerWindow, interactionHookTimeoutSec)
	}
	if claudeAskedTTL != 4*time.Hour || permHookDeadline != 5*time.Second {
		t.Fatalf("ttl=%s perm=%s", claudeAskedTTL, permHookDeadline)
	}

	f := &fakeTmux{list: []string{"q-persist"}, alive: map[string]bool{"q-persist": true}, capture: letterQuestionPane}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "q-persist", hookSIDOwn)
	n.AXScreenReader = true
	raw := letterQuestionRaw()
	publishSyntheticInteraction(t, bundle, "req-letter", "AskUserQuestion", raw, n.SessionID)
	sum := sha256.Sum256(raw)
	digest16 := hex.EncodeToString(sum[:])[:16]
	perm := filepath.Join(bundle, "perm")
	if err := writeClaudePermFile(claudeAskedPath(perm, "req-letter"), claudeAskedNotice{
		At: time.Now().UTC().Format(time.RFC3339Nano), Session: n.SessionID, Tool: "AskUserQuestion", Digest: digest16,
	}); err != nil {
		t.Fatal(err)
	}
	epoch, err := mintClaudeVisibleEpoch(perm, n.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	handler := newTestHandler(t, a)
	before := chatJSON(t, handler, n.ID)
	interaction, _ := before["claude_interaction"].(map[string]any)
	if interaction["request_id"] != "req-letter" || interaction["phase"] != "pending" || interaction["native_keys"] != false {
		t.Fatalf("before expiry = %#v", before["claude_interaction"])
	}
	if before["attention"] != "question" || before["perm_dialog_id"] != epoch.Epoch {
		t.Fatalf("before attention=%v dialog=%v", before["attention"], before["perm_dialog_id"])
	}

	// Past 135s, 150s, 180s, the 4h notice TTL, overnight, and several days.
	// Equality with a deadline is closed; these stamps are far on the closed side.
	closed := time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339Nano)
	rec, err := readInteractionRequest(bundle, "req-letter")
	if err != nil {
		t.Fatal(err)
	}
	rec.AnswerUntil = closed
	rec.HelperUntil = closed
	if err := writeClaudePermFile(filepath.Join(bundle, "perm", "interact", "req", "req-letter.json"), rec); err != nil {
		t.Fatal(err)
	}
	if err := retireExpiredInteractionPayloads(bundle, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(bundle, "perm", "interact", "req", "req-letter.json")); !os.IsNotExist(statErr) {
		t.Fatal("helper payload was not retired")
	}
	if err := writeClaudePermFile(claudeAskedPath(perm, "req-letter"), claudeAskedNotice{
		At: closed, Session: n.SessionID, Tool: "AskUserQuestion", Digest: digest16,
	}); err != nil {
		t.Fatal(err)
	}

	a.poll()
	after := chatJSON(t, handler, n.ID)
	got, _ := after["claude_interaction"].(map[string]any)
	if got["request_id"] != "req-letter" || got["tool"] != "AskUserQuestion" || got["phase"] != "native" || got["native_keys"] != true {
		t.Fatalf("after helper expiry and notice TTL, interaction = %#v attention=%v dialog=%v", after["claude_interaction"], after["attention"], after["perm_dialog_id"])
	}
	questions, _ := got["questions"].([]any)
	if len(questions) != 1 {
		t.Fatalf("questions = %#v", got["questions"])
	}
	q, _ := questions[0].(map[string]any)
	if q["text"] != "Pick a letter?" || q["id"] != "q1" {
		t.Fatalf("question = %#v", q)
	}
	if after["attention"] != "question" {
		t.Fatalf("attention = %v", after["attention"])
	}
	if after["perm_dialog_id"] != epoch.Epoch {
		t.Fatalf("perm_dialog_id = %v, want %s", after["perm_dialog_id"], epoch.Epoch)
	}
	choices, _ := got["native_choices"].([]any)
	if len(choices) != 2 {
		t.Fatalf("native_choices = %#v", got["native_choices"])
	}
	first, _ := choices[0].(map[string]any)
	second, _ := choices[1].(map[string]any)
	if first["question_id"] != "q1" || first["option_id"] != "q1o1" || first["key"] != "1" || first["label"] != "Alpha" {
		t.Fatalf("first choice = %#v", first)
	}
	if second["option_id"] != "q1o2" || second["key"] != "2" || second["label"] != "Beta" {
		t.Fatalf("second choice = %#v", second)
	}
	if got["delivery"] == "emitted" || got["phase"] == "expired" {
		t.Fatalf("expiry was presented as an answer: %#v", got)
	}
}

func chatJSON(t *testing.T, handler http.Handler, id string) map[string]any {
	t.Helper()
	rec := routeRequest(handler, http.MethodGet, "/api/nodes/"+id+"/chat", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat = %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

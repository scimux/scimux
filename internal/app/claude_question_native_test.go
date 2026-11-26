package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/sessionworker"
)

func setupStandingQuestion(t *testing.T, id, pane string, raw []byte) (*app, *fakeTmux, *Node, string, string) {
	t.Helper()
	if pane == "" {
		pane = letterQuestionPane
	}
	if raw == nil {
		raw = letterQuestionRaw()
	}
	f := &fakeTmux{list: []string{id}, alive: map[string]bool{id: true}, capture: pane}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, id, hookSIDOwn)
	n.AXScreenReader = true
	publishSyntheticInteraction(t, bundle, "req-letter", "AskUserQuestion", raw, n.SessionID)
	model := mustClassify(t, "AskUserQuestion", raw)
	perm := filepath.Join(bundle, "perm")
	if err := writeClaudePermFile(claudeAskedPath(perm, "req-letter"), claudeAskedNotice{
		At: time.Now().UTC().Format(time.RFC3339Nano), Session: n.SessionID, Tool: "AskUserQuestion", Digest: model.Digest[:16],
	}); err != nil {
		t.Fatal(err)
	}
	epoch, err := mintClaudeVisibleEpoch(perm, n.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	return a, f, n, bundle, epoch.Epoch
}

func retireQuestionPayload(t *testing.T, bundle, id string) {
	t.Helper()
	rec, err := readInteractionRequest(bundle, id)
	if err != nil {
		t.Fatal(err)
	}
	closed := time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339Nano)
	rec.AnswerUntil = closed
	rec.HelperUntil = closed
	if err := writeClaudePermFile(filepath.Join(bundle, "perm", "interact", "req", id+".json"), rec); err != nil {
		t.Fatal(err)
	}
	if err := retireExpiredInteractionPayloads(bundle, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func nativeBody(epoch, questionID, optionID string) string {
	body := map[string]any{
		"request_id": "req-letter",
		"action":     "native",
		"dialog_id":  epoch,
		"answers": []map[string]any{{
			"question_id": questionID,
			"option_ids":  []string{optionID},
		}},
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}

func keyTails(f *fakeTmux) [][]string {
	var out [][]string
	for _, call := range sendKeysCalls(f) {
		for i, arg := range call {
			if arg != "send-keys" {
				continue
			}
			rest := call[i+1:]
			if len(rest) >= 2 && rest[0] == "-t" {
				rest = rest[2:]
			}
			out = append(out, append([]string(nil), rest...))
		}
	}
	return out
}

func TestNativeSendReachesOpenDialogOnce(t *testing.T) {
	a, f, n, bundle, epoch := setupStandingQuestion(t, "q-native", "", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	attachInteractionWorker(t, a, n)
	handler := newTestHandler(t, a)

	first := postInteraction(handler, n.ID, nativeBody(epoch, "q1", "q1o1"), true)
	if first.Code != http.StatusAccepted {
		t.Fatalf("native = %d %s", first.Code, first.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["delivery"] != "sent" || result["emitted"] != false || result["phase"] != "sent" || result["request_id"] != "req-letter" {
		t.Fatalf("result = %#v", result)
	}
	if tails := keyTails(f); len(tails) != 1 || !reflect.DeepEqual(tails[0], []string{"1", "Enter"}) {
		t.Fatalf("keys = %#v", tails)
	}
	stood, ok := readStandingQuestion(bundle, "req-letter")
	if !ok || stood.Status != standingOutstanding || stood.SentEpoch != epoch || stood.SentKey != "1" {
		t.Fatalf("standing after send = %+v ok=%v", stood, ok)
	}
	after := chatJSON(t, handler, n.ID)
	got, _ := after["claude_interaction"].(map[string]any)
	if got["phase"] != "sent" || got["request_id"] != "req-letter" || after["attention"] != "question" {
		t.Fatalf("after send = %#v attention=%v", got, after["attention"])
	}
	if strings.Contains(strings.ToLower(first.Body.String()), "consum") {
		t.Fatalf("result claimed consumption: %s", first.Body.String())
	}

	second := postInteraction(handler, n.ID, nativeBody(epoch, "q1", "q1o2"), true)
	if second.Code != http.StatusConflict || len(keyTails(f)) != 1 {
		t.Fatalf("second = %d keys=%#v %s", second.Code, keyTails(f), second.Body.String())
	}

	perm := filepath.Join(bundle, "perm")
	if err := writeClaudePermFile(claudeAskedPath(perm, "req-letter"), claudeAskedNotice{
		At: time.Now().UTC().Format(time.RFC3339Nano), Session: n.SessionID, Tool: "AskUserQuestion", Digest: stood.Digest[:16],
	}); err != nil {
		t.Fatal(err)
	}
	next, err := mintClaudeVisibleEpoch(perm, n.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if next.Epoch == epoch {
		t.Fatal("replacement epoch reused the retired id")
	}
	again := postInteraction(handler, n.ID, nativeBody(next.Epoch, "q1", "q1o2"), true)
	if again.Code != http.StatusConflict || len(keyTails(f)) != 1 {
		t.Fatalf("repeated notification resent one question: %d keys=%#v %s", again.Code, keyTails(f), again.Body.String())
	}
}

func TestNativeRejectedWhileHelperOwnsTheQuestion(t *testing.T) {
	a, f, n, _, epoch := setupStandingQuestion(t, "q-owned", "", nil)
	attachInteractionWorker(t, a, n)
	rec := postInteraction(newTestHandler(t, a), n.ID, nativeBody(epoch, "q1", "q1o1"), true)
	if rec.Code != http.StatusConflict || len(keyTails(f)) != 0 {
		t.Fatalf("native while owned = %d keys=%#v %s", rec.Code, keyTails(f), rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "structured") {
		t.Fatalf("ownership error = %s", rec.Body.String())
	}
}

func TestStructuredAnswerAfterExpiryDoesNotDismiss(t *testing.T) {
	a, f, n, bundle, _ := setupStandingQuestion(t, "q-struct", "", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	attachInteractionWorker(t, a, n)
	handler := newTestHandler(t, a)
	rec := postInteraction(handler, n.ID, `{"request_id":"req-letter","action":"answer","answers":[{"question_id":"q1","option_ids":["q1o1"]}]}`, true)
	if rec.Code != http.StatusConflict || len(keyTails(f)) != 0 {
		t.Fatalf("structured after expiry = %d keys=%#v %s", rec.Code, keyTails(f), rec.Body.String())
	}
	after := chatJSON(t, handler, n.ID)
	got, _ := after["claude_interaction"].(map[string]any)
	if got["request_id"] != "req-letter" || got["phase"] != "native" {
		t.Fatalf("dismissed = %#v", got)
	}
}

func TestNativeRefusesUnsendableAnswers(t *testing.T) {
	a, f, n, bundle, epoch := setupStandingQuestion(t, "q-refuse", "", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	attachInteractionWorker(t, a, n)
	handler := newTestHandler(t, a)
	cases := []string{
		`{"request_id":"req-letter","action":"native","dialog_id":"` + epoch + `","answers":[{"question_id":"q1","free_text":"typed"}]}`,
		`{"request_id":"req-letter","action":"native","dialog_id":"` + epoch + `","answers":[{"question_id":"q1","option_ids":["q1o1","q1o2"]}]}`,
	}
	for _, body := range cases {
		rec := postInteraction(handler, n.ID, body, true)
		if rec.Code != http.StatusConflict || len(keyTails(f)) != 0 {
			t.Fatalf("body %s => %d keys=%#v %s", body, rec.Code, keyTails(f), rec.Body.String())
		}
	}
	got := chatJSON(t, handler, n.ID)
	interaction, _ := got["claude_interaction"].(map[string]any)
	if interaction["request_id"] != "req-letter" || interaction["phase"] != "native" {
		t.Fatalf("refusal dismissed the question: %#v", interaction)
	}
}

func TestNativeDoesNotAnswerADifferentDialog(t *testing.T) {
	a, f, n, bundle, epoch := setupStandingQuestion(t, "q-other", axPermissionPane, nil)
	retireQuestionPayload(t, bundle, "req-letter")
	attachInteractionWorker(t, a, n)
	handler := newTestHandler(t, a)
	rec := postInteraction(handler, n.ID, nativeBody(epoch, "q1", "q1o1"), true)
	if rec.Code != http.StatusConflict || len(keyTails(f)) != 0 {
		t.Fatalf("mismatch = %d keys=%#v %s", rec.Code, keyTails(f), rec.Body.String())
	}
	stood, ok := readStandingQuestion(bundle, "req-letter")
	if !ok || stood.Status != standingOutstanding || stood.SentEpoch != "" {
		t.Fatalf("mismatch mutated standing: %+v", stood)
	}
	key := routeRequest(handler, http.MethodPost, "/api/nodes/"+n.ID+"/key", `{"key":"1","dialog_id":"`+epoch+`"}`, true)
	if key.Code != http.StatusOK || len(keyTails(f)) != 1 || !reflect.DeepEqual(keyTails(f)[0], []string{"1", "Enter"}) {
		t.Fatalf("permission key = %d keys=%#v %s", key.Code, keyTails(f), key.Body.String())
	}
	stood, ok = readStandingQuestion(bundle, "req-letter")
	if !ok || stood.Status != standingOutstanding || stood.SentKey != "" {
		t.Fatalf("permission key was attributed to the question: %+v", stood)
	}
}

func TestNativeDoesNotMatchAnUnrelatedQuestionWithTheSameOptions(t *testing.T) {
	a, f, n, bundle, epoch := setupStandingQuestion(t, "q-same-options", "Another question?\n\n  1. Alpha\n  2. Beta\n  3. Other\n\nEnter selection [1-3], or Escape to cancel:", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	attachInteractionWorker(t, a, n)
	rec := postInteraction(newTestHandler(t, a), n.ID, nativeBody(epoch, "q1", "q1o1"), true)
	if rec.Code != http.StatusConflict || len(keyTails(f)) != 0 {
		t.Fatalf("same labels answered unrelated question = %d keys=%#v %s", rec.Code, keyTails(f), rec.Body.String())
	}
}

func TestNativeDoesNotExposeDuplicateOptionLabels(t *testing.T) {
	_, choices, block := matchStandingMenu([]claudeStandingQuestionItem{{
		ID: "q1", Options: []claudeStandingOption{{ID: "a", Label: "Same"}, {ID: "b", Label: "Same"}},
	}}, []PermOption{{Key: "1", Name: "Same"}, {Key: "2", Name: "Same"}})
	if block != "menu_mismatch" || len(choices) != 0 {
		t.Fatalf("ambiguous duplicate labels = block %q choices %+v", block, choices)
	}
}

func TestNativeDoesNotRaceAnAlreadyPublishedStructuredAnswer(t *testing.T) {
	a, f, n, bundle, epoch := setupStandingQuestion(t, "q-published-answer", "", nil)
	rec, err := readInteractionRequest(bundle, "req-letter")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	interactionNow = func() time.Time { return now }
	t.Cleanup(func() { interactionNow = time.Now })
	rec.AnswerUntil = now.Add(-time.Second).Format(time.RFC3339Nano)
	rec.HelperUntil = now.Add(time.Minute).Format(time.RFC3339Nano)
	if err := writeClaudePermFile(filepath.Join(bundle, "perm", "interact", "req", "req-letter.json"), rec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "perm", "interact", "ans", "req-letter.json"), []byte(`{"hookSpecificOutput":{"decision":{"behavior":"allow"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	attachInteractionWorker(t, a, n)
	answer := postInteraction(newTestHandler(t, a), n.ID, nativeBody(epoch, "q1", "q1o1"), true)
	if answer.Code == http.StatusAccepted || len(keyTails(f)) != 0 {
		t.Fatalf("native duplicated a published structured answer = %d keys=%#v %s", answer.Code, keyTails(f), answer.Body.String())
	}
}

func TestNativeCancelSendsEscapeOnly(t *testing.T) {
	a, f, n, bundle, epoch := setupStandingQuestion(t, "q-cancel", "", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	attachInteractionWorker(t, a, n)
	handler := newTestHandler(t, a)
	body := `{"request_id":"req-letter","action":"cancel","dialog_id":"` + epoch + `"}`
	rec := postInteraction(handler, n.ID, body, true)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("cancel = %d %s", rec.Code, rec.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["delivery"] != "sent" || result["emitted"] != false {
		t.Fatalf("cancel result = %#v", result)
	}
	if tails := keyTails(f); len(tails) != 1 || !reflect.DeepEqual(tails[0], []string{"Escape"}) {
		t.Fatalf("cancel keys = %#v", tails)
	}
	stood, ok := readStandingQuestion(bundle, "req-letter")
	if !ok || stood.Status != standingOutstanding || stood.SentKey != "Escape" {
		t.Fatalf("cancel standing = %+v", stood)
	}
}

func TestExpiredQuestionCanHandOffToExistingNativeControls(t *testing.T) {
	a, f, n, bundle, epoch := setupStandingQuestion(t, "q-handoff", "", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	attachInteractionWorker(t, a, n)
	handler := newTestHandler(t, a)
	rec := postInteraction(handler, n.ID, `{"request_id":"req-letter","action":"terminal"}`, true)
	if rec.Code != http.StatusAccepted || len(keyTails(f)) != 0 {
		t.Fatalf("handoff = %d keys=%#v %s", rec.Code, keyTails(f), rec.Body.String())
	}
	standing, ok := readStandingQuestion(bundle, "req-letter")
	if !ok || !standing.Manual || standing.Status != standingOutstanding {
		t.Fatalf("handoff did not retain the question: %+v", standing)
	}
	view, ok := a.claudeInteractionView(n)
	if !ok || view.Phase != "manual" || view.DialogID != epoch || !view.NativeKeys || !interactionNeedsAttention(view.Phase) {
		t.Fatalf("manual handoff view = %+v ok=%v", view, ok)
	}
	key := routeRequest(handler, http.MethodPost, "/api/nodes/"+n.ID+"/key", `{"key":"1","dialog_id":"`+epoch+`"}`, true)
	if key.Code != http.StatusOK || len(keyTails(f)) != 1 {
		t.Fatalf("existing native key after handoff = %d keys=%#v %s", key.Code, keyTails(f), key.Body.String())
	}
	if _, err := mintClaudeVisibleEpoch(filepath.Join(bundle, "perm"), n.SessionID); err != nil {
		t.Fatal(err)
	}
	view, ok = a.claudeInteractionView(n)
	if !ok || view.Phase != "sent" || view.NativeKeys || !interactionNeedsAttention(view.Phase) {
		t.Fatalf("native key hid standing question: %+v ok=%v", view, ok)
	}
}

func TestStaleEpochTurnAndGenerationAreRejected(t *testing.T) {
	a, f, n, bundle, epoch := setupStandingQuestion(t, "q-stale", "", nil)
	if err := writeClaudePermFile(filepath.Join(bundle, "perm", "turn.json"), map[string]any{"turn": "turn-n", "gen": 1}); err != nil {
		t.Fatal(err)
	}
	stood, ok := readStandingQuestion(bundle, "req-letter")
	if !ok {
		t.Fatal("missing standing")
	}
	stood.Turn = "turn-n"
	stood.TurnGen = 1
	if err := writeClaudePermFile(filepath.Join(bundle, "perm", "interact", "standing", "req-letter.json"), stood); err != nil {
		t.Fatal(err)
	}
	retireQuestionPayload(t, bundle, "req-letter")
	attachInteractionWorker(t, a, n)
	handler := newTestHandler(t, a)

	staleEpoch := postInteraction(handler, n.ID, nativeBody("not-the-epoch", "q1", "q1o1"), true)
	if staleEpoch.Code != http.StatusConflict || len(keyTails(f)) != 0 {
		t.Fatalf("stale epoch = %d keys=%#v %s", staleEpoch.Code, keyTails(f), staleEpoch.Body.String())
	}
	if err := writeClaudePermFile(filepath.Join(bundle, "perm", "turn.json"), map[string]any{"turn": "turn-n-plus", "gen": 2}); err != nil {
		t.Fatal(err)
	}
	staleTurn := postInteraction(handler, n.ID, nativeBody(epoch, "q1", "q1o1"), true)
	if staleTurn.Code != http.StatusConflict || len(keyTails(f)) != 0 || !strings.Contains(staleTurn.Body.String(), "turn") {
		t.Fatalf("stale turn = %d keys=%#v %s", staleTurn.Code, keyTails(f), staleTurn.Body.String())
	}
	if err := writeClaudePermFile(filepath.Join(bundle, "perm", "turn.json"), map[string]any{"turn": "turn-n", "gen": 1}); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.claudeGens[n.ID] = a.claudeGens[n.ID] + 1
	a.mu.Unlock()
	staleGen := postInteraction(handler, n.ID, nativeBody(epoch, "q1", "q1o1"), true)
	if staleGen.Code != http.StatusConflict || len(keyTails(f)) != 0 {
		t.Fatalf("stale generation = %d keys=%#v %s", staleGen.Code, keyTails(f), staleGen.Body.String())
	}
}

func TestTranscriptRetirementAndTurnResetAreExplicit(t *testing.T) {
	a, _, n, bundle, _ := setupStandingQuestion(t, "q-retire", "", nil)
	perm := filepath.Join(bundle, "perm")
	if !retireOldestAskedNotice(perm, time.Now().Add(time.Second), time.Now()) {
		t.Fatal("notice was not retired")
	}
	stood, ok := readStandingQuestion(bundle, "req-letter")
	if !ok || stood.Status != standingExternal {
		t.Fatalf("external = %+v ok=%v", stood, ok)
	}
	view, viewOK := a.claudeInteractionView(n)
	if !viewOK || view.Phase != "external" || interactionNeedsAttention(view.Phase) {
		t.Fatalf("external view = %+v ok=%v", view, viewOK)
	}

	a2, _, n2, bundle2, _ := setupStandingQuestion(t, "q-reset", "", nil)
	a2.resetClaudePermissionTurn(n2)
	reset, ok := readStandingQuestion(bundle2, "req-letter")
	if !ok || reset.Status != standingInvalidated {
		t.Fatalf("invalidated = %+v ok=%v", reset, ok)
	}
	invalid, viewOK := a2.claudeInteractionView(n2)
	if !viewOK || invalid.Phase != "invalidated" || interactionNeedsAttention(invalid.Phase) {
		t.Fatalf("invalidated view = %+v ok=%v", invalid, viewOK)
	}
}

func TestReplacedQuestionIsInvalidatedAndAmbiguousMenusRefuse(t *testing.T) {
	a, f, n, bundle, _ := setupStandingQuestion(t, "q-replace", "", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	publishSyntheticInteraction(t, bundle, "req-next", "AskUserQuestion", letterQuestionRaw(), n.SessionID)
	first, ok := readStandingQuestion(bundle, "req-letter")
	if !ok || first.Status != standingInvalidated || first.Reason != "replaced" {
		t.Fatalf("replaced = %+v ok=%v", first, ok)
	}
	view, viewOK := a.claudeInteractionView(n)
	if !viewOK || view.RequestID != "req-next" || view.Phase != "pending" {
		t.Fatalf("replacement view = %+v ok=%v", view, viewOK)
	}

	same := []byte(`{"questions":[{"question":"First?","header":"A","options":[{"label":"Alpha","description":""},{"label":"Beta","description":""}],"multiSelect":false},{"question":"Second?","header":"B","options":[{"label":"Alpha","description":""},{"label":"Beta","description":""}],"multiSelect":false}]}`)
	a3, f3, n3, bundle3, epoch3 := setupStandingQuestion(t, "q-ambiguous", letterQuestionPane, same)
	retireQuestionPayload(t, bundle3, "req-letter")
	attachInteractionWorker(t, a3, n3)
	rec := postInteraction(newTestHandler(t, a3), n3.ID, nativeBody(epoch3, "q1", "q1o1"), true)
	if rec.Code != http.StatusConflict || len(keyTails(f3)) != 0 || !strings.Contains(rec.Body.String(), "menu_mismatch") {
		t.Fatalf("ambiguous = %d keys=%#v %s", rec.Code, keyTails(f3), rec.Body.String())
	}
	_ = f
}

func TestPlanAndOrdinaryNoticesStillExpire(t *testing.T) {
	f := &fakeTmux{list: []string{"q-plan"}, alive: map[string]bool{"q-plan": true}, capture: axPermissionPane}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "q-plan", hookSIDOwn)
	publishSyntheticInteraction(t, bundle, "req-plan", "ExitPlanMode", tinyPlanRaw(), n.SessionID)
	if _, err := os.Stat(filepath.Join(bundle, "perm", "interact", "standing", "req-plan.json")); !os.IsNotExist(err) {
		t.Fatal("plan wrote a standing question")
	}
	retireQuestionPayload(t, bundle, "req-plan")
	if _, ok := a.claudeInteractionView(n); ok {
		t.Fatal("expired plan stayed in the view")
	}
	perm := filepath.Join(bundle, "perm")
	old := time.Now().Add(-5 * time.Hour).UTC().Format(time.RFC3339Nano)
	if err := writeClaudePermFile(claudeAskedPath(perm, "bash-old"), claudeAskedNotice{
		At: old, Session: n.SessionID, Tool: "Bash", Digest: "abcdef0123456789",
	}); err != nil {
		t.Fatal(err)
	}
	if notes := readClaudeAskedFiles(perm, time.Now()); len(notes) != 0 {
		t.Fatalf("bash notice outlived 4h: %+v", notes)
	}
}

func TestRecoveryKeepsQuestionWithoutClaimingHelperDelivery(t *testing.T) {
	a, f, n, bundle, epoch := setupStandingQuestion(t, "q-recover", "", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	recoverClaudeInteractionBundle(bundle, time.Now().Add(72*time.Hour))
	view, ok := a.claudeInteractionView(n)
	if !ok || view.Phase != "native" || view.RequestID != "req-letter" || len(view.NativeChoices) != 2 {
		t.Fatalf("recovered view = %+v ok=%v", view, ok)
	}
	attachInteractionWorker(t, a, n)
	rec := postInteraction(newTestHandler(t, a), n.ID, `{"request_id":"req-letter","action":"answer","answers":[{"question_id":"q1","option_ids":["q1o1"]}]}`, true)
	if rec.Code != http.StatusConflict || len(keyTails(f)) != 0 {
		t.Fatalf("recovery claimed helper delivery = %d keys=%#v %s", rec.Code, keyTails(f), rec.Body.String())
	}
	native := postInteraction(newTestHandler(t, a), n.ID, nativeBody(epoch, "q1", "q1o1"), true)
	if native.Code != http.StatusAccepted || len(keyTails(f)) != 1 {
		t.Fatalf("recovered native = %d keys=%#v %s", native.Code, keyTails(f), native.Body.String())
	}
}

func TestQuestionViewAtDeadlineInstants(t *testing.T) {
	a, _, n, bundle, _ := setupStandingQuestion(t, "q-bound", "", nil)
	rec, err := readInteractionRequest(bundle, "req-letter")
	if err != nil {
		t.Fatal(err)
	}
	answerAt := parseInteractionTime(rec.AnswerUntil)
	helperAt := parseInteractionTime(rec.HelperUntil)
	hookAt := parseInteractionTime(rec.At).Add(time.Duration(interactionHookTimeoutSec) * time.Second)
	prev := interactionNow
	t.Cleanup(func() { interactionNow = prev })

	interactionNow = func() time.Time { return answerAt.Add(-time.Nanosecond) }
	before, ok := a.claudeInteractionView(n)
	if !ok || before.Phase != "pending" || before.NativeKeys {
		t.Fatalf("before answer deadline = %+v ok=%v", before, ok)
	}
	interactionNow = func() time.Time { return answerAt }
	atAnswer, ok := a.claudeInteractionView(n)
	if !ok || atAnswer.Phase != "native" || !atAnswer.NativeKeys || len(atAnswer.NativeChoices) != 2 {
		t.Fatalf("at answer deadline = %+v ok=%v", atAnswer, ok)
	}
	interactionNow = func() time.Time { return helperAt }
	atHelper, ok := a.claudeInteractionView(n)
	if !ok || atHelper.Phase != "native" || len(atHelper.NativeChoices) != 2 {
		t.Fatalf("at helper deadline = %+v ok=%v", atHelper, ok)
	}
	interactionNow = func() time.Time { return hookAt }
	atHook, ok := a.claudeInteractionView(n)
	if !ok || atHook.Phase != "native" || atHook.RequestID != "req-letter" {
		t.Fatalf("at hook timeout = %+v ok=%v", atHook, ok)
	}
	interactionNow = func() time.Time { return helperAt.Add(72 * time.Hour) }
	later, ok := a.claudeInteractionView(n)
	if !ok || later.Phase != "native" || len(later.NativeChoices) != 2 {
		t.Fatalf("days later = %+v ok=%v", later, ok)
	}
	if interactionAnswerWindow != 135*time.Second || interactionHelperDeadline != 150*time.Second || claudeAskedTTL != 4*time.Hour {
		t.Fatal("a boundary test lengthened a deadline")
	}
}

func TestNativeAuditFailureSendsNothing(t *testing.T) {
	a, f, n, bundle, epoch := setupStandingQuestion(t, "q-audit", "", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	attachInteractionWorker(t, a, n)
	a.sessionsDir = ""
	rec := postInteraction(newTestHandler(t, a), n.ID, nativeBody(epoch, "q1", "q1o1"), true)
	if rec.Code == http.StatusAccepted || len(keyTails(f)) != 0 {
		t.Fatalf("audit failure = %d keys=%#v %s", rec.Code, keyTails(f), rec.Body.String())
	}
	stood, ok := readStandingQuestion(bundle, "req-letter")
	if !ok || stood.Status != standingOutstanding || stood.SentEpoch != "" {
		t.Fatalf("audit failure mutated standing: %+v", stood)
	}
}

func TestCaptureAndSendFailuresKeepTheQuestion(t *testing.T) {
	a, f, n, bundle, epoch := setupStandingQuestion(t, "q-fail", "", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	attachInteractionWorker(t, a, n)
	handler := newTestHandler(t, a)
	f.captureErr = true
	miss := postInteraction(handler, n.ID, nativeBody(epoch, "q1", "q1o1"), true)
	if miss.Code == http.StatusAccepted || len(keyTails(f)) != 0 {
		t.Fatalf("capture failure = %d keys=%#v %s", miss.Code, keyTails(f), miss.Body.String())
	}
	f.captureErr = false
	f.sendKeysErr = true
	broken := postInteraction(handler, n.ID, nativeBody(epoch, "q1", "q1o1"), true)
	if broken.Code == http.StatusAccepted {
		t.Fatalf("send failure reported success: %s", broken.Body.String())
	}
	stood, ok := readStandingQuestion(bundle, "req-letter")
	if !ok || stood.Status != standingOutstanding || stood.SentEpoch != epoch || stood.SentKey == "" || stood.Delivery != "unconfirmed" {
		t.Fatalf("send failure did not retain its no-replay uncertainty: %+v", stood)
	}
	view, viewOK := a.claudeInteractionView(n)
	if !viewOK || view.Phase != "unconfirmed" || view.RequestID != "req-letter" || !interactionNeedsAttention(view.Phase) {
		t.Fatalf("send failure lost its persistent uncertainty view: %+v", view)
	}
}

func TestNativeRejectsMalformedSelection(t *testing.T) {
	a, f, n, bundle, epoch := setupStandingQuestion(t, "q-malformed", "", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	attachInteractionWorker(t, a, n)
	handler := newTestHandler(t, a)
	missingDialog := postInteraction(handler, n.ID, `{"request_id":"req-letter","action":"native","answers":[{"question_id":"q1","option_ids":["q1o1"]}]}`, true)
	if missingDialog.Code != http.StatusBadRequest || len(keyTails(f)) != 0 {
		t.Fatalf("missing dialog = %d keys=%#v %s", missingDialog.Code, keyTails(f), missingDialog.Body.String())
	}
	emptyAnswer := postInteraction(handler, n.ID, `{"request_id":"req-letter","action":"native","dialog_id":"`+epoch+`","answers":[]}`, true)
	if emptyAnswer.Code != http.StatusBadRequest || len(keyTails(f)) != 0 {
		t.Fatalf("empty answer = %d keys=%#v %s", emptyAnswer.Code, keyTails(f), emptyAnswer.Body.String())
	}
	emptyOption := postInteraction(handler, n.ID, `{"request_id":"req-letter","action":"native","dialog_id":"`+epoch+`","answers":[{"question_id":"q1","option_ids":[""]}]}`, true)
	if emptyOption.Code != http.StatusBadRequest || len(keyTails(f)) != 0 {
		t.Fatalf("empty option = %d keys=%#v %s", emptyOption.Code, keyTails(f), emptyOption.Body.String())
	}
}

func TestNativeTargetsTheOneMatchingQuestion(t *testing.T) {
	a, f, n, bundle, epoch := setupStandingQuestion(t, "q-one", letterQuestionPane, twoQuestionRaw())
	retireQuestionPayload(t, bundle, "req-letter")
	attachInteractionWorker(t, a, n)
	handler := newTestHandler(t, a)
	other := postInteraction(handler, n.ID, nativeBody(epoch, "q2", "q2o1"), true)
	if other.Code != http.StatusConflict || len(keyTails(f)) != 0 || !strings.Contains(other.Body.String(), "menu_mismatch") {
		t.Fatalf("other question = %d keys=%#v %s", other.Code, keyTails(f), other.Body.String())
	}
	unknown := postInteraction(handler, n.ID, nativeBody(epoch, "q1", "q1o9"), true)
	if unknown.Code != http.StatusConflict || len(keyTails(f)) != 0 || !strings.Contains(unknown.Body.String(), "menu_mismatch") {
		t.Fatalf("unknown option = %d keys=%#v %s", unknown.Code, keyTails(f), unknown.Body.String())
	}
	match := postInteraction(handler, n.ID, nativeBody(epoch, "q1", "q1o1"), true)
	if match.Code != http.StatusAccepted || len(keyTails(f)) != 1 || !reflect.DeepEqual(keyTails(f)[0], []string{"1", "Enter"}) {
		t.Fatalf("matching question = %d keys=%#v %s", match.Code, keyTails(f), match.Body.String())
	}
}

func TestMultiSelectCannotBeSentOrCancelled(t *testing.T) {
	const pane = "Pick colors?\n\n  1. Red\n  2. Green\n  3. Blue\n\nEnter selection [1-3], or Escape to cancel:"
	a, f, n, bundle, epoch := setupStandingQuestion(t, "q-multi", pane, colorQuestionRaw())
	retireQuestionPayload(t, bundle, "req-letter")
	attachInteractionWorker(t, a, n)
	handler := newTestHandler(t, a)
	answer := postInteraction(handler, n.ID, nativeBody(epoch, "q1", "q1o1"), true)
	if answer.Code != http.StatusConflict || len(keyTails(f)) != 0 || !strings.Contains(answer.Body.String(), "multi") {
		t.Fatalf("multi answer = %d keys=%#v %s", answer.Code, keyTails(f), answer.Body.String())
	}
	cancel := postInteraction(handler, n.ID, `{"request_id":"req-letter","action":"cancel","dialog_id":"`+epoch+`"}`, true)
	if cancel.Code != http.StatusConflict || len(keyTails(f)) != 0 || !strings.Contains(cancel.Body.String(), "multi") {
		t.Fatalf("multi cancel = %d keys=%#v %s", cancel.Code, keyTails(f), cancel.Body.String())
	}
	stood, ok := readStandingQuestion(bundle, "req-letter")
	if !ok || stood.Status != standingOutstanding || stood.SentEpoch != "" {
		t.Fatalf("multi mutated standing: %+v", stood)
	}
}

func TestTranscriptEvidenceDoesNotGuessAndCancelStaysExplicit(t *testing.T) {
	a, f, n, bundle, epoch := setupStandingQuestion(t, "q-evidence", "", nil)
	perm := filepath.Join(bundle, "perm")
	stood, ok := readStandingQuestion(bundle, "req-letter")
	if !ok {
		t.Fatal("missing standing")
	}
	if err := writeClaudePermFile(claudeAskedPath(perm, "bash-old"), claudeAskedNotice{
		At: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), Session: n.SessionID, Tool: "Bash", Digest: "abcdef0123456789",
	}); err != nil {
		t.Fatal(err)
	}
	noteStandingExternal(perm, claudeAskedNotice{Tool: "AskUserQuestion", Session: n.SessionID})
	noteStandingExternal(perm, claudeAskedNotice{Tool: "AskUserQuestion", Session: n.SessionID, Digest: "0000000000000000"})
	noteStandingExternal(perm, claudeAskedNotice{Tool: "AskUserQuestion", Session: "other-session", Digest: stood.Digest[:16]})
	if nRetired := retireAskedByToolEvidence(perm, []toolEvidence{{
		Tool: "Bash", Digest: "abcdef0123456789", At: time.Now(),
	}}, time.Now()); nRetired != 1 {
		t.Fatalf("bash retire = %d", nRetired)
	}
	stood, ok = readStandingQuestion(bundle, "req-letter")
	if !ok || stood.Status != standingOutstanding {
		t.Fatalf("bash evidence touched the question: %+v", stood)
	}
	if nRetired := retireAskedByToolEvidence(perm, []toolEvidence{{
		Tool: "AskUserQuestion", Digest: stood.Digest[:16], At: time.Now().Add(time.Second),
	}}, time.Now().Add(time.Second)); nRetired != 1 {
		t.Fatalf("question retire = %d", nRetired)
	}
	stood, ok = readStandingQuestion(bundle, "req-letter")
	if !ok || stood.Status != standingExternal || stood.Reason != "transcript" {
		t.Fatalf("external = %+v", stood)
	}
	attachInteractionWorker(t, a, n)
	cancel := postInteraction(newTestHandler(t, a), n.ID, `{"request_id":"req-letter","action":"cancel","dialog_id":"`+epoch+`"}`, true)
	if cancel.Code != http.StatusConflict || len(keyTails(f)) != 0 {
		t.Fatalf("cancel after external = %d keys=%#v %s", cancel.Code, keyTails(f), cancel.Body.String())
	}
}

func TestExternalStatusSurvivesPayloadRetirement(t *testing.T) {
	a, _, n, bundle, _ := setupStandingQuestion(t, "q-external-view", "", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	perm := filepath.Join(bundle, "perm")
	if !retireOldestAskedNotice(perm, time.Now().Add(time.Second), time.Now()) {
		t.Fatal("notice was not retired")
	}
	view, ok := a.claudeInteractionView(n)
	if !ok || view.Phase != "external" || view.RequestID != "req-letter" || interactionNeedsAttention(view.Phase) {
		t.Fatalf("external after retirement = %+v ok=%v", view, ok)
	}
}

func TestTwoOutstandingQuestionsAreNotGuessed(t *testing.T) {
	a, _, n, bundle, _ := setupStandingQuestion(t, "q-two", "", nil)
	stood, ok := readStandingQuestion(bundle, "req-letter")
	if !ok {
		t.Fatal("missing standing")
	}
	other := stood
	other.ID = "req-other"
	if err := writeClaudePermFile(filepath.Join(bundle, "perm", "interact", "standing", "req-other.json"), other); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(bundle, "perm", "interact", "active.json")); err != nil {
		t.Fatal(err)
	}
	view, ok := a.claudeInteractionView(n)
	if !ok || view.Phase != "ambiguous" || view.Block != "multiple_questions" || len(view.Questions) != 2 || !interactionNeedsAttention(view.Phase) {
		t.Fatalf("ambiguous questions disappeared or were guessed: %+v ok=%v", view, ok)
	}
	foreign := stood
	foreign.ID = "req-foreign"
	foreign.SessionID = "other-session"
	if err := writeClaudePermFile(filepath.Join(bundle, "perm", "interact", "standing", "req-foreign.json"), foreign); err != nil {
		t.Fatal(err)
	}
	if _, viewOK := standingVisibleForSession(bundle, "other-session"); !viewOK {
		t.Fatal("the other session lost its own question")
	}
	if view, viewOK := a.claudeInteractionView(n); !viewOK || view.Phase != "ambiguous" || len(view.Questions) != 2 {
		t.Fatalf("another session changed the local ambiguity: %+v ok=%v", view, viewOK)
	}
}

func TestReplacedReasonSurvivesSessionInvalidation(t *testing.T) {
	_, _, n, bundle, _ := setupStandingQuestion(t, "q-reason", "", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	publishSyntheticInteraction(t, bundle, "req-next", "AskUserQuestion", letterQuestionRaw(), n.SessionID)
	invalidateStandingSession(filepath.Join(bundle, "perm"), n.SessionID, "session")
	first, ok := readStandingQuestion(bundle, "req-letter")
	if !ok || first.Status != standingInvalidated || first.Reason != "replaced" {
		t.Fatalf("replaced reason = %+v", first)
	}
	next, ok := readStandingQuestion(bundle, "req-next")
	if !ok || next.Status != standingInvalidated || next.Reason != "session" {
		t.Fatalf("session invalidation = %+v", next)
	}
}

func TestSameEpochCannotBeSentTwice(t *testing.T) {
	a, f, n, bundle, epoch := setupStandingQuestion(t, "q-epoch", "", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	attachInteractionWorker(t, a, n)
	handler := newTestHandler(t, a)
	first := postInteraction(handler, n.ID, nativeBody(epoch, "q1", "q1o1"), true)
	if first.Code != http.StatusAccepted || len(keyTails(f)) != 1 {
		t.Fatalf("first = %d keys=%#v %s", first.Code, keyTails(f), first.Body.String())
	}
	perm := filepath.Join(bundle, "perm")
	if err := writeClaudePermFile(claudeVisibleEpochPath(perm), claudeVisibleEpoch{
		Epoch: epoch, Session: n.SessionID, At: time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	again := postInteraction(handler, n.ID, nativeBody(epoch, "q1", "q1o2"), true)
	if again.Code != http.StatusConflict || len(keyTails(f)) != 1 || !strings.Contains(again.Body.String(), "already sent") {
		t.Fatalf("restored epoch = %d keys=%#v %s", again.Code, keyTails(f), again.Body.String())
	}
}

func TestNativeRelockRefusesAChangedMenuOrSequence(t *testing.T) {
	a, f, n, bundle, epoch := setupStandingQuestion(t, "q-relock", "", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	attachInteractionWorker(t, a, n)
	handler := newTestHandler(t, a)
	standingNativeRelockPause = func(*Node) { f.capture = axPermissionPane }
	t.Cleanup(func() { standingNativeRelockPause = nil })
	changed := postInteraction(handler, n.ID, nativeBody(epoch, "q1", "q1o1"), true)
	if changed.Code != http.StatusConflict || len(keyTails(f)) != 0 || !strings.Contains(changed.Body.String(), "menu_mismatch") {
		t.Fatalf("changed menu = %d keys=%#v %s", changed.Code, keyTails(f), changed.Body.String())
	}
	f.capture = letterQuestionPane
	standingNativeRelockPause = func(node *Node) { node.AXScreenReader = false }
	sequence := postInteraction(handler, n.ID, nativeBody(epoch, "q1", "q1o1"), true)
	if sequence.Code != http.StatusConflict || len(keyTails(f)) != 0 || !strings.Contains(sequence.Body.String(), "menu_mismatch") {
		t.Fatalf("changed sequence = %d keys=%#v %s", sequence.Code, keyTails(f), sequence.Body.String())
	}
	stood, ok := readStandingQuestion(bundle, "req-letter")
	if !ok || stood.SentEpoch != "" {
		t.Fatalf("relock consumed the question: %+v", stood)
	}
}

func TestNativePostSendFailuresDoNotClaimSuccess(t *testing.T) {
	a, f, n, bundle, epoch := setupStandingQuestion(t, "q-postsend", "", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	attachInteractionWorker(t, a, n)
	handler := newTestHandler(t, a)
	standingNativeAfterSend = func() { a.sessionsDir = "" }
	t.Cleanup(func() { standingNativeAfterSend = nil })
	audited := postInteraction(handler, n.ID, nativeBody(epoch, "q1", "q1o1"), true)
	if audited.Code == http.StatusAccepted || len(keyTails(f)) != 1 || !strings.Contains(audited.Body.String(), "key was sent") {
		t.Fatalf("post-send audit = %d keys=%#v %s", audited.Code, keyTails(f), audited.Body.String())
	}
	stood, ok := readStandingQuestion(bundle, "req-letter")
	if !ok || stood.SentEpoch != epoch || stood.Status != standingOutstanding {
		t.Fatalf("post-send audit lost the sent epoch: %+v", stood)
	}

	a2, f2, n2, bundle2, epoch2 := setupStandingQuestion(t, "q-write", "", nil)
	retireQuestionPayload(t, bundle2, "req-letter")
	attachInteractionWorker(t, a2, n2)
	path := filepath.Join(bundle2, "perm", "interact", "standing", "req-letter.json")
	standingNativeAfterSend = func() {
		if err := os.Remove(path); err != nil {
			t.Errorf("remove standing: %v", err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Errorf("replace standing: %v", err)
		}
	}
	broken := postInteraction(newTestHandler(t, a2), n2.ID, nativeBody(epoch2, "q1", "q1o1"), true)
	if broken.Code == http.StatusAccepted || len(keyTails(f2)) != 1 || !strings.Contains(broken.Body.String(), "key was sent") {
		t.Fatalf("standing write = %d keys=%#v %s", broken.Code, keyTails(f2), broken.Body.String())
	}
	if ep, ok := readClaudeVisibleEpoch(filepath.Join(bundle2, "perm")); ok {
		t.Fatalf("failed standing write left the epoch reusable: %+v", ep)
	}
}

func TestNativeFenceFailureSendsNothing(t *testing.T) {
	a, f, n, bundle, epoch := setupStandingQuestion(t, "q-fence", "", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	attachInteractionWorker(t, a, n)
	fence := filepath.Join(bundle, "perm", "interact", "fence.lock")
	if err := os.Remove(fence); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Mkdir(fence, 0o700); err != nil {
		t.Fatal(err)
	}
	rec := postInteraction(newTestHandler(t, a), n.ID, nativeBody(epoch, "q1", "q1o1"), true)
	if rec.Code == http.StatusAccepted || len(keyTails(f)) != 0 {
		t.Fatalf("fence = %d keys=%#v %s", rec.Code, keyTails(f), rec.Body.String())
	}
	stood, ok := readStandingQuestion(bundle, "req-letter")
	if !ok || stood.SentEpoch != "" {
		t.Fatalf("fence failure consumed the question: %+v", stood)
	}
}

func TestRequestWithoutStandingFileStillDisappears(t *testing.T) {
	a := newTestApp(t, &fakeTmux{list: []string{"q-old"}, alive: map[string]bool{"q-old": true}})
	n, bundle := seedPermClaude(t, a, "q-old", hookSIDOwn)
	past := time.Now().Add(-72 * time.Hour)
	rec := sampleRequest(bundle, past)
	rec.AnswerUntil = past.UTC().Format(time.RFC3339Nano)
	rec.HelperUntil = past.UTC().Format(time.RFC3339Nano)
	publishInteraction(t, bundle, rec)
	if _, err := os.Stat(filepath.Join(bundle, "perm", "interact", "standing", "req-letter.json")); !os.IsNotExist(err) {
		t.Fatal("a request written without the standing helper created one")
	}
	if err := retireExpiredInteractionPayloads(bundle, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.claudeInteractionView(n); ok {
		t.Fatal("a request without a standing file stayed visible")
	}
}

func TestStandingRecordEdges(t *testing.T) {
	letter := claudeInteractionModel{Tool: "AskUserQuestion", Digest: strings.Repeat("ab", 32)}
	if err := writeStandingQuestion(t.TempDir(), claudeInteractionRequest{ID: "req"}, claudeInteractionModel{Tool: "ExitPlanMode"}); err != nil {
		t.Fatal(err)
	}
	if err := writeStandingQuestion(t.TempDir(), claudeInteractionRequest{}, claudeInteractionModel{Tool: "AskUserQuestion", Native: true}); err != nil {
		t.Fatal(err)
	}
	if err := writeStandingQuestion(t.TempDir(), claudeInteractionRequest{}, letter); err != nil {
		t.Fatal(err)
	}
	if err := writeStandingQuestion("relative", claudeInteractionRequest{ID: "req-letter", SessionID: "sess"}, letter); err == nil {
		t.Fatal("relative bundle wrote a standing question")
	}
	blocked := t.TempDir()
	if err := os.MkdirAll(filepath.Join(blocked, "perm"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "perm", "interact"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeStandingQuestion(blocked, claudeInteractionRequest{ID: "req-letter", SessionID: "sess"}, letter); !errors.Is(err, errInteractionIO) {
		t.Fatalf("mkdir = %v", err)
	}
	occupied := t.TempDir()
	occupiedStanding := filepath.Join(occupied, "perm", "interact", "standing")
	if err := os.MkdirAll(occupiedStanding, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(occupiedStanding, "req-letter.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeStandingQuestion(occupied, claudeInteractionRequest{ID: "req-letter", SessionID: "sess"}, letter); !errors.Is(err, errInteractionIO) {
		t.Fatalf("write = %v", err)
	}

	rollback := t.TempDir()
	rollbackStanding := filepath.Join(rollback, "perm", "interact", "standing")
	if err := os.MkdirAll(rollbackStanding, 0o700); err != nil {
		t.Fatal(err)
	}
	old := claudeStandingQuestion{
		ID: "req-old", SessionID: "sess", Tool: "AskUserQuestion", Digest: strings.Repeat("ab", 32), Status: standingOutstanding,
	}
	if err := writeClaudePermFile(filepath.Join(rollbackStanding, "req-old.json"), old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rollbackStanding, "req-old.json.tmp"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeStandingQuestion(rollback, claudeInteractionRequest{ID: "req-new", SessionID: "sess"}, letter); !errors.Is(err, errInteractionIO) {
		t.Fatalf("invalidate rollback = %v", err)
	}
	if _, err := os.Stat(filepath.Join(rollbackStanding, "req-new.json")); !os.IsNotExist(err) {
		t.Fatal("failed invalidation left the new standing file")
	}

	missing := t.TempDir()
	if err := os.MkdirAll(filepath.Join(missing, "perm", "interact"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := invalidateOtherStanding(missing, "sess", "keep"); err != nil {
		t.Fatal(err)
	}
	if err := invalidateOtherStanding("relative", "sess", "keep"); err == nil {
		t.Fatal("relative invalidation succeeded")
	}
	fileDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(fileDir, "perm", "interact"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fileDir, "perm", "interact", "standing"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := invalidateOtherStanding(fileDir, "sess", "keep"); !errors.Is(err, errInteractionIO) {
		t.Fatalf("standing file = %v", err)
	}

	skips := t.TempDir()
	skipStanding := filepath.Join(skips, "perm", "interact", "standing")
	if err := os.MkdirAll(filepath.Join(skipStanding, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skipStanding, "note.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	keep := old
	keep.ID = "keep"
	external := old
	external.ID = "req-ext"
	external.Status = standingExternal
	foreign := old
	foreign.ID = "req-foreign"
	foreign.SessionID = "other"
	badStatus := old
	badStatus.ID = "req-bad"
	badStatus.Status = "nope"
	for _, st := range []claudeStandingQuestion{keep, external, foreign, badStatus} {
		if err := writeClaudePermFile(filepath.Join(skipStanding, st.ID+".json"), st); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(skipStanding, "broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skipStanding, "archive.json"), []byte(`{"id":"archive"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := invalidateOtherStanding(skips, "sess", "keep"); err != nil {
		t.Fatal(err)
	}
	if got, ok := readStandingQuestion(skips, "keep"); !ok || got.Status != standingOutstanding {
		t.Fatalf("keep = %+v", got)
	}
	if got, ok := readStandingQuestion(skips, "req-foreign"); !ok || got.Status != standingOutstanding {
		t.Fatalf("other session = %+v", got)
	}
	if got, ok := readStandingQuestion(skips, "req-ext"); !ok || got.Status != standingExternal {
		t.Fatalf("external = %+v", got)
	}
	if _, ok := readStandingQuestion(skips, "req-bad"); ok {
		t.Fatal("bad status was accepted")
	}
	if _, ok := readStandingQuestion(skips, ".."); ok {
		t.Fatal("unsafe id was accepted")
	}
	if _, ok := readStandingQuestion("relative", "keep"); ok {
		t.Fatal("relative read succeeded")
	}
	if _, ok := readStandingQuestion(skips, "missing"); ok {
		t.Fatal("missing read succeeded")
	}
	emptyDigest := keep
	emptyDigest.ID = "req-empty"
	emptyDigest.Digest = ""
	if err := writeClaudePermFile(filepath.Join(skipStanding, "req-empty.json"), emptyDigest); err != nil {
		t.Fatal(err)
	}
	if _, ok := readStandingQuestion(skips, "req-empty"); ok {
		t.Fatal("empty digest was accepted")
	}
	wrongTool := keep
	wrongTool.ID = "req-tool"
	wrongTool.Tool = "Bash"
	if err := writeClaudePermFile(filepath.Join(skipStanding, "req-tool.json"), wrongTool); err != nil {
		t.Fatal(err)
	}
	if _, ok := readStandingQuestion(skips, "req-tool"); ok {
		t.Fatal("bash standing was accepted")
	}
	if listStandingQuestions("relative") != nil {
		t.Fatal("relative list succeeded")
	}
	if listStandingQuestions(fileDir) != nil {
		t.Fatal("file standing list succeeded")
	}
	if len(listStandingQuestions(skips)) == 0 {
		t.Fatal("skip directory listed nothing")
	}

	if claudeAskedHeldByStanding("", claudeAskedNotice{Tool: "AskUserQuestion", Session: "sess", Digest: "0123456789abcdef"}) {
		t.Fatal("empty perm held a notice")
	}
	if claudeAskedHeldByStanding(filepath.Join(skips, "perm"), claudeAskedNotice{Tool: "AskUserQuestion", Session: "sess", Digest: "0123456789abcdef"}) {
		t.Fatal("unrelated digest was held")
	}
	if claudeAskedHeldByStanding(filepath.Join(skips, "perm"), claudeAskedNotice{Tool: "AskUserQuestion", Session: "nobody", Digest: keep.Digest[:16]}) {
		t.Fatal("other session was held")
	}
	invalidateStandingSession("", "sess", "session")
	invalidateStandingSession(filepath.Join(skips, "perm"), "sess", "session")
	if got, ok := readStandingQuestion(skips, "req-foreign"); !ok || got.Status != standingOutstanding {
		t.Fatalf("session invalidation crossed sessions: %+v", got)
	}
	if got, ok := readStandingQuestion(skips, "keep"); !ok || got.Status != standingInvalidated || got.Reason != "session" {
		t.Fatalf("session invalidation = %+v", got)
	}

	if paneMatchesQuestion([]string{"A"}, []string{"A", "B"}) || paneMatchesQuestion([]string{"", "B"}, []string{"", "B"}) {
		t.Fatal("short or blank labels matched")
	}
	if !paneMatchesQuestion([]string{"Alpha", "Beta"}, []string{"Alpha", "Beta"}) {
		t.Fatal("exact labels did not match")
	}
	if paneMatchesQuestion([]string{"Alpha", "Beta"}, []string{"Alpha", "Beta", "Later"}) {
		t.Fatal("a non-Other trailing row matched")
	}
	if _, _, block := matchStandingMenu(nil, nil); block != "menu_mismatch" {
		t.Fatalf("empty questions = %s", block)
	}
	_, _, block := matchStandingMenu([]claudeStandingQuestionItem{
		{ID: "m", Multi: true, Options: []claudeStandingOption{{ID: "a", Label: "Red"}, {ID: "b", Label: "Blue"}}},
		{ID: "empty"},
		{ID: "q", Options: []claudeStandingOption{{Label: "Alpha"}, {ID: "b", Label: "Beta"}}},
	}, []PermOption{{Key: "1", Name: "Alpha"}, {Key: "2", Name: "Beta"}})
	if block != "menu_mismatch" {
		t.Fatalf("blank option id = %s", block)
	}
	app := &app{}
	if _, ok := app.projectStandingQuestion(nil, claudeStandingQuestion{ID: "q", Tool: "AskUserQuestion"}); ok {
		t.Fatal("nil node projected")
	}
	if _, ok := app.projectStandingQuestion(&Node{}, claudeStandingQuestion{Tool: "AskUserQuestion"}); ok {
		t.Fatal("empty id projected")
	}
	if _, ok := app.projectStandingQuestion(&Node{SessionID: "sess"}, claudeStandingQuestion{ID: "q", Tool: "Bash", SessionID: "sess"}); ok {
		t.Fatal("bash projected")
	}
	if _, ok := app.projectStandingQuestion(&Node{SessionID: "sess"}, claudeStandingQuestion{ID: "q", Tool: "AskUserQuestion", SessionID: "other"}); ok {
		t.Fatal("other session projected")
	}
	if _, ok := app.projectStandingQuestion(&Node{}, claudeStandingQuestion{ID: "q", Tool: "AskUserQuestion", Status: "nope"}); ok {
		t.Fatal("bad status projected")
	}
	if _, ok := app.claudeStandingInteractionView(nil, skips); ok {
		t.Fatal("nil node view")
	}
	if _, ok := app.claudeStandingInteractionView(&Node{SessionID: "sess"}, ""); ok {
		t.Fatal("empty bundle view")
	}
	left := standingNativeShot{key: "1", oid: "q1o1", label: "Alpha", keys: []string{"1", "Enter"}}
	right := left
	right.keys = []string{"1", "Tab"}
	if standingShotMatches(left, right) {
		t.Fatal("different key sequence matched")
	}
}

func TestNativeStoreAndRelockFailuresSendNothing(t *testing.T) {
	a, f, n, bundle, epoch := setupStandingQuestion(t, "q-store", "", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	attachInteractionWorker(t, a, n)
	if err := os.Remove(a.storePath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Mkdir(a.storePath, 0o700); err != nil {
		t.Fatal(err)
	}
	stored := postInteraction(newTestHandler(t, a), n.ID, nativeBody(epoch, "q1", "q1o1"), true)
	if stored.Code == http.StatusAccepted || len(keyTails(f)) != 0 {
		t.Fatalf("store = %d keys=%#v %s", stored.Code, keyTails(f), stored.Body.String())
	}
	stood, ok := readStandingQuestion(bundle, "req-letter")
	if !ok || stood.SentEpoch != "" {
		t.Fatalf("store failure consumed the question: %+v", stood)
	}

	a2, f2, n2, bundle2, epoch2 := setupStandingQuestion(t, "q-outcome", "", nil)
	retireQuestionPayload(t, bundle2, "req-letter")
	attachInteractionWorker(t, a2, n2)
	standingNativeRelockPause = func(*Node) {
		a2.sessionsDir = ""
		f2.capture = axPermissionPane
	}
	t.Cleanup(func() { standingNativeRelockPause = nil })
	outcome := postInteraction(newTestHandler(t, a2), n2.ID, nativeBody(epoch2, "q1", "q1o1"), true)
	if outcome.Code == http.StatusAccepted || len(keyTails(f2)) != 0 || !strings.Contains(outcome.Body.String(), "outcome for") {
		t.Fatalf("outcome audit = %d keys=%#v %s", outcome.Code, keyTails(f2), outcome.Body.String())
	}

	a3, f3, n3, bundle3, epoch3 := setupStandingQuestion(t, "q-relock-fence", "", nil)
	retireQuestionPayload(t, bundle3, "req-letter")
	attachInteractionWorker(t, a3, n3)
	fence := filepath.Join(bundle3, "perm", "interact", "fence.lock")
	standingNativeRelockPause = func(*Node) {
		_ = os.Remove(fence)
		if err := os.Mkdir(fence, 0o700); err != nil {
			t.Errorf("fence: %v", err)
		}
	}
	fenced := postInteraction(newTestHandler(t, a3), n3.ID, nativeBody(epoch3, "q1", "q1o1"), true)
	if fenced.Code == http.StatusAccepted || len(keyTails(f3)) != 0 {
		t.Fatalf("second fence = %d keys=%#v %s", fenced.Code, keyTails(f3), fenced.Body.String())
	}
}

func TestNativeRelockStructuredClaimAndUncertainFenceFailuresSendNothing(t *testing.T) {
	newSetup := func(t *testing.T, id string) (*app, *fakeTmux, *Node, string, string, http.Handler) {
		t.Helper()
		a, f, n, bundle, epoch := setupStandingQuestion(t, id, "", nil)
		retireQuestionPayload(t, bundle, "req-letter")
		attachInteractionWorker(t, a, n)
		return a, f, n, bundle, epoch, newTestHandler(t, a)
	}
	t.Run("structured claim arrives before relock", func(t *testing.T) {
		_, f, n, _, epoch, handler := newSetup(t, "q-structured-before-relock")
		standingNativeBeforeOwnershipCheck = func(bundle, id string) {
			claim := filepath.Join(bundle, "perm", "interact", "claim", id)
			if err := exclWrite(claim, []byte("claim")); err != nil {
				t.Errorf("claim: %v", err)
			}
		}
		t.Cleanup(func() { standingNativeBeforeOwnershipCheck = nil })
		resp := postInteraction(handler, n.ID, nativeBody(epoch, "q1", "q1o1"), true)
		if resp.Code == http.StatusAccepted || len(keyTails(f)) != 0 {
			t.Fatalf("claim race = %d sends=%v %s", resp.Code, keyTails(f), resp.Body.String())
		}
	})
	t.Run("standing record fence cannot be persisted", func(t *testing.T) {
		_, f, n, bundle, epoch, handler := newSetup(t, "q-standing-fence-write-failure")
		tmp := filepath.Join(bundle, "perm", "interact", "standing", "req-letter.json.tmp")
		if err := os.Mkdir(tmp, 0o700); err != nil {
			t.Fatal(err)
		}
		resp := postInteraction(handler, n.ID, nativeBody(epoch, "q1", "q1o1"), true)
		if resp.Code == http.StatusAccepted || len(keyTails(f)) != 0 {
			t.Fatalf("fence write = %d sends=%v %s", resp.Code, keyTails(f), resp.Body.String())
		}
	})
	t.Run("helper expiry fence fails after no-replay is persisted", func(t *testing.T) {
		_, f, n, bundle, epoch, handler := newSetup(t, "q-helper-fence-write-failure")
		expired := filepath.Join(bundle, "perm", "interact", "expired")
		standingNativeRelockPause = func(*Node) {
			if err := os.RemoveAll(expired); err != nil {
				t.Errorf("remove expired directory: %v", err)
				return
			}
			if err := os.WriteFile(expired, []byte("not a directory"), 0o600); err != nil {
				t.Errorf("replace expired directory: %v", err)
			}
		}
		t.Cleanup(func() { standingNativeRelockPause = nil })
		body := nativeBody(epoch, "q1", "q1o1")
		resp := postInteraction(handler, n.ID, body, true)
		st, ok := readStandingQuestion(bundle, "req-letter")
		if resp.Code == http.StatusAccepted || len(keyTails(f)) != 0 || !ok || st.SentKey == "" || st.Delivery != "unconfirmed" {
			t.Fatalf("helper fence failure = %d sends=%v standing=%+v ok=%v %s", resp.Code, keyTails(f), st, ok, resp.Body.String())
		}
		retry := postInteraction(handler, n.ID, body, true)
		if retry.Code == http.StatusAccepted || len(keyTails(f)) != 0 {
			t.Fatalf("uncertain helper-fence outcome replayed: %d sends=%v %s", retry.Code, keyTails(f), retry.Body.String())
		}
	})
}

func TestIdlePaneIsMenuUnavailable(t *testing.T) {
	a, f, n, bundle, epoch := setupStandingQuestion(t, "q-idle", "", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	attachInteractionWorker(t, a, n)
	f.capture = "idle\n"
	rec := postInteraction(newTestHandler(t, a), n.ID, nativeBody(epoch, "q1", "q1o1"), true)
	if rec.Code != http.StatusConflict || len(keyTails(f)) != 0 || !strings.Contains(rec.Body.String(), "menu_unavailable") {
		t.Fatalf("idle = %d keys=%#v %s", rec.Code, keyTails(f), rec.Body.String())
	}
}

func TestManualTerminalAnswerIsOneShotAcrossFreshNotifications(t *testing.T) {
	a, f, n, bundle, epoch := setupStandingQuestion(t, "q-manual-once", "", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	attachInteractionWorker(t, a, n)
	h := newTestHandler(t, a)
	handoff := postInteraction(h, n.ID, `{"request_id":"req-letter","action":"terminal"}`, true)
	if handoff.Code != http.StatusAccepted {
		t.Fatalf("terminal handoff = %d %s", handoff.Code, handoff.Body.String())
	}
	first := routeRequest(h, http.MethodPost, "/api/nodes/"+n.ID+"/key", `{"key":"1","dialog_id":"`+epoch+`"}`, true)
	var err error
	if first.Code != http.StatusOK || len(keyTails(f)) != 1 {
		t.Fatalf("manual answer = %d sends=%v %s", first.Code, keyTails(f), first.Body.String())
	}
	secondEpoch, err := mintClaudeVisibleEpoch(filepath.Join(bundle, "perm"), n.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	second := routeRequest(h, http.MethodPost, "/api/nodes/"+n.ID+"/key", `{"key":"2","dialog_id":"`+secondEpoch.Epoch+`"}`, true)
	if second.Code != http.StatusConflict || len(keyTails(f)) != 1 {
		t.Fatalf("fresh notification replay = %d sends=%v %s", second.Code, keyTails(f), second.Body.String())
	}
}

func TestUncertainManualTerminalAnswerCannotBeReplayed(t *testing.T) {
	a, f, n, bundle, epoch := setupStandingQuestion(t, "q-manual-uncertain", "", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	attachInteractionWorker(t, a, n)
	h := newTestHandler(t, a)
	handoff := postInteraction(h, n.ID, `{"request_id":"req-letter","action":"terminal"}`, true)
	if handoff.Code != http.StatusAccepted {
		t.Fatalf("terminal handoff = %d %s", handoff.Code, handoff.Body.String())
	}
	f.sendKeysErr = true
	uncertain := routeRequest(h, http.MethodPost, "/api/nodes/"+n.ID+"/key", `{"key":"1","dialog_id":"`+epoch+`"}`, true)
	if uncertain.Code != http.StatusInternalServerError || len(keyTails(f)) != 1 {
		t.Fatalf("uncertain manual send = %d sends=%v %s", uncertain.Code, keyTails(f), uncertain.Body.String())
	}
	f.sendKeysErr = false
	newEpoch, err := mintClaudeVisibleEpoch(filepath.Join(bundle, "perm"), n.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	replay := routeRequest(h, http.MethodPost, "/api/nodes/"+n.ID+"/key", `{"key":"1","dialog_id":"`+newEpoch.Epoch+`"}`, true)
	if replay.Code != http.StatusConflict || len(keyTails(f)) != 1 {
		t.Fatalf("uncertain manual replay = %d sends=%v %s", replay.Code, keyTails(f), replay.Body.String())
	}
}

func TestTerminalHandoffEnablesOneShotNativeWorkerAnswer(t *testing.T) {
	a, f, n, bundle, epoch := setupStandingQuestion(t, "q-worker-manual-answer", "", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	h := testClaudeSessionHarness(a, n.ID)
	caller := claudeInteractionCaller{NodeID: n.ID, SessionID: n.SessionID, BundleID: filepath.Base(bundle)}
	if generation, nodeID, ok := authoritativeInteractionGeneration(bundle); ok {
		caller.Generation = generation
		if nodeID != "" {
			caller.NodeID = nodeID
		}
	}
	result, err := h.handoffStandingQuestion(n, bundle, caller, sessionworker.InteractionAction{RequestID: "req-letter", Action: "terminal"}, func(sessionlog.Event) error { return nil })
	if err != nil || result.Phase != "manual" {
		t.Fatalf("handoff = %+v, %v", result, err)
	}
	prepared, err := h.PreparePermission(context.Background(), sessionworker.PermissionDecision{RequestID: epoch, Key: "1"})
	if err != nil || prepared.Token == "" {
		t.Fatalf("prepare = %+v, %v", prepared, err)
	}
	if err := h.DeliverPermission(context.Background(), prepared.Token); err != nil {
		t.Fatal(err)
	}
	if got := keyTails(f); len(got) != 1 || !reflect.DeepEqual(got[0], []string{"1", "Enter"}) {
		t.Fatalf("native worker keys = %v", got)
	}
	st, ok := readStandingQuestion(bundle, "req-letter")
	if !ok || st.SentKey != "1" || st.Delivery != "sent" {
		t.Fatalf("manual delivery state = %+v ok=%v", st, ok)
	}
	if _, err := h.PreparePermission(context.Background(), sessionworker.PermissionDecision{RequestID: epoch, Key: "2"}); !errors.Is(err, errClaudeWorkerConflict) {
		t.Fatalf("second manual answer was prepared: %v", err)
	}
}

func TestOutstandingQuestionCannotUseOrdinaryKeyPathForStalePrompt(t *testing.T) {
	a, f, n, bundle, epoch := setupStandingQuestion(t, "q-stale-worker-prompt", "", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	h := testClaudeSessionHarness(a, n.ID)
	f.capture = "Different prompt\n1. Alpha\n2. Beta\n3. Other\n\nEnter selection [1-3], or Escape to cancel:"
	if _, err := h.PreparePermission(context.Background(), sessionworker.PermissionDecision{RequestID: epoch, Key: "1"}); err == nil {
		t.Fatal("stale standing-question menu fell through to ordinary permission key path")
	}
	if containsSub(f.subcommands(), "send-keys") {
		t.Fatal("stale standing-question prepare sent a key")
	}
}

func TestPreparePermissionRechecksStandingFence(t *testing.T) {
	a, _, n, bundle, epoch := setupStandingQuestion(t, "q-prepare-fence-race", "", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	h := testClaudeSessionHarness(a, n.ID)
	prev := claudeNativePreparePause
	claudeNativePreparePause = func(*Node) {
		st := standingOrZero(bundle, "req-letter")
		st.SentKey = "1"
		st.Delivery = "unconfirmed"
		if err := writeClaudePermFile(filepath.Join(bundle, "perm", "interact", "standing", "req-letter.json"), st); err != nil {
			t.Errorf("publish no-replay fence: %v", err)
		}
	}
	t.Cleanup(func() { claudeNativePreparePause = prev })
	if _, err := h.PreparePermission(context.Background(), sessionworker.PermissionDecision{RequestID: epoch, Key: "1"}); !errors.Is(err, errClaudeWorkerConflict) {
		t.Fatalf("prepare passed after the standing sent fence appeared: %v", err)
	}
}

func TestPreparePermissionRejectsUnsupportedOtherChoice(t *testing.T) {
	a, _, n, bundle, epoch := setupStandingQuestion(t, "q-prepare-other", "", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	h := testClaudeSessionHarness(a, n.ID)
	if _, err := h.PreparePermission(context.Background(), sessionworker.PermissionDecision{RequestID: epoch, Key: "3"}); !errors.Is(err, errClaudeWorkerConflict) {
		t.Fatalf("Other was treated as a whitelisted standing option: %v", err)
	}
}

func TestPreparePermissionRechecksMenuAfterValidation(t *testing.T) {
	a, f, n, bundle, epoch := setupStandingQuestion(t, "q-prepare-menu-race", "", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	h := testClaudeSessionHarness(a, n.ID)
	prev := claudeNativeQuestionAfterValidation
	claudeNativeQuestionAfterValidation = func(*Node) { f.capture = axPermissionPane }
	t.Cleanup(func() { claudeNativeQuestionAfterValidation = prev })
	if _, err := h.PreparePermission(context.Background(), sessionworker.PermissionDecision{RequestID: epoch, Key: "1"}); !errors.Is(err, errClaudeWorkerConflict) {
		t.Fatalf("prepare accepted a menu replaced after validation: %v", err)
	}
}

func TestManualDeliverFailurePropagatesThroughWorkerControl(t *testing.T) {
	a, f, n, bundle, epoch := setupStandingQuestion(t, "q-manual-deliver-error", "", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	h := testClaudeSessionHarness(a, n.ID)
	caller := claudeInteractionCaller{NodeID: n.ID, SessionID: n.SessionID, BundleID: filepath.Base(bundle)}
	if generation, nodeID, ok := authoritativeInteractionGeneration(bundle); ok {
		caller.Generation = generation
		if nodeID != "" {
			caller.NodeID = nodeID
		}
	}
	if _, err := h.handoffStandingQuestion(n, bundle, caller, sessionworker.InteractionAction{RequestID: "req-letter", Action: "terminal"}, func(sessionlog.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	prepared, err := h.PreparePermission(context.Background(), sessionworker.PermissionDecision{RequestID: epoch, Key: "1"})
	if err != nil {
		t.Fatal(err)
	}
	f.capture = "Changed prompt\n1. Alpha\n2. Beta\n3. Other\n\nEnter selection [1-3], or Escape to cancel:"
	if err := h.DeliverPermission(context.Background(), prepared.Token); !errors.Is(err, errClaudeWorkerConflict) {
		t.Fatalf("manual worker delivery did not report stale dialog: %v", err)
	}
	if len(keyTails(f)) != 0 {
		t.Fatalf("stale manual delivery sent keys: %v", keyTails(f))
	}
}

func TestManualNativeWorkerFailureOutcomeMatrix(t *testing.T) {
	type fixture struct {
		a        *app
		f        *fakeTmux
		n        *Node
		bundle   string
		epoch    string
		h        *claudeSessionHarness
		prepared preparedClaudeAnswer
	}
	newFixture := func(t *testing.T, id string) fixture {
		t.Helper()
		a, f, n, bundle, epoch := setupStandingQuestion(t, id, "", nil)
		retireQuestionPayload(t, bundle, "req-letter")
		h := testClaudeSessionHarness(a, n.ID)
		caller := claudeInteractionCaller{NodeID: n.ID, SessionID: n.SessionID, BundleID: filepath.Base(bundle)}
		if generation, nodeID, ok := authoritativeInteractionGeneration(bundle); ok {
			caller.Generation = generation
			if nodeID != "" {
				caller.NodeID = nodeID
			}
		}
		if _, err := h.handoffStandingQuestion(n, bundle, caller, sessionworker.InteractionAction{RequestID: "req-letter", Action: "terminal"}, func(sessionlog.Event) error { return nil }); err != nil {
			t.Fatal(err)
		}
		prepared, err := h.PreparePermission(context.Background(), sessionworker.PermissionDecision{RequestID: epoch, Key: "1"})
		if err != nil {
			t.Fatal(err)
		}
		return fixture{a: a, f: f, n: n, bundle: bundle, epoch: epoch, h: h, prepared: preparedClaudeAnswer{dialogID: epoch, keys: prepared.Keys, standingID: "req-letter"}}
	}
	tests := []struct {
		name   string
		mutate func(*testing.T, *fixture)
	}{
		{name: "fence lock", mutate: func(t *testing.T, fx *fixture) {
			interact := filepath.Join(fx.bundle, "perm", "interact")
			if err := os.RemoveAll(interact); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(interact, []byte("not a directory"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "missing question", mutate: func(t *testing.T, fx *fixture) {
			if err := os.Remove(filepath.Join(fx.bundle, "perm", "interact", "standing", "req-letter.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "structured claim", mutate: func(t *testing.T, fx *fixture) {
			if err := exclWrite(filepath.Join(fx.bundle, "perm", "interact", "claim", "req-letter"), []byte("claim")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "manual epoch mismatch", mutate: func(t *testing.T, fx *fixture) {
			st := standingOrZero(fx.bundle, "req-letter")
			st.Manual = true
			st.ManualEpoch = "old-epoch"
			if err := writeClaudePermFile(filepath.Join(fx.bundle, "perm", "interact", "standing", "req-letter.json"), st); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "dialog replaced", mutate: func(t *testing.T, fx *fixture) {
			st := standingOrZero(fx.bundle, "req-letter")
			st.Manual = false
			if err := writeClaudePermFile(filepath.Join(fx.bundle, "perm", "interact", "standing", "req-letter.json"), st); err != nil {
				t.Fatal(err)
			}
			if _, err := mintClaudeVisibleEpoch(filepath.Join(fx.bundle, "perm"), fx.n.SessionID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "validation mismatch", mutate: func(_ *testing.T, fx *fixture) {
			fx.f.capture = "Different question\n1. Alpha\n2. Beta\n3. Other\n\nEnter selection [1-3], or Escape to cancel:"
		}},
		{name: "menu replaced after validation", mutate: func(_ *testing.T, fx *fixture) {
			standingManualAfterValidation = func(*Node) { fx.f.capture = "Unrelated\n1. Other\n2. Menu" }
		}},
		{name: "key sequence mismatch", mutate: func(_ *testing.T, fx *fixture) {
			fx.prepared.keys = []string{"9", "Enter"}
		}},
		{name: "no replay write failure", mutate: func(t *testing.T, fx *fixture) {
			if err := os.Mkdir(filepath.Join(fx.bundle, "perm", "interact", "standing", "req-letter.json.tmp"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "helper expiry write failure", mutate: func(t *testing.T, fx *fixture) {
			expired := filepath.Join(fx.bundle, "perm", "interact", "expired")
			if err := os.RemoveAll(expired); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(expired, []byte("not a directory"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "send error", mutate: func(_ *testing.T, fx *fixture) { fx.f.sendKeysErr = true }},
		{name: "post send standing write failure", mutate: func(t *testing.T, fx *fixture) {
			standingNativeAfterSend = func() {
				if err := os.Mkdir(filepath.Join(fx.bundle, "perm", "interact", "standing", "req-letter.json.tmp"), 0o700); err != nil {
					t.Errorf("post-send temp directory: %v", err)
				}
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t, "q-manual-matrix-"+strings.ReplaceAll(tc.name, " ", "-"))
			prevAfterSend, prevAfterValidation := standingNativeAfterSend, standingManualAfterValidation
			t.Cleanup(func() {
				standingNativeAfterSend = prevAfterSend
				standingManualAfterValidation = prevAfterValidation
			})
			tc.mutate(t, &fx)
			if err := fx.h.deliverManualStandingKey(fx.n, fx.prepared); err == nil {
				t.Fatal("invalid manual native delivery was accepted")
			}
		})
	}
}

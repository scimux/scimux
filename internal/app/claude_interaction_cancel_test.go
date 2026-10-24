package app

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// pin: an unemitted cancel does not settle, with or without an acknowledgement.
func TestCancelNotEmittedDoesNotSettle(t *testing.T) {
	a, n, bundle, turn := beginCancelTurn(t, "n1")
	missing := sampleRequest(bundle, time.Now().UTC())
	missing.ID = "cancel-no-ack"
	missing.SessionID = n.SessionID
	missing.Turn = turn.Turn
	missing.TurnGen = turn.Gen
	writeCancelAnswer(t, bundle, missing)
	quiet := sampleRequest(bundle, time.Now().UTC())
	quiet.ID = "cancel-not-emitted"
	quiet.SessionID = n.SessionID
	quiet.Turn = turn.Turn
	quiet.TurnGen = turn.Gen
	writeCancelAnswer(t, bundle, quiet)
	if err := noteClaudeInteractionEmission(bundle, quiet.ID, claudeInteractionAck{Emitted: false}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{missing.ID, quiet.ID} {
		ok, why := a.settleClaudeInteractionCancel(n, id)
		if ok || why != "not emitted" {
			t.Fatalf("%s = %v %q", id, ok, why)
		}
		if interactionFileExists(bundle, "interrupted", id) {
			t.Fatalf("%s wrote an interrupted marker", id)
		}
	}
	if got := a.claudeAcceptedTurnOf(n.ID); got != turn {
		t.Fatalf("turn = %+v, want %+v", got, turn)
	}
}

// pin: a cancel for turn T leaves successor T2 and its auto-approve untouched.
func TestCancelForSuccessorTurnDoesNotSettle(t *testing.T) {
	a, n, bundle, turn := beginCancelTurn(t, "n1")
	rec := cancelRequest(bundle, n, turn, "cancel-predecessor")
	publishEmittedCancel(t, bundle, rec)
	a.closeClaudeAcceptedTurnLocked(n.ID)
	next, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	armed := &autoApproveState{Phase: autoPhaseArmed, LeaseID: "lease-successor", Count: 4}
	a.mu.Lock()
	a.autoApprove[n.ID] = armed
	a.mu.Unlock()
	ok, why := a.settleClaudeInteractionCancel(n, rec.ID)
	if ok || why != "successor turn" {
		t.Fatalf("settle = %v %q", ok, why)
	}
	if got := a.claudeAcceptedTurnOf(n.ID); got != next {
		t.Fatalf("current = %+v, want %+v", got, next)
	}
	if got := a.claudeClosingTurnOf(n.ID); got.Turn != turn.Turn || got.Gen != turn.Gen || got.Session != turn.Session {
		t.Fatalf("closing = %+v, want %+v", got, turn)
	}
	a.mu.Lock()
	st := a.autoApprove[n.ID]
	a.mu.Unlock()
	if st != armed {
		t.Fatalf("auto-approve = %+v, want %+v", st, armed)
	}
	if interactionFileExists(bundle, "interrupted", rec.ID) {
		t.Fatal("successor settlement wrote an interrupted marker")
	}
}

// pin: an emitted cancel settles when only the closing turn still matches.
func TestEmittedCancelSettlesClosingTurn(t *testing.T) {
	a, n, bundle, turn := beginCancelTurn(t, "n1")
	rec := cancelRequest(bundle, n, turn, "cancel-closing")
	publishEmittedCancel(t, bundle, rec)
	a.closeClaudeAcceptedTurnLocked(n.ID)
	ok, why := a.settleClaudeInteractionCancel(n, rec.ID)
	if !ok || why != "settled" {
		t.Fatalf("settle = %v %q", ok, why)
	}
	if claudeInteractionPhase(bundle, rec.ID, time.Now()) != "interrupted" {
		t.Fatal("closing-turn cancel was not marked interrupted")
	}
	if got := a.claudeAcceptedTurnOf(n.ID).Turn; got != "" {
		t.Fatalf("current = %q", got)
	}
	if got := a.claudeClosingTurnOf(n.ID); got.Turn != turn.Turn || got.Gen != turn.Gen {
		t.Fatalf("closing = %+v, want %+v", got, turn)
	}
}

// pin: settlement still works from the cancel fence after req and ans are retired.
func TestCancelSettlesFromFenceAfterRetirement(t *testing.T) {
	a, n, bundle, turn := beginCancelTurn(t, "n1")
	now := time.Now().UTC()
	rec := cancelRequest(bundle, n, turn, "cancel-retired")
	rec.HelperUntil = now.Add(-time.Millisecond).Format(time.RFC3339Nano)
	publishEmittedCancel(t, bundle, rec)
	if err := retireExpiredInteractionPayloads(bundle, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(bundle, "perm", "interact", "req", rec.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("setup: request payload was not retired")
	}
	if _, err := os.Stat(filepath.Join(bundle, "perm", "interact", "ans", rec.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("setup: answer payload was not retired")
	}
	fence := filepath.Join(bundle, "perm", "interact", "cancel", rec.ID)
	if _, err := os.Stat(fence); err != nil {
		t.Fatal(err)
	}
	ok, why := a.settleClaudeInteractionCancel(n, rec.ID)
	if !ok || why != "settled" {
		t.Fatalf("settle = %v %q", ok, why)
	}
	pendingCancelInteractions(bundle)
	if _, err := os.Stat(fence); !os.IsNotExist(err) {
		t.Fatal("pendingCancelInteractions left the cancel fence")
	}
}

// pin: emitted answer, discuss, request_changes, and approve do not settle.
func TestEmittedNonCancelAnswerDoesNotSettle(t *testing.T) {
	a, n, bundle, turn := beginCancelTurn(t, "n1")
	letter := mustClassify(t, "AskUserQuestion", letterQuestionRaw())
	plan := mustClassify(t, "ExitPlanMode", tinyPlanRaw())
	cases := []struct {
		id    string
		model claudeInteractionModel
		reply claudeInteractionReply
	}{
		{"act-answer", letter, letterReply()},
		{"act-discuss", letter, claudeInteractionReply{Action: "discuss", Feedback: "Ask it another way."}},
		{"act-changes", plan, claudeInteractionReply{Action: "request_changes", Feedback: "Keep the plan smaller."}},
		{"act-approve", plan, claudeInteractionReply{Action: "approve", Mode: "restore"}},
	}
	for _, tc := range cases {
		rec := sampleRequest(bundle, time.Now().UTC())
		rec.ID = tc.id
		rec.SessionID = n.SessionID
		rec.Turn = turn.Turn
		rec.TurnGen = turn.Gen
		rec.Tool = tc.model.Tool
		rec.Original = append([]byte(nil), tc.model.Original...)
		_ = os.Remove(filepath.Join(bundle, "perm", "interact", "active.json"))
		publishInteraction(t, bundle, rec)
		env, err := buildClaudeInteractionReply(tc.model, tc.reply)
		if err != nil {
			t.Fatal(err)
		}
		if err := exclWrite(filepath.Join(bundle, "perm", "interact", "ans", rec.ID+".json"), env.Stdout); err != nil {
			t.Fatal(err)
		}
		if err := noteClaudeInteractionEmission(bundle, rec.ID, claudeInteractionAck{Emitted: true}); err != nil {
			t.Fatal(err)
		}
		ok, why := a.settleClaudeInteractionCancel(n, rec.ID)
		if ok || why != "not emitted" {
			t.Fatalf("%s = %v %q", tc.id, ok, why)
		}
		if interactionFileExists(bundle, "interrupted", rec.ID) {
			t.Fatalf("%s wrote an interrupted marker", tc.id)
		}
	}
	if got := a.claudeAcceptedTurnOf(n.ID); got != turn {
		t.Fatalf("turn = %+v, want %+v", got, turn)
	}
}

// pin: settlement matches an explicit interrupt, plus the interrupted marker.
func TestCancelSettlementMatchesInterrupt(t *testing.T) {
	settled := newComparableCancel(t, "n-settle")
	ended := newComparableCancel(t, "n-end")
	beforeSettle := time.Now()
	ok, why := settled.app.settleClaudeInteractionCancel(settled.node, settled.id)
	afterSettle := time.Now()
	if !ok || why != "settled" {
		t.Fatalf("settle = %v %q", ok, why)
	}
	beforeEnd := time.Now()
	ended.app.endClaudePermissionTurn(ended.node)
	afterEnd := time.Now()

	gotS := boundaryMeaning(settled)
	gotE := boundaryMeaning(ended)
	if gotS.current != gotE.current || gotS.closingGen != gotE.closingGen || gotS.closingSession != gotE.closingSession {
		t.Fatalf("turns settle=%+v end=%+v", gotS, gotE)
	}
	if gotS.current.Turn != "" || gotS.closingTurn != settled.turn.Turn || gotS.closingGen != settled.turn.Gen || gotS.closingSession != settled.turn.Session {
		t.Fatalf("settled turn token = %+v, want closing %+v", gotS, settled.turn)
	}
	if gotE.closingTurn != ended.turn.Turn || gotE.closingGen != ended.turn.Gen || gotE.closingSession != ended.turn.Session {
		t.Fatalf("ended turn token = %+v, want closing %+v", gotE, ended.turn)
	}
	if gotS.autoPresent || gotE.autoPresent || gotS.autoLease != gotE.autoLease || gotS.autoPhase != gotE.autoPhase || gotS.autoCount != gotE.autoCount {
		t.Fatalf("auto-approve settle=%+v end=%+v", gotS, gotE)
	}
	if gotS.attn != gotE.attn {
		t.Fatalf("attention settle=%q end=%q", gotS.attn, gotE.attn)
	}
	if gotS.leaseOK != gotE.leaseOK || gotS.leaseID != gotE.leaseID {
		t.Fatalf("lease settle=%q/%v end=%q/%v", gotS.leaseID, gotS.leaseOK, gotE.leaseID, gotE.leaseOK)
	}
	if gotS.tombstoneSession != gotE.tombstoneSession || gotS.tombstoneSession != settled.node.SessionID {
		t.Fatalf("asked tombstone session settle=%q end=%q", gotS.tombstoneSession, gotE.tombstoneSession)
	}
	if gotS.askedSessions != gotE.askedSessions {
		t.Fatalf("remaining asks settle=%v end=%v", gotS.askedSessions, gotE.askedSessions)
	}
	if !gotS.epochAbsent || !gotE.epochAbsent {
		t.Fatalf("visible epoch settle absent=%v end absent=%v", gotS.epochAbsent, gotE.epochAbsent)
	}
	if gotS.elicitations != gotE.elicitations {
		t.Fatalf("elicitations settle=%v end=%v", gotS.elicitations, gotE.elicitations)
	}
	if gotS.closedSession != gotE.closedSession || gotS.closedGen != gotE.closedGen || gotS.closedSession != settled.node.SessionID || gotS.closedGen != settled.turn.Gen {
		t.Fatalf("turn-closed settle=%+v end=%+v", gotS, gotE)
	}
	if gotS.closedTurn != settled.turn.Turn || gotE.closedTurn != ended.turn.Turn {
		t.Fatalf("turn-closed turns settle=%q end=%q", gotS.closedTurn, gotE.closedTurn)
	}
	assertTurnClosedStamp(t, "settle", settled.perm, beforeSettle, afterSettle)
	assertTurnClosedStamp(t, "end", ended.perm, beforeEnd, afterEnd)
	if claudeInteractionPhase(settled.bundle, settled.id, time.Now()) != "interrupted" {
		t.Fatal("settled fixture lacks the interrupted marker")
	}
}

// pin: may-settle covers the BASE reasons that remain once endTurn is true.
func TestClaudeInteractionCancelMaySettleTable(t *testing.T) {
	cur := claudeAcceptedTurn{Turn: "turn-n", Gen: 2}
	closing := claudeAcceptedTurn{Turn: "turn-prev", Gen: 1}
	cases := []struct {
		name    string
		emitted bool
		turn    string
		current claudeAcceptedTurn
		closing claudeAcceptedTurn
		wantOK  bool
		wantWhy string
	}{
		{"not emitted", false, "turn-n", cur, closing, false, "not emitted"},
		{"turn mismatch", true, "", cur, closing, false, "turn mismatch"},
		{"successor turn", true, "turn-n", claudeAcceptedTurn{Turn: "turn-n1", Gen: 3}, cur, false, "successor turn"},
		{"settled", true, "turn-n", cur, closing, true, "settled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, why := claudeInteractionCancelMaySettle(tc.emitted, tc.turn, tc.current, tc.closing)
			if ok != tc.wantOK || why != tc.wantWhy {
				t.Fatalf("%s = %v %q", tc.name, ok, why)
			}
		})
	}
}

// red: a direct call settles the emitted cancel for the current turn without a tailer.
func TestSettleEmittedCancelsSettlesCurrentTurn(t *testing.T) {
	fx := newPollCancel(t, "")
	fx.app.settleEmittedClaudeInteractionCancels(fx.node)
	if claudeInteractionPhase(fx.bundle, fx.id, time.Now()) != "interrupted" {
		t.Fatal("direct settle did not mark the emitted cancel interrupted")
	}
	fx.app.mu.Lock()
	st := fx.app.autoApprove[fx.node.ID]
	fx.app.mu.Unlock()
	if st != nil {
		t.Fatalf("auto-approve = %+v, want cleared", st)
	}
	if lease, ok := readClaudePermLease(filepath.Join(fx.bundle, "perm"), time.Now()); ok {
		t.Fatalf("lease %q still published", lease)
	}
	if got := fx.app.claudeAcceptedTurnOf(fx.node.ID).Turn; got != "" {
		t.Fatalf("turn = %q, want closed", got)
	}
}

// red: the first call settles, and a second call changes nothing more.
func TestSettleEmittedCancelsIsIdempotent(t *testing.T) {
	fx := newPollCancel(t, "")
	fx.app.settleEmittedClaudeInteractionCancels(fx.node)
	if claudeInteractionPhase(fx.bundle, fx.id, time.Now()) != "interrupted" {
		t.Fatal("first settle did not mark the emitted cancel interrupted")
	}
	first := snapshotCancelSettle(fx)
	fx.app.settleEmittedClaudeInteractionCancels(fx.node)
	second := snapshotCancelSettle(fx)
	if first != second {
		t.Fatalf("second settle changed state: %+v -> %+v", first, second)
	}
}

// red: one call settles only the first pending cancel in sorted order.
func TestSettleEmittedCancelsStopsAtFirstSettled(t *testing.T) {
	a, n, bundle, turn := beginCancelTurn(t, "n1")
	first := cancelRequest(bundle, n, turn, "a-cancel")
	second := cancelRequest(bundle, n, turn, "b-cancel")
	publishEmittedCancel(t, bundle, first)
	_ = os.Remove(filepath.Join(bundle, "perm", "interact", "active.json"))
	publishEmittedCancel(t, bundle, second)
	a.settleEmittedClaudeInteractionCancels(n)
	if claudeInteractionPhase(bundle, first.ID, time.Now()) != "interrupted" {
		t.Fatal("first pending cancel was not settled")
	}
	if claudeInteractionPhase(bundle, second.ID, time.Now()) == "interrupted" {
		t.Fatal("second pending cancel was also settled")
	}
}

// pin: a settled turn whose generation is ahead of the last closed marker
// catches the marker up to that generation.
func TestCancelSettlementCatchesUpClosedGeneration(t *testing.T) {
	a, n, bundle, first := beginCancelTurn(t, "n1")
	perm := filepath.Join(bundle, "perm")
	a.closeClaudeAcceptedTurnLocked(n.ID)
	if _, ok := readClaudeTurnClosed(perm); ok {
		t.Fatal("moving the live turn to closing wrote a closed marker")
	}
	later, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	if later.Gen != first.Gen+1 {
		t.Fatalf("later generation = %d, want %d", later.Gen, first.Gen+1)
	}
	rec := cancelRequest(bundle, n, later, "gen-catchup")
	publishEmittedCancel(t, bundle, rec)
	a.settleEmittedClaudeInteractionCancels(n)
	closed, ok := readClaudeTurnClosed(perm)
	if !ok {
		t.Fatal("settlement did not write a closed marker")
	}
	if closed.Gen != later.Gen || closed.Turn != later.Turn || closed.Session != n.SessionID {
		t.Fatalf("closed marker = %+v, want turn %s gen %d session %s", closed, later.Turn, later.Gen, n.SessionID)
	}
	if claudeInteractionPhase(bundle, rec.ID, time.Now()) != "interrupted" {
		t.Fatal("emitted cancel for the later turn was not settled")
	}
}

type cancelSettleSnapshot struct {
	phase   string
	turn    string
	closing string
	auto    bool
	lease   bool
}

func snapshotCancelSettle(fx pollCancel) cancelSettleSnapshot {
	fx.app.mu.Lock()
	auto := fx.app.autoApprove[fx.node.ID] != nil
	fx.app.mu.Unlock()
	_, lease := readClaudePermLease(filepath.Join(fx.bundle, "perm"), time.Now())
	return cancelSettleSnapshot{
		phase:   claudeInteractionPhase(fx.bundle, fx.id, time.Now()),
		turn:    fx.app.claudeAcceptedTurnOf(fx.node.ID).Turn,
		closing: fx.app.claudeClosingTurnOf(fx.node.ID).Turn,
		auto:    auto,
		lease:   lease,
	}
}

// red: a quiet tick settles an emitted cancel even when the transcript path is empty.
func TestPollSettlesEmittedCancelWithoutTranscript(t *testing.T) {
	fx := newPollCancel(t, "")
	pollQuiet(fx.app, fx.tmux, fx.node.ID)
	if claudeInteractionPhase(fx.bundle, fx.id, time.Now()) != "interrupted" {
		t.Fatal("quiet tick did not mark the emitted cancel interrupted")
	}
	fx.app.mu.Lock()
	st := fx.app.autoApprove[fx.node.ID]
	fx.app.mu.Unlock()
	if st != nil {
		t.Fatalf("auto-approve = %+v, want cleared", st)
	}
	if lease, ok := readClaudePermLease(filepath.Join(fx.bundle, "perm"), time.Now()); ok {
		t.Fatalf("lease %q still published", lease)
	}
	if got := fx.app.claudeAcceptedTurnOf(fx.node.ID).Turn; got != "" {
		t.Fatalf("claudeTurns[%s] = %q, want empty", fx.node.ID, got)
	}
}

// red: an active tick keeps the turn locked, and the following quiet tick releases it.
func TestSendAllowedAfterQuietCancelSettlement(t *testing.T) {
	fx := newPollCancel(t, "")
	pollActive(fx.app, fx.tmux)
	if _, err := fx.app.beginClaudeAcceptedTurn(fx.node); err == nil {
		t.Fatal("active tick allowed a new turn before the cancel settled")
	}
	pollQuiet(fx.app, fx.tmux, fx.node.ID)
	if claudeInteractionPhase(fx.bundle, fx.id, time.Now()) != "interrupted" {
		t.Fatal("quiet tick did not settle the emitted cancel")
	}
	if _, err := fx.app.beginClaudeAcceptedTurn(fx.node); err != nil {
		t.Fatalf("begin after quiet settlement: %v", err)
	}
}

// red: a fresh interruption notice on an active tick must not settle the cancel.
func TestPollNeverSettlesCancelOnActiveTick(t *testing.T) {
	path := filepath.Join(t.TempDir(), "interrupt.jsonl")
	writeTranscript(t, path, "")
	fx := newPollCancel(t, path)
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	appendTranscript(t, path, interruptLine(stamp, "[Request interrupted by user]"))
	pollActive(fx.app, fx.tmux)
	if claudeInteractionPhase(fx.bundle, fx.id, time.Now()) == "interrupted" || fx.app.claudeAcceptedTurnOf(fx.node.ID).Turn == "" {
		t.Fatal("active tick settled an emitted cancel")
	}
}

// pin: an active tick with no transcript tailer does not settle an emitted cancel.
func TestPollDoesNotSettleCancelWithoutTranscriptOnActiveTick(t *testing.T) {
	fx := newPollCancel(t, "")
	pollActive(fx.app, fx.tmux)
	if claudeInteractionPhase(fx.bundle, fx.id, time.Now()) == "interrupted" {
		t.Fatal("active tick settled an emitted cancel without a transcript")
	}
	if got := fx.app.claudeAcceptedTurnOf(fx.node.ID); got != fx.turn {
		t.Fatalf("turn = %+v, want %+v", got, fx.turn)
	}
	if _, err := fx.app.beginClaudeAcceptedTurn(fx.node); err == nil {
		t.Fatal("beginClaudeAcceptedTurn succeeded while the cancel's turn was still current")
	}
}

func beginCancelTurn(t *testing.T, id string) (*app, *Node, string, claudeAcceptedTurn) {
	t.Helper()
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, id, hookSIDOwn)
	turn, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	return a, n, bundle, turn
}

func cancelRequest(bundle string, n *Node, turn claudeAcceptedTurn, id string) claudeInteractionRequest {
	rec := sampleRequest(bundle, time.Now().UTC())
	rec.ID = id
	rec.SessionID = n.SessionID
	rec.Turn = turn.Turn
	rec.TurnGen = turn.Gen
	return rec
}

func writeCancelAnswer(t *testing.T, bundle string, rec claudeInteractionRequest) {
	t.Helper()
	_ = os.Remove(filepath.Join(bundle, "perm", "interact", "active.json"))
	publishInteraction(t, bundle, rec)
	env, err := buildClaudeInteractionReply(mustClassify(t, rec.Tool, rec.Original), claudeInteractionReply{Action: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	if err := exclWrite(filepath.Join(bundle, "perm", "interact", "ans", rec.ID+".json"), env.Stdout); err != nil {
		t.Fatal(err)
	}
}

type comparableCancel struct {
	app    *app
	node   *Node
	bundle string
	perm   string
	turn   claudeAcceptedTurn
	id     string
}

func newComparableCancel(t *testing.T, id string) comparableCancel {
	t.Helper()
	a, n, bundle, turn := beginCancelTurn(t, id)
	rec := cancelRequest(bundle, n, turn, "cancel-match")
	publishEmittedCancel(t, bundle, rec)
	perm := filepath.Join(bundle, "perm")
	a.mu.Lock()
	a.autoApprove[n.ID] = &autoApproveState{Phase: autoPhaseArmed, LeaseID: "lease-pin", Count: 3}
	a.attn[n.ID] = "approval"
	a.mu.Unlock()
	a.publishClaudeLease(bundle, "lease-pin")
	now := time.Now().UTC()
	writeClaudeAskedNotice(perm, "ask-own", claudePermissionEvent{SessionID: n.SessionID, ToolName: "Bash", ToolInput: []byte("own")}, now)
	writeClaudeAskedNotice(perm, "ask-other", claudePermissionEvent{SessionID: hookSIDSuccessor, ToolName: "Edit", ToolInput: []byte("other")}, now)
	if _, err := mintClaudeVisibleEpoch(perm, n.SessionID); err != nil {
		t.Fatal(err)
	}
	own := elicitationEventJSON("Elicitation", n.SessionID, "docs-server", "Need a token", map[string]any{"mode": "form"})
	if err := RunClaudeElicitationHook(bundle, bytes.NewReader(own), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	foreign := elicitationEventJSON("Elicitation", hookSIDSuccessor, "other-server", "Other session", map[string]any{"mode": "form"})
	if err := RunClaudeElicitationHook(bundle, bytes.NewReader(foreign), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	return comparableCancel{app: a, node: n, bundle: bundle, perm: perm, turn: turn, id: rec.ID}
}

type boundarySnapshot struct {
	current          claudeAcceptedTurn
	closingTurn      string
	closingGen       int
	closingSession   string
	autoPresent      bool
	autoPhase        string
	autoLease        string
	autoCount        int
	attn             string
	leaseOK          bool
	leaseID          string
	tombstoneSession string
	askedSessions    string
	epochAbsent      bool
	elicitations     string
	closedSession    string
	closedTurn       string
	closedGen        int
}

func boundaryMeaning(fx comparableCancel) boundarySnapshot {
	var out boundarySnapshot
	out.current = fx.app.claudeAcceptedTurnOf(fx.node.ID)
	closing := fx.app.claudeClosingTurnOf(fx.node.ID)
	out.closingTurn, out.closingGen, out.closingSession = closing.Turn, closing.Gen, closing.Session
	fx.app.mu.Lock()
	if st := fx.app.autoApprove[fx.node.ID]; st != nil {
		out.autoPresent = true
		out.autoPhase = string(st.Phase)
		out.autoLease = st.LeaseID
		out.autoCount = st.Count
	}
	out.attn = fx.app.attn[fx.node.ID]
	fx.app.mu.Unlock()
	out.leaseID, out.leaseOK = readClaudePermLease(fx.perm, time.Now())
	var sessions []string
	for _, asked := range readClaudeAskedFiles(fx.perm, time.Now()) {
		sessions = append(sessions, asked.note.Session)
	}
	sort.Strings(sessions)
	out.askedSessions = stringsJoin(sessions)
	if claudeRequestAnswered(fx.perm, "ask-own") {
		out.tombstoneSession = fx.node.SessionID
	}
	_, epochOK := readClaudeVisibleEpoch(fx.perm)
	out.epochAbsent = !epochOK
	var elicit []string
	for _, f := range readClaudeElicitationFiles(fx.bundle, time.Now()) {
		elicit = append(elicit, f.rec.SessionID)
	}
	sort.Strings(elicit)
	out.elicitations = stringsJoin(elicit)
	if closed, ok := readClaudeTurnClosed(fx.perm); ok {
		out.closedSession = closed.Session
		out.closedTurn = closed.Turn
		out.closedGen = closed.Gen
	}
	return out
}

func stringsJoin(parts []string) string {
	out := ""
	for i, part := range parts {
		if i > 0 {
			out += ","
		}
		out += part
	}
	return out
}

func assertTurnClosedStamp(t *testing.T, name, perm string, before, after time.Time) {
	t.Helper()
	closed, ok := readClaudeTurnClosed(perm)
	if !ok || closed.At == "" {
		t.Fatalf("%s turn-closed timestamp missing", name)
	}
	stamp, err := time.Parse(time.RFC3339, closed.At)
	if err != nil {
		t.Fatalf("%s turn-closed timestamp %q: %v", name, closed.At, err)
	}
	if stamp.Before(before) || stamp.After(after) {
		t.Fatalf("%s turn-closed timestamp %s outside %s .. %s", name, stamp, before, after)
	}
}

type pollCancel struct {
	app    *app
	tmux   *fakeTmux
	node   *Node
	bundle string
	turn   claudeAcceptedTurn
	id     string
}

func newPollCancel(t *testing.T, transcript string) pollCancel {
	t.Helper()
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}, capture: "pane-a"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	if transcript != "" {
		n.Transcript = transcript
	}
	turn, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	rec := cancelRequest(bundle, n, turn, "cancel-poll")
	publishEmittedCancel(t, bundle, rec)
	a.mu.Lock()
	a.autoApprove[n.ID] = &autoApproveState{Phase: autoPhaseArmed, LeaseID: "lease-pin", Count: 1}
	a.mu.Unlock()
	a.publishClaudeLease(bundle, "lease-pin")
	return pollCancel{app: a, tmux: f, node: n, bundle: bundle, turn: turn, id: rec.ID}
}

func pollActive(a *app, f *fakeTmux) {
	a.poll()
	f.capture = "pane-b"
	a.poll()
}

func pollQuiet(a *app, f *fakeTmux, id string) {
	a.mu.Lock()
	a.prevCap[id] = f.capture
	a.lastChg[id] = time.Now().Add(-paneQuietAfter - time.Second)
	a.mu.Unlock()
	a.poll()
}

// red: a quiet tick settles an emitted cancel and must not restore the dialog
// snapshot taken before that settlement. No transcript and no tailer.
func TestCancelQuietSettlementLeavesAttentionEmpty(t *testing.T) {
	fx := newPollCancel(t, "")
	fx.app.mu.Lock()
	delete(fx.app.autoApprove, fx.node.ID)
	fx.app.mu.Unlock()
	fx.app.retractClaudeLease(fx.bundle)
	perm := filepath.Join(fx.bundle, "perm")
	if _, err := mintClaudeVisibleEpoch(perm, fx.node.SessionID); err != nil {
		t.Fatal(err)
	}
	if dlg := fx.app.claudeVisibleDialog(fx.node); dlg.Attn == "" {
		t.Fatal("setup: no visible dialog")
	}
	if fx.node.Transcript != "" {
		t.Fatal("setup: transcript is present")
	}
	pollQuiet(fx.app, fx.tmux, fx.node.ID)
	if claudeInteractionPhase(fx.bundle, fx.id, time.Now()) != "interrupted" {
		t.Fatal("quiet tick did not settle the emitted cancel")
	}
	if got := fx.app.claudeAcceptedTurnOf(fx.node.ID).Turn; got != "" {
		t.Fatalf("turn = %q, want closed", got)
	}
	if _, ok := readClaudeVisibleEpoch(perm); ok {
		t.Fatal("visible epoch still present after settlement")
	}
	fx.app.mu.Lock()
	attn := fx.app.attn[fx.node.ID]
	live := fx.app.live[fx.node.ID]
	fx.app.mu.Unlock()
	if live != "quiet" {
		t.Fatalf("liveness = %q, want quiet", live)
	}
	if attn != "" {
		t.Fatalf("settled cancel restored stale dialog attention: %q", attn)
	}
}

// red: the same quiet settlement must not republish the pre-settlement dialog
// when a tailer runs and does not retire that dialog itself.
func TestCancelQuietSettlementWithTailerLeavesAttentionEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quiet.jsonl")
	writeTranscript(t, path, "{\"type\":\"user\",\"timestamp\":\"t1\",\"message\":{\"role\":\"user\",\"content\":\"synthetic\"}}\n")
	fx := newPollCancel(t, path)
	fx.app.mu.Lock()
	delete(fx.app.autoApprove, fx.node.ID)
	fx.app.mu.Unlock()
	fx.app.retractClaudeLease(fx.bundle)
	perm := filepath.Join(fx.bundle, "perm")
	if _, err := mintClaudeVisibleEpoch(perm, fx.node.SessionID); err != nil {
		t.Fatal(err)
	}
	if fx.app.tailerFor(fx.node) == nil {
		t.Fatal("setup: no tailer")
	}
	pollQuiet(fx.app, fx.tmux, fx.node.ID)
	if claudeInteractionPhase(fx.bundle, fx.id, time.Now()) != "interrupted" {
		t.Fatal("quiet tick did not settle the emitted cancel")
	}
	if got := fx.app.claudeAcceptedTurnOf(fx.node.ID).Turn; got != "" {
		t.Fatalf("turn = %q, want closed", got)
	}
	if _, ok := readClaudeVisibleEpoch(perm); ok {
		t.Fatal("visible epoch still present after settlement")
	}
	fx.app.mu.Lock()
	attn := fx.app.attn[fx.node.ID]
	fx.app.mu.Unlock()
	if attn != "" {
		t.Fatalf("settled cancel restored stale dialog attention: %q", attn)
	}
}

// pin: a quiet tick keeps attention that still belongs to a current dialog.
func TestClaudeInteractionQuietTickKeepsCurrentDialogAttention(t *testing.T) {
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}, capture: "pane-a"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	perm := filepath.Join(bundle, "perm")
	if _, err := mintClaudeVisibleEpoch(perm, n.SessionID); err != nil {
		t.Fatal(err)
	}
	if dlg := a.claudeVisibleDialog(n); dlg.Attn == "" {
		t.Fatal("setup: no visible dialog")
	}
	pollQuiet(a, f, n.ID)
	a.mu.Lock()
	attn := a.attn[n.ID]
	live := a.live[n.ID]
	a.mu.Unlock()
	if live != "quiet" {
		t.Fatalf("liveness = %q, want quiet", live)
	}
	if attn == "" {
		t.Fatal("quiet tick cleared current dialog attention")
	}
	if _, ok := readClaudeVisibleEpoch(perm); !ok {
		t.Fatal("quiet tick removed the current visible epoch")
	}
	if got := a.claudeAcceptedTurnOf(n.ID).Turn; got != "" {
		t.Fatalf("turn = %q, want untouched", got)
	}
}

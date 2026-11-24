package app

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/sessionworker"
)

func TestReviewStandardNativeQuestionRows(t *testing.T) {
	model := mustClassify(t, "AskUserQuestion", letterQuestionRaw())
	st := standingFromModel(claudeInteractionRequest{ID: "review"}, model)
	opts := []PermOption{{Key: "1", Name: "Alpha"}, {Key: "2", Name: "Beta"}, {Key: "3", Name: "Other"}, {Key: "4", Name: "Chat about this"}}
	_, choices, block := matchStandingMenu(st.Questions, opts)
	if block != "" || len(choices) != 2 {
		t.Fatalf("standard native menu rejected: block=%q choices=%v", block, choices)
	}
}

func TestReviewNativeQuestionHeader(t *testing.T) {
	model := mustClassify(t, "AskUserQuestion", letterQuestionRaw())
	st := standingFromModel(claudeInteractionRequest{ID: "review"}, model)
	pane := "Question: Letter\n> Pick a letter?\n1. Alpha\n2. Beta\n3. Other\nSelect with numbers [1-3]. Then Enter to submit or Escape to cancel:"
	if !standingPaneQuestionMatches(st.Questions, pane) {
		t.Fatal("matching native question rejected because it has AX header and quote prefix")
	}
}

func TestReviewTerminalCannotBypassNoReplay(t *testing.T) {
	a, f, n, bundle, epoch := setupStandingQuestion(t, "review-replay", "", nil)
	retireQuestionPayload(t, bundle, "req-letter")
	attachInteractionWorker(t, a, n)
	handler := newTestHandler(t, a)
	first := postInteraction(handler, n.ID, nativeBody(epoch, "q1", "q1o1"), true)
	if first.Code != http.StatusAccepted {
		t.Fatalf("setup send: %d %s", first.Code, first.Body.String())
	}
	next, err := mintClaudeVisibleEpoch(filepath.Join(bundle, "perm"), n.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	handoff := postInteraction(handler, n.ID, `{"request_id":"req-letter","action":"terminal"}`, true)
	key := routeRequest(handler, http.MethodPost, "/api/nodes/"+n.ID+"/key", `{"key":"2","dialog_id":"`+next.Epoch+`"}`, true)
	if key.Code == http.StatusOK || len(keyTails(f)) != 1 {
		t.Fatalf("terminal bypassed no-replay: handoff=%d key=%d sends=%v", handoff.Code, key.Code, keyTails(f))
	}
}

func TestReviewStandingPaneParserOutcomeMatrix(t *testing.T) {
	model := mustClassify(t, "AskUserQuestion", letterQuestionRaw())
	st := standingFromModel(claudeInteractionRequest{ID: "review"}, model)
	if !standingMenuMatchesLabels(st.Questions, []PermOption{{Name: "Alpha"}, {Name: "Beta"}, {Name: "Other"}}) {
		t.Fatal("same labels were not treated as a fail-closed ambiguity")
	}
	if standingMenuMatchesLabels(st.Questions, []PermOption{{Name: "Yes"}, {Name: "No"}}) || standingMenuMatchesLabels(st.Questions, nil) {
		t.Fatal("different or absent labels matched the standing question")
	}
	if standingMenuMatchesLabels(st.Questions, []PermOption{{Name: "Alpha"}}) {
		t.Fatal("short menu matched the standing question")
	}
	valid := "Question: Letter\n> Pick a letter?\n1. Alpha\n2. Beta\n3. Other\n4. Chat about this"
	if !standingPaneQuestionMatches(st.Questions, valid) {
		t.Fatal("valid AX rows rejected")
	}
	if !standingPaneQuestionMatches(st.Questions, "Pick a letter?\n1. Alpha\n2. Beta\nOther") {
		t.Fatal("un-numbered Other suffix rejected")
	}
	if standingPaneQuestionMatches(st.Questions, "Different question\n1. Alpha\n2. Beta") {
		t.Fatal("question text mismatch accepted")
	}
	if standingPaneQuestionMatches(nil, valid) {
		t.Fatal("empty question set accepted")
	}
	if standingPaneQuestionMatches([]claudeStandingQuestionItem{{Multi: true, Text: "Pick a letter?", Options: st.Questions[0].Options}}, valid) {
		t.Fatal("multi-select question treated as single-select")
	}
	if standingPaneQuestionMatches([]claudeStandingQuestionItem{{Text: "", Options: st.Questions[0].Options}}, valid) {
		t.Fatal("question without text accepted")
	}
	if standingPaneQuestionMatches([]claudeStandingQuestionItem{{Text: "Pick a letter?"}}, valid) {
		t.Fatal("question without options accepted")
	}
	withoutHeader := st.Questions[0]
	withoutHeader.Header = ""
	if !standingPaneQuestionMatches([]claudeStandingQuestionItem{withoutHeader}, "Pick a letter?\n>\n1. Alpha\n2. Beta") {
		t.Fatal("quote-only line or absent AX header rejected")
	}
	duplicate := append(append([]claudeStandingQuestionItem(nil), st.Questions...), st.Questions[0])
	if standingPaneQuestionMatches(duplicate, valid) {
		t.Fatal("ambiguous duplicate question accepted")
	}
	if got := paneLines("Other\n1. Alpha\nnot a row\n2. Beta"); strings.Join(got, ",") != "Alpha,Beta" {
		t.Fatalf("pane rows = %q", got)
	}
	if got := paneLines("1. Alpha\nOther"); strings.Join(got, ",") != "Alpha,Other" {
		t.Fatalf("Other suffix = %q", got)
	}
	for _, line := range []string{"1. Alpha", "10. Alpha"} {
		if !numberedMenuLine(line) {
			t.Errorf("valid row %q rejected", line)
		}
	}
	for _, line := range []string{"", ". Alpha", "0. Alpha", "1. ", "1x. Alpha", "1"} {
		if numberedMenuLine(line) {
			t.Errorf("invalid row %q accepted", line)
		}
	}
}

func TestReviewStandingStructuredDecisionOutcomeMatrix(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind string
		raw  string
		want bool
	}{
		{name: "none", want: false},
		{name: "nonterminal answer", kind: "ans", raw: `{}`, want: true},
		{name: "terminal answer", kind: "ans", raw: `{"terminal":true}`, want: false},
		{name: "nonterminal ack", kind: "ack", raw: `{"emitted":true}`, want: true},
		{name: "terminal ack", kind: "ack", raw: `{"emitted":true,"terminal":true}`, want: false},
		{name: "malformed ack", kind: "ack", raw: `{`, want: true},
		{name: "claim", kind: "claim", raw: `claimed`, want: true},
		{name: "recorded", kind: "recorded", raw: `recorded`, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundle := t.TempDir()
			if tc.kind != "" {
				name := "req-letter"
				if tc.kind == "ans" || tc.kind == "ack" {
					name += ".json"
				}
				path := filepath.Join(bundle, "perm", "interact", tc.kind, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(tc.raw), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := standingStructuredDecisionPending(bundle, "req-letter"); got != tc.want {
				t.Fatalf("pending = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReviewStandingDialogValidationOutcomes(t *testing.T) {
	a, f, n, _, _, h, _ := reviewStandingHarness(t, "review-dialog-validation", false)
	st := standingOrZero(a.claudeHookBundle(n), "req-letter")
	if err := h.validateStandingDialog(n, st, claudeVisibleDialog{}); err == nil {
		t.Fatal("missing dialog accepted")
	}
	if err := h.validateStandingDialog(n, st, claudeVisibleDialog{DialogID: "epoch"}); err != nil {
		t.Fatalf("matching dialog rejected: %v", err)
	}
	f.capture = "Unrelated prompt\n1. Unrelated\n2. Menu"
	if err := h.validateStandingDialog(n, st, claudeVisibleDialog{DialogID: "epoch"}); err == nil {
		t.Fatal("unrelated menu accepted")
	}
	f.capture = "Question: Letter\nPick a letter?"
	if err := h.validateStandingDialog(n, st, claudeVisibleDialog{DialogID: "epoch"}); err == nil {
		t.Fatal("missing options accepted")
	}
	f.capture = "Unrelated prompt\n1. Alpha\n2. Beta\n3. Other\n\nEnter selection [1-3], or Escape to cancel:"
	if err := h.validateStandingDialog(n, st, claudeVisibleDialog{DialogID: "epoch"}); err == nil {
		t.Fatal("stale prompt accepted")
	}
}

func TestReviewStandingStoreFailureOutcomes(t *testing.T) {
	model := claudeInteractionModel{Tool: "AskUserQuestion", Digest: strings.Repeat("a", 64)}
	rec := claudeInteractionRequest{ID: "incoming", SessionID: "session"}
	oldMarshal := marshalStandingRecord
	t.Cleanup(func() { marshalStandingRecord = oldMarshal })
	marshalStandingRecord = func(any) ([]byte, error) { return nil, errors.New("synthetic marshal failure") }
	if err := writeStandingQuestion(t.TempDir(), rec, model); !errors.Is(err, errInteractionIO) {
		t.Fatalf("marshal failure = %v", err)
	}
	marshalStandingRecord = oldMarshal

	if err := enforceStandingBudget("relative", "incoming.json", 1); err == nil {
		t.Fatal("unsafe budget path accepted")
	}
	blocked := t.TempDir()
	if err := os.MkdirAll(filepath.Join(blocked, "perm", "interact"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "perm", "interact", "standing"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := enforceStandingBudget(blocked, "incoming.json", 1); err == nil {
		t.Fatal("unreadable standing directory accepted")
	}

	entryBundle := t.TempDir()
	entryDir := filepath.Join(entryBundle, "perm", "interact", "standing")
	if err := os.MkdirAll(entryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(entryDir, "missing-target"), filepath.Join(entryDir, "broken.json")); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}
	if err := enforceStandingBudget(entryBundle, "incoming.json", 1); !errors.Is(err, errInteractionIO) {
		t.Fatalf("entry info failure = %v", err)
	}

	largeBundle := t.TempDir()
	largeDir := filepath.Join(largeBundle, "perm", "interact", "standing")
	if err := os.MkdirAll(largeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(largeDir, "large.json"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(largeDir, "large.json"), standingMaxBytes+1); err != nil {
		t.Fatal(err)
	}
	if err := enforceStandingBudget(largeBundle, "incoming.json", 1); !errors.Is(err, errInteractionIO) {
		t.Fatalf("oversized existing record = %v", err)
	}
	if err := enforceStandingBudget(t.TempDir(), "incoming.json", -1); !errors.Is(err, errInteractionIO) {
		t.Fatalf("negative incoming size = %v", err)
	}
	if err := enforceStandingBudget(t.TempDir(), "incoming.json", standingMaxBytes+1); !errors.Is(err, errInteractionIO) {
		t.Fatalf("oversized incoming record = %v", err)
	}

	fullResolved := func(t *testing.T) string {
		t.Helper()
		bundle := t.TempDir()
		dir := filepath.Join(bundle, "perm", "interact", "standing")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < standingMaxFiles; i++ {
			item := claudeStandingQuestion{ID: strings.Repeat("x", 1) + string(rune('a'+i%26)) + string(rune('A'+i/26)), SessionID: "session", Tool: "AskUserQuestion", Digest: strings.Repeat("b", 64), Status: standingExternal}
			if err := writeClaudePermFile(filepath.Join(dir, item.ID+".json"), item); err != nil {
				t.Fatal(err)
			}
		}
		return bundle
	}

	removeBundle := fullResolved(t)
	oldRemove := removeStandingRecord
	t.Cleanup(func() { removeStandingRecord = oldRemove })
	removeStandingRecord = func(string) error { return errors.New("synthetic remove failure") }
	if err := enforceStandingBudget(removeBundle, "incoming.json", 1); !errors.Is(err, errInteractionIO) {
		t.Fatalf("cleanup remove failure = %v", err)
	}
	removeStandingRecord = oldRemove

	syncBundle := fullResolved(t)
	oldSync := syncStandingDirectory
	t.Cleanup(func() { syncStandingDirectory = oldSync })
	syncStandingDirectory = func(string) error { return errors.New("synthetic sync failure") }
	if err := enforceStandingBudget(syncBundle, "incoming.json", 1); !errors.Is(err, errInteractionIO) {
		t.Fatalf("cleanup sync failure = %v", err)
	}
	syncStandingDirectory = oldSync
}

func reviewStandingHarness(t *testing.T, id string, expire bool) (*app, *fakeTmux, *Node, string, string, *claudeSessionHarness, claudeInteractionCaller) {
	t.Helper()
	id = strings.ReplaceAll(id, "/", "-")
	a, f, n, bundle, epoch := setupStandingQuestion(t, id, "", nil)
	if expire {
		retireQuestionPayload(t, bundle, "req-letter")
	}
	h := testClaudeSessionHarness(a, n.ID)
	caller := claudeInteractionCaller{NodeID: n.ID, SessionID: n.SessionID, BundleID: filepath.Base(bundle)}
	if generation, nodeID, ok := authoritativeInteractionGeneration(bundle); ok {
		caller.Generation = generation
		if nodeID != "" {
			caller.NodeID = nodeID
		}
	}
	return a, f, n, bundle, epoch, h, caller
}

func TestReviewStandingManualStateFailureOutcomes(t *testing.T) {
	if err := markStandingManual("relative", "req-letter", "epoch"); err == nil {
		t.Fatal("unsafe lock path accepted")
	}
	badRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(badRoot, "perm"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badRoot, "perm", "interact"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := markStandingManual(badRoot, "req-letter", "epoch"); err == nil {
		t.Fatal("unavailable fence accepted")
	}

	for _, test := range []struct {
		name string
		set  func(*testing.T, string) string
	}{
		{name: "missing record", set: func(t *testing.T, bundle string) string {
			if err := os.Remove(filepath.Join(bundle, "perm", "interact", "standing", "req-letter.json")); err != nil {
				t.Fatal(err)
			}
			return "epoch"
		}},
		{name: "empty epoch", set: func(_ *testing.T, _ string) string { return "" }},
		{name: "sent record", set: func(t *testing.T, bundle string) string {
			st := standingOrZero(bundle, "req-letter")
			st.SentKey = "1"
			if err := writeClaudePermFile(filepath.Join(bundle, "perm", "interact", "standing", st.ID+".json"), st); err != nil {
				t.Fatal(err)
			}
			return "epoch"
		}},
		{name: "structured answer pending", set: func(t *testing.T, bundle string) string {
			if err := exclWrite(filepath.Join(bundle, "perm", "interact", "claim", "req-letter"), []byte("claim")); err != nil {
				t.Fatal(err)
			}
			return "epoch"
		}},
		{name: "write failure", set: func(t *testing.T, bundle string) string {
			if err := os.Mkdir(filepath.Join(bundle, "perm", "interact", "standing", "req-letter.json.tmp"), 0o700); err != nil {
				t.Fatal(err)
			}
			return "epoch"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, _, bundle, _, _, _ := reviewStandingHarness(t, "review-mark-"+strings.ReplaceAll(test.name, " ", "-"), false)
			epoch := test.set(t, bundle)
			err := markStandingManual(bundle, "req-letter", epoch)
			if err == nil {
				t.Fatal("invalid manual state was accepted")
			}
		})
	}
}

func TestReviewStandingHandoffFailureOutcomes(t *testing.T) {
	run := func(t *testing.T, setup func(*testing.T, *app, *fakeTmux, *Node, string, *claudeSessionHarness, *claudeInteractionCaller), audit func(*testing.T, string) error, wantErr bool, wantPhase string) {
		t.Helper()
		a, f, n, bundle, epoch, h, caller := reviewStandingHarness(t, "review-handoff-"+t.Name(), true)
		setup(t, a, f, n, bundle, h, &caller)
		auditFn := func(sessionlog.Event) error { return audit(t, bundle) }
		result, err := h.handoffStandingQuestion(n, bundle, caller, sessionworker.InteractionAction{RequestID: "req-letter", Action: "terminal"}, auditFn)
		if wantErr && err == nil {
			t.Fatalf("handoff accepted unexpectedly: %+v", result)
		}
		if !wantErr && err != nil {
			t.Fatalf("handoff failed: %v", err)
		}
		if wantPhase != "" && result.Phase != wantPhase {
			t.Fatalf("phase = %q, want %q; result=%+v", result.Phase, wantPhase, result)
		}
		_ = epoch
	}
	noop := func(*testing.T, *app, *fakeTmux, *Node, string, *claudeSessionHarness, *claudeInteractionCaller) {}
	noAuditError := func(*testing.T, string) error { return nil }

	t.Run("audit failure", func(t *testing.T) {
		run(t, noop, func(*testing.T, string) error { return errors.New("audit failed") }, true, "")
	})
	t.Run("structured decision pending", func(t *testing.T) {
		run(t, func(t *testing.T, _ *app, _ *fakeTmux, _ *Node, bundle string, _ *claudeSessionHarness, _ *claudeInteractionCaller) {
			if err := exclWrite(filepath.Join(bundle, "perm", "interact", "claim", "req-letter"), []byte("claim")); err != nil {
				t.Fatal(err)
			}
		}, noAuditError, true, "")
	})
	t.Run("manual state idempotent", func(t *testing.T) {
		run(t, func(t *testing.T, _ *app, _ *fakeTmux, _ *Node, bundle string, _ *claudeSessionHarness, _ *claudeInteractionCaller) {
			if err := markStandingManual(bundle, "req-letter", currentTestEpoch(t, bundle)); err != nil {
				t.Fatal(err)
			}
		}, noAuditError, false, "manual")
	})
	t.Run("sent state idempotent", func(t *testing.T) {
		run(t, func(t *testing.T, _ *app, _ *fakeTmux, _ *Node, bundle string, _ *claudeSessionHarness, _ *claudeInteractionCaller) {
			st := standingOrZero(bundle, "req-letter")
			st.SentKey = "1"
			st.Delivery = "unconfirmed"
			if err := writeClaudePermFile(filepath.Join(bundle, "perm", "interact", "standing", st.ID+".json"), st); err != nil {
				t.Fatal(err)
			}
		}, noAuditError, false, "unconfirmed")
	})
	t.Run("audit changes dialog epoch", func(t *testing.T) {
		run(t, noop, func(t *testing.T, bundle string) error {
			_, err := mintClaudeVisibleEpoch(filepath.Join(bundle, "perm"), hookSIDOwn)
			return err
		}, true, "")
	})
	t.Run("audit changes question state", func(t *testing.T) {
		run(t, noop, func(t *testing.T, bundle string) error {
			st := standingOrZero(bundle, "req-letter")
			st.Status = standingExternal
			return writeClaudePermFile(filepath.Join(bundle, "perm", "interact", "standing", st.ID+".json"), st)
		}, true, "")
	})
	t.Run("audit blocks final state write", func(t *testing.T) {
		run(t, noop, func(t *testing.T, bundle string) error {
			return os.Mkdir(filepath.Join(bundle, "perm", "interact", "standing", "req-letter.json.tmp"), 0o700)
		}, true, "")
	})
}

func TestReviewStandingHandoffPreflightOutcomes(t *testing.T) {
	check := func(t *testing.T, configure func(*testing.T, *app, *fakeTmux, *Node, string, *claudeSessionHarness, *claudeInteractionCaller), audit func(*testing.T, string) error) {
		t.Helper()
		a, f, n, bundle, _, h, caller := reviewStandingHarness(t, "review-handoff-preflight-"+t.Name(), true)
		if configure != nil {
			configure(t, a, f, n, bundle, h, &caller)
		}
		fn := func(sessionlog.Event) error {
			if audit == nil {
				return nil
			}
			return audit(t, bundle)
		}
		_, err := h.handoffStandingQuestion(n, bundle, caller, sessionworker.InteractionAction{RequestID: "req-letter", Action: "terminal"}, fn)
		if err == nil {
			t.Fatal("invalid handoff was accepted")
		}
	}
	noop := func(*testing.T, *app, *fakeTmux, *Node, string, *claudeSessionHarness, *claudeInteractionCaller) {}
	t.Run("lock failure", func(t *testing.T) {
		_, _, n, _, _, h, caller := reviewStandingHarness(t, "review-handoff-lock", true)
		if _, err := h.handoffStandingQuestion(n, "relative", caller, sessionworker.InteractionAction{RequestID: "req-letter", Action: "terminal"}, func(sessionlog.Event) error { return nil }); err == nil {
			t.Fatal("unsafe lock path accepted")
		}
	})
	t.Run("missing record", func(t *testing.T) {
		check(t, func(t *testing.T, _ *app, _ *fakeTmux, _ *Node, bundle string, _ *claudeSessionHarness, _ *claudeInteractionCaller) {
			if err := os.Remove(filepath.Join(bundle, "perm", "interact", "standing", "req-letter.json")); err != nil {
				t.Fatal(err)
			}
		}, nil)
	})
	t.Run("caller identity mismatch", func(t *testing.T) {
		check(t, func(_ *testing.T, _ *app, _ *fakeTmux, _ *Node, _ string, _ *claudeSessionHarness, caller *claudeInteractionCaller) {
			caller.NodeID = "different-node"
		}, nil)
	})
	t.Run("missing dialog", func(t *testing.T) {
		check(t, func(_ *testing.T, a *app, _ *fakeTmux, n *Node, _ string, _ *claudeSessionHarness, _ *claudeInteractionCaller) {
			a.retireClaudeVisibleDialog(n, currentTestEpoch(t, a.claudeHookBundle(n)))
		}, nil)
	})
	t.Run("menu mismatch", func(t *testing.T) {
		check(t, func(_ *testing.T, _ *app, f *fakeTmux, _ *Node, _ string, _ *claudeSessionHarness, _ *claudeInteractionCaller) {
			f.capture = "Unrelated prompt\n1. Unrelated\n2. Menu"
		}, nil)
	})
	t.Run("audit replaces lock file", func(t *testing.T) {
		check(t, noop, func(t *testing.T, bundle string) error {
			fence := filepath.Join(bundle, "perm", "interact", "fence.lock")
			if err := os.Remove(fence); err != nil {
				return err
			}
			return os.Mkdir(fence, 0o700)
		})
	})
	t.Run("audit changes menu", func(t *testing.T) {
		var f *fakeTmux
		check(t, func(_ *testing.T, _ *app, ft *fakeTmux, _ *Node, _ string, _ *claudeSessionHarness, _ *claudeInteractionCaller) {
			f = ft
		}, func(_ *testing.T, _ string) error {
			f.capture = "Unrelated prompt\n1. Alpha\n2. Beta\n3. Other\n\nEnter selection [1-3], or Escape to cancel:"
			return nil
		})
	})
}

func TestReviewStandingHandoffWaitsForStructuredHelper(t *testing.T) {
	a, _, n, bundle, _, h, caller := reviewStandingHarness(t, "review-handoff-helper-live", false)
	_, err := h.handoffStandingQuestion(n, bundle, caller, sessionworker.InteractionAction{RequestID: "req-letter", Action: "terminal"}, func(sessionlog.Event) error {
		return nil
	})
	if err == nil || standingOrZero(a.claudeHookBundle(n), "req-letter").Manual {
		t.Fatalf("live structured helper lost authority: err=%v standing=%+v", err, standingOrZero(a.claudeHookBundle(n), "req-letter"))
	}
}

func TestReviewTerminalHandoffAuditFailureDoesNotMarkManual(t *testing.T) {
	a, f, _, bundle, _, h, _ := reviewStandingHarness(t, "review-handoff-audit-failure", true)
	a.sessionsDir = ""
	_, err := h.SubmitClaudeInteraction(context.Background(), sessionworker.InteractionAction{RequestID: "req-letter", Action: "terminal"})
	st := standingOrZero(bundle, "req-letter")
	if err == nil || st.Manual || len(keyTails(f)) != 0 {
		t.Fatalf("failed handoff audit claimed control: err=%v standing=%+v keys=%v", err, st, keyTails(f))
	}
}

func TestReviewStructuredTerminalWithoutDialogCannotMarkManual(t *testing.T) {
	a, _, n, bundle, epoch, h, _ := reviewStandingHarness(t, "review-terminal-no-dialog", false)
	a.retireClaudeVisibleDialog(n, epoch)
	_, err := h.SubmitClaudeInteraction(context.Background(), sessionworker.InteractionAction{RequestID: "req-letter", Action: "terminal"})
	st := standingOrZero(bundle, "req-letter")
	if err == nil || st.Manual || st.ManualEpoch != "" {
		t.Fatalf("missing dialog was recorded as manual handoff: err=%v standing=%+v", err, st)
	}
}

func currentTestEpoch(t *testing.T, bundle string) string {
	t.Helper()
	item, ok := readClaudeVisibleEpoch(filepath.Join(bundle, "perm"))
	if !ok {
		t.Fatal("dialog epoch missing")
	}
	return item.Epoch
}

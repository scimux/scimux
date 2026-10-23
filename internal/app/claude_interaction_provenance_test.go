package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/transcript"
)

func TestClearGenerationWriteFailurePreservesBindingAndHistory(t *testing.T) {
	t.Setenv("SCIMUX_EXPERIMENTAL_CLAUDE_INTERACTIONS", "1")
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	old := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn, claudeUserLine("old", 0))
	next := writeClaudeProject(t, a.home, "-w-proj", hookSIDOther, claudeUserLine("new", 0))
	n.Transcript, n.Dir = old, "/w/proj"
	a.claudeGens[n.ID] = 1
	tailer := &transcript.Tailer{Path: old}
	a.tailers[n.ID] = tailer
	if err := a.appendRecord(storeRecord{Type: "node", Node: n}); err != nil {
		t.Fatal(err)
	}
	logPath := a.sessionLogPath(n.ID)
	writer := &sessionlog.Writer{Path: logPath}
	if err := writer.Append(sessionlog.Event{T: "user", Text: "Keep this history."}); err != nil {
		t.Fatal(err)
	}
	before := mustRead(t, logPath)
	publishSyntheticInteraction(t, bundle, "clear-old", "AskUserQuestion", letterQuestionRaw(), hookSIDOwn)
	storePath := a.storePath
	a.storePath = t.TempDir() // A directory fails append even when tests run as root.
	ev := claudeSessionStartEvent{HookEventName: "SessionStart", Source: "clear", Cwd: n.Dir, SessionID: hookSIDOther, TranscriptPath: next}
	if err := a.processClaudeHookEventAt(n.ID, ev, 1); err == nil {
		t.Fatal("clear succeeded without a durable generation")
	}
	a.storePath = storePath
	if n.Transcript != old || n.SessionID != hookSIDOwn || a.claudeGeneration(n.ID) != 1 || a.tailers[n.ID] != tailer {
		t.Fatal("failed pre-commit clear changed the old binding")
	}
	if a.isDeadTranscript(n.ID, old, hookSIDOwn) || !a.claudeInteractionBlocksNativeKey(n) {
		t.Fatal("failed pre-commit clear retired the old session or interaction")
	}
	if !bytes.Equal(before, mustRead(t, logPath)) || a.claudeLaunchError(n.ID) != claudeResumeExplain {
		t.Fatal("failed pre-commit clear changed history or claimed a committed binding")
	}
	// Retrying the same hook must work: the temporary path claim was released.
	if err := a.processClaudeHookEventAt(n.ID, ev, 1); err != nil {
		t.Fatal(err)
	}
	if n.Transcript != next || n.SessionID != hookSIDOther || a.claudeGeneration(n.ID) != 2 {
		t.Fatal("clear did not recover after restoring the durable store")
	}
}

// Synthetic decisions exercise actual durable history, not captured hook traces.
func TestInteractionOutcomePreservesApplicationProvenance(t *testing.T) {
	for _, action := range []string{"answer", "discuss", "approve"} {
		for _, failure := range []string{"expired", "turn", "publication"} {
			t.Run(action+"/"+failure, func(t *testing.T) {
				bundle := interactionBundle(t)
				now := time.Now().UTC()
				rec := sampleRequest(bundle, now)
				reply := letterReply()
				if action == "discuss" {
					reply = claudeInteractionReply{Action: action, Feedback: "Human feedback must appear once."}
				}
				if action == "approve" {
					rec.Tool, rec.PermissionMode, rec.Original = "ExitPlanMode", "plan", tinyPlanRaw()
					reply = claudeInteractionReply{Action: action, Mode: "acceptEdits"}
				}
				publishInteraction(t, bundle, rec)
				path := filepath.Join(t.TempDir(), "audit.jsonl")
				writer := &sessionlog.Writer{Path: path}
				appends := 0
				_, err := submitClaudeInteractionClock(bundle, sampleCaller(bundle), rec.ID, reply, func() time.Time { return now }, func(ev sessionlog.Event) error {
					if err := writer.Append(ev); err != nil {
						return err
					}
					appends++
					if appends == 1 {
						switch failure {
						case "expired":
							now = now.Add(2 * time.Minute)
						case "turn":
							if err := writeClaudeAcceptedTurn(filepath.Join(bundle, "perm"), claudeAcceptedTurn{Turn: "successor", Gen: 2}); err != nil {
								t.Fatal(err)
							}
						case "publication":
							if err := os.Mkdir(filepath.Join(bundle, "perm", "interact", "ans", rec.ID+".json"), 0700); err != nil {
								t.Fatal(err)
							}
						}
					}
					return nil
				})
				if err == nil || appends != 2 {
					t.Fatalf("want failed submission with intent and outcome, got err=%v appends=%d", err, appends)
				}
				events := sessionlog.ReadEvents(path)
				if len(events) != 2 || events[0].Decision == nil || events[1].Decision == nil {
					t.Fatalf("durable events: %+v", events)
				}
				intent, outcome := events[0].Decision, events[1].Decision
				if intent.Source != "manual" || intent.Manual == nil || intent.Manual.Action != action || intent.Manual.Delivery != "recorded" {
					t.Fatalf("human intent changed: %+v", intent)
				}
				if intent.Manual.Feedback != reply.Feedback || action == "answer" && len(intent.Manual.Selections) != 1 || action == "approve" && intent.Manual.Reviewed == "" {
					t.Fatalf("human content was lost: %+v", intent.Manual)
				}
				if outcome.Source != "application" || outcome.Manual != nil || outcome.RequestID != intent.RequestID || outcome.LeaseID != "" {
					t.Fatalf("outcome duplicates human authorship: %+v", outcome)
				}
				raw, err := json.Marshal(outcome)
				if err != nil {
					t.Fatal(err)
				}
				var payload struct {
					Outcome map[string]string `json:"outcome"`
				}
				if err := json.Unmarshal(raw, &payload); err != nil {
					t.Fatal(err)
				}
				wantDelivery := "not_delivered"
				wantContext := "Application delivery outcome: this submission was rejected before delivery."
				if failure == "publication" {
					wantDelivery = "unconfirmed"
					wantContext = "Application delivery outcome: submission failed; delivery is unconfirmed."
				}
				if len(payload.Outcome) != 2 || payload.Outcome["delivery"] != wantDelivery || payload.Outcome["app_context"] != wantContext {
					t.Fatalf("outcome must contain only status and fixed context, without raw errors or human content: %s", raw)
				}
				if outcome.Title != "" || outcome.Reason != "" || len(outcome.Options) != 0 || outcome.Selected.Kind != wantDelivery {
					t.Fatalf("outcome repeats the human choice: %+v", outcome)
				}
			})
		}
	}
}

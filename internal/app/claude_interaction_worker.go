package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/sessionworker"
)

// claudeHookBundle is the on-disk hook directory for an owned Claude node.
// An unsafe hook id yields an empty path so a request cannot escape the root.
func (a *app) claudeHookBundle(n *Node) string {
	id := a.claudeHookID(n.ID)
	if id == "" || !safePathComponent(id) {
		return ""
	}
	root := a.claudeHooksDir()
	if root == "" {
		return ""
	}
	return filepath.Join(root, id)
}

func interactionNativeKeys(phase string) bool {
	switch phase {
	case "pending", "claimed", "recorded", "emitted", "unconfirmed":
		return false
	default:
		return true
	}
}

func interactionNeedsAttention(phase string) bool {
	return phase == "pending" || phase == "claimed"
}

// claudeInteractionView is the browser projection of the one active manual
// interaction. It is empty when the bundle was not minted with the gate, or
// when no request owns the slot. PlanFilePath is never copied.
func (a *app) claudeInteractionView(n *Node) (sessionworker.InteractionView, bool) {
	var zero sessionworker.InteractionView
	if n == nil || n.Agent != "claude" {
		return zero, false
	}
	bundle := a.claudeHookBundle(n)
	if bundle == "" || !bundleSupportsInteraction(bundle) {
		return zero, false
	}
	id, ok := readInteractionActive(bundle)
	if !ok {
		return zero, false
	}
	rec, err := readInteractionRequest(bundle, id)
	if err != nil {
		return zero, false
	}
	model, err := classifyClaudeInteraction(rec.Tool, rec.Original)
	if err != nil || model.Native {
		return zero, false
	}
	phase := claudeInteractionPhase(bundle, id, time.Now())
	view := sessionworker.InteractionView{
		RequestID:   id,
		Phase:       phase,
		Tool:        rec.Tool,
		Digest:      rec.Digest,
		AnswerUntil: rec.AnswerUntil,
		Plan:        model.Plan,
		NativeKeys:  interactionNativeKeys(phase),
	}
	for _, question := range model.Questions {
		item := sessionworker.InteractionQuestion{
			ID: question.ID, Text: question.Text, Header: question.Header,
			Multi: question.Multi, Preview: question.Preview,
		}
		for _, option := range question.Options {
			item.Options = append(item.Options, sessionworker.InteractionOption{
				ID: option.ID, Label: option.Label, Description: option.Description,
			})
		}
		view.Questions = append(view.Questions, item)
	}
	if rec.Tool == "ExitPlanMode" {
		view.Modes = []sessionworker.InteractionModeChoice{
			{ID: "restore", Label: interactionModeRestoreLabel},
			{ID: "default", Label: interactionModeDefaultLabel},
			{ID: "acceptEdits", Label: interactionModeEditsLabel},
		}
	}
	return view, true
}

func (a *app) claudeInteractionNeedsAttention(n *Node) bool {
	view, ok := a.claudeInteractionView(n)
	return ok && interactionNeedsAttention(view.Phase)
}

func (a *app) claudeInteractionBlocksNativeKey(n *Node) bool {
	view, ok := a.claudeInteractionView(n)
	return ok && !view.NativeKeys
}

func (h *claudeSessionHarness) ClaudeInteraction(context.Context) (sessionworker.InteractionView, error) {
	view, ok := h.app.claudeInteractionView(h.node())
	if !ok {
		return sessionworker.InteractionView{}, sessionworker.ErrInteractionUnsupported
	}
	return view, nil
}

func (h *claudeSessionHarness) SubmitClaudeInteraction(_ context.Context, action sessionworker.InteractionAction) (sessionworker.InteractionResult, error) {
	defer h.refreshState()
	n := h.node()
	if n == nil {
		return sessionworker.InteractionResult{}, errClaudeWorkerConflict
	}
	bundle := h.app.claudeHookBundle(n)
	if bundle == "" || !bundleSupportsInteraction(bundle) {
		return sessionworker.InteractionResult{}, sessionworker.ErrInteractionUnsupported
	}
	caller := claudeInteractionCaller{
		NodeID: n.ID, SessionID: n.SessionID, BundleID: filepath.Base(bundle),
	}
	if gen, node, ok := authoritativeInteractionGeneration(bundle); ok {
		caller.Generation = gen
		if node != "" {
			caller.NodeID = node
		}
	}
	if turn, ok := readClaudeAcceptedTurn(filepath.Join(bundle, "perm")); ok {
		caller.Turn = turn.Turn
		caller.TurnGen = turn.Gen
	}
	reply := claudeInteractionReply{Action: action.Action, Feedback: action.Feedback, Mode: action.Mode}
	for _, answer := range action.Answers {
		reply.Answers = append(reply.Answers, claudeInteractionAnswer{
			QuestionID: answer.QuestionID,
			OptionIDs:  append([]string(nil), answer.OptionIDs...),
			FreeText:   answer.FreeText,
		})
	}
	receipt, err := submitClaudeInteractionClock(bundle, caller, action.RequestID, reply, time.Now, func(ev sessionlog.Event) error {
		if h.app.sessionsDir == "" {
			return errors.New("session log unavailable")
		}
		return (&sessionlog.Writer{Path: h.app.sessionLogPath(n.ID)}).Append(ev)
	})
	if err != nil {
		return sessionworker.InteractionResult{}, classifyHarnessInteractionErr(err)
	}
	return sessionworker.InteractionResult{
		Status: receipt.Status, Delivery: receipt.Delivery, RequestID: receipt.RequestID,
		Emitted: receipt.Emitted, Phase: receipt.Phase,
	}, nil
}

func classifyHarnessInteractionErr(err error) error {
	switch {
	case errors.Is(err, errInteractionBadRequest):
		return fmt.Errorf("%w: %s", sessionworker.ErrInteractionBadRequest, err.Error())
	case errors.Is(err, errInteractionConflict):
		return fmt.Errorf("%w: %s", errClaudeWorkerConflict, err.Error())
	default:
		return err
	}
}

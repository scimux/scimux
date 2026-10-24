// claude_interaction.go — pure contract for a human answer to Claude
// AskUserQuestion and ExitPlanMode.
//
// This file does not read plan files, does not approve ordinary tool calls,
// and does not register PreToolUse. A structured reply is built only from the
// hook's original tool_input bytes plus the human's typed choice.
//
// Limits are byte lengths of valid UTF-8, chosen before the boundary tests.
// They are narrower than the 64 KiB hook stdin bound and the 1 MiB public body
// ceiling. Oversized question or plan content keeps the native path. Oversized
// human replies are rejected. Nothing here is truncated to fit.
//
//	feedback and each free-text answer: 4096
//	public interaction POST body:        32768
//	question text:                       2048
//	option label:                        512
//	option description:                  2048
//	header:                              80
//	preview:                             2048
//	plan text:                           32768
//	rendered question/plan payload:      49152
//
// A capable bundle's PermissionRequest command timeout is 180 seconds.
// The helper stops at 150 seconds. The app accepts a decision only until
// 135 seconds. The ordinary 5 second permission deadline is unchanged.
// Multi-select labels join with ", " in option order. That join was observed
// on Claude Code 2.1.304; it is not a claim about every later version.
package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	interactionFeedbackMax     = 4096
	interactionFreeTextMax     = 4096
	interactionRenderedMax     = 48 * 1024
	interactionPostMax         = 32 * 1024
	interactionQuestionTextMax = 2048
	interactionOptionLabelMax  = 512
	interactionOptionDescMax   = 2048
	interactionHeaderMax       = 80
	interactionPreviewMax      = 2048
	interactionPlanTextMax     = 32 * 1024
	interactionHookTimeoutSec  = 180
	interactionHelperDeadline  = 150 * time.Second
	interactionRejectMargin    = 15 * time.Second
	interactionAnswerWindow    = interactionHelperDeadline - interactionRejectMargin
	interactionMultiJoin       = ", "
)

const (
	interactionContextDiscuss = "scimux sent this as discussion. The human did not select an option. Claude should stay on this question and may revise its next question. This application context is not the human's feedback."
	interactionContextChanges = "scimux sent this as a request to revise the plan. The human did not approve the plan. Stay in plan mode and do not start implementation. This application context is not the human's feedback."
	interactionContextCancel  = "The human requested interruption of this interaction. This is not an answer, not feedback, and not a mode change."
)

const (
	interactionModeRestoreLabel = "Restore Claude's previous mode"
	interactionModeDefaultLabel = "Manual review (default)"
	interactionModeEditsLabel   = "Accept edits for this session"
)

// claudeInteractionModel is the server-held view of one supported request.
// Native means the ordinary manual path must be kept.
type claudeInteractionModel struct {
	Tool         string
	Native       bool
	NativeReason string
	Questions    []claudeInteractionQuestion
	Plan         string
	PlanFilePath string
	Original     []byte
	Digest       string
}

type claudeInteractionQuestion struct {
	ID      string
	Text    string
	Header  string
	Multi   bool
	Preview string
	Options []claudeInteractionOption
}

type claudeInteractionOption struct {
	ID          string
	Label       string
	Description string
}

type claudeInteractionAnswer struct {
	QuestionID string
	OptionIDs  []string
	FreeText   string
}

type claudeInteractionReply struct {
	Action   string
	Answers  []claudeInteractionAnswer
	Feedback string
	Mode     string
}

type claudeInteractionSelection struct {
	QuestionID string
	Question   string
	OptionIDs  []string
	Labels     []string
	FreeText   string
	Kind       string
}

type claudeInteractionAudit struct {
	Action          string
	Tool            string
	InputDigest     string
	Feedback        string
	AppContext      string
	Mode            string
	ModeLabel       string
	EstablishedMode string
	Selections      []claudeInteractionSelection
}

type claudeInteractionEnvelope struct {
	Stdout   []byte
	Terminal bool
	Audit    claudeInteractionAudit
}

type jsonField struct {
	Key string
	Raw []byte
}

func interactionNative(reason string) (claudeInteractionModel, error) {
	return claudeInteractionModel{Native: true, NativeReason: reason}, nil
}

// jsonHasReplacementRune reports whether any JSON string, including object
// keys, decodes to text containing U+FFFD. Invalid JSON returns false.
func jsonHasReplacementRune(raw []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	// A number outside float64 range is still valid JSON. UseNumber keeps
	// scanning so a later string is not skipped. A decoder error is not itself
	// a replacement character.
	dec.UseNumber()
	for {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		if s, ok := tok.(string); ok && strings.ContainsRune(s, utf8.RuneError) {
			return true
		}
	}
}

func classifyClaudeInteraction(tool string, raw []byte) (claudeInteractionModel, error) {
	if tool != "AskUserQuestion" && tool != "ExitPlanMode" {
		return interactionNative("unsupported tool")
	}
	if !utf8.Valid(raw) {
		return interactionNative("invalid utf-8")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return interactionNative("not object")
	}
	if !json.Valid(raw) {
		return interactionNative("malformed json")
	}
	if jsonHasReplacementRune(raw) {
		return interactionNative("malformed json")
	}
	fields, err := walkJSONObject(raw)
	if err != nil {
		return interactionNative("malformed json")
	}
	switch tool {
	case "AskUserQuestion":
		return classifyQuestionInput(raw, fields)
	default:
		return classifyPlanInput(raw, fields)
	}
}

func classifyQuestionInput(raw []byte, fields []jsonField) (claudeInteractionModel, error) {
	var questionsRaw []byte
	hasQuestions := false
	for _, field := range fields {
		switch field.Key {
		case "answers":
			return interactionNative("answers present")
		case "questions":
			questionsRaw, hasQuestions = field.Raw, true
		default:
			if !benignJSONValue(field.Raw) {
				return interactionNative("unknown member")
			}
		}
	}
	if !hasQuestions {
		return interactionNative("question count")
	}
	elems, err := walkJSONArray(bytes.TrimSpace(questionsRaw))
	if err != nil {
		return interactionNative("question shape")
	}
	if len(elems) < 1 || len(elems) > 4 {
		return interactionNative("question count")
	}
	questions := make([]claudeInteractionQuestion, 0, len(elems))
	seen := map[string]bool{}
	rendered := 0
	for i, elem := range elems {
		question, reason, size := parseInteractionQuestion(elem, i+1)
		if reason != "" {
			return interactionNative(reason)
		}
		if seen[question.Text] {
			return interactionNative("duplicate question")
		}
		seen[question.Text] = true
		rendered += size
		questions = append(questions, question)
	}
	if rendered > interactionRenderedMax {
		return interactionNative("rendered length")
	}
	return acceptedInteraction("AskUserQuestion", raw, questions, "", ""), nil
}

func parseInteractionQuestion(raw []byte, index int) (claudeInteractionQuestion, string, int) {
	fields, err := walkJSONObject(bytes.TrimSpace(raw))
	if err != nil {
		return claudeInteractionQuestion{}, "question shape", 0
	}
	var textRaw, headerRaw, previewRaw, optionsRaw, multiRaw []byte
	hasText, hasHeader, hasPreview, hasOptions, hasMulti := false, false, false, false, false
	for _, field := range fields {
		switch field.Key {
		case "question":
			textRaw, hasText = field.Raw, true
		case "header":
			headerRaw, hasHeader = field.Raw, true
		case "preview":
			previewRaw, hasPreview = field.Raw, true
		case "options":
			optionsRaw, hasOptions = field.Raw, true
		case "multiSelect":
			multiRaw, hasMulti = field.Raw, true
		default:
			if !benignJSONValue(field.Raw) {
				return claudeInteractionQuestion{}, "unknown member", 0
			}
		}
	}
	text, reason := requiredInteractionString(hasText, textRaw, "empty question", "question type", "question length", interactionQuestionTextMax)
	if reason != "" {
		return claudeInteractionQuestion{}, reason, 0
	}
	header := ""
	if hasHeader {
		header, reason = optionalInteractionString(headerRaw, "header type", "header length", interactionHeaderMax)
		if reason != "" {
			return claudeInteractionQuestion{}, reason, 0
		}
	}
	preview := ""
	if hasPreview && !jsonNull(previewRaw) {
		preview, reason = optionalInteractionString(previewRaw, "preview type", "preview length", interactionPreviewMax)
		if reason != "" {
			return claudeInteractionQuestion{}, reason, 0
		}
	}
	multi := false
	if hasMulti {
		var ok bool
		multi, ok = decodeJSONBool(multiRaw)
		if !ok {
			return claudeInteractionQuestion{}, "multiSelect type", 0
		}
	}
	if !hasOptions {
		return claudeInteractionQuestion{}, "option count", 0
	}
	optElems, err := walkJSONArray(bytes.TrimSpace(optionsRaw))
	if err != nil {
		return claudeInteractionQuestion{}, "option shape", 0
	}
	if len(optElems) < 2 || len(optElems) > 4 {
		return claudeInteractionQuestion{}, "option count", 0
	}
	question := claudeInteractionQuestion{
		ID:      fmt.Sprintf("q%d", index),
		Text:    text,
		Header:  header,
		Multi:   multi,
		Preview: preview,
	}
	labels := make([]string, 0, len(optElems))
	seenLabel := map[string]bool{}
	size := len(text) + len(header) + len(preview)
	for i, elem := range optElems {
		opt, reason := parseInteractionOption(elem, question.ID, i+1)
		if reason != "" {
			return claudeInteractionQuestion{}, reason, 0
		}
		if seenLabel[opt.Label] {
			return claudeInteractionQuestion{}, "duplicate label", 0
		}
		seenLabel[opt.Label] = true
		labels = append(labels, opt.Label)
		size += len(opt.Label) + len(opt.Description)
		question.Options = append(question.Options, opt)
	}
	if claudeMultiJoinAmbiguous(labels) {
		return claudeInteractionQuestion{}, "ambiguous labels", 0
	}
	return question, "", size
}

func parseInteractionOption(raw []byte, questionID string, index int) (claudeInteractionOption, string) {
	fields, err := walkJSONObject(bytes.TrimSpace(raw))
	if err != nil {
		return claudeInteractionOption{}, "option shape"
	}
	var labelRaw, descRaw []byte
	hasLabel, hasDesc := false, false
	for _, field := range fields {
		switch field.Key {
		case "label":
			labelRaw, hasLabel = field.Raw, true
		case "description":
			descRaw, hasDesc = field.Raw, true
		default:
			if !benignJSONValue(field.Raw) {
				return claudeInteractionOption{}, "unknown member"
			}
		}
	}
	label, reason := requiredInteractionString(hasLabel, labelRaw, "empty label", "label type", "label length", interactionOptionLabelMax)
	if reason != "" {
		return claudeInteractionOption{}, reason
	}
	desc := ""
	if hasDesc && !jsonNull(descRaw) {
		desc, reason = optionalInteractionString(descRaw, "description type", "description length", interactionOptionDescMax)
		if reason != "" {
			return claudeInteractionOption{}, reason
		}
	}
	return claudeInteractionOption{
		ID:          fmt.Sprintf("%so%d", questionID, index),
		Label:       label,
		Description: desc,
	}, ""
}

func classifyPlanInput(raw []byte, fields []jsonField) (claudeInteractionModel, error) {
	var planRaw, pathRaw, promptsRaw []byte
	hasPlan, hasPath, hasPrompts := false, false, false
	for _, field := range fields {
		switch field.Key {
		case "plan":
			planRaw, hasPlan = field.Raw, true
		case "planFilePath":
			pathRaw, hasPath = field.Raw, true
		case "allowedPrompts":
			promptsRaw, hasPrompts = field.Raw, true
		default:
			if !benignJSONValue(field.Raw) {
				return interactionNative("unknown member")
			}
		}
	}
	if !hasPlan || jsonNull(planRaw) {
		return interactionNative("plan missing")
	}
	plan, ok := decodeJSONStringValue(planRaw)
	if !ok {
		return interactionNative("plan type")
	}
	if plan == "" {
		return interactionNative("plan missing")
	}
	if len(plan) > interactionPlanTextMax || len(plan) > interactionRenderedMax {
		return interactionNative("plan length")
	}
	path := ""
	if hasPath && !jsonNull(pathRaw) {
		path, ok = decodeJSONStringValue(pathRaw)
		if !ok {
			return interactionNative("plan path type")
		}
	}
	if hasPrompts && !jsonNull(promptsRaw) {
		if _, err := walkJSONArray(bytes.TrimSpace(promptsRaw)); err != nil {
			return interactionNative("allowedPrompts type")
		}
	}
	return acceptedInteraction("ExitPlanMode", raw, nil, plan, path), nil
}

func acceptedInteraction(tool string, raw []byte, questions []claudeInteractionQuestion, plan, path string) claudeInteractionModel {
	original := append([]byte(nil), raw...)
	sum := sha256.Sum256(original)
	return claudeInteractionModel{
		Tool:         tool,
		Questions:    questions,
		Plan:         plan,
		PlanFilePath: path,
		Original:     original,
		Digest:       hex.EncodeToString(sum[:]),
	}
}

func requiredInteractionString(present bool, raw []byte, emptyReason, typeReason, lengthReason string, max int) (string, string) {
	if !present || jsonNull(raw) {
		return "", emptyReason
	}
	value, ok := decodeJSONStringValue(raw)
	if !ok {
		return "", typeReason
	}
	if value == "" {
		return "", emptyReason
	}
	if len(value) > max {
		return "", lengthReason
	}
	return value, ""
}

func optionalInteractionString(raw []byte, typeReason, lengthReason string, max int) (string, string) {
	value, ok := decodeJSONStringValue(raw)
	if !ok {
		return "", typeReason
	}
	if len(value) > max {
		return "", lengthReason
	}
	return value, ""
}

func benignJSONValue(raw []byte) bool {
	var value any
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&value); err != nil || dec.More() {
		return false
	}
	switch typed := value.(type) {
	case nil:
		return true
	case bool:
		return !typed
	case string:
		return typed == ""
	case []any:
		return len(typed) == 0
	case map[string]any:
		return len(typed) == 0
	default:
		return false
	}
}

func jsonNull(raw []byte) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func decodeJSONStringValue(raw []byte) (string, bool) {
	text := bytes.TrimSpace(raw)
	if len(text) == 0 || text[0] != '"' {
		return "", false
	}
	var value string
	if err := json.Unmarshal(text, &value); err != nil {
		return "", false
	}
	if strings.ContainsRune(value, utf8.RuneError) {
		return "", false
	}
	return value, true
}

func decodeJSONBool(raw []byte) (bool, bool) {
	switch string(bytes.TrimSpace(raw)) {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}

func claudeMultiJoinAmbiguous(labels []string) bool {
	n := len(labels)
	if n == 0 {
		return false
	}
	for mask := 1; mask < (1 << n); mask++ {
		if bits.OnesCount(uint(mask)) < 2 {
			continue
		}
		parts := make([]string, 0, n)
		for i := 0; i < n; i++ {
			if mask&(1<<i) != 0 {
				parts = append(parts, labels[i])
			}
		}
		joined := strings.Join(parts, interactionMultiJoin)
		for i, label := range labels {
			if mask&(1<<i) != 0 {
				continue
			}
			if label == joined {
				return true
			}
		}
	}
	for _, label := range labels {
		if strings.Contains(label, ",") {
			return true
		}
	}
	return false
}

func buildClaudeInteractionReply(model claudeInteractionModel, reply claudeInteractionReply) (claudeInteractionEnvelope, error) {
	if model.Native || (model.Tool != "AskUserQuestion" && model.Tool != "ExitPlanMode") {
		return claudeInteractionEnvelope{}, interactionBad("unsupported interaction")
	}
	switch reply.Action {
	case "answer":
		return buildInteractionAnswer(model, reply)
	case "discuss":
		return buildInteractionDeny(model, reply, false, interactionContextDiscuss, model.Tool == "AskUserQuestion")
	case "request_changes":
		return buildInteractionDeny(model, reply, false, interactionContextChanges, model.Tool == "ExitPlanMode")
	case "cancel":
		return buildInteractionDeny(model, reply, true, interactionContextCancel, true)
	case "approve":
		return buildInteractionApprove(model, reply)
	case "terminal":
		return buildInteractionTerminal(model, reply)
	default:
		return claudeInteractionEnvelope{}, interactionBad("unknown action")
	}
}

func buildInteractionAnswer(model claudeInteractionModel, reply claudeInteractionReply) (claudeInteractionEnvelope, error) {
	if model.Tool != "AskUserQuestion" {
		return claudeInteractionEnvelope{}, interactionBad("answer is only for questions")
	}
	if reply.Feedback != "" || reply.Mode != "" {
		return claudeInteractionEnvelope{}, interactionBad("answer carries another decision")
	}
	if len(reply.Answers) != len(model.Questions) {
		return claudeInteractionEnvelope{}, interactionBad("incomplete answers")
	}
	byID := map[string]claudeInteractionAnswer{}
	for _, answer := range reply.Answers {
		if _, ok := byID[answer.QuestionID]; ok || answer.QuestionID == "" {
			return claudeInteractionEnvelope{}, interactionBad("duplicate question")
		}
		byID[answer.QuestionID] = answer
	}
	keys := make([]string, 0, len(model.Questions))
	vals := make([]string, 0, len(model.Questions))
	selections := make([]claudeInteractionSelection, 0, len(model.Questions))
	for _, question := range model.Questions {
		answer, ok := byID[question.ID]
		if !ok {
			return claudeInteractionEnvelope{}, interactionBad("unknown question")
		}
		value, selection, err := selectionForQuestion(question, answer)
		if err != nil {
			return claudeInteractionEnvelope{}, err
		}
		keys = append(keys, question.Text)
		vals = append(vals, value)
		selections = append(selections, selection)
	}
	encoded, err := encodeAnswersObject(keys, vals)
	if err != nil {
		return claudeInteractionEnvelope{}, err
	}
	updated, err := spliceJSONMember(model.Original, "answers", encoded)
	if err != nil {
		return claudeInteractionEnvelope{}, interactionBad("original input cannot be preserved")
	}
	stdout := marshalInteractionValue(interactionHookBody{
		HookSpecificOutput: interactionHookOutput{
			HookEventName: "PermissionRequest",
			Decision:      interactionDecisionOut{Behavior: "allow", UpdatedInput: updated},
		},
	})
	return claudeInteractionEnvelope{
		Stdout: stdout,
		Audit:  baseInteractionAudit(model, "answer", selections),
	}, nil
}

func selectionForQuestion(question claudeInteractionQuestion, answer claudeInteractionAnswer) (string, claudeInteractionSelection, error) {
	selection := claudeInteractionSelection{QuestionID: question.ID, Question: question.Text}
	hasText := answer.FreeText != ""
	hasOpts := len(answer.OptionIDs) > 0
	if hasText && hasOpts {
		return "", claudeInteractionSelection{}, interactionBad("option and free text")
	}
	if hasText {
		if strings.TrimSpace(answer.FreeText) == "" {
			return "", claudeInteractionSelection{}, interactionBad("blank free text")
		}
		if len(answer.FreeText) > interactionFreeTextMax {
			return "", claudeInteractionSelection{}, interactionBad("free text length")
		}
		if question.Multi {
			return "", claudeInteractionSelection{}, interactionBad("free text on multi")
		}
		selection.Kind = "free_text"
		selection.FreeText = answer.FreeText
		return answer.FreeText, selection, nil
	}
	if !hasOpts {
		return "", claudeInteractionSelection{}, interactionBad("empty selection")
	}
	if !question.Multi && len(answer.OptionIDs) != 1 {
		return "", claudeInteractionSelection{}, interactionBad("single selection")
	}
	chosen := map[string]bool{}
	for _, id := range answer.OptionIDs {
		if chosen[id] {
			return "", claudeInteractionSelection{}, interactionBad("duplicate option")
		}
		chosen[id] = true
	}
	var ordered []claudeInteractionOption
	for _, opt := range question.Options {
		if chosen[opt.ID] {
			ordered = append(ordered, opt)
			delete(chosen, opt.ID)
		}
	}
	if len(chosen) != 0 {
		return "", claudeInteractionSelection{}, interactionBad("unknown option")
	}
	labels := make([]string, len(ordered))
	ids := make([]string, len(ordered))
	for i, opt := range ordered {
		labels[i] = opt.Label
		ids[i] = opt.ID
	}
	selection.OptionIDs = ids
	selection.Labels = labels
	if question.Multi {
		selection.Kind = "multi"
	} else {
		selection.Kind = "option"
	}
	return strings.Join(labels, interactionMultiJoin), selection, nil
}

func buildInteractionDeny(model claudeInteractionModel, reply claudeInteractionReply, interrupt bool, context string, allowed bool) (claudeInteractionEnvelope, error) {
	if !allowed {
		return claudeInteractionEnvelope{}, interactionBad("action does not apply")
	}
	if len(reply.Answers) != 0 || reply.Mode != "" {
		return claudeInteractionEnvelope{}, interactionBad("denial carries another decision")
	}
	feedback := reply.Feedback
	if interrupt {
		if feedback != "" {
			return claudeInteractionEnvelope{}, interactionBad("cancel carries feedback")
		}
	} else if strings.TrimSpace(feedback) == "" {
		return claudeInteractionEnvelope{}, interactionBad("blank feedback")
	} else if len(feedback) > interactionFeedbackMax {
		return claudeInteractionEnvelope{}, interactionBad("feedback length")
	}
	flag := interrupt
	stdout := marshalInteractionValue(interactionHookBody{
		HookSpecificOutput: interactionHookOutput{
			HookEventName: "PermissionRequest",
			Decision: interactionDecisionOut{
				Behavior:  "deny",
				Message:   interactionDenyMessage(feedback, context),
				Interrupt: &flag,
			},
		},
	})
	audit := baseInteractionAudit(model, reply.Action, nil)
	if !interrupt {
		audit.Feedback = feedback
	}
	audit.AppContext = context
	return claudeInteractionEnvelope{Stdout: stdout, Audit: audit}, nil
}

func interactionDenyMessage(feedback, context string) string {
	if feedback == "" {
		return "Application context:\n" + context
	}
	return "Human feedback:\n" + feedback + "\n\nApplication context:\n" + context
}

func buildInteractionApprove(model claudeInteractionModel, reply claudeInteractionReply) (claudeInteractionEnvelope, error) {
	if model.Tool != "ExitPlanMode" {
		return claudeInteractionEnvelope{}, interactionBad("approve is only for a plan")
	}
	if reply.Feedback != "" || len(reply.Answers) != 0 {
		return claudeInteractionEnvelope{}, interactionBad("approve carries another decision")
	}
	label, perms, err := planModeUpdate(reply.Mode)
	if err != nil {
		return claudeInteractionEnvelope{}, err
	}
	decision := interactionDecisionOut{Behavior: "allow", UpdatedInput: append([]byte(nil), model.Original...)}
	if perms != nil {
		decision.UpdatedPermissions = marshalInteractionValue(perms)
	}
	stdout := marshalInteractionValue(interactionHookBody{
		HookSpecificOutput: interactionHookOutput{HookEventName: "PermissionRequest", Decision: decision},
	})
	audit := baseInteractionAudit(model, "approve", nil)
	audit.Mode = reply.Mode
	audit.ModeLabel = label
	return claudeInteractionEnvelope{Stdout: stdout, Audit: audit}, nil
}

func planModeUpdate(mode string) (string, []interactionPermission, error) {
	switch mode {
	case "restore":
		return interactionModeRestoreLabel, nil, nil
	case "default":
		return interactionModeDefaultLabel, []interactionPermission{{Type: "setMode", Mode: "default", Destination: "session"}}, nil
	case "acceptEdits":
		return interactionModeEditsLabel, []interactionPermission{{Type: "setMode", Mode: "acceptEdits", Destination: "session"}}, nil
	default:
		return "", nil, interactionBad("mode")
	}
}

func buildInteractionTerminal(model claudeInteractionModel, reply claudeInteractionReply) (claudeInteractionEnvelope, error) {
	if reply.Feedback != "" || reply.Mode != "" || len(reply.Answers) != 0 {
		return claudeInteractionEnvelope{}, interactionBad("terminal carries another decision")
	}
	return claudeInteractionEnvelope{Terminal: true, Audit: baseInteractionAudit(model, "terminal", nil)}, nil
}

func baseInteractionAudit(model claudeInteractionModel, action string, selections []claudeInteractionSelection) claudeInteractionAudit {
	return claudeInteractionAudit{
		Action:      action,
		Tool:        model.Tool,
		InputDigest: model.Digest,
		Selections:  selections,
	}
}

func interactionBad(msg string) error {
	return fmt.Errorf("claude interaction: %s", msg)
}

type interactionHookBody struct {
	HookSpecificOutput interactionHookOutput `json:"hookSpecificOutput"`
}

type interactionHookOutput struct {
	HookEventName string                 `json:"hookEventName"`
	Decision      interactionDecisionOut `json:"decision"`
}

type interactionDecisionOut struct {
	Behavior           string          `json:"behavior"`
	UpdatedInput       json.RawMessage `json:"updatedInput,omitempty"`
	UpdatedPermissions json.RawMessage `json:"updatedPermissions,omitempty"`
	Message            string          `json:"message,omitempty"`
	Interrupt          *bool           `json:"interrupt,omitempty"`
}

type interactionPermission struct {
	Type        string `json:"type"`
	Mode        string `json:"mode"`
	Destination string `json:"destination"`
}

func marshalInteractionValue(value any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		// Structs used on the reply path are JSON-safe. Unsupported values are
		// a programmer error, not a hook decision.
		panic(err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func encodeAnswersObject(keys, vals []string) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := appendJSONString(&buf, keys[i]); err != nil {
			return nil, err
		}
		buf.WriteByte(':')
		if err := appendJSONString(&buf, vals[i]); err != nil {
			return nil, err
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func appendJSONString(buf *bytes.Buffer, value string) error {
	if !utf8.ValidString(value) {
		return interactionBad("invalid utf-8")
	}
	// A valid UTF-8 string has no encode failure. The error is ignored so the
	// production path does not carry a branch tests cannot reach.
	var tmp bytes.Buffer
	enc := json.NewEncoder(&tmp)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
	buf.Write(bytes.TrimRight(tmp.Bytes(), "\n"))
	return nil
}

func spliceJSONMember(object []byte, key string, value []byte) ([]byte, error) {
	fields, err := walkJSONObject(object)
	if err != nil {
		return nil, err
	}
	// walkJSONObject accepted the object, so the last non-space byte is '}'.
	trimmed := bytes.TrimRight(object, " \t\r\n")
	closeAt := len(trimmed) - 1
	var buf bytes.Buffer
	buf.Write(object[:closeAt])
	if len(fields) > 0 {
		buf.WriteByte(',')
	}
	if err := appendJSONString(&buf, key); err != nil {
		return nil, err
	}
	buf.WriteByte(':')
	buf.Write(value)
	buf.Write(object[closeAt:])
	return buf.Bytes(), nil
}

func walkJSONObject(raw []byte) ([]jsonField, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, errors.New("json value is not an object")
	}
	var fields []jsonField
	seen := map[string]bool{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		// encoding/json yields object keys as strings. Reject a key containing
		// U+FFFD before duplicate detection: lone surrogates, a literal
		// U+FFFD, and \ufffd all decode to RuneError.
		key := keyTok.(string)
		if strings.ContainsRune(key, utf8.RuneError) {
			return nil, errors.New("json object key is invalid")
		}
		if seen[key] {
			return nil, errors.New("json object repeats a key")
		}
		seen[key] = true
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		fields = append(fields, jsonField{Key: key, Raw: append([]byte(nil), value...)})
	}
	tok, err = dec.Token()
	if err != nil || tok != json.Delim('}') {
		return nil, errors.New("json object is truncated")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("json object has trailing bytes")
	}
	return fields, nil
}

func walkJSONArray(raw []byte) ([][]byte, error) {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, errors.New("json value is not an array")
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, err
	}
	out := make([][]byte, len(elems))
	for i, elem := range elems {
		out[i] = append([]byte(nil), elem...)
	}
	return out, nil
}

package muse

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

var (
	ErrNoChoice            = errors.New("muse: choice not offered")
	ErrFeedbackUnsupported = errors.New("muse: this choice does not accept feedback")
	ErrStaleApproval       = errors.New("muse: approval fence is stale")
	ErrNoSession           = errors.New("muse: no session")
	ErrForbiddenMethod     = errors.New("muse: method is forbidden")
)

type RequirementRef struct {
	ApprovalID  string `json:"approvalId"`
	SourceIndex int    `json:"sourceIndex"`
}

type SuggestedPrefix struct {
	ArgvPrefix []string `json:"argvPrefix"`
	Label      string   `json:"label"`
}

// Resolution is an object, not a string. Typing it as a string would drop
// the surrounding approval params.
type Resolution struct {
	Kind       string   `json:"kind"`
	ArgvPrefix []string `json:"argvPrefix"`
	Diagnostic string   `json:"diagnostic"`
}

type Stage struct {
	Argv            []string         `json:"argv"`
	ArgvComplete    bool             `json:"argvComplete"`
	Position        int              `json:"position"`
	TotalStages     int              `json:"totalStages"`
	Requirement     RequirementRef   `json:"requirementId"`
	Resolution      Resolution       `json:"resolution"`
	SuggestedPrefix *SuggestedPrefix `json:"suggestedPrefix"`
}

type ApprovalOrigin struct {
	Kind    string `json:"kind"`
	Command string `json:"command"`
	URL     string `json:"url"`
}

type Subject struct {
	Kind          string          `json:"kind"`
	Command       string          `json:"command"`
	Path          string          `json:"path"`
	Access        string          `json:"access"`
	Host          string          `json:"host"`
	Port          int             `json:"port"`
	Protocol      string          `json:"protocol"`
	Target        string          `json:"target"`
	ToolName      string          `json:"toolName"`
	Origin        *ApprovalOrigin `json:"origin"`
	WorkspaceRoot string          `json:"workspaceRoot"`
	Stages        []Stage         `json:"stages"`
}

type Choice struct {
	ChoiceID        string `json:"choiceId"`
	Decision        string `json:"decision"`
	Label           string `json:"label"`
	Scope           string `json:"scope"`
	RulePreview     string `json:"rulePreview"`
	AcceptsFeedback bool   `json:"acceptsFeedback"`
}

// IsRejection is conservative: only the three named approvals are consent.
// Unknown decisions, timeout, denial, abort, and every other value reject.
func IsRejection(decision string) bool {
	switch decision {
	case "approved", "approvedForSession", "approvedPolicyAmendment":
		return false
	}
	return true
}

func (c Choice) IsRejection() bool { return IsRejection(c.Decision) }

type Approval struct {
	ApprovalID     string          `json:"approvalId"`
	SessionID      string          `json:"sessionId"`
	TurnID         string          `json:"turnId"`
	ItemID         string          `json:"itemId"`
	ToolCallID     string          `json:"toolCallId"`
	TaskID         string          `json:"taskId"`
	ToolName       string          `json:"toolName"`
	RawArgs        string          `json:"rawArgs"`
	JudgeEscalated bool            `json:"judgeEscalated"`
	ProtectedWrite bool            `json:"protectedWrite"`
	ViewCursor     string          `json:"viewCursor"`
	Requirement    RequirementRef  `json:"currentRequirementId"`
	Subject        Subject         `json:"subject"`
	Choices        []Choice        `json:"availableChoices"`
	Prov           json.RawMessage `json:"-"`
}

func (a Approval) Describe() string {
	var b strings.Builder
	kind := a.Subject.Kind
	if kind == "" {
		kind = "approval"
	}
	b.WriteString(kind)
	switch {
	case a.Subject.Command != "":
		b.WriteString(": " + a.Subject.Command)
	case a.Subject.Path != "":
		b.WriteString(": " + a.Subject.Path)
		if a.Subject.Access != "" {
			b.WriteString(" (" + a.Subject.Access + ")")
		}
	case a.Subject.Host != "":
		b.WriteString(": " + a.Subject.Host)
		if a.Subject.Port != 0 {
			b.WriteString(":" + strconv.Itoa(a.Subject.Port))
		}
	case a.Subject.Target != "":
		b.WriteString(": " + a.Subject.Target)
	case a.Subject.ToolName != "":
		b.WriteString(": " + a.Subject.ToolName)
	case a.ToolName != "":
		b.WriteString(": " + a.ToolName)
	}
	if n := len(a.Subject.Stages); n > 1 {
		b.WriteString(" [stage " + strconv.Itoa(a.Requirement.SourceIndex+1) + "/" + strconv.Itoa(n) + "]")
	}
	if a.JudgeEscalated {
		b.WriteString(" (escalated)")
	}
	return b.String()
}

func decodeApproval(params json.RawMessage) (Approval, error) {
	var a Approval
	if err := json.Unmarshal(params, &a); err != nil {
		return Approval{}, fmt.Errorf("malformed approval: %w", err)
	}
	if a.ApprovalID == "" {
		return Approval{}, errors.New("approval without approvalId")
	}
	if a.Subject.Kind == "" {
		return Approval{}, errors.New("approval subject without kind")
	}
	a.Prov = packProv(namedProv(params, locNotification))
	return cloneApproval(a), nil
}

type Update struct {
	ApprovalID  string          `json:"approvalId"`
	SessionID   string          `json:"sessionId"`
	ViewCursor  string          `json:"viewCursor"`
	Requirement RequirementRef  `json:"currentRequirementId"`
	Subject     Subject         `json:"subject"`
	Choices     []Choice        `json:"availableChoices"`
	ChangeKind  string          `json:"-"`
	Prov        json.RawMessage `json:"-"`
}

func decodeApprovalUpdate(params json.RawMessage) (Update, error) {
	var u Update
	if err := json.Unmarshal(params, &u); err != nil {
		return Update{}, fmt.Errorf("malformed approval/updated: %w", err)
	}
	if u.ApprovalID == "" {
		return Update{}, errors.New("approval update without approvalId")
	}
	var wrap struct {
		Change struct {
			Kind string `json:"kind"`
		} `json:"change"`
	}
	_ = json.Unmarshal(params, &wrap)
	u.ChangeKind = wrap.Change.Kind
	u.Prov = packProv(namedProv(params, locNotification))
	return cloneUpdate(u), nil
}

type Resolved struct {
	ApprovalID    string          `json:"approvalId"`
	SessionID     string          `json:"sessionId"`
	TurnID        string          `json:"turnId"`
	ItemID        string          `json:"itemId"`
	ViewCursor    string          `json:"viewCursor"`
	Decision      string          `json:"decision"`
	PolicyResult  string          `json:"policyResult"`
	ResolvedBy    string          `json:"resolvedBy"`
	DecidedBy     string          `json:"decidedByCommandId"`
	StageEvidence []Stage         `json:"stageEvidence"`
	Prov          json.RawMessage `json:"-"`
}

func decodeApprovalResolved(params json.RawMessage) (Resolved, error) {
	var r Resolved
	if err := json.Unmarshal(params, &r); err != nil {
		return Resolved{}, fmt.Errorf("malformed approval/resolved: %w", err)
	}
	if r.ApprovalID == "" {
		return Resolved{}, errors.New("approval resolution without approvalId")
	}
	r.Prov = packProv(namedProv(params, locNotification))
	return cloneResolved(r), nil
}

func (a Approval) applyUpdate(u Update) Approval {
	a.Requirement = u.Requirement
	a.Choices = cloneChoices(u.Choices)
	if u.ViewCursor != "" {
		a.ViewCursor = u.ViewCursor
	}
	if u.Subject.Kind != "" {
		a.Subject = cloneSubject(u.Subject)
	}
	a.Prov = mergePackedProv(a.Prov, u.Prov)
	return a
}

func mergePackedProv(existing json.RawMessage, extra json.RawMessage) json.RawMessage {
	var prev []provEntry
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &prev)
	}
	var more []provEntry
	if len(extra) > 0 {
		_ = json.Unmarshal(extra, &more)
	}
	return packProv(prev, more)
}

type DecideParams struct {
	SessionID   string         `json:"sessionId"`
	ApprovalID  string         `json:"approvalId"`
	ChoiceID    string         `json:"choiceId"`
	CommandID   string         `json:"commandId"`
	Requirement RequirementRef `json:"requirementId"`
	Feedback    *string        `json:"feedback,omitempty"`
}

func (a Approval) decideParams(choiceID, feedback string) (DecideParams, error) {
	var chosen *Choice
	for i := range a.Choices {
		if a.Choices[i].ChoiceID == choiceID {
			chosen = &a.Choices[i]
			break
		}
	}
	if chosen == nil {
		return DecideParams{}, fmt.Errorf("%w: %q", ErrNoChoice, choiceID)
	}
	cmdID, err := NewCommandID()
	if err != nil {
		return DecideParams{}, err
	}
	p := DecideParams{
		SessionID:   a.SessionID,
		ApprovalID:  a.ApprovalID,
		ChoiceID:    choiceID,
		CommandID:   cmdID,
		Requirement: a.Requirement,
	}
	if feedback != "" {
		if !chosen.AcceptsFeedback {
			return DecideParams{}, fmt.Errorf("%w: %q", ErrFeedbackUnsupported, choiceID)
		}
		fb := feedback
		p.Feedback = &fb
	}
	return p, nil
}

type approvalState struct {
	mu  sync.Mutex
	cur *Approval
}

func newApprovalState() *approvalState { return &approvalState{} }

func (s *approvalState) setRequested(a Approval) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := cloneApproval(a)
	s.cur = &cp
}

func (s *approvalState) pending() (Approval, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return Approval{}, false
	}
	return cloneApproval(*s.cur), true
}

func (s *approvalState) clear() {
	s.mu.Lock()
	s.cur = nil
	s.mu.Unlock()
}

func (s *approvalState) applyUpdate(params json.RawMessage) (Update, error) {
	u, err := decodeApprovalUpdate(params)
	if err != nil {
		return Update{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.cur == nil:
		rec := approvalFromUpdate(u)
		s.cur = &rec
	case s.cur.ApprovalID == u.ApprovalID:
		next := s.cur.applyUpdate(u)
		s.cur = &next
	}
	return u, nil
}

func approvalFromUpdate(u Update) Approval {
	return Approval{
		ApprovalID:  u.ApprovalID,
		SessionID:   u.SessionID,
		ViewCursor:  u.ViewCursor,
		Requirement: u.Requirement,
		Subject:     cloneSubject(u.Subject),
		Choices:     cloneChoices(u.Choices),
		Prov:        copyRaw(u.Prov),
	}
}

func (s *approvalState) applyResolved(params json.RawMessage) (Resolved, error) {
	r, err := decodeApprovalResolved(params)
	if err != nil {
		return Resolved{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != nil && s.cur.ApprovalID == r.ApprovalID {
		s.cur = nil
	}
	return r, nil
}

func (s *approvalState) prepareDecide(approvalID string, expected RequirementRef, choiceID, feedback string) (DecideParams, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil || s.cur.ApprovalID != approvalID || s.cur.Requirement != expected {
		return DecideParams{}, ErrStaleApproval
	}
	p, err := s.cur.decideParams(choiceID, feedback)
	if err != nil {
		return DecideParams{}, err
	}
	return p, nil
}

type UserInputRequest struct {
	UserInputID string
	SessionID   string
	ToolName    string
	Questions   []UserInputQuestion
}

type UserInputQuestion struct {
	ID     string `json:"id"`
	Prompt string `json:"prompt"`
}

func decodeUserInput(params json.RawMessage) (UserInputRequest, error) {
	var p struct {
		UserInputID string              `json:"userInputId"`
		RequestID   string              `json:"requestId"` // inbound alias only
		SessionID   string              `json:"sessionId"`
		ToolName    string              `json:"toolName"`
		Questions   []UserInputQuestion `json:"questions"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return UserInputRequest{}, err
	}
	id := p.UserInputID
	if id == "" {
		id = p.RequestID
	}
	out := UserInputRequest{
		UserInputID: id,
		SessionID:   p.SessionID,
		ToolName:    p.ToolName,
		Questions:   append([]UserInputQuestion(nil), p.Questions...),
	}
	return out, nil
}

func cloneApproval(a Approval) Approval {
	a.Choices = cloneChoices(a.Choices)
	a.Subject = cloneSubject(a.Subject)
	a.Prov = copyRaw(a.Prov)
	return a
}

func cloneUpdate(u Update) Update {
	u.Choices = cloneChoices(u.Choices)
	u.Subject = cloneSubject(u.Subject)
	u.Prov = copyRaw(u.Prov)
	return u
}

func cloneResolved(r Resolved) Resolved {
	r.StageEvidence = cloneStages(r.StageEvidence)
	r.Prov = copyRaw(r.Prov)
	return r
}

func cloneChoices(in []Choice) []Choice {
	if in == nil {
		return nil
	}
	out := make([]Choice, len(in))
	copy(out, in)
	return out
}

func cloneSubject(s Subject) Subject {
	s.Stages = cloneStages(s.Stages)
	if s.Origin != nil {
		origin := *s.Origin
		s.Origin = &origin
	}
	return s
}

func cloneStages(in []Stage) []Stage {
	if in == nil {
		return nil
	}
	out := make([]Stage, len(in))
	for i, st := range in {
		st.Argv = append([]string(nil), st.Argv...)
		st.Resolution.ArgvPrefix = append([]string(nil), st.Resolution.ArgvPrefix...)
		if st.SuggestedPrefix != nil {
			sp := *st.SuggestedPrefix
			sp.ArgvPrefix = append([]string(nil), sp.ArgvPrefix...)
			st.SuggestedPrefix = &sp
		}
		out[i] = st
	}
	return out
}

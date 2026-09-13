package muse

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestDecodeApprovalRequiresIDAndSubjectKind(t *testing.T) {
	if _, err := decodeApproval(rawJSON(t, `{"subject":{"kind":"command"}}`)); err == nil {
		t.Fatal("missing approvalId must fail")
	}
	if _, err := decodeApproval(rawJSON(t, `{"approvalId":"a1"}`)); err == nil {
		t.Fatal("missing subject.kind must fail")
	}
	if _, err := decodeApproval(rawJSON(t, `{"approvalId":"a1","subject":{"kind":""}}`)); err == nil {
		t.Fatal("empty subject.kind must fail")
	}
}

func TestDecodeApprovalUnknownKindAndStages(t *testing.T) {
	a, err := decodeApproval(rawJSON(t, `{
		"approvalId":"ap1","sessionId":"s1","turnId":"t1","itemId":"i1",
		"toolCallId":"c1","taskId":"k1","toolName":"shell",
		"rawArgs":"{\"cmd\":\"echo\"}","judgeEscalated":true,"protectedWrite":true,
		"viewCursor":"cur","currentRequirementId":{"approvalId":"ap1","sourceIndex":0},
		"subject":{
			"kind":"mysteryGate","command":"echo hi","path":"/tmp/x","access":"write",
			"host":"h","port":22,"protocol":"ssh","target":"tgt","toolName":"shell",
			"origin":"model","workspaceRoot":"/ws",
			"stages":[
				{"argv":["echo","hi"],"argvComplete":false,"position":1,"totalStages":2,
				 "requirementId":{"approvalId":"ap1","sourceIndex":0},
				 "resolution":{"kind":"unresolved"}},
				{"argv":["cat","f"],"argvComplete":true,"position":2,"totalStages":2,
				 "requirementId":{"approvalId":"ap1","sourceIndex":1},
				 "resolution":{"kind":"knownSafe","argvPrefix":["cat"],"diagnostic":"ok"},
				 "suggestedPrefix":{"argvPrefix":["cat"]}}
			]
		},
		"availableChoices":[
			{"choiceId":"allow_once","decision":"approved","label":"Once","scope":"once"},
			{"choiceId":"abort","decision":"abort","label":"Abort","acceptsFeedback":true}
		],
		"_meta":{"p":1}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if a.ApprovalID != "ap1" || a.Subject.Kind != "mysteryGate" {
		t.Fatalf("id/kind %+v", a)
	}
	if a.Describe() == "" || strings.EqualFold(a.Describe(), "command") {
		t.Fatalf("unknown kind needs a generic description, got %q", a.Describe())
	}
	if len(a.Subject.Stages) != 2 {
		t.Fatalf("stages=%d", len(a.Subject.Stages))
	}
	if a.Subject.Stages[0].Resolution.Kind != "unresolved" || a.Subject.Stages[1].Resolution.Kind != "knownSafe" {
		t.Fatalf("resolution objects dropped: %+v", a.Subject.Stages)
	}
	if a.Requirement.ApprovalID != "ap1" || a.Requirement.SourceIndex != 0 {
		t.Fatalf("requirement %+v", a.Requirement)
	}
	assertProvHas(t, a.Prov, "notification", "_meta")
}

func TestIsRejectionConservative(t *testing.T) {
	non := []string{"approved", "approvedForSession", "approvedPolicyAmendment"}
	rej := []string{"abort", "denied", "timeout", "mystery", ""}
	for _, d := range non {
		if IsRejection(d) {
			t.Fatalf("%s must not be a rejection", d)
		}
	}
	for _, d := range rej {
		if !IsRejection(d) {
			t.Fatalf("%s must be a rejection", d)
		}
	}
}

func TestApprovalUpdateReplacesFenceAndKeepsStages(t *testing.T) {
	st := newApprovalState()
	a, _ := decodeApproval(rawJSON(t, `{
		"approvalId":"ap1","subject":{"kind":"command","command":"echo",
			"stages":[{"argv":["echo"],"position":1,"totalStages":1,"resolution":{"kind":"unresolved"}}]},
		"currentRequirementId":{"approvalId":"ap1","sourceIndex":0},
		"availableChoices":[{"choiceId":"allow_once","decision":"approved"}]
	}`))
	st.setRequested(a)
	if _, err := st.applyUpdate(rawJSON(t, `{"change":{"kind":"mysteryChange"}}`)); err == nil {
		t.Fatal("update without approvalId must fail")
	}
	up, err := st.applyUpdate(rawJSON(t, `{
		"approvalId":"ap1","change":{"kind":"mysteryChange"},
		"currentRequirementId":{"approvalId":"ap1","sourceIndex":1},
		"availableChoices":[{"choiceId":"abort","decision":"abort","acceptsFeedback":true}],
		"subject":{"kind":"command","command":"echo",
			"stages":[
				{"argv":["echo"],"position":1,"totalStages":2,"resolution":{"kind":"unresolved"}},
				{"argv":["cat"],"position":2,"totalStages":2,"resolution":{"kind":"knownSafe"}}
			]}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if up.ChangeKind != "mysteryChange" {
		t.Fatalf("unknown change kind dropped: %q", up.ChangeKind)
	}
	p, ok := st.pending()
	if !ok || p.Requirement.SourceIndex != 1 {
		t.Fatalf("fence not refreshed: %+v", p.Requirement)
	}
	if len(p.Choices) != 1 || p.Choices[0].ChoiceID != "abort" {
		t.Fatalf("choices not replaced: %+v", p.Choices)
	}
	if len(p.Subject.Stages) != 2 {
		t.Fatal("stage evidence lost on update")
	}
}

func TestResolutionClearsPendingAndKeepsStages(t *testing.T) {
	st := newApprovalState()
	a, _ := decodeApproval(rawJSON(t, `{
		"approvalId":"ap1","subject":{"kind":"command","command":"x",
			"stages":[{"argv":["x"],"resolution":{"kind":"unresolved"}}]},
		"availableChoices":[{"choiceId":"allow_once","decision":"approved"}]
	}`))
	st.setRequested(a)
	if _, err := st.applyResolved(rawJSON(t, `{"decision":"abort"}`)); err == nil {
		t.Fatal("resolved without approvalId must fail")
	}
	res, err := st.applyResolved(rawJSON(t, `{
		"approvalId":"ap1","decision":"abort","resolvedBy":"mysteryResolver",
		"stageEvidence":[{"argv":["x"],"resolution":{"kind":"denied"}}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.ResolvedBy != "mysteryResolver" {
		t.Fatalf("unknown resolver dropped: %q", res.ResolvedBy)
	}
	if _, ok := st.pending(); ok {
		t.Fatal("only resolution may clear pending")
	}
	if len(res.StageEvidence) != 1 || res.StageEvidence[0].Resolution.Kind != "denied" {
		t.Fatalf("stage evidence lost: %+v", res.StageEvidence)
	}
}

func TestDecisionAckDoesNotClearPending(t *testing.T) {
	st := newApprovalState()
	a, _ := decodeApproval(rawJSON(t, `{
		"approvalId":"ap1","subject":{"kind":"command","command":"x"},
		"availableChoices":[{"choiceId":"allow_once","decision":"approved"}]
	}`))
	st.setRequested(a)
	if _, ok := st.pending(); !ok {
		t.Fatal("successful decide must not clear pending")
	}
}

func TestValidateChoiceAndFeedback(t *testing.T) {
	st := newApprovalState()
	a, _ := decodeApproval(rawJSON(t, `{
		"approvalId":"ap1","subject":{"kind":"command","command":"x"},
		"currentRequirementId":{"approvalId":"ap1","sourceIndex":3},
		"availableChoices":[
			{"choiceId":"allow_once","decision":"approved"},
			{"choiceId":"abort","decision":"abort","acceptsFeedback":true}
		]
	}`))
	st.setRequested(a)
	fence := RequirementRef{ApprovalID: "ap1", SourceIndex: 3}
	if _, err := st.prepareDecide("ap1", fence, "nope", ""); !errors.Is(err, ErrNoChoice) {
		t.Fatalf("unknown choice err=%v", err)
	}
	if _, err := st.prepareDecide("ap1", fence, "allow_once", "why"); !errors.Is(err, ErrFeedbackUnsupported) {
		t.Fatalf("feedback err=%v", err)
	}
	params, err := st.prepareDecide("ap1", fence, "abort", "because")
	if err != nil {
		t.Fatal(err)
	}
	if params.CommandID == "" {
		t.Fatal("Decide must mint a command id")
	}
	b, _ := json.Marshal(params)
	if !strings.Contains(string(b), `"sourceIndex":3`) {
		t.Fatalf("fence not copied: %s", b)
	}
	if !strings.Contains(string(b), "because") {
		t.Fatalf("feedback missing: %s", b)
	}
}

func TestStaleFenceAfterUpdate(t *testing.T) {
	st := newApprovalState()
	a, _ := decodeApproval(rawJSON(t, `{
		"approvalId":"ap1","subject":{"kind":"command","command":"x"},
		"currentRequirementId":{"approvalId":"ap1","sourceIndex":0},
		"availableChoices":[{"choiceId":"allow_once","decision":"approved"}]
	}`))
	st.setRequested(a)
	_, err := st.applyUpdate(rawJSON(t, `{
		"approvalId":"ap1",
		"currentRequirementId":{"approvalId":"ap1","sourceIndex":9},
		"availableChoices":[{"choiceId":"abort","decision":"abort"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	oldFence := RequirementRef{ApprovalID: "ap1", SourceIndex: 0}
	if _, err := st.prepareDecide("ap1", oldFence, "abort", ""); !errors.Is(err, ErrStaleApproval) {
		t.Fatalf("old fence err=%v, want ErrStaleApproval", err)
	}
	newFence := RequirementRef{ApprovalID: "ap1", SourceIndex: 9}
	params, err := st.prepareDecide("ap1", newFence, "abort", "")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(params)
	if strings.Contains(string(b), `"sourceIndex":0`) {
		t.Fatalf("stale fence used: %s", b)
	}
	if !strings.Contains(string(b), `"sourceIndex":9`) {
		t.Fatalf("current fence missing: %s", b)
	}
	if _, err := st.prepareDecide("ap1", newFence, "allow_once", ""); !errors.Is(err, ErrNoChoice) {
		t.Fatalf("old choice still valid: %v", err)
	}
}

func TestUpdateDoesNotHybridizeApprovals(t *testing.T) {
	st := newApprovalState()
	a, _ := decodeApproval(rawJSON(t, `{
		"approvalId":"apA","sessionId":"sA","subject":{"kind":"command","command":"echo"},
		"currentRequirementId":{"approvalId":"apA","sourceIndex":0},
		"availableChoices":[{"choiceId":"allow_once","decision":"approved"}]
	}`))
	st.setRequested(a)
	_, err := st.applyUpdate(rawJSON(t, `{
		"approvalId":"apB","sessionId":"sB",
		"currentRequirementId":{"approvalId":"apB","sourceIndex":7},
		"availableChoices":[{"choiceId":"abort","decision":"abort"}],
		"subject":{"kind":"network","host":"h"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	p, ok := st.pending()
	if !ok {
		t.Fatal("A must remain pending")
	}
	if p.ApprovalID != "apA" {
		t.Fatalf("ID became %q", p.ApprovalID)
	}
	if p.Requirement.SourceIndex != 0 || p.Requirement.ApprovalID != "apA" {
		t.Fatalf("hybrid fence: %+v", p.Requirement)
	}
	if p.Subject.Kind != "command" {
		t.Fatalf("subject mixed: %+v", p.Subject)
	}
	if len(p.Choices) != 1 || p.Choices[0].ChoiceID != "allow_once" {
		t.Fatalf("choices mixed: %+v", p.Choices)
	}
}

func TestResolutionOtherIDDoesNotClear(t *testing.T) {
	st := newApprovalState()
	a, _ := decodeApproval(rawJSON(t, `{
		"approvalId":"apA","subject":{"kind":"command","command":"x"},
		"availableChoices":[{"choiceId":"allow_once","decision":"approved"}]
	}`))
	st.setRequested(a)
	if _, err := st.applyResolved(rawJSON(t, `{"approvalId":"apB","decision":"abort"}`)); err != nil {
		t.Fatal(err)
	}
	p, ok := st.pending()
	if !ok || p.ApprovalID != "apA" {
		t.Fatalf("B's resolution cleared A: ok=%v %+v", ok, p)
	}
}

func TestUnseenUpdateRecoversWhollyFromUpdate(t *testing.T) {
	st := newApprovalState()
	_, err := st.applyUpdate(rawJSON(t, `{
		"approvalId":"apB","sessionId":"sB",
		"currentRequirementId":{"approvalId":"apB","sourceIndex":2},
		"availableChoices":[{"choiceId":"abort","decision":"abort"}],
		"subject":{"kind":"command","command":"ls"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	p, ok := st.pending()
	if !ok || p.ApprovalID != "apB" || p.Requirement.SourceIndex != 2 || p.SessionID != "sB" {
		t.Fatalf("recovered %+v", p)
	}
	if p.ApprovalID != p.Requirement.ApprovalID {
		t.Fatalf("hybrid recovered: %+v", p)
	}
}

func TestConcurrentUpdateVersusDecide(t *testing.T) {
	st := newApprovalState()
	a, _ := decodeApproval(rawJSON(t, `{
		"approvalId":"ap1","subject":{"kind":"command","command":"x"},
		"currentRequirementId":{"approvalId":"ap1","sourceIndex":0},
		"availableChoices":[{"choiceId":"abort","decision":"abort"}]
	}`))
	st.setRequested(a)
	const n = 32
	errc := make(chan error, n)
	var wg sync.WaitGroup
	wg.Add(n + 1)
	go func() {
		defer wg.Done()
		_, _ = st.applyUpdate(rawJSON(t, `{
			"approvalId":"ap1",
			"currentRequirementId":{"approvalId":"ap1","sourceIndex":1},
			"availableChoices":[{"choiceId":"abort","decision":"abort"}]
		}`))
	}()
	old := RequirementRef{ApprovalID: "ap1", SourceIndex: 0}
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			p, err := st.prepareDecide("ap1", old, "abort", "")
			if err == nil {
				if p.Requirement != old || p.ApprovalID != "ap1" {
					errc <- fmt.Errorf("sent unauthorized fence %+v", p.Requirement)
					return
				}
			} else if !errors.Is(err, ErrStaleApproval) {
				errc <- err
				return
			}
			errc <- nil
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestApprovalProvenanceOnPendingAndUpdate(t *testing.T) {
	st := newApprovalState()
	a, err := decodeApproval(rawJSON(t, `{
		"approvalId":"ap1","subject":{"kind":"command","command":"x"},
		"availableChoices":[{"choiceId":"abort","decision":"abort"}],
		"_meta":{"from":"req"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	assertProvHas(t, a.Prov, "notification", "_meta")
	st.setRequested(a)
	copy(a.Prov, bytes.Repeat([]byte("z"), len(a.Prov)))
	p, _ := st.pending()
	if bytes.Contains(p.Prov, []byte("zzz")) {
		t.Fatal("mutating decoded approval leaked into store")
	}
	copy(p.Prov, bytes.Repeat([]byte("y"), len(p.Prov)))
	p2, _ := st.pending()
	if bytes.Contains(p2.Prov, []byte("yyy")) {
		t.Fatal("mutating PendingApproval leaked")
	}
	_, err = st.applyUpdate(rawJSON(t, `{
		"approvalId":"ap1",
		"currentRequirementId":{"approvalId":"ap1","sourceIndex":1},
		"availableChoices":[{"choiceId":"abort","decision":"abort"}],
		"provenance":{"from":"upd"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	p3, _ := st.pending()
	assertProvHas(t, p3.Prov, "notification", "_meta")
	assertProvHas(t, p3.Prov, "notification", "provenance")
}

func TestApprovalDefensiveCopy(t *testing.T) {
	raw := []byte(`{"approvalId":"ap1","subject":{"kind":"command","command":"x","stages":[{"argv":["x"],"resolution":{"kind":"unresolved"}}]},"availableChoices":[{"choiceId":"a","decision":"approved"}],"_meta":{"k":"orig"}}`)
	a, err := decodeApproval(raw)
	if err != nil {
		t.Fatal(err)
	}
	raw[bytesIndex(raw, "orig")] = 'Z'
	if strings.Contains(string(a.Prov), "Zrig") {
		t.Fatal("mutating input mutated approval")
	}
	a.Choices[0].ChoiceID = "mutated"
	a.Subject.Stages[0].Argv[0] = "mutated"
	st := newApprovalState()
	st.setRequested(a)
	p, _ := st.pending()
	p.Choices[0].ChoiceID = "other"
	p2, _ := st.pending()
	if p2.Choices[0].ChoiceID == "other" {
		t.Fatal("pending snapshot aliases stored state")
	}
}

func TestChoiceIsRejectionAndDescribeBranches(t *testing.T) {
	if (Choice{Decision: "approved"}).IsRejection() {
		t.Fatal("approved")
	}
	if !(Choice{Decision: "abort"}).IsRejection() {
		t.Fatal("abort")
	}
	cases := []Approval{
		{Subject: Subject{Kind: "fileAccess", Path: "/p", Access: "write"}},
		{Subject: Subject{Kind: "network", Host: "h", Port: 443}},
		{Subject: Subject{Kind: "process", Target: "pid"}},
		{Subject: Subject{Kind: "tool", ToolName: "shell"}},
		{ToolName: "fallback", Subject: Subject{Kind: "mystery"}},
		{JudgeEscalated: true, Requirement: RequirementRef{SourceIndex: 1}, Subject: Subject{Kind: "shell", Command: "x", Stages: []Stage{{}, {}}}},
		{Subject: Subject{Kind: ""}},
	}
	for _, a := range cases {
		if a.Describe() == "" {
			t.Fatalf("empty describe for %+v", a.Subject)
		}
	}
}

func TestApprovalDecodeErrorsAndNilClones(t *testing.T) {
	if _, err := decodeApproval(json.RawMessage(`{`)); err == nil {
		t.Fatal("malformed approval")
	}
	if _, err := decodeApprovalUpdate(json.RawMessage(`{`)); err == nil {
		t.Fatal("malformed update")
	}
	if _, err := decodeApprovalResolved(json.RawMessage(`{`)); err == nil {
		t.Fatal("malformed resolved")
	}
	if _, err := decodeUserInput(json.RawMessage(`{`)); err == nil {
		t.Fatal("malformed user input")
	}
	if cloneChoices(nil) != nil || cloneStages(nil) != nil {
		t.Fatal("nil clones")
	}
	st := newApprovalState()
	if _, err := st.prepareDecide("x", RequirementRef{}, "x", ""); !errors.Is(err, ErrStaleApproval) && !errors.Is(err, ErrNoChoice) {
		t.Fatalf("empty pending: %v", err)
	}
	a := Approval{ApprovalID: "ap", Subject: Subject{Kind: "command"}, ViewCursor: "old"}
	got := a.applyUpdate(Update{ViewCursor: "", Subject: Subject{}})
	if got.ViewCursor != "old" {
		t.Fatal("empty viewCursor must not clobber")
	}
}

func TestDecodeUserInput(t *testing.T) {
	u, err := decodeUserInput(rawJSON(t, `{
		"userInputId":"r1","sessionId":"s1","toolName":"ask",
		"questions":[{"id":"q1","prompt":"name?"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if u.UserInputID != "r1" || u.SessionID != "s1" || u.ToolName != "ask" || len(u.Questions) != 1 {
		t.Fatalf("%+v", u)
	}
	alias, err := decodeUserInput(rawJSON(t, `{"requestId":"alias","sessionId":"s1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if alias.UserInputID != "alias" {
		t.Fatalf("inbound requestId alias: %+v", alias)
	}
}

func bytesIndex(b []byte, s string) int {
	return strings.Index(string(b), s)
}

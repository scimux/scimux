package codex

import (
	"encoding/json"
	"strings"
	"testing"
)

// Independently authored request covering string and object decision variants.
const syntheticApproval = `{"threadId":"fixture-thread","turnId":"fixture-turn","itemId":"fixture-command","cwd":"/fixture/workspace","reason":"Run the fixture command.","command":"printf fixture","availableDecisions":["accept",{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["printf"]}},"cancel"]}`

func TestDecodeApprovalSyntheticFrame(t *testing.T) {
	a := decodeApproval("item/commandExecution/requestApproval", []byte(syntheticApproval))
	if a.Method != "item/commandExecution/requestApproval" {
		t.Fatalf("method = %q", a.Method)
	}
	if !strings.HasPrefix(a.Reason, "Run the fixture command.") {
		t.Fatalf("reason = %q", a.Reason)
	}
	if a.ThreadID == "" || a.TurnID == "" {
		t.Fatal("thread/turn id not decoded")
	}
	if len(a.AvailableDecisions) != 3 {
		t.Fatalf("want 3 decisions, got %d", len(a.AvailableDecisions))
	}
	// Bare string enums.
	if a.AvailableDecisions[0].Key != "accept" || a.AvailableDecisions[0].Payload != nil {
		t.Fatalf("decision[0] = %+v", a.AvailableDecisions[0])
	}
	if a.AvailableDecisions[2].Key != "cancel" {
		t.Fatalf("decision[2] = %+v", a.AvailableDecisions[2])
	}
	// Object variant preserves its payload.
	amend := a.AvailableDecisions[1]
	if amend.Key != "acceptWithExecpolicyAmendment" || amend.Payload == nil {
		t.Fatalf("decision[1] = %+v", amend)
	}
	if !strings.Contains(string(amend.Payload), "execpolicy_amendment") {
		t.Fatalf("amendment payload lost: %s", amend.Payload)
	}
}

func TestDecisionIsRejection(t *testing.T) {
	for _, k := range []string{"cancel", "decline", "denied", "abort"} {
		if !(Decision{Key: k}).IsRejection() {
			t.Fatalf("%q should be a rejection", k)
		}
	}
	for _, k := range []string{"accept", "approved", "acceptForSession"} {
		if (Decision{Key: k}).IsRejection() {
			t.Fatalf("%q should not be a rejection", k)
		}
	}
}

func TestBuildDecisionResultBareKey(t *testing.T) {
	r := buildDecisionResult("accept", nil)
	b, _ := json.Marshal(r)
	if string(b) != `{"decision":"accept"}` {
		t.Fatalf("bare decision = %s", b)
	}
}

func TestBuildDecisionResultObjectVariant(t *testing.T) {
	payload := json.RawMessage(`{"execpolicy_amendment":["printf"]}`)
	r := buildDecisionResult("acceptWithExecpolicyAmendment", payload)
	b, _ := json.Marshal(r)
	want := `{"decision":{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["printf"]}}}`
	if string(b) != want {
		t.Fatalf("object decision = %s", b)
	}
}

func TestParseDecisionGarbage(t *testing.T) {
	// A malformed decision entry parses to an empty Decision, not a panic.
	d := parseDecision([]byte(`12345`))
	if d.Key != "" {
		t.Fatalf("numeric decision should yield empty key, got %+v", d)
	}
}

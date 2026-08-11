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
	r := buildDecisionResult("item/commandExecution/requestApproval", "accept", nil)
	b, _ := json.Marshal(r)
	if string(b) != `{"decision":"accept"}` {
		t.Fatalf("bare decision = %s", b)
	}
}

func TestBuildDecisionResultObjectVariant(t *testing.T) {
	payload := json.RawMessage(`{"execpolicy_amendment":["printf"]}`)
	r := buildDecisionResult("item/commandExecution/requestApproval", "acceptWithExecpolicyAmendment", payload)
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

func TestDecodeFileChangeApprovalUsesFixedDecisions(t *testing.T) {
	a := decodeApproval("item/fileChange/requestApproval", []byte(`{
		"threadId":"t","turnId":"u","itemId":"i","startedAtMs":1,
		"reason":"write file","grantRoot":"/w"
	}`))
	got := decisionKeys(a.AvailableDecisions)
	want := []string{"accept", "acceptForSession", "decline", "cancel"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("file-change decisions = %v, want %v", got, want)
	}
}

func TestBuildPermissionsApprovalResponse(t *testing.T) {
	a := decodeApproval("item/permissions/requestApproval", []byte(`{
		"threadId":"t","turnId":"u","itemId":"i","environmentId":"local",
		"startedAtMs":1,"cwd":"/w","reason":"need write",
		"permissions":{"network":null,"fileSystem":{"write":["/w"]}}
	}`))
	if got := decisionKeys(a.AvailableDecisions); strings.Join(got, ",") != "allowForTurn,allowForSession,declinePermissions" {
		t.Fatalf("permissions decisions = %v", got)
	}
	r := buildDecisionResult(a.Method, a.AvailableDecisions[0].Key, a.AvailableDecisions[0].Payload)
	b, _ := json.Marshal(r)
	if !strings.Contains(string(b), `"scope":"turn"`) || !strings.Contains(string(b), `"fileSystem":{"write":["/w"]}`) {
		t.Fatalf("permissions response = %s", b)
	}
}

func decisionKeys(ds []Decision) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Key
	}
	return out
}

// G1c: build the ask body from payload fields the codex app-server documents
// (grantRoot, permissions.fileSystem/network), never the JSON-RPC method name
// when a safer label exists. Method name remains the last-resort fallback.
func TestApprovalTitleFileChangeNamesWhatChanges(t *testing.T) {
	a := decodeApproval("item/fileChange/requestApproval", []byte(`{
		"threadId":"t","turnId":"u","itemId":"i","startedAtMs":1,
		"reason":"write file","grantRoot":"/w"
	}`))
	got := approvalTitle(a)
	if got == "" {
		t.Fatal("title must be non-empty")
	}
	if got == "item/fileChange/requestApproval" {
		t.Fatalf("title is the method name %q — must name what is changing", got)
	}
	if !strings.Contains(got, "/w") {
		t.Fatalf("title %q must name the grant root /w", got)
	}
}

func TestApprovalTitlePermissionsStatesScope(t *testing.T) {
	a := decodeApproval("item/permissions/requestApproval", []byte(`{
		"threadId":"t","turnId":"u","itemId":"i","environmentId":"local",
		"startedAtMs":1,"cwd":"/w","reason":"need write",
		"permissions":{"network":null,"fileSystem":{"write":["/w"]}}
	}`))
	got := approvalTitle(a)
	if got == "" {
		t.Fatal("title must be non-empty")
	}
	if got == "item/permissions/requestApproval" {
		t.Fatalf("title is the method name %q — must state the permission scope", got)
	}
	// Documented shape: permissions.fileSystem.write is an array of paths.
	if !strings.Contains(got, "/w") {
		t.Fatalf("title %q must mention the write path /w", got)
	}
	// Network-only request (official README: network.enabled).
	netOnly := decodeApproval("item/permissions/requestApproval", []byte(`{
		"threadId":"t","turnId":"u","itemId":"i",
		"permissions":{"network":{"enabled":true}}
	}`))
	netTitle := approvalTitle(netOnly)
	if netTitle == "" || netTitle == "item/permissions/requestApproval" {
		t.Fatalf("network-only title = %q, want a scope description", netTitle)
	}
	if !strings.Contains(strings.ToLower(netTitle), "network") {
		t.Fatalf("network-only title %q must mention network", netTitle)
	}
}

func TestApprovalTitleCommandUnchanged(t *testing.T) {
	a := decodeApproval("item/commandExecution/requestApproval", []byte(syntheticApproval))
	got := approvalTitle(a)
	if !strings.Contains(got, "printf") {
		t.Fatalf("command approval title = %q, want the command", got)
	}
	if got != a.Command {
		t.Fatalf("command approval title = %q, want exact Command %q", got, a.Command)
	}
}

// G1b: the synthetic exec approval fixture carries a reason; Pending must
// expose it so the chat key row can render the explanation above the command.
func TestPendingExposesReasonFromSyntheticFixture(t *testing.T) {
	a := decodeApproval("item/commandExecution/requestApproval", []byte(syntheticApproval))
	if !strings.HasPrefix(a.Reason, "Run the fixture command.") {
		t.Fatalf("fixture reason not decoded: %q", a.Reason)
	}
	s := &Session{
		nodeID: "n1",
		pending: []*pendingPermission{{
			seq:      1,
			approval: a,
			ch:       make(chan chosen, 1),
		}},
	}
	p, ok := s.pendingInfo()
	if !ok {
		t.Fatal("expected pending")
	}
	if p.Reason == "" {
		t.Fatal("PendingPermission.Reason is empty — reason was decoded then dropped")
	}
	if p.Reason != a.Reason {
		t.Fatalf("Reason = %q, want %q", p.Reason, a.Reason)
	}
	// Title remains the command; reason is a separate field, not mixed into Title.
	if p.Title != a.Command {
		t.Fatalf("Title = %q, want Command (reason stays separate)", p.Title)
	}
}

func TestApprovalTitleDegradesSafely(t *testing.T) {
	// P5b: known methods without usable params get a short human phrase —
	// never a JSON-RPC method name (those are not an ask the human can read).
	// Unknown / empty still degrade, never panic, never empty.
	empty := decodeApproval("item/fileChange/requestApproval", []byte(`{}`))
	if got := approvalTitle(empty); got == "" || strings.Contains(got, "/requestApproval") {
		t.Fatalf("empty fileChange title = %q, want human phrase without method path", got)
	}
	malformed := decodeApproval("item/permissions/requestApproval", []byte(`not-json`))
	if got := approvalTitle(malformed); got == "" || strings.Contains(got, "/requestApproval") {
		t.Fatalf("malformed permissions title = %q, want human phrase without method path", got)
	}
	unknown := decodeApproval("item/futureThing/requestApproval", []byte(`{"foo":1}`))
	if got := approvalTitle(unknown); got == "" || strings.Contains(got, "/requestApproval") {
		t.Fatalf("unknown method title = %q, want human fallback without method path", got)
	}
	// Completely empty Approval still yields something non-empty.
	if got := approvalTitle(Approval{}); got == "" {
		t.Fatal("zero Approval title must not be empty")
	}
}

// P5b: each known method family has a truthful generic label when payload
// fields are absent — not the wire method name.
func TestApprovalTitleHumanFallbacksNeverExposeMethodPath(t *testing.T) {
	cases := []struct {
		method string
		raw    string
		want   string
	}{
		{"item/fileChange/requestApproval", `{}`, "File changes"},
		{"applyPatchApproval", `{}`, "File changes"},
		{"item/permissions/requestApproval", `{"permissions":{}}`, "Permission change"},
		{"item/commandExecution/requestApproval", `{}`, "Command execution"},
		{"execCommandApproval", `{}`, "Command execution"},
		{"item/futureThing/requestApproval", `{}`, "approval"},
		{"", `{}`, "approval"},
	}
	for _, tc := range cases {
		a := decodeApproval(tc.method, []byte(tc.raw))
		got := approvalTitle(a)
		if got != tc.want {
			t.Errorf("method %q title = %q, want %q", tc.method, got, tc.want)
		}
		if strings.Contains(got, "/requestApproval") {
			t.Errorf("method %q title %q must not contain /requestApproval", tc.method, got)
		}
	}
}

func TestMapApprovalToolKindByMethod(t *testing.T) {
	// Kind comes from what the approval is, not merely whether Command is set.
	if got := mapApprovalToolKind(Approval{
		Method:  "item/commandExecution/requestApproval",
		Command: "ls",
	}); got != "execute" {
		t.Errorf("commandExecution = %q, want execute", got)
	}
	if got := mapApprovalToolKind(Approval{
		Method: "item/fileChange/requestApproval",
		Raw:    json.RawMessage(`{"grantRoot":"/w"}`),
	}); got != "edit" {
		t.Errorf("fileChange = %q, want edit", got)
	}
	// Permissions are prose scope descriptions → no code kind.
	if got := mapApprovalToolKind(Approval{
		Method: "item/permissions/requestApproval",
	}); got != "" {
		t.Errorf("permissions = %q, want \"\"", got)
	}
	// Unknown / empty still safe.
	if got := mapApprovalToolKind(Approval{}); got != "" {
		t.Errorf("empty = %q, want \"\"", got)
	}
	// Legacy: Command alone still maps to execute (recorded shape).
	if got := mapApprovalToolKind(Approval{Command: "ls"}); got != "execute" {
		t.Errorf("Command-only = %q, want execute", got)
	}
}

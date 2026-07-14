package codex

import "encoding/json"

// Approval is a decoded server->client approval request. Codex enumerates the
// valid decisions per request in AvailableDecisions (finding 66): there is no
// single opaque option id, so the UI must render exactly these choices. The
// method determines the decision enum (ReviewDecision vs
// CommandExecutionApprovalDecision), which is why Method is preserved.
type Approval struct {
	Method   string // e.g. "item/commandExecution/requestApproval"
	Reason   string
	Command  string
	Cwd      string
	ThreadID string
	TurnID   string
	// AvailableDecisions is the server-supplied set of valid choices. Each is
	// either a bare string enum ("accept","cancel") or a single-key object
	// variant (e.g. {"acceptWithExecpolicyAmendment":{...}}).
	AvailableDecisions []Decision
	Raw                json.RawMessage // full params, for anything not modelled
}

// Decision is one choice from AvailableDecisions. Key is the string enum or the
// object variant's single key; Payload is the variant's value (nil for bare
// strings). This is the internal typed option finding 66 asks for; the
// API-facing option (scimux's PermOption{Key,Name}) is derived from Key.
type Decision struct {
	Key     string
	Payload json.RawMessage
}

// IsRejection reports whether a decision key denies/cancels the request.
func (d Decision) IsRejection() bool {
	switch d.Key {
	case "cancel", "decline", "denied", "abort":
		return true
	}
	return false
}

// ApprovalFunc decides an approval. It receives the decoded request and returns
// the chosen decision key plus optional payload. Returning ok=false rejects
// with a JSON-RPC error (the request is left unanswered as approved). scimux's
// permission whitelist is exactly such a function.
type ApprovalFunc func(a Approval) (key string, payload json.RawMessage, ok bool)

// decodeApproval parses an approval request's params defensively.
func decodeApproval(method string, params json.RawMessage) Approval {
	var p struct {
		Reason             string            `json:"reason"`
		Command            string            `json:"command"`
		Cwd                string            `json:"cwd"`
		ThreadID           string            `json:"threadId"`
		TurnID             string            `json:"turnId"`
		AvailableDecisions []json.RawMessage `json:"availableDecisions"`
	}
	_ = json.Unmarshal(params, &p)
	a := Approval{
		Method: method, Reason: p.Reason, Command: p.Command, Cwd: p.Cwd,
		ThreadID: p.ThreadID, TurnID: p.TurnID, Raw: params,
	}
	for _, d := range p.AvailableDecisions {
		a.AvailableDecisions = append(a.AvailableDecisions, parseDecision(d))
	}
	return a
}

// parseDecision handles both a bare string enum and a single-key object variant.
func parseDecision(raw json.RawMessage) Decision {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return Decision{Key: s}
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		for k, v := range obj {
			return Decision{Key: k, Payload: v}
		}
	}
	return Decision{}
}

// buildDecisionResult renders the {"decision": ...} response body. A bare key
// yields a string decision; a key with payload yields the object variant, so
// e.g. acceptWithExecpolicyAmendment round-trips its amendment back to codex.
func buildDecisionResult(key string, payload json.RawMessage) map[string]any {
	if len(payload) == 0 {
		return map[string]any{"decision": key}
	}
	return map[string]any{"decision": map[string]json.RawMessage{key: payload}}
}

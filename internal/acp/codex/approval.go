package codex

import "encoding/json"

// Approval is a decoded server->client approval request. Codex uses different
// request/response shapes per approval method; AvailableDecisions is the
// normalized list scimux may render and later send back.
type Approval struct {
	Method   string // e.g. "item/commandExecution/requestApproval"
	Reason   string
	Command  string
	Cwd      string
	ThreadID string
	TurnID   string
	// AvailableDecisions is either the server-supplied set of valid choices
	// (command execution) or the method's fixed response enum (file changes,
	// legacy exec/patch, permissions). Each decision carries enough payload to
	// build the method-specific JSON-RPC response.
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
	case "cancel", "decline", "denied", "abort", "declinePermissions":
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
		Permissions        json.RawMessage   `json:"permissions"`
	}
	_ = json.Unmarshal(params, &p)
	a := Approval{
		Method: method, Reason: p.Reason, Command: p.Command, Cwd: p.Cwd,
		ThreadID: p.ThreadID, TurnID: p.TurnID, Raw: params,
	}
	switch method {
	case "item/commandExecution/requestApproval":
		for _, d := range p.AvailableDecisions {
			a.AvailableDecisions = append(a.AvailableDecisions, parseDecision(d))
		}
		if len(a.AvailableDecisions) == 0 {
			a.AvailableDecisions = []Decision{{Key: "accept"}, {Key: "acceptForSession"}, {Key: "decline"}, {Key: "cancel"}}
		}
	case "item/fileChange/requestApproval":
		a.AvailableDecisions = []Decision{{Key: "accept"}, {Key: "acceptForSession"}, {Key: "decline"}, {Key: "cancel"}}
	case "execCommandApproval", "applyPatchApproval":
		a.AvailableDecisions = []Decision{{Key: "approved"}, {Key: "approved_for_session"}, {Key: "denied"}, {Key: "abort"}}
	case "item/permissions/requestApproval":
		a.AvailableDecisions = permissionDecisions(p.Permissions)
	}
	return a
}

func permissionDecisions(permissions json.RawMessage) []Decision {
	return []Decision{
		{Key: "allowForTurn", Payload: permissionResponse(permissions, "turn")},
		{Key: "allowForSession", Payload: permissionResponse(permissions, "session")},
		{Key: "declinePermissions", Payload: json.RawMessage(`{"permissions":{},"scope":"turn"}`)},
	}
}

func permissionResponse(permissions json.RawMessage, scope string) json.RawMessage {
	var requested struct {
		Network    json.RawMessage `json:"network"`
		FileSystem json.RawMessage `json:"fileSystem"`
	}
	_ = json.Unmarshal(permissions, &requested)
	granted := map[string]json.RawMessage{}
	if len(requested.Network) > 0 && string(requested.Network) != "null" {
		granted["network"] = requested.Network
	}
	if len(requested.FileSystem) > 0 && string(requested.FileSystem) != "null" {
		granted["fileSystem"] = requested.FileSystem
	}
	resp := map[string]any{"permissions": granted, "scope": scope}
	b, _ := json.Marshal(resp)
	return b
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
func buildDecisionResult(method, key string, payload json.RawMessage) any {
	if method == "item/permissions/requestApproval" {
		var v any
		if len(payload) != 0 && json.Unmarshal(payload, &v) == nil {
			return v
		}
		return map[string]any{"permissions": map[string]any{}, "scope": "turn"}
	}
	if len(payload) == 0 {
		return map[string]any{"decision": key}
	}
	return map[string]any{"decision": map[string]json.RawMessage{key: payload}}
}

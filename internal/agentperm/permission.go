// Package agentperm is the shared, dependency-light permission presentation
// used by ACP, Codex, and the HTTP/UI layer. It is a leaf: it must not import
// those packages, a transport, or an SDK.
//
// Empty ToolKind, option Kind, or Reason means "unknown" — never an error,
// never a guess.
package agentperm

// Option is one answerable permission choice: the key a supervisor presses,
// the agent's human-readable label, and the option's role kind when known
// ("allow" | "allow_always" | "reject" | "reject_always" | "").
type Option struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"`
}

// Pending is the UI-facing view of one outstanding approval: a title, the
// tool's kind when known, an optional reason, and the answerable options.
//
// RequestID is an opaque stable identity for the current pending request.
// It stays fixed while the same request is pending and advances for the next
// request even when title/options match.
type Pending struct {
	RequestID string // opaque; stable while this request is pending
	Title     string
	ToolKind  string
	Reason    string // why the agent is asking; empty when unknown
	Options   []Option
}

package sessionworker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// CapabilityClaudeInteractionV1 is the optional worker RPC for a structured
// Claude question or plan. It is not part of the major-1 denominator. A new
// binary advertises it even when the chat uses a legacy hook bundle.
// Callers distinguish that advertisement from the bundle's own capability.
const CapabilityClaudeInteractionV1 = "claude-interaction-v1"

// ErrInteractionUnsupported means this worker binary or this harness cannot
// serve the interaction RPC. ErrInteractionBadRequest means the action itself
// is malformed. Stale and competing claims use ErrConflict.
var (
	ErrInteractionUnsupported = errors.New("claude interaction: unsupported")
	ErrInteractionBadRequest  = errors.New("claude interaction: bad request")
)

// InteractionOwner is implemented only by the Claude session harness. It is
// not a method of Harness. Other adapters neither implement it nor stub it.
type InteractionOwner interface {
	ClaudeInteraction(context.Context) (InteractionView, error)
	SubmitClaudeInteraction(context.Context, InteractionAction) (InteractionResult, error)
}

// InteractionView is the public projection of one owned manual interaction.
// It carries no filesystem path, raw hook document, or permission update.
type InteractionView struct {
	RequestID   string                  `json:"request_id"`
	Phase       string                  `json:"phase"`
	Tool        string                  `json:"tool"`
	Digest      string                  `json:"digest,omitempty"`
	AnswerUntil string                  `json:"answer_until,omitempty"`
	Questions   []InteractionQuestion   `json:"questions,omitempty"`
	Plan        string                  `json:"plan,omitempty"`
	Modes       []InteractionModeChoice `json:"modes,omitempty"`
	Delivery    string                  `json:"delivery,omitempty"`
	NativeKeys  bool                    `json:"native_keys"`
}

// InteractionQuestion is one server-assigned question. IDs are stable for the
// life of the request. Labels are display text, not a second identity.
type InteractionQuestion struct {
	ID      string              `json:"id"`
	Text    string              `json:"text"`
	Header  string              `json:"header,omitempty"`
	Multi   bool                `json:"multi"`
	Preview string              `json:"preview,omitempty"`
	Options []InteractionOption `json:"options"`
}

// InteractionOption is one server-assigned choice.
type InteractionOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// InteractionModeChoice is a plan-approval mode the server is willing to build.
// The browser sends the id. It never sends an updatedPermissions document.
type InteractionModeChoice struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// InteractionAction is the only body the public route and the worker RPC accept.
type InteractionAction struct {
	RequestID string                 `json:"request_id"`
	Action    string                 `json:"action"`
	Answers   []InteractionSelection `json:"answers,omitempty"`
	Feedback  string                 `json:"feedback,omitempty"`
	Mode      string                 `json:"mode,omitempty"`
}

// InteractionSelection names server-issued question and option ids. Free text
// is not matched back onto an option label.
type InteractionSelection struct {
	QuestionID string   `json:"question_id"`
	OptionIDs  []string `json:"option_ids,omitempty"`
	FreeText   string   `json:"free_text,omitempty"`
}

// InteractionResult is a queued decision. Emitted is false when the worker has
// only recorded the reply. Status accepted is not delivery to Claude.
type InteractionResult struct {
	Status    string `json:"status"`
	Delivery  string `json:"delivery"`
	RequestID string `json:"request_id"`
	Emitted   bool   `json:"emitted"`
	Phase     string `json:"phase"`
}

// SubmitClaudeInteraction calls the optional RPC. A 404 from an old worker or
// a harness that does not implement InteractionOwner is ErrInteractionUnsupported.
// The returned result is the worker's durable intent, not proof Claude consumed it.
func (c *Client) SubmitClaudeInteraction(ctx context.Context, action InteractionAction) (InteractionResult, error) {
	var out InteractionResult
	err := c.do(ctx, http.MethodPost, "/v1/claude-interaction", action, &out)
	if err != nil {
		return InteractionResult{}, classifyInteractionRPC(err)
	}
	return out, nil
}

func classifyInteractionRPC(err error) error {
	remote, ok := err.(*remoteError)
	if !ok {
		return err
	}
	switch remote.status {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrInteractionUnsupported, remote.message)
	case http.StatusBadRequest:
		return fmt.Errorf("%w: %s", ErrInteractionBadRequest, remote.message)
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrConflict, remote.message)
	default:
		return err
	}
}

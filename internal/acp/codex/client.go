package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Errors surfaced to a caller (and, later, to scimux's HTTP layer).
var (
	ErrClosed     = errors.New("codex client is closed")
	ErrTurnActive = errors.New("a turn is already in flight")
)

// Client is the high-level, transport-agnostic Codex app-server driver: it owns
// the JSON-RPC peer, routes streaming notifications into the Event sink, and
// answers approvals through the injected ApprovalFunc. It is the piece that
// moves into scimux as the sibling codexWire next to the SDK-backed acpWire.
type Client struct {
	t    Transport
	peer *peer
	sink func(Event)

	mu       sync.Mutex
	approve  ApprovalFunc
	turnDone chan turnResult
	closed   bool
	// curTurnID is the server-assigned id of the turn in flight (from the
	// turn/start result, or the turn/started notification for servers that
	// respond late). turn/interrupt requires it; cleared when the turn ends.
	// turnIDReady closes once the id is known (or the turn ends without one),
	// so an interrupt racing the turn/start response can wait instead of
	// silently skipping the wire call.
	curTurnID   string
	turnIDReady chan struct{}
	// deltas accumulates item/agentMessage/delta fragments keyed by item id.
	// Flushed on item/completed when the completed item carries no text itself
	// (some server versions stream the full answer as deltas and emit an empty
	// agentMessage in the completed notification — finding 76).
	deltas map[string]*strings.Builder

	done     chan struct{} // closed once the read loop exits (transport gone)
	doneOnce sync.Once
}

type turnResult struct {
	reason string
	err    error
}

// InitializeResult is the subset of the initialize response scimux cares about.
type InitializeResult struct {
	UserAgent      string `json:"userAgent"`
	CodexHome      string `json:"codexHome"`
	PlatformFamily string `json:"platformFamily"`
	PlatformOs     string `json:"platformOs"`
}

// ThreadInfo is the durable identity + effective config of a started thread.
// Path and Model come straight from the server (deterministic correlation, no
// discovery heuristic — settles findings 3/14/20/70). EffectivePolicy is read
// back because the server may downgrade a requested policy (approval floor).
type ThreadInfo struct {
	ID              string
	Path            string // exact rollout JSONL path
	Model           string
	EffectivePolicy string
	SandboxType     string
	ReasoningEffort string
}

// StartThreadParams configures thread/start.
type StartThreadParams struct {
	Cwd            string
	ApprovalPolicy string // "on-request" | "untrusted" | ... (may be downgraded)
	Model          string // optional override
	// Effort maps a scimux effort level onto codex's model_reasoning_effort
	// config override (the same key the CLI takes via `-c`). Empty leaves the
	// agent's configured default standing; the effective value is read back as
	// ThreadInfo.ReasoningEffort.
	Effort string
}

// NewClient wires a peer over the transport and starts the read loop. sink
// receives log Events as they are decoded (nil is allowed — events are then
// dropped). Call Initialize next.
func NewClient(t Transport, sink func(Event), trace Tracer) *Client {
	c := &Client{t: t, sink: sink, done: make(chan struct{})}
	c.peer = newPeer(t.Stdin(), trace)
	c.peer.onNotify = c.onNotify
	c.peer.onRequest = c.onRequest
	go func() {
		_ = c.peer.readLoop(t.Stdout())
		c.doneOnce.Do(func() { close(c.done) })
	}()
	return c
}

// Done is closed when the read loop exits — i.e. the transport's stdout closed
// because the codex subprocess died. The Manager watches it to flip a node's
// liveness to "exited", the app-server analogue of reaping a tmux pane.
func (c *Client) Done() <-chan struct{} { return c.done }

// SetApprovalHandler installs the approval decision function. Without one, every
// approval request is rejected with a JSON-RPC error (fail-closed).
func (c *Client) SetApprovalHandler(fn ApprovalFunc) {
	c.mu.Lock()
	c.approve = fn
	c.mu.Unlock()
}

func (c *Client) emit(ev Event) {
	if ev.Time == "" {
		ev.Time = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if c.sink != nil {
		c.sink(ev)
	}
}

// Initialize performs the handshake.
func (c *Client) Initialize(ctx context.Context, clientName, clientVersion string) (InitializeResult, error) {
	var out InitializeResult
	raw, err := c.callCtx(ctx, "initialize", map[string]any{
		"clientInfo": map[string]string{"name": clientName, "version": clientVersion},
	})
	if err != nil {
		return out, err
	}
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

// StartThread opens a thread and returns its durable identity + effective
// config. The effective approval policy is read from the response, not assumed.
func (c *Client) StartThread(ctx context.Context, p StartThreadParams) (ThreadInfo, error) {
	params := map[string]any{"cwd": p.Cwd, "approvalPolicy": p.ApprovalPolicy}
	if p.Model != "" || p.Effort != "" {
		config := map[string]any{}
		if p.Model != "" {
			config["model"] = p.Model
		}
		if p.Effort != "" {
			config["model_reasoning_effort"] = p.Effort
		}
		params["config"] = config
	}
	raw, err := c.callCtx(ctx, "thread/start", params)
	if err != nil {
		return ThreadInfo{}, err
	}
	var r struct {
		Thread struct {
			ID   string `json:"id"`
			Path string `json:"path"`
		} `json:"thread"`
		Model          string `json:"model"`
		ApprovalPolicy string `json:"approvalPolicy"`
		Sandbox        struct {
			Type string `json:"type"`
		} `json:"sandbox"`
		ReasoningEffort string `json:"reasoningEffort"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return ThreadInfo{}, fmt.Errorf("decode thread/start: %w", err)
	}
	return ThreadInfo{
		ID: r.Thread.ID, Path: r.Thread.Path, Model: r.Model,
		EffectivePolicy: r.ApprovalPolicy, SandboxType: r.Sandbox.Type,
		ReasoningEffort: r.ReasoningEffort,
	}, nil
}

// RunTurn sends a user turn and blocks until the server signals turn/completed
// (or ctx is cancelled / the peer closes). Streaming events reach the sink as
// they arrive. Only one turn may be in flight at a time.
func (c *Client) RunTurn(ctx context.Context, threadID, text string) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	if c.turnDone != nil {
		c.mu.Unlock()
		return ErrTurnActive
	}
	done := make(chan turnResult, 1)
	c.turnDone = done
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.turnDone = nil
		c.mu.Unlock()
	}()

	// The user turn is recorded by the Manager before it calls RunTurn (so a
	// log-append failure refuses the send), mirroring acp.Manager.Send. The
	// client therefore does not emit the user event itself, and onNotify skips
	// the server's userMessage echo.

	c.mu.Lock()
	c.turnIDReady = make(chan struct{})
	c.mu.Unlock()

	// turn/start runs detached from ctx so a cancel racing the response still
	// publishes the turn id (setTurnID) — an interrupt needs it to address the
	// turn that may already be running server-side.
	type startRes struct {
		raw json.RawMessage
		err error
	}
	startCh := make(chan startRes, 1)
	go func() {
		raw, err := c.peer.call("turn/start", map[string]any{
			"threadId": threadID,
			"input":    []map[string]any{{"type": "text", "text": text}},
		})
		if err == nil {
			c.setTurnID(turnIDFromResult(raw))
		}
		startCh <- startRes{raw, err}
	}()

	// cancelAndSettle: abandoning the local wait alone would leave codex
	// executing the turn server-side while scimux reports it as over — tell
	// the server to stop (turn/interrupt), then give it a moment to confirm
	// via turn/completed so a follow-up Send cannot collide with a live turn.
	cancelAndSettle := func() error {
		c.interruptTurn(threadID)
		select {
		case <-done:
		case <-time.After(interruptSettle):
		case <-c.done:
		}
		return ctx.Err()
	}

	select {
	case r := <-startCh:
		if r.err != nil {
			return r.err
		}
	case <-ctx.Done():
		return cancelAndSettle()
	case <-c.done:
		return fmt.Errorf("peer closed during turn")
	}

	select {
	case r := <-done:
		return r.err
	case <-ctx.Done():
		return cancelAndSettle()
	case <-c.done:
		return fmt.Errorf("peer closed during turn")
	}
}

// interruptSettle bounds how long an interrupted RunTurn waits for the server
// to confirm the turn is over before reporting the cancellation anyway.
var interruptSettle = 5 * time.Second

// interruptTurn asks the app-server to stop the running turn (turn/interrupt
// requires both ids). If the id is not known yet — the interrupt raced the
// turn/start response — it waits for the id (or the turn's end, or the peer's
// death) before giving up. Best-effort beyond that: without an id there is
// nothing addressable to interrupt.
func (c *Client) interruptTurn(threadID string) {
	c.mu.Lock()
	turnID, ready := c.curTurnID, c.turnIDReady
	c.mu.Unlock()
	if turnID == "" && ready != nil {
		select {
		case <-ready:
		case <-time.After(interruptSettle):
		case <-c.done:
		}
		c.mu.Lock()
		turnID = c.curTurnID
		c.mu.Unlock()
	}
	if turnID == "" {
		return
	}
	ictx, cancel := context.WithTimeout(context.Background(), interruptSettle)
	defer cancel()
	_, _ = c.callCtx(ictx, "turn/interrupt", map[string]any{
		"threadId": threadID,
		"turnId":   turnID,
	})
}

// setTurnID publishes the in-flight turn's id and unblocks any interrupt
// waiting for it. Empty ids are ignored.
func (c *Client) setTurnID(id string) {
	if id == "" {
		return
	}
	c.mu.Lock()
	c.curTurnID = id
	if c.turnIDReady != nil {
		close(c.turnIDReady)
		c.turnIDReady = nil
	}
	c.mu.Unlock()
}

// turnIDFromResult extracts the turn id from a turn/start result ({"turn":{"id":...}}).
func turnIDFromResult(res json.RawMessage) string {
	var v struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(res, &v)
	return v.Turn.ID
}

// onNotify routes streaming notifications to the sink and detects turn end.
func (c *Client) onNotify(method string, params json.RawMessage) {
	switch method {
	case "item/agentMessage/delta":
		// Buffer streamed assistant fragments keyed by item id. Some server
		// versions deliver the full answer as deltas and emit an empty text
		// field in the item/completed notification; we flush the buffer there
		// (finding 76).
		itemID, delta := decodeAgentDelta(params)
		if itemID != "" && delta != "" {
			c.mu.Lock()
			if c.deltas == nil {
				c.deltas = make(map[string]*strings.Builder)
			}
			b := c.deltas[itemID]
			if b == nil {
				b = &strings.Builder{}
				c.deltas[itemID] = b
			}
			b.WriteString(delta)
			c.mu.Unlock()
		}
	case "item/completed":
		// Flush buffered deltas before calling decodeItem: delta-only streams
		// emit an agentMessage with empty text, which decodeItem returns nil for.
		// We must still surface the assembled text (finding 76).
		var promoted string
		if itemID := decodeItemID(params); itemID != "" {
			c.mu.Lock()
			b := c.deltas[itemID]
			delete(c.deltas, itemID)
			c.mu.Unlock()
			if b != nil {
				promoted = b.String()
			}
		}
		ev := decodeItem(params, true)
		if ev == nil && promoted != "" {
			// Delta-only agentMessage: inline text was empty so decodeItem
			// returned nil; synthesise the event from buffered deltas.
			ev = &Event{T: "assistant", Text: promoted}
		} else if ev != nil && ev.T == "assistant" && strings.TrimSpace(ev.Text) == "" && promoted != "" {
			ev.Text = promoted
		}
		if ev == nil || ev.T == "user" {
			break
		}
		c.emit(*ev)
	case "thread/tokenUsage/updated":
		if u := decodeTokenUsage(params); u != nil {
			c.emit(Event{T: "usage", Usage: u})
		}
	case "turn/started":
		c.setTurnID(turnIDFromResult(params))
	case "turn/completed":
		// Safety-net flush: if the server ended the turn without emitting
		// item/completed for a buffered delta item (protocol churn or an
		// interrupted item), emit whatever text was accumulated so it is not
		// silently lost (finding 84).
		c.flushDeltas()
		c.signalTurn(turnResult{reason: "turn/completed"})
	case "turn/failed", "error":
		// Clear buffered deltas on failure; do not emit partial text as
		// assistant output for a turn that did not complete (finding 84).
		c.mu.Lock()
		c.deltas = nil
		c.mu.Unlock()
		c.emit(Event{T: "error", Error: string(params)})
		c.signalTurn(turnResult{reason: "error", err: fmt.Errorf("turn failed: %s", string(params))})
	}
}

// flushDeltas emits any buffered delta fragments as assistant events and clears
// the buffer. Called on turn/completed as a safety net for delta items whose
// item/completed was never delivered.
func (c *Client) flushDeltas() {
	c.mu.Lock()
	leftover := c.deltas
	c.deltas = nil
	c.mu.Unlock()
	for _, b := range leftover {
		if text := b.String(); text != "" {
			c.emit(Event{T: "assistant", Text: text})
		}
	}
}

func (c *Client) signalTurn(r turnResult) {
	c.mu.Lock()
	done := c.turnDone
	c.curTurnID = "" // the turn is over; nothing addressable to interrupt
	if c.turnIDReady != nil {
		close(c.turnIDReady) // unblock a waiting interrupt; it reads "" and stops
		c.turnIDReady = nil
	}
	c.mu.Unlock()
	if done != nil {
		select {
		case done <- r:
		default:
		}
	}
}

// onRequest answers server->client requests: approvals through the injected
// handler, a couple of infra requests we can satisfy, everything else rejected
// cleanly so the turn never hangs.
func (c *Client) onRequest(method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "execCommandApproval", "applyPatchApproval",
		"item/commandExecution/requestApproval",
		"item/fileChange/requestApproval",
		"item/permissions/requestApproval":
		return c.handleApproval(method, params)
	case "currentTime/read":
		return map[string]any{"currentTime": time.Now().Format(time.RFC3339)}, nil
	default:
		return nil, &rpcError{Code: -32601, Message: "unhandled server request: " + method}
	}
}

func (c *Client) handleApproval(method string, params json.RawMessage) (any, *rpcError) {
	a := decodeApproval(method, params)
	// Record the approval as a tool event (decision evidence, like scimux's
	// remote-key store records the pane's bottom lines).
	c.emit(Event{T: "tool", Tool: &ToolEvent{
		Title: a.Command, Kind: "approval", Status: "pending", RawInput: a.Reason,
	}})

	c.mu.Lock()
	fn := c.approve
	c.mu.Unlock()
	if fn == nil {
		return nil, &rpcError{Code: -32000, Message: "no approval handler; rejecting"}
	}
	key, payload, ok := fn(a)
	if !ok {
		return nil, &rpcError{Code: -32000, Message: "approval rejected by handler"}
	}
	return buildDecisionResult(a.Method, key, payload), nil
}

// callCtx runs peer.call but honours ctx cancellation.
func (c *Client) callCtx(ctx context.Context, method string, params any) (json.RawMessage, error) {
	type res struct {
		raw json.RawMessage
		err error
	}
	ch := make(chan res, 1)
	go func() {
		raw, err := c.peer.call(method, params)
		ch <- res{raw, err}
	}()
	select {
	case r := <-ch:
		return r.raw, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close tears down the transport. In-flight calls are released with an error.
func (c *Client) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return c.t.Close()
}

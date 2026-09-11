package muse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"
)

var clientNameRe = regexp.MustCompile(`^[a-z0-9_]+$`)

// Client is a stateful MSP connection. It is not an application manager:
// no procManager, settings, UI, credential discovery, or launch wiring.
type Client struct {
	tr   Transport
	peer *peer
	sink func(Event)

	mu sync.Mutex

	fingerprint string
	mismatch    bool
	caps        capabilities

	session    Session
	cursor     string
	activeTurn string
	recent     terminalHistory
	inFlight   int

	fold          *fold
	spend         Spend
	spendUE       UsageEvent
	occupancy     Occupancy
	occupancyUE   UsageEvent
	gaps          int
	catalogSource string

	approvals  *approvalState
	approvalFn func(Approval)

	rpcCtx       context.Context
	rpcCancel    context.CancelFunc
	uiCancelWait time.Duration

	doneOnce sync.Once
	done     chan struct{}
}

// recentTerminalCap bounds remembered terminal turn IDs per session.
const recentTerminalCap = 64

type terminalHistory struct {
	order   []string
	have    map[string]struct{}
	emitted map[string]struct{}
}

func (h *terminalHistory) has(id string) bool {
	_, ok := h.have[id]
	return ok
}

func (h *terminalHistory) add(id string) {
	if id == "" {
		return
	}
	if h.have == nil {
		h.have = map[string]struct{}{}
	}
	if h.emitted == nil {
		h.emitted = map[string]struct{}{}
	}
	if _, ok := h.have[id]; ok {
		return
	}
	for len(h.order) >= recentTerminalCap {
		old := h.order[0]
		h.order = h.order[1:]
		delete(h.have, old)
		delete(h.emitted, old)
	}
	h.order = append(h.order, id)
	h.have[id] = struct{}{}
}

func (h *terminalHistory) markEmitted(id string) bool {
	if !h.has(id) {
		return false
	}
	if h.emitted == nil {
		h.emitted = map[string]struct{}{}
	}
	if _, ok := h.emitted[id]; ok {
		return false
	}
	h.emitted[id] = struct{}{}
	return true
}

func (h *terminalHistory) reset() {
	h.order = nil
	h.have = map[string]struct{}{}
	h.emitted = map[string]struct{}{}
}

func (h *terminalHistory) count() int { return len(h.order) }

func NewClient(tr Transport, sink func(Event), trace Tracer) *Client {
	rpcCtx, rpcCancel := context.WithCancel(context.Background())
	c := &Client{
		tr:           tr,
		sink:         sink,
		fold:         newFold(),
		approvals:    newApprovalState(),
		rpcCtx:       rpcCtx,
		rpcCancel:    rpcCancel,
		uiCancelWait: 15 * time.Second,
		done:         make(chan struct{}),
	}
	c.peer = newPeer(tr.Stdout(), tr.Stdin())
	c.peer.trace = trace
	c.peer.onProtocol = c.onProtocol
	c.peer.onNotify = c.onNotify
	c.peer.onRequest = c.onRequest
	c.peer.start()
	go func() {
		<-c.peer.done()
		c.doneOnce.Do(func() { close(c.done) })
	}()
	return c
}

func (c *Client) Done() <-chan struct{} { return c.done }

func (c *Client) Live() error {
	select {
	case <-c.done:
		return ErrClosed
	default:
		return nil
	}
}

func (c *Client) Close() error {
	if c.rpcCancel != nil {
		c.rpcCancel()
	}
	_ = c.peer.Close()
	if c.tr != nil {
		_ = c.tr.Close()
	}
	return nil
}

func (c *Client) emit(e Event) {
	if c.sink == nil {
		return
	}
	e.Prov = copyRaw(e.Prov)
	if e.Tool != nil {
		t := *e.Tool
		e.Tool = &t
	}
	if e.Usage != nil {
		u := *e.Usage
		e.Usage = &u
	}
	if e.Time == "" {
		e.Time = time.Now().UTC().Format(time.RFC3339Nano)
	}
	c.sink(e)
}

// Initialize sends initialize then initialized. Fingerprint drift warns
// but does not block. Client names match [a-z0-9_]+ and are never museprobe.
func (c *Client) Initialize(ctx context.Context, clientName, clientVersion string) error {
	if clientName == "museprobe" || !clientNameRe.MatchString(clientName) {
		return fmt.Errorf("invalid client name %q", clientName)
	}
	if clientVersion == "" {
		clientVersion = "0"
	}
	res, err := c.peer.Call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    clientName,
			"version": clientVersion,
		},
		"capabilities": map[string]any{
			"requestedCapabilities": []string{"userShell"},
			"userInputDialogs":      false,
			"experimentalApi":       false,
		},
	})
	if err != nil {
		return err
	}
	var info InitializeResult
	if err := json.Unmarshal(res, &info); err != nil {
		return fmt.Errorf("malformed initialize result: %w", err)
	}
	fp := info.FingerprintValue()
	caps := decodeCapabilities(info.Capabilities)
	if info.UserInputDialogs != nil {
		caps.UserInputCapable = *info.UserInputDialogs
	}
	if info.SessionDurability == "ephemeral" {
		caps.Durable = false
	}
	c.mu.Lock()
	c.fingerprint = fp
	// Missing fingerprint cannot be verified: report mismatch, do not fail.
	c.mismatch = fp != PinnedFingerprint
	c.caps = caps
	c.mu.Unlock()
	return c.peer.Notify("initialized", map[string]any{})
}

func (c *Client) ServerFingerprint() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fingerprint
}

func (c *Client) FingerprintMismatch() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mismatch
}

func (c *Client) userInputCapable() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.caps.UserInputCapable
}

func (c *Client) sessionDurable() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.caps.Durable
}

func (c *Client) StartSession(ctx context.Context, p StartParams) error {
	cmdID, err := NewCommandID()
	if err != nil {
		return err
	}
	params := map[string]any{"commandId": cmdID}
	if p.Cwd != "" {
		params["workspaceRoot"] = p.Cwd
	}
	if p.Model != "" {
		params["modelId"] = p.Model
	}
	if p.ApprovalMode != "" {
		params["approvalMode"] = p.ApprovalMode
	}
	// Initial session/start omits sessionId so the server mints it.
	res, err := c.peer.Call(ctx, "session/start", params)
	if err != nil {
		return err
	}
	sess, cursor, err := parseSessionStart(res)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.session = sess
	c.cursor = cursor
	c.activeTurn = ""
	c.recent.reset()
	c.inFlight = 0
	c.fold = newFold()
	c.spend = Spend{}
	c.spendUE = UsageEvent{}
	c.occupancy = Occupancy{}
	c.occupancyUE = UsageEvent{}
	c.gaps = 0
	c.mu.Unlock()
	c.approvals.clear()
	return nil
}

func parseSessionStart(res json.RawMessage) (Session, string, error) {
	var out struct {
		SessionID  string  `json:"sessionId"`
		ViewCursor string  `json:"viewCursor"`
		Session    Session `json:"session"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return Session{}, "", fmt.Errorf("malformed session/start result: %w", err)
	}
	if out.Session.SessionID != "" {
		cur := out.ViewCursor
		if cur == "" {
			cur = out.Session.ViewCursor
		}
		return out.Session, cur, nil
	}
	if out.SessionID == "" {
		return Session{}, "", fmt.Errorf("session/start result missing sessionId")
	}
	return Session{SessionID: out.SessionID, ViewCursor: out.ViewCursor}, out.ViewCursor, nil
}

// Clear starts a fresh session on the same connection. Previous state is
// discarded only after the new start succeeds.
func (c *Client) Clear(ctx context.Context, p StartParams) error {
	if c.SessionID() == "" {
		return ErrNoSession
	}
	return c.StartSession(ctx, p)
}

func (c *Client) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session.SessionID
}

func (c *Client) ViewCursor() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cursor
}

func (c *Client) ActiveTurnID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activeTurn
}

func (c *Client) TurnActive() bool { return c.ActiveTurnID() != "" }

// StartTurn is admission only. An assistant item completing does not end
// the turn; only turn/completed does.
func (c *Client) StartTurn(ctx context.Context, prompt string) error {
	sid := c.SessionID()
	if sid == "" {
		return ErrNoSession
	}
	cmdID, err := NewCommandID()
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.inFlight++
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.inFlight > 0 {
			c.inFlight--
		}
		c.mu.Unlock()
	}()
	res, err := c.peer.callOrdered(ctx, "turn/start", map[string]any{
		"commandId": cmdID,
		"sessionId": sid,
		"ifBusy":    "queue",
		"input":     []any{map[string]any{"type": "text", "text": prompt}},
	})
	if err != nil {
		return err
	}
	var out TurnResult
	if err := json.Unmarshal(res, &out); err != nil {
		return fmt.Errorf("malformed turn/start result: %w", err)
	}
	queued := out.Disposition == "queued" || out.Status == "queued"
	started := !queued && (out.Disposition == "started" || out.StartedNewTurn || (out.Disposition == "" && out.TurnID != ""))
	if started && out.TurnID == "" {
		return fmt.Errorf("turn/start acknowledgement missing turnId")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if queued {
		return nil
	}
	if started {
		if c.recent.has(out.TurnID) {
			return nil
		}
		c.activeTurn = out.TurnID
	}
	return nil
}

func (c *Client) Interrupt(ctx context.Context) error {
	sid := c.SessionID()
	if sid == "" {
		return ErrNoSession
	}
	tid := c.ActiveTurnID()
	if tid == "" {
		return fmt.Errorf("%w: no active turn", ErrNoSession)
	}
	cmdID, err := NewCommandID()
	if err != nil {
		return err
	}
	_, err = c.peer.Call(ctx, "turn/interrupt", map[string]any{
		"commandId": cmdID,
		"sessionId": sid,
		"turnId":    tid,
	})
	return err
}

func (c *Client) Models(ctx context.Context) ([]Model, error) {
	params := map[string]any{}
	if sid := c.SessionID(); sid != "" {
		params["sessionId"] = sid
	}
	res, err := c.peer.Call(ctx, "model/list", params)
	if err != nil {
		return nil, err
	}
	var out struct {
		Source string `json:"source"`
		Models []struct {
			ID           string  `json:"id"`
			ModelID      string  `json:"modelId"`
			Name         string  `json:"name"`
			Label        string  `json:"label"`
			DisplayLabel string  `json:"displayLabel"`
			Source       string  `json:"source"`
			ProfileID    string  `json:"profileId"`
			ContextLimit *int    `json:"contextLimit"`
			OutputLimit  *int    `json:"outputLimit"`
			ReleaseDate  *string `json:"releaseDate"`
			IsDefault    bool    `json:"isDefault"`
		} `json:"models"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("malformed model/list result: %w", err)
	}
	c.mu.Lock()
	c.catalogSource = out.Source
	c.mu.Unlock()
	models := make([]Model, 0, len(out.Models))
	for _, m := range out.Models {
		id := m.ID
		if id == "" {
			id = m.ModelID
		}
		label := m.Label
		if label == "" {
			label = m.DisplayLabel
		}
		if label == "" {
			label = m.Name
		}
		models = append(models, Model{
			ID:           id,
			Name:         m.Name,
			Label:        label,
			Source:       m.Source,
			ProfileID:    m.ProfileID,
			ContextLimit: m.ContextLimit,
			OutputLimit:  m.OutputLimit,
			ReleaseDate:  m.ReleaseDate,
			IsDefault:    m.IsDefault,
		})
	}
	return models, nil
}

func (c *Client) CatalogSource() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.catalogSource
}

func (c *Client) SetApprovalHandler(fn func(Approval)) {
	c.mu.Lock()
	c.approvalFn = fn
	c.mu.Unlock()
}

func (c *Client) PendingApproval() (Approval, bool) {
	return c.approvals.pending()
}

func (c *Client) Decide(ctx context.Context, approvalID string, expected RequirementRef, choiceID, feedback string) error {
	params, err := c.approvals.prepareDecide(approvalID, expected, choiceID, feedback)
	if err != nil {
		return err
	}
	if params.SessionID == "" {
		params.SessionID = c.SessionID()
	}
	if params.SessionID == "" {
		return ErrNoSession
	}
	_, err = c.peer.Call(ctx, "approval/decide", params)
	return err
}

func (c *Client) Context() UsageEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.occupancyUE
}

func (c *Client) Spend() UsageEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.spendUE
}

func (c *Client) Gaps() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gaps
}

func (c *Client) Streaming() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fold.streaming()
}

func (c *Client) Raw(ctx context.Context, method string, params any) error {
	if method == "session/fork" || method == "session/compact" {
		return fmt.Errorf("%w: %s", ErrForbiddenMethod, method)
	}
	_, err := c.peer.Call(ctx, method, params)
	return err
}

func notifySessionID(params json.RawMessage) string {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(params, &p)
	return p.SessionID
}

func (c *Client) isForeignSession(params json.RawMessage) bool {
	sid := notifySessionID(params)
	if sid == "" {
		return false
	}
	c.mu.Lock()
	cur := c.session.SessionID
	c.mu.Unlock()
	return cur != "" && sid != cur
}

func (c *Client) onProtocol(method string, params json.RawMessage) {
	if c.isForeignSession(params) {
		return
	}
	switch method {
	case "turn/started":
		c.handleTurnStarted(params)
	case "turn/completed":
		c.noteTurnCompleted(params)
	}
}

func (c *Client) onNotify(method string, params json.RawMessage) {
	if c.isForeignSession(params) {
		return
	}
	if cur := cursorOf(params); cur != "" {
		c.mu.Lock()
		c.cursor = cur
		c.mu.Unlock()
	}
	switch method {
	case "approval/requested":
		c.handleApprovalRequested(params)
		return
	case "approval/updated":
		c.handleApprovalUpdated(params)
		return
	case "approval/resolved":
		c.handleApprovalResolved(params)
		return
	case "userInput/requested":
		c.declineUserInput(params)
		return
	case "turn/started":
		return
	case "turn/completed":
		c.emitTurnCompleted(params)
		return
	case "session/tokenUsage":
		u := decodeTokenUsage(params)
		if u == nil {
			return
		}
		next := usageToSpend(u)
		var cum struct {
			Cumulative struct {
				TotalTokens int `json:"totalTokens"`
			} `json:"cumulative"`
		}
		_ = json.Unmarshal(params, &cum)
		if cum.Cumulative.TotalTokens > next.TotalTokens {
			next.TotalTokens = cum.Cumulative.TotalTokens
		}
		c.mu.Lock()
		c.spend = accumulateSpend(c.spend, next)
		c.spendUE = UsageEvent{
			InputTokens:         c.spend.InputTokens,
			OutputTokens:        c.spend.OutputTokens,
			CachedReadTokens:    c.spend.CachedReadTokens,
			CacheCreationTokens: c.spend.CacheCreationTokens,
			ReasoningTokens:     c.spend.ReasoningTokens,
			TotalTokens:         c.spend.TotalTokens,
			Model:               c.spend.Model,
			TurnID:              c.spend.TurnID,
		}
		c.mu.Unlock()
		c.emit(Event{T: "usage", Usage: u})
		return
	case "session/contextUsage":
		u := decodeContextUsage(params)
		if u == nil {
			return
		}
		c.mu.Lock()
		c.occupancy = Occupancy{Used: u.Used, Size: u.Size}
		c.occupancyUE = UsageEvent{Used: u.Used, Size: u.Size}
		c.mu.Unlock()
		c.emit(Event{T: "usage", Usage: u})
		return
	case "view/gap":
		c.mu.Lock()
		c.gaps++
		evs := c.fold.apply(method, params)
		c.mu.Unlock()
		for _, ev := range evs {
			if ev != nil {
				c.emit(*ev)
			}
		}
		return
	}
	c.mu.Lock()
	evs := c.fold.apply(method, params)
	c.mu.Unlock()
	for _, ev := range evs {
		if ev != nil {
			c.emit(*ev)
		}
	}
}

func (c *Client) onRequest(method string, params json.RawMessage) (any, error) {
	switch method {
	case "approval/request":
		// Request form is a presentation fallback only. Decision data is
		// never returned here; the notification path is authoritative.
		raw := copyRaw(params)
		return postAck{result: map[string]any{}, fn: func() { c.handleApprovalRequested(raw) }}, nil
	case "userInput/request":
		raw := copyRaw(params)
		return postAck{result: map[string]any{}, fn: func() { c.declineUserInput(raw) }}, nil
	}
	return nil, &RPCError{Code: -32601, Message: "method not found: " + method}
}

func (c *Client) handleTurnStarted(params json.RawMessage) {
	var p struct {
		TurnID string `json:"turnId"`
	}
	if json.Unmarshal(params, &p) != nil || p.TurnID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.recent.has(p.TurnID) {
		return
	}
	if c.activeTurn != "" && c.activeTurn != p.TurnID {
		return
	}
	c.activeTurn = p.TurnID
}

func (c *Client) noteTurnCompleted(params json.RawMessage) {
	var p struct {
		TurnID string `json:"turnId"`
	}
	_ = json.Unmarshal(params, &p)
	if p.TurnID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.recent.has(p.TurnID) {
		return
	}
	if c.activeTurn != "" && c.activeTurn != p.TurnID {
		return
	}
	if c.activeTurn == p.TurnID {
		c.activeTurn = ""
		c.recent.add(p.TurnID)
		return
	}
	if c.inFlight > 0 {
		c.recent.add(p.TurnID)
	}
}

func (c *Client) emitTurnCompleted(params json.RawMessage) {
	var p struct {
		TurnID string `json:"turnId"`
	}
	_ = json.Unmarshal(params, &p)
	c.mu.Lock()
	ok := c.recent.markEmitted(p.TurnID)
	var evs []*Event
	if ok {
		evs = c.fold.apply("turn/completed", params)
	}
	c.mu.Unlock()
	if !ok {
		return
	}
	for _, ev := range evs {
		if ev != nil {
			c.emit(*ev)
		}
	}
}

func (c *Client) handleApprovalRequested(params json.RawMessage) {
	a, err := decodeApproval(params)
	if err != nil {
		c.emit(Event{T: "error", Error: err.Error()})
		return
	}
	c.present(a)
}

func (c *Client) handleApprovalUpdated(params json.RawMessage) {
	u, err := c.approvals.applyUpdate(params)
	if err != nil {
		c.emit(Event{T: "error", Error: err.Error()})
		return
	}
	p, ok := c.approvals.pending()
	if !ok || p.ApprovalID != u.ApprovalID {
		return
	}
	c.present(p)
}

func (c *Client) handleApprovalResolved(params json.RawMessage) {
	prior, ok := c.approvals.pending()
	r, err := c.approvals.applyResolved(params)
	if err != nil {
		c.emit(Event{T: "error", Error: err.Error()})
		return
	}
	prov := r.Prov
	if ok && prior.ApprovalID == r.ApprovalID {
		prov = mergePackedProv(prior.Prov, r.Prov)
	}
	c.emit(Event{T: "tool", Tool: &ToolEvent{
		ID: r.ApprovalID, Title: "approval " + r.Decision + " by " + r.ResolvedBy,
		Kind: "approval", Status: "completed",
	}, Prov: copyRaw(prov)})
}

func (c *Client) present(a Approval) {
	c.approvals.setRequested(a)
	c.mu.Lock()
	fn := c.approvalFn
	c.mu.Unlock()
	if fn != nil {
		go fn(cloneApproval(a))
	}
	c.emit(Event{T: "tool", Tool: &ToolEvent{
		ID: a.ApprovalID, Title: a.Describe(), Kind: "approval", Status: "inProgress",
		RawInput: a.RawArgs,
	}, Prov: copyRaw(a.Prov)})
}

func (c *Client) declineUserInput(params json.RawMessage) {
	u, err := decodeUserInput(params)
	if err != nil {
		c.emit(Event{T: "error", Error: "malformed user input request"})
		return
	}
	if u.UserInputID == "" {
		c.emit(Event{T: "error", Error: "interactive user input is unsupported: missing userInputId"})
		return
	}
	session := u.SessionID
	if session == "" {
		session = c.SessionID()
	}
	msg := "interactive user input is unsupported"
	if u.ToolName != "" {
		msg += " (" + u.ToolName + ")"
	}
	c.emit(Event{T: "error", Error: msg})
	if session == "" {
		c.emit(Event{T: "error", Error: "user input cancelled locally: no session id"})
		return
	}
	wait := c.uiCancelWait
	if wait <= 0 {
		wait = 15 * time.Second
	}
	parent := c.rpcCtx
	if parent == nil {
		parent = context.Background()
	}
	go func() {
		cmdID, err := NewCommandID()
		if err != nil {
			c.emit(Event{T: "error", Error: err.Error()})
			return
		}
		ctx, cancel := context.WithTimeout(parent, wait)
		defer cancel()
		_, err = c.peer.Call(ctx, "userInput/cancel", map[string]any{
			"commandId":   cmdID,
			"sessionId":   session,
			"userInputId": u.UserInputID,
			"reason":      "scimux does not present interactive user-input dialogs",
		})
		if err == nil {
			return
		}
		if errors.Is(err, ErrClosed) || errors.Is(err, context.Canceled) {
			return
		}
		c.emit(Event{T: "error", Error: "userInput/cancel failed: " + err.Error()})
	}()
}

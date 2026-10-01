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

	approvals   *approvalState
	approvalFn  func(Approval)
	sessionGen  uint64
	approvalGen uint64
	op          opGate

	rpcCtx       context.Context
	rpcCancel    context.CancelFunc
	uiCancelWait time.Duration

	sinkMu      sync.RWMutex
	sessionSink func(captured string, e Event)

	closeMu   sync.Mutex
	closing   bool
	closed    bool
	closeErr  error
	closeDone chan struct{}

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
	return newClient(tr, sink, nil, trace)
}

func newClient(tr Transport, sink func(Event), sessionSink func(string, Event), trace Tracer) *Client {
	rpcCtx, rpcCancel := context.WithCancel(context.Background())
	c := &Client{
		tr:           tr,
		sink:         sink,
		sessionSink:  sessionSink,
		fold:         newFold(),
		approvals:    newApprovalState(),
		rpcCtx:       rpcCtx,
		rpcCancel:    rpcCancel,
		uiCancelWait: 15 * time.Second,
		closeDone:    make(chan struct{}),
		done:         make(chan struct{}),
	}
	c.peer = newPeer(tr.Stdout(), tr.Stdin())
	c.peer.trace = trace
	c.peer.onProtocol = c.onProtocol
	c.peer.onScope = c.sessionSnapshot
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

func (c *Client) isClosed() bool {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	return c.closed || c.closing
}

func (c *Client) Live() error {
	if c.isClosed() {
		return ErrClosed
	}
	select {
	case <-c.done:
		return ErrClosed
	default:
	}
	if c.peer != nil {
		select {
		case <-c.peer.done():
			return ErrClosed
		default:
		}
	}
	return nil
}

func (c *Client) Close() error {
	c.closeMu.Lock()
	if c.closed {
		err := c.closeErr
		c.closeMu.Unlock()
		return err
	}
	if c.closing {
		done := c.closeDone
		c.closeMu.Unlock()
		if done != nil {
			<-done
		}
		c.closeMu.Lock()
		err := c.closeErr
		c.closeMu.Unlock()
		return err
	}
	c.closing = true
	done := c.closeDone
	if done == nil {
		done = make(chan struct{})
		c.closeDone = done
	}
	c.closeMu.Unlock()

	c.doneOnce.Do(func() {
		if c.done != nil {
			close(c.done)
		}
	})
	c.op.shutdown()
	if c.rpcCancel != nil {
		c.rpcCancel()
	}
	if c.peer != nil {
		_ = c.peer.Close()
	}
	var err error
	if c.tr != nil {
		err = c.tr.Close()
	}
	c.closeMu.Lock()
	c.closed = true
	c.closeErr = err
	c.closeMu.Unlock()
	close(done)
	return err
}

func (c *Client) emit(e Event) {
	c.dispatchSink("", e)
}

func (c *Client) stillCurrent(captured string) bool {
	if captured == "" {
		return true
	}
	return c.SessionID() == captured
}

func (c *Client) emitIfCurrent(captured string, e Event) {
	if !c.stillCurrent(captured) {
		return
	}
	if captured == "" {
		c.emit(e)
		return
	}
	c.dispatchSink(captured, e)
}

func (c *Client) dispatchSink(captured string, e Event) {
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
	c.sinkMu.RLock()
	ss := c.sessionSink
	sk := c.sink
	c.sinkMu.RUnlock()
	if ss != nil {
		ss(captured, e)
		return
	}
	if sk != nil {
		sk(e)
	}
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
	if err := c.op.acquire(ctx, c.Done()); err != nil {
		return err
	}
	defer c.op.release()
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
	// Apply runs on the peer dispatch thread so the new session is
	// installed before any later frame's onProtocol/onNotify.
	_, err = c.peer.callApply(ctx, "session/start", params, c.applySessionStart)
	return err
}

func (c *Client) applySessionStart(res json.RawMessage) error {
	sess, cursor, err := parseSessionStart(res)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.sessionGen++
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
	c.approvals.clear()
	c.mu.Unlock()
	return nil
}

// withSession runs fn only if captured still names the current session.
// Lock order: Client.mu then approvalState.mu. Callers must not invoke
// sinks, log writes, or user callbacks from fn.
func (c *Client) withSession(captured string, fn func()) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if captured != "" && c.session.SessionID != captured {
		return false
	}
	fn()
	return true
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
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.approvals.pending()
	if !ok {
		return Approval{}, false
	}
	if c.approvalGen != 0 && c.approvalGen != c.sessionGen {
		return Approval{}, false
	}
	if a.SessionID != "" && a.SessionID != c.session.SessionID {
		return Approval{}, false
	}
	return a, true
}

func (c *Client) Decide(ctx context.Context, approvalID string, expected RequirementRef, choiceID, feedback string) error {
	if err := c.op.acquire(ctx, c.Done()); err != nil {
		return err
	}
	defer c.op.release()
	c.mu.Lock()
	if c.approvalGen != 0 && c.approvalGen != c.sessionGen {
		c.mu.Unlock()
		return ErrStaleApproval
	}
	params, err := c.approvals.prepareDecide(approvalID, expected, choiceID, feedback)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	if params.SessionID == "" {
		params.SessionID = c.session.SessionID
	}
	if params.SessionID == "" {
		c.mu.Unlock()
		return ErrNoSession
	}
	if params.SessionID != c.session.SessionID {
		c.mu.Unlock()
		return ErrStaleApproval
	}
	c.mu.Unlock()
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

func (c *Client) sessionSnapshot(params json.RawMessage) (captured string, skip bool) {
	sid := notifySessionID(params)
	c.mu.Lock()
	captured = c.session.SessionID
	c.mu.Unlock()
	if sid != "" && captured != "" && sid != captured {
		return captured, true
	}
	return captured, false
}

func (c *Client) onNotify(method string, params json.RawMessage, captured string, skip bool) {
	if skip {
		return
	}
	if cur := cursorOf(params); cur != "" && c.stillCurrent(captured) {
		c.mu.Lock()
		if captured == "" || c.session.SessionID == captured {
			c.cursor = cur
		}
		c.mu.Unlock()
	}
	switch method {
	case "approval/requested":
		c.handleApprovalRequested(captured, params)
		return
	case "approval/updated":
		c.handleApprovalUpdated(captured, params)
		return
	case "approval/resolved":
		c.handleApprovalResolved(captured, params)
		return
	case "userInput/requested":
		c.declineUserInput(captured, params)
		return
	case "turn/started":
		return
	case "turn/completed":
		c.emitTurnCompleted(captured, params)
		return
	case "session/tokenUsage":
		if !c.stillCurrent(captured) {
			return
		}
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
		if captured != "" && c.session.SessionID != captured {
			c.mu.Unlock()
			return
		}
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
		c.emitIfCurrent(captured, Event{T: "usage", Usage: u})
		return
	case "session/contextUsage":
		u := decodeContextUsage(params)
		if u == nil {
			return
		}
		c.mu.Lock()
		if captured != "" && c.session.SessionID != captured {
			c.mu.Unlock()
			return
		}
		c.occupancy = Occupancy{Used: u.Used, Size: u.Size}
		c.occupancyUE = UsageEvent{Used: u.Used, Size: u.Size}
		c.mu.Unlock()
		c.emitIfCurrent(captured, Event{T: "usage", Usage: u})
		return
	case "view/gap":
		c.mu.Lock()
		if captured != "" && c.session.SessionID != captured {
			c.mu.Unlock()
			return
		}
		c.gaps++
		evs := c.fold.apply(method, params)
		c.mu.Unlock()
		for _, ev := range evs {
			if ev != nil {
				c.emitIfCurrent(captured, *ev)
			}
		}
		return
	}
	c.mu.Lock()
	if captured != "" && c.session.SessionID != captured {
		c.mu.Unlock()
		return
	}
	evs := c.fold.apply(method, params)
	c.mu.Unlock()
	for _, ev := range evs {
		if ev != nil {
			c.emitIfCurrent(captured, *ev)
		}
	}
}

func (c *Client) onRequest(method string, params json.RawMessage, captured string, skip bool) (any, error) {
	switch method {
	case "approval/request":
		// Request form is a presentation fallback only. Decision data is
		// never returned here; the notification path is authoritative.
		raw := copyRaw(params)
		return postAck{result: map[string]any{}, fn: func() {
			if skip {
				return
			}
			c.handleApprovalRequested(captured, raw)
		}}, nil
	case "userInput/request":
		raw := copyRaw(params)
		return postAck{result: map[string]any{}, fn: func() {
			if skip {
				return
			}
			c.declineUserInput(captured, raw)
		}}, nil
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

func (c *Client) emitTurnCompleted(captured string, params json.RawMessage) {
	var p struct {
		TurnID string `json:"turnId"`
	}
	_ = json.Unmarshal(params, &p)
	c.mu.Lock()
	if captured != "" && c.session.SessionID != captured {
		c.mu.Unlock()
		return
	}
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
			c.emitIfCurrent(captured, *ev)
		}
	}
}

func (c *Client) handleApprovalRequested(captured string, params json.RawMessage) {
	a, err := decodeApproval(params)
	if err != nil {
		c.emitIfCurrent(captured, Event{T: "error", Error: err.Error()})
		return
	}
	var fn func(Approval)
	if !c.withSession(captured, func() {
		c.approvals.setRequested(a)
		c.approvalGen = c.sessionGen
		fn = c.approvalFn
	}) {
		return
	}
	if fn != nil {
		go fn(cloneApproval(a))
	}
	c.emitIfCurrent(captured, Event{T: "tool", Tool: &ToolEvent{
		ID: a.ApprovalID, Title: a.Describe(), Kind: "approval", Status: "inProgress",
		RawInput: a.RawArgs,
	}, Prov: copyRaw(a.Prov)})
}

func (c *Client) handleApprovalUpdated(captured string, params json.RawMessage) {
	var (
		u   Update
		err error
		p   Approval
		ok  bool
		fn  func(Approval)
	)
	if !c.withSession(captured, func() {
		u, err = c.approvals.applyUpdate(params)
		if err != nil {
			return
		}
		p, ok = c.approvals.pending()
		if ok {
			c.approvalGen = c.sessionGen
		}
		fn = c.approvalFn
	}) {
		return
	}
	if err != nil {
		c.emitIfCurrent(captured, Event{T: "error", Error: err.Error()})
		return
	}
	if !ok || p.ApprovalID != u.ApprovalID {
		return
	}
	if fn != nil {
		go fn(cloneApproval(p))
	}
	c.emitIfCurrent(captured, Event{T: "tool", Tool: &ToolEvent{
		ID: p.ApprovalID, Title: p.Describe(), Kind: "approval", Status: "inProgress",
		RawInput: p.RawArgs,
	}, Prov: copyRaw(p.Prov)})
}

func (c *Client) handleApprovalResolved(captured string, params json.RawMessage) {
	var (
		prior Approval
		had   bool
		r     Resolved
		err   error
	)
	if !c.withSession(captured, func() {
		prior, had = c.approvals.pending()
		r, err = c.approvals.applyResolved(params)
	}) {
		return
	}
	if err != nil {
		c.emitIfCurrent(captured, Event{T: "error", Error: err.Error()})
		return
	}
	prov := r.Prov
	if had && prior.ApprovalID == r.ApprovalID {
		prov = mergePackedProv(prior.Prov, r.Prov)
	}
	c.emitIfCurrent(captured, Event{T: "tool", Tool: &ToolEvent{
		ID: r.ApprovalID, Title: "approval " + r.Decision + " by " + r.ResolvedBy,
		Kind: "approval", Status: "completed",
	}, Prov: copyRaw(prov)})
}

func (c *Client) declineUserInput(captured string, params json.RawMessage) {
	u, err := decodeUserInput(params)
	if err != nil {
		c.emitIfCurrent(captured, Event{T: "error", Error: "malformed user input request"})
		return
	}
	if u.UserInputID == "" {
		c.emitIfCurrent(captured, Event{T: "error", Error: "interactive user input is unsupported: missing userInputId"})
		return
	}
	session := u.SessionID
	if session == "" {
		session = captured
	}
	msg := "interactive user input is unsupported"
	if u.ToolName != "" {
		msg += " (" + u.ToolName + ")"
	}
	c.emitIfCurrent(captured, Event{T: "error", Error: msg})
	if session == "" {
		c.emitIfCurrent(captured, Event{T: "error", Error: "user input cancelled locally: no session id"})
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
	go c.cancelUnsupportedUserInput(captured, session, u.UserInputID, parent, wait)
}

// cancelUnsupportedUserInput runs after the unsupported-input notice. Close
// and session transitions can win while it waits for the operation gate.
func (c *Client) cancelUnsupportedUserInput(captured, session, userInputID string, parent context.Context, wait time.Duration) {
	cmdID, err := NewCommandID()
	if err != nil {
		c.emitIfCurrent(captured, Event{T: "error", Error: err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(parent, wait)
	defer cancel()
	if err := c.op.acquire(ctx, c.Done()); err != nil {
		if errors.Is(err, ErrClosed) || errors.Is(err, context.Canceled) {
			return
		}
		c.emitIfCurrent(captured, Event{T: "error", Error: "userInput/cancel failed: " + err.Error()})
		return
	}
	defer c.op.release()
	if captured != c.SessionID() {
		return
	}
	_, err = c.peer.Call(ctx, "userInput/cancel", map[string]any{
		"commandId":   cmdID,
		"sessionId":   session,
		"userInputId": userInputID,
		"reason":      "scimux does not present interactive user-input dialogs",
	})
	if err == nil {
		return
	}
	if errors.Is(err, ErrClosed) || errors.Is(err, context.Canceled) {
		return
	}
	c.emitIfCurrent(captured, Event{T: "error", Error: "userInput/cancel failed: " + err.Error()})
}

// opGate serializes session transitions with Decide and user-input
// cancellation. Close wakes waiters; the gate is never held across
// callbacks or log writes.
type opGate struct {
	mu     sync.Mutex
	held   bool
	closed bool
	wait   chan struct{}
}

func (g *opGate) acquire(ctx context.Context, interrupt <-chan struct{}) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		g.mu.Lock()
		if g.closed {
			g.mu.Unlock()
			return ErrClosed
		}
		if !g.held {
			g.held = true
			g.mu.Unlock()
			return nil
		}
		wait := g.wait
		if wait == nil {
			wait = make(chan struct{})
			g.wait = wait
		}
		g.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		case <-interrupt:
			return ErrClosed
		}
	}
}

func (g *opGate) release() {
	g.mu.Lock()
	g.held = false
	wait := g.wait
	g.wait = nil
	g.mu.Unlock()
	if wait != nil {
		close(wait)
	}
}

func (g *opGate) shutdown() {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.closed = true
	wait := g.wait
	g.wait = nil
	g.mu.Unlock()
	if wait != nil {
		close(wait)
	}
}

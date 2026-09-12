package muse

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

const maxFrame = 8 << 20

// Tracer records raw JSON-RPC lines. Callback-only; it never writes files.
// Bytes passed to Trace are a copy and may be retained by the implementation.
type Tracer interface {
	Trace(direction string, line []byte)
}

// ErrClosed is returned to every waiter when the peer is closed or the
// read loop ends.
var ErrClosed = errors.New("muse: connection closed")

// RPCError is a typed JSON-RPC error. Kind and Retryable come from the
// published MSP registry; a wire `retryable` value must not override it.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
	Method  string          `json:"-"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return "muse: rpc error"
	}
	kind := e.Kind()
	if kind == "" {
		kind = "code " + strconv.Itoa(e.Code)
	}
	if e.Method != "" {
		return "muse: " + e.Method + ": " + kind + ": " + e.Message
	}
	return "muse: " + kind + ": " + e.Message
}

var errorTable = map[int]struct {
	kind      string
	retryable bool
}{
	-32700: {"parseError", false},
	-32600: {"invalidRequest", false},
	-32601: {"methodNotFound", false},
	-32602: {"invalidParams", false},
	-32603: {"internal", false},
	-32001: {"overloaded", true},
	-32002: {"inputTooLarge", false},
	-32010: {"capabilityRequired", false},
	-32011: {"notFound", false},
	-32013: {"interrupted", false},
	-32014: {"cancelled", false},
	-32020: {"sessionNotFound", false},
	-32021: {"sessionInUse", false},
	-32022: {"sessionAmbiguous", false},
	-32023: {"forkBoundaryInvalid", false},
	-32024: {"sessionNotLoaded", false},
	-32025: {"sessionStreamMismatch", false},
	-32030: {"commandRejected", false},
	-32031: {"backpressured", true},
	-32040: {"viewTruncated", false},
	-32041: {"outputUnavailable", false},
	-32042: {"boundaryPruned", false},
	-32050: {"approvalNotFound", false},
	-32051: {"approvalAlreadyResolved", false},
	-32052: {"approvalChoiceInvalid", false},
	-32053: {"approvalRequirementStale", false},
	-32054: {"approvalReviewerUnavailable", false},
	-32055: {"userInputNotFound", false},
	-32056: {"userInputAlreadySettled", false},
	-32057: {"userInputAnswerInvalid", false},
}

// Kind is the registry name for this code, or "" for an unknown code.
func (e *RPCError) Kind() string {
	if e == nil {
		return ""
	}
	return errorTable[e.Code].kind
}

// Retryable reports the registry value. Unknown codes are not retryable.
func (e *RPCError) Retryable() bool {
	if e == nil {
		return false
	}
	return errorTable[e.Code].retryable
}

type pendingCall struct {
	ch      chan json.RawMessage
	er      chan error
	method  string
	seq     uint64
	ordered bool
	apply   func(json.RawMessage) error
}

type idClass int

const (
	idAbsent idClass = iota
	idNull
	idValidString
	idValidNumber
	idInvalid
)

type orderedKind int

const (
	orderedNotify orderedKind = iota
	orderedResponse
	orderedRequest
)

type orderedFrame struct {
	kind orderedKind
	in   inbound
	seq  uint64
}

type notifyItem struct {
	in       inbound
	captured string
	skip     bool
}

type inbound struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *RPCError       `json:"error"`
}

type notifyQueue struct {
	mu   sync.Mutex
	q    []notifyItem
	wait chan struct{}
}

// peer is newline-delimited JSON-RPC 2.0 over a reader/write-closer pair.
type peer struct {
	r     io.Reader
	rc    io.Closer
	w     io.WriteCloser
	trace Tracer

	wmu sync.Mutex

	mu        sync.Mutex
	nextID    int
	pend      map[string]*pendingCall
	closed    bool
	closeCh   chan struct{}
	closeOnce sync.Once
	readDone  chan struct{}
	readOnce  sync.Once

	onNotify   func(method string, params json.RawMessage, captured string, skip bool)
	onProtocol func(method string, params json.RawMessage)
	onRequest  func(method string, params json.RawMessage, captured string, skip bool) (any, error)
	onScope    func(params json.RawMessage) (captured string, skip bool)

	nq notifyQueue

	dispq struct {
		mu   sync.Mutex
		q    []orderedFrame
		wait chan struct{}
	}
	seq        uint64
	appliedSeq uint64
	appliedCh  chan struct{}
}

func newPeer(r io.Reader, w io.WriteCloser) *peer {
	p := &peer{
		r:         r,
		w:         w,
		nextID:    1,
		pend:      map[string]*pendingCall{},
		closeCh:   make(chan struct{}),
		readDone:  make(chan struct{}),
		appliedCh: make(chan struct{}),
	}
	if c, ok := r.(io.Closer); ok {
		p.rc = c
	}
	return p
}

func (p *peer) start() {
	go p.dispatchLoop()
	go p.notifyLoop()
	go func() { _ = p.readLoop() }()
}

func (p *peer) done() <-chan struct{} { return p.closeCh }

func (p *peer) readFinished() <-chan struct{} { return p.readDone }

func (p *peer) Close() error {
	p.fail(nil)
	return nil
}

func (p *peer) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return p.call(ctx, method, params, false, nil)
}

func (p *peer) callOrdered(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return p.call(ctx, method, params, true, nil)
}

func (p *peer) callApply(ctx context.Context, method string, params any, apply func(json.RawMessage) error) (json.RawMessage, error) {
	return p.call(ctx, method, params, false, apply)
}

func (p *peer) call(ctx context.Context, method string, params any, ordered bool, apply func(json.RawMessage) error) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrClosed
	}
	id := p.nextID
	p.nextID++
	key := canonID(json.RawMessage(strconv.Itoa(id)))
	pd := &pendingCall{
		ch:      make(chan json.RawMessage, 1),
		er:      make(chan error, 1),
		method:  method,
		ordered: ordered,
		apply:   apply,
	}
	p.pend[key] = pd
	p.mu.Unlock()

	frame := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		frame["params"] = params
	}
	if err := p.write(frame); err != nil {
		p.mu.Lock()
		delete(p.pend, key)
		p.mu.Unlock()
		return nil, err
	}

	ctxDone := ctx.Done()
	closed := p.closeCh
	for {
		select {
		case res := <-pd.ch:
			if ordered {
				p.waitClaimedApplied(pd.seq)
			}
			return res, nil
		case err := <-pd.er:
			return nil, err
		case <-ctxDone:
			if p.retirePending(key, pd) {
				return nil, ctx.Err()
			}
			ctxDone = nil
		case <-closed:
			if p.retirePending(key, pd) {
				return nil, fmt.Errorf("%w (while calling %s)", ErrClosed, method)
			}
			closed = nil
		}
	}
}

// retirePending claims a still-pending call for cancellation or Close.
// It returns false when resolve already claimed the call for apply.
func (p *peer) retirePending(key string, pd *pendingCall) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pend[key] != pd {
		return false
	}
	delete(p.pend, key)
	return true
}

func (p *peer) Notify(method string, params any) error {
	frame := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		frame["params"] = params
	}
	return p.write(frame)
}

func (p *peer) write(frame map[string]any) error {
	line, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	p.wmu.Lock()
	defer p.wmu.Unlock()

	p.mu.Lock()
	closed := p.closed
	tr := p.trace
	p.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if tr != nil {
		tr.Trace("->", append([]byte(nil), line[:len(line)-1]...))
	}
	_, err = p.w.Write(line)
	return err
}

func (p *peer) readLoop() error {
	defer p.readOnce.Do(func() { close(p.readDone) })
	defer p.fail(nil)
	sc := bufio.NewScanner(p.r)
	sc.Buffer(make([]byte, 0, 64*1024), maxFrame)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		raw := append([]byte(nil), line...)
		p.mu.Lock()
		tr := p.trace
		closed := p.closed
		p.mu.Unlock()
		if closed {
			return ErrClosed
		}
		if tr != nil {
			tr.Trace("<-", append([]byte(nil), raw...))
		}
		var in inbound
		if err := json.Unmarshal(raw, &in); err != nil {
			continue
		}
		cls := classifyRPCID(in.ID)
		if in.JSONRPC != "2.0" {
			if in.Method != "" && cls != idAbsent && cls != idNull {
				go p.replyInvalidRequest()
			}
			continue
		}
		switch {
		case in.Method != "" && (cls == idValidString || cls == idValidNumber):
			p.enqueueOrdered(orderedRequest, in)
		case in.Method != "" && cls == idInvalid:
			go p.replyInvalidRequest()
		case in.Method != "" && (cls == idAbsent || cls == idNull):
			p.enqueueOrdered(orderedNotify, in)
		case in.Method == "" && (cls == idValidString || cls == idValidNumber):
			p.enqueueOrdered(orderedResponse, in)
		}
	}
	if err := sc.Err(); err != nil {
		p.fail(err)
		return err
	}
	return nil
}

func (p *peer) resolve(in inbound) {
	hasErr := in.Error != nil
	hasRes := in.Result != nil
	if hasErr == hasRes {
		return
	}
	key := canonID(in.ID)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	pd := p.pend[key]
	delete(p.pend, key)
	p.mu.Unlock()
	if pd == nil {
		return
	}
	if hasErr {
		e := *in.Error
		e.Method = pd.method
		e.Data = copyRaw(e.Data)
		pd.er <- &e
		return
	}
	res := copyRaw(in.Result)
	if pd.apply != nil {
		var applyErr error
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					applyErr = fmt.Errorf("apply %s: %v", pd.method, rec)
				}
			}()
			applyErr = pd.apply(res)
		}()
		if applyErr != nil {
			pd.er <- applyErr
			return
		}
	}
	pd.ch <- res
}

func (p *peer) replyInvalidRequest() {
	_ = p.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      nil,
		"error":   map[string]any{"code": -32600, "message": "invalid request"},
	})
}

// postAck is a request-handler result that writes the JSON-RPC response
// first, then runs fn. Request-form approval/user-input use this so the
// empty ack cannot race with a follow-up command on the same writer.
type postAck struct {
	result any
	fn     func()
}

func (p *peer) serve(in inbound, captured string, skip bool) {
	var (
		result  any
		rpcErr  *RPCError
		afterFn func()
	)
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				rpcErr = &RPCError{Code: -32603, Message: fmt.Sprintf("internal error: %v", rec)}
			}
		}()
		if p.onRequest == nil {
			rpcErr = &RPCError{Code: -32601, Message: "method not found"}
			return
		}
		res, err := p.onRequest(in.Method, copyRaw(in.Params), captured, skip)
		if err != nil {
			var typed *RPCError
			if errors.As(err, &typed) {
				rpcErr = typed
				return
			}
			rpcErr = &RPCError{Code: -32603, Message: err.Error()}
			return
		}
		result = res
	}()
	frame := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(copyRaw(in.ID))}
	if rpcErr != nil {
		errObj := map[string]any{"code": rpcErr.Code, "message": rpcErr.Message}
		if len(rpcErr.Data) > 0 {
			errObj["data"] = rpcErr.Data
		}
		frame["error"] = errObj
	} else {
		if after, ok := result.(postAck); ok {
			result = after.result
			afterFn = after.fn
		}
		if result == nil {
			result = map[string]any{}
		}
		frame["result"] = result
	}
	_ = p.write(frame)
	if afterFn != nil {
		go afterFn()
	}
}

func (p *peer) enqueueOrdered(kind orderedKind, in inbound) {
	select {
	case <-p.closeCh:
		return
	default:
	}
	p.dispq.mu.Lock()
	defer p.dispq.mu.Unlock()
	select {
	case <-p.closeCh:
		return
	default:
	}
	p.seq++
	p.dispq.q = append(p.dispq.q, orderedFrame{kind: kind, in: in, seq: p.seq})
	if p.dispq.wait != nil {
		close(p.dispq.wait)
		p.dispq.wait = nil
	}
}

func (p *peer) dispatchLoop() {
	for {
		p.dispq.mu.Lock()
		if len(p.dispq.q) == 0 {
			wait := make(chan struct{})
			p.dispq.wait = wait
			p.dispq.mu.Unlock()
			select {
			case <-wait:
			case <-p.closeCh:
				return
			}
			continue
		}
		fr := p.dispq.q[0]
		p.dispq.q[0] = orderedFrame{}
		p.dispq.q = p.dispq.q[1:]
		p.dispq.mu.Unlock()

		select {
		case <-p.closeCh:
			return
		default:
		}
		switch fr.kind {
		case orderedNotify:
			captured, skip := p.scopeOf(fr.in.Params)
			if !skip && p.onProtocol != nil {
				func() {
					defer func() { _ = recover() }()
					p.onProtocol(fr.in.Method, copyRaw(fr.in.Params))
				}()
			}
			p.enqueueNotify(notifyItem{in: fr.in, captured: captured, skip: skip})
		case orderedResponse:
			p.mu.Lock()
			if !p.closed {
				if pd := p.pend[canonID(fr.in.ID)]; pd != nil {
					pd.seq = fr.seq
				}
			}
			p.mu.Unlock()
			p.resolve(fr.in)
		case orderedRequest:
			captured, skip := p.scopeOf(fr.in.Params)
			in := fr.in
			go p.serve(in, captured, skip)
		}
		p.advanceApplied(fr.seq)
	}
}

func (p *peer) advanceApplied(seq uint64) {
	p.mu.Lock()
	if seq > p.appliedSeq {
		p.appliedSeq = seq
	}
	ch := p.appliedCh
	p.appliedCh = make(chan struct{})
	p.mu.Unlock()
	close(ch)
}

// waitClaimedApplied waits only for the dispatcher to finish the response's
// sequence. Once resolve has claimed the pending call, later cancellation or
// Close cannot replace that response with an error. The dispatcher always
// advances the sequence after publishing a claimed response.
func (p *peer) waitClaimedApplied(seq uint64) {
	if seq == 0 {
		return
	}
	for {
		p.mu.Lock()
		if p.appliedSeq >= seq {
			p.mu.Unlock()
			return
		}
		ch := p.appliedCh
		p.mu.Unlock()
		<-ch
	}
}

func (p *peer) waitApplied(ctx context.Context, seq uint64) error {
	if seq == 0 {
		return nil
	}
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return ErrClosed
		}
		if p.appliedSeq >= seq {
			p.mu.Unlock()
			return nil
		}
		ch := p.appliedCh
		p.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		case <-p.closeCh:
			return ErrClosed
		}
	}
}

func (p *peer) scopeOf(params json.RawMessage) (string, bool) {
	if p.onScope == nil {
		return "", false
	}
	var captured string
	var skip bool
	func() {
		defer func() { _ = recover() }()
		captured, skip = p.onScope(copyRaw(params))
	}()
	return captured, skip
}

func (p *peer) enqueueNotify(item notifyItem) {
	select {
	case <-p.closeCh:
		return
	default:
	}
	p.nq.mu.Lock()
	defer p.nq.mu.Unlock()
	select {
	case <-p.closeCh:
		return
	default:
	}
	p.nq.q = append(p.nq.q, item)
	if p.nq.wait != nil {
		close(p.nq.wait)
		p.nq.wait = nil
	}
}

func (p *peer) notifyLoop() {
	for {
		p.nq.mu.Lock()
		if len(p.nq.q) == 0 {
			wait := make(chan struct{})
			p.nq.wait = wait
			p.nq.mu.Unlock()
			select {
			case <-wait:
			case <-p.closeCh:
				return
			}
			continue
		}
		item := p.nq.q[0]
		p.nq.q[0] = notifyItem{}
		p.nq.q = p.nq.q[1:]
		fn := p.onNotify
		p.nq.mu.Unlock()

		select {
		case <-p.closeCh:
			return
		default:
		}
		if fn != nil {
			func() {
				defer func() { _ = recover() }()
				fn(item.in.Method, copyRaw(item.in.Params), item.captured, item.skip)
			}()
		}
	}
}

func (p *peer) fail(cause error) {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		pend := p.pend
		p.pend = map[string]*pendingCall{}
		p.mu.Unlock()
		close(p.closeCh)
		err := ErrClosed
		if cause != nil && !errors.Is(cause, ErrClosed) {
			err = fmt.Errorf("%w: %v", ErrClosed, cause)
		}
		for _, pd := range pend {
			pd.er <- fmt.Errorf("%w (while calling %s)", err, pd.method)
		}
		_ = p.w.Close()
		if p.rc != nil {
			_ = p.rc.Close()
		}
	})
}

// classifyRPCID distinguishes absent, JSON null, valid string/number, and
// invalid present values. Presence is not inferred from canonID=="".
func classifyRPCID(raw json.RawMessage) idClass {
	if raw == nil {
		return idAbsent
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return idAbsent
	}
	if bytes.Equal(raw, []byte("null")) {
		return idNull
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return idInvalid
		}
		return idValidString
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		var n json.Number
		if json.Unmarshal(raw, &n) != nil {
			return idInvalid
		}
		return idValidNumber
	default:
		return idInvalid
	}
}

// canonID normalises a JSON-RPC id. Numeric 1 and the string "1" match;
// JSON null is not an id; the string "null" is a distinct valid id;
// non-canonical numeric strings such as "01" do not match 1.
func validRPCID(raw json.RawMessage) bool {
	cls := classifyRPCID(raw)
	return cls == idValidString || cls == idValidNumber
}

func canonID(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return ""
		}
		if isCanonicalNonNegInt(s) {
			return "n:" + s
		}
		return "s:" + s
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		var n json.Number
		if json.Unmarshal(raw, &n) != nil {
			return ""
		}
		return "n:" + string(n)
	default:
		return ""
	}
}

func isCanonicalNonNegInt(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '0' {
		return s == "0"
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func copyRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	out := make(json.RawMessage, len(raw))
	copy(out, raw)
	return out
}

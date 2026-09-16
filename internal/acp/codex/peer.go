package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// peer is a minimal newline-delimited JSON-RPC peer over a Transport's stdio,
// modelled on internal/acp's use of the ACP SDK but speaking Codex's own
// app-server protocol. Clean-room: shapes come only from the first-party
// generated JSON Schema, never GPL Codex source.
//
// Framing (verified live against codex-cli 0.144.1): one JSON object per line,
// '\n' terminated, no Content-Length. Responses omit the "jsonrpc" field. IDs
// may be a string or an int.
type peer struct {
	w   io.Writer
	log Tracer // optional frame trace; nil is fine

	// wmu serializes frame writes and is never held with mu: a write into a
	// full stdin pipe may block indefinitely, and the pending map must stay
	// reachable (deliverResponse, failAll) while it does.
	wmu sync.Mutex

	mu      sync.Mutex
	nextID  int
	pending map[string]pendingCall // keyed by canonID so string ids ("1") match int ids (1)
	closed  bool

	onRequest serverRequestHandler
	onNotify  notificationHandler
}

// Tracer records every frame crossing the wire (direction, method, raw bytes).
// The probe writes an NDJSON trace; scimux would omit it or point it at a log.
type Tracer func(dir, method string, raw []byte)

// serverRequestHandler answers a server->client request (e.g. an approval).
// A non-nil result sends a success response; a non-nil err sends a JSON-RPC
// error. This is the seam scimux's permission whitelist plugs into.
type serverRequestHandler func(method string, params json.RawMessage) (result any, err *rpcError)

// notificationHandler observes a server->client notification (streaming).
type notificationHandler func(method string, params json.RawMessage)

type rpcMessage struct {
	Method string           `json:"method,omitempty"`
	ID     *json.RawMessage `json:"id,omitempty"`
	Params json.RawMessage  `json:"params,omitempty"`
	Result json.RawMessage  `json:"result,omitempty"`
	Error  *rpcError        `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

type pendingCall struct {
	ch       chan rpcMessage
	observer func(result json.RawMessage, err error)
}

func newPeer(w io.Writer, log Tracer) *peer {
	return &peer{w: w, log: log, nextID: 1, pending: map[string]pendingCall{}}
}

// canonID converts a raw JSON id value (int or quoted string) to a canonical
// string key for the pending map. "1" and 1 both normalize to "1" so a server
// that echoes a numeric id as a quoted string still matches the pending call.
func canonID(raw *json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var n int
	if json.Unmarshal(*raw, &n) == nil {
		return strconv.Itoa(n)
	}
	var s string
	if json.Unmarshal(*raw, &s) == nil {
		if n, err := strconv.Atoi(s); err == nil {
			return strconv.Itoa(n)
		}
		return s
	}
	return string(*raw)
}

func (p *peer) trace(dir, method string, raw []byte) {
	if p.log != nil {
		p.log(dir, method, raw)
	}
}

// readLoop consumes frames until stdout closes. Server requests and
// notifications are dispatched on their own goroutines so a slow handler (an
// approval waiting on a human) never blocks the reader — the backpressure
// property finding 67 calls out. It fails every in-flight call on exit so a
// caller blocked in call() is released instead of hanging.
func (p *peer) readLoop(r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		raw := append([]byte(nil), line...)
		var m rpcMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			p.trace("err", "decode", raw)
			continue
		}
		switch {
		case m.Method != "" && m.ID != nil:
			// Server->client request (e.g. an approval): dispatch on its own
			// goroutine so a handler that blocks on a human never stalls the
			// reader — the backpressure property finding 67 calls out.
			p.trace("in-req", m.Method, raw)
			go p.dispatchRequest(m)
		case m.Method != "":
			// Notification: dispatch in order, synchronously. The wire delivers
			// streaming updates in sequence and callers rely on that order
			// (assistant text logged before turn/completed is observed).
			// Handlers must not block.
			p.trace("in-note", m.Method, raw)
			if p.onNotify != nil {
				p.onNotify(m.Method, m.Params)
			}
		case m.ID != nil:
			p.trace("in-resp", "", raw)
			p.deliverResponse(m)
		default:
			p.trace("in-?", "", raw)
		}
	}
	err := sc.Err()
	p.failAll(err)
	return err
}

// failAll releases every pending caller when the read loop ends, so a shutdown
// or crashed server surfaces as an error rather than a deadlock.
func (p *peer) failAll(err error) {
	p.mu.Lock()
	p.closed = true
	msg := "peer closed"
	if err != nil {
		msg = err.Error()
	}
	failed := make([]pendingCall, 0, len(p.pending))
	for id, call := range p.pending {
		failed = append(failed, call)
		delete(p.pending, id)
	}
	p.mu.Unlock()

	response := rpcMessage{Error: &rpcError{Code: -32000, Message: msg}}
	for _, call := range failed {
		deliverPending(call, response)
	}
}

func (p *peer) dispatchRequest(m rpcMessage) {
	var result any
	var rerr *rpcError
	if p.onRequest != nil {
		result, rerr = p.onRequest(m.Method, m.Params)
	} else {
		rerr = &rpcError{Code: -32601, Message: "no handler for " + m.Method}
	}
	resp := map[string]any{"id": m.ID}
	if rerr != nil {
		resp["error"] = rerr
	} else {
		resp["result"] = result
	}
	_ = p.send(resp)
}

func (p *peer) deliverResponse(m rpcMessage) {
	key := canonID(m.ID)
	p.mu.Lock()
	call, ok := p.pending[key]
	delete(p.pending, key)
	p.mu.Unlock()
	if ok {
		deliverPending(call, m)
	}
}

func deliverPending(call pendingCall, response rpcMessage) {
	if call.observer != nil {
		var err error
		if response.Error != nil {
			err = response.Error
		}
		call.observer(response.Result, err)
	}
	call.ch <- response
}

// call sends a client->server request and blocks for its response.
func (p *peer) call(method string, params any) (json.RawMessage, error) {
	return p.callObserved(method, params, nil)
}

// callObserved runs observer synchronously on the peer read loop when the
// response arrives, before any later wire notification is dispatched.
func (p *peer) callObserved(method string, params any, observer func(json.RawMessage, error)) (json.RawMessage, error) {
	return p.callCtxObserved(context.Background(), method, params, observer)
}

// callCtx is call with cancellation: on ctx.Done() the pending id is
// deregistered, so a timed-out call leaves no parked goroutine and no stale
// pending-map entry behind. The response channel is buffered, so a response
// that races the cancellation is dropped, not leaked.
func (p *peer) callCtx(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return p.callCtxObserved(ctx, method, params, nil)
}

func (p *peer) callCtxObserved(ctx context.Context, method string, params any, observer func(json.RawMessage, error)) (json.RawMessage, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, fmt.Errorf("peer closed")
	}
	id := p.nextID
	p.nextID++
	key := strconv.Itoa(id)
	ch := make(chan rpcMessage, 1)
	p.pending[key] = pendingCall{ch: ch, observer: observer}
	p.mu.Unlock()

	if err := p.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		p.mu.Lock()
		delete(p.pending, key)
		p.mu.Unlock()
		return nil, err
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		p.mu.Lock()
		delete(p.pending, key)
		p.mu.Unlock()
		return nil, ctx.Err()
	}
}

// notify sends a client->server notification (no id, no response expected).
func (p *peer) notify(method string, params any) error {
	return p.send(map[string]any{"method": method, "params": params})
}

func (p *peer) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	p.wmu.Lock()
	defer p.wmu.Unlock()
	p.trace("out", methodOf(v), b)
	_, err = p.w.Write(b)
	return err
}

func methodOf(v any) string {
	if mp, ok := v.(map[string]any); ok {
		if s, ok := mp["method"].(string); ok {
			return s
		}
	}
	return ""
}
